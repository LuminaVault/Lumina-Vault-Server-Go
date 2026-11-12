package passkey

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"hash"
	"math/big"

	"lumina/internal/wire"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// VerifyError is any ceremony failure. Reason names the swift-webauthn error it stands
// for (tests read it); it never reaches a client, which gets Swift's bare 500.
type VerifyError struct{ Reason string }

func (e *VerifyError) Error() string { return "passkey: " + e.Reason }

func fail(reason string) error { return &VerifyError{Reason: reason} }

// IsVerifyError reports whether err is a ceremony failure.
func IsVerifyError(err error) bool {
	var v *VerifyError
	return errors.As(err, &v)
}

// cborMode decodes as SwiftCBOR's CBOR.decode with maximumDepth 16 does: the first item
// only (trailing bytes are ignored), indefinite lengths and tags allowed, a repeated map
// key accepted (SwiftCBOR keeps the last; fxamacker may keep either, which only matters
// to a hand-built attestation object). go-webauthn's webauthncbor is CTAP2 canonical
// (depth 5, no indefinite lengths, no tags, duplicate keys refused) and would refuse
// input Swift accepts.
var cborMode, _ = cbor.DecOptions{
	MaxNestedLevels: 16,
	IndefLength:     cbor.IndefLengthAllowed,
	TagsMd:          cbor.TagsAllowed,
	DupMapKey:       cbor.DupMapKeyQuiet,
}.DecMode()

// decodeFirst decodes the first CBOR item of b and returns its length in bytes.
func decodeFirst(b []byte) (any, int, error) {
	var v any
	rest, err := cborMode.UnmarshalFirst(b, &v)
	if err != nil {
		return nil, 0, err
	}
	return v, len(b) - len(rest), nil
}

// RegistrationResponse is RegistrationCredential as the request decoded it.
type RegistrationResponse struct {
	ID                string // the "id" string, verbatim: Swift never checks it against rawId
	Type              string
	RawID             []byte
	ClientDataJSON    []byte
	AttestationObject []byte
}

// Registered is swift-webauthn's Credential, the fields WebAuthnService stores.
type Registered struct {
	// ID is `parsedData.id.urlDecoded.asString()`: the client's id string turned into
	// padded standard base64. The duplicate check looks up the id verbatim instead.
	ID        string
	PublicKey []byte // the COSE_Key bytes from the authenticator data, as sent
	SignCount uint32
}

// AssertionResponse is AuthenticationCredential as the request decoded it.
type AssertionResponse struct {
	ID                string
	Type              string
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
}

// clientData is CollectedClientData: three required strings, nothing else read
// (no crossOrigin, no topOrigin, no tokenBinding).
type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

func parseClientData(raw []byte) (clientData, error) {
	var cd clientData
	if err := wire.DecodeInto(raw, &cd); err != nil {
		return cd, fail("clientDataJSON")
	}
	return cd, nil
}

// verify is CollectedClientData.verify against every accepted origin at once: Swift
// runs a manager per origin and takes the first that verifies, and only the origin
// differs between them.
func (c Config) verifyClientData(cd clientData, challenge []byte, ceremony protocol.CeremonyType) error {
	if cd.Type != string(ceremony) {
		return fail("ceremonyTypeDoesNotMatch")
	}
	if cd.Challenge != URLEncode(challenge) {
		return fail("challengeDoesNotMatch")
	}
	if !c.acceptsOrigin(cd.Origin) {
		return fail("originDoesNotMatch")
	}
	return nil
}

type authData struct {
	rpIDHash  []byte
	flags     protocol.AuthenticatorFlags
	counter   uint32
	publicKey []byte // attested credential public key, nil without the AT flag
	attested  bool
}

const (
	minAuthDataLength = 37
	aaguidSize        = 16
	maxCredentialID   = 1023
)

// parseAuthData is AuthenticatorData(bytes:).
func parseAuthData(b []byte) (authData, error) {
	if len(b) < minAuthDataLength {
		return authData{}, fail("authDataTooShort")
	}
	ad := authData{
		rpIDHash: b[:32],
		flags:    protocol.AuthenticatorFlags(b[32]),
		counter:  binary.BigEndian.Uint32(b[33:37]),
	}
	remaining := len(b) - minAuthDataLength
	if ad.flags.HasAttestedCredentialData() {
		if len(b) <= minAuthDataLength+aaguidSize+2 {
			return authData{}, fail("attestedCredentialDataMissing")
		}
		idLength := int(binary.BigEndian.Uint16(b[53:55]))
		if idLength > maxCredentialID {
			return authData{}, fail("credentialIDTooLong")
		}
		end := 55 + idLength
		if len(b) < end {
			return authData{}, fail("credentialIDTooShort")
		}
		_, n, err := decodeFirst(b[end:])
		if err != nil {
			return authData{}, fail("invalidPublicKeyLength")
		}
		ad.attested = true
		ad.publicKey = b[end : end+n]
		remaining -= aaguidSize + 2 + idLength + n
	} else if !ad.flags.HasExtensions() && len(b) != minAuthDataLength {
		return authData{}, fail("attestedCredentialFlagNotSet")
	}
	if ad.flags.HasExtensions() {
		if remaining == 0 {
			return authData{}, fail("extensionDataMissing")
		}
		remaining = 0 // the rest is extension data, never parsed
	}
	if remaining != 0 {
		return authData{}, fail("leftOverBytesInAuthenticatorData")
	}
	return ad, nil
}

func (c Config) verifyRPAndPresence(ad authData) error {
	want := sha256.Sum256([]byte(c.RPID))
	if !bytes.Equal(want[:], ad.rpIDHash) {
		return fail("relyingPartyIDHashDoesNotMatch")
	}
	// Swift: user verification is never required (requireUserVerification defaults to
	// false and WebAuthnService does not pass it); only presence is checked.
	if !ad.flags.HasUserPresent() {
		return fail("userPresentFlagNotSet")
	}
	return nil
}

// VerifyRegistration is WebAuthnManager.finishRegistration up to the duplicate check,
// which the caller runs (it needs the database). Swift's checks, in its order.
//
// Swift: only the "none" attestation format is accepted (packed, tpm and the rest are
// commented out in the library and throw), so nothing about the authenticator is
// attested; the credential id the client sends is stored without being compared with
// rawId or with the id inside the authenticator data.
func (c Config) VerifyRegistration(challenge []byte, r RegistrationResponse) (Registered, error) {
	if r.Type != string(protocol.PublicKeyCredentialType) {
		return Registered{}, fail("invalidCredentialCreationType")
	}
	cd, err := parseClientData(r.ClientDataJSON)
	if err != nil {
		return Registered{}, err
	}
	obj, _, err := decodeFirst(r.AttestationObject)
	if err != nil {
		return Registered{}, fail("invalidAttestationObject")
	}
	m, _ := obj.(map[any]any)
	rawAuthData, ok := m["authData"].([]byte)
	if !ok {
		return Registered{}, fail("invalidAuthData")
	}
	format, ok := m["fmt"].(string)
	if !ok {
		return Registered{}, fail("invalidFmt")
	}
	attStmt, ok := m["attStmt"]
	if !ok {
		return Registered{}, fail("missingAttStmt")
	}
	ad, err := parseAuthData(rawAuthData)
	if err != nil {
		return Registered{}, err
	}

	if err := c.verifyClientData(cd, challenge, protocol.CreateCeremony); err != nil {
		return Registered{}, err
	}
	if err := c.verifyRPAndPresence(ad); err != nil {
		return Registered{}, err
	}
	if !ad.attested {
		return Registered{}, fail("attestedCredentialDataMissing")
	}
	if _, err := parseCOSEKey(ad.publicKey); err != nil {
		return Registered{}, err
	}
	// Every algorithm parseCOSEKey accepts is in SupportedAlgorithms, so Swift's
	// unsupportedCredentialPublicKeyAlgorithm cannot happen here.
	if format != string(protocol.AttestationFormatNone) {
		return Registered{}, fail("attestationVerificationNotSupported")
	}
	if stmt, ok := attStmt.(map[any]any); !ok || len(stmt) != 0 {
		return Registered{}, fail("attestationStatementMustBeEmpty")
	}
	if len(r.RawID) > maxCredentialID {
		return Registered{}, fail("credentialRawIDTooLong")
	}
	return Registered{ID: URLDecoded(r.ID), PublicKey: ad.publicKey, SignCount: ad.counter}, nil
}

// VerifyAssertion is WebAuthnManager.finishAuthentication: it returns the new sign
// count. Swift: the user handle is never compared with the account, and a counter that
// does not grow (when either side is non-zero) is a failure, not a clone warning.
func (c Config) VerifyAssertion(challenge []byte, r AssertionResponse, publicKey []byte, storedSignCount uint32) (uint32, error) {
	if r.Type != string(protocol.PublicKeyCredentialType) {
		return 0, fail("invalidAssertionCredentialType")
	}
	cd, err := parseClientData(r.ClientDataJSON)
	if err != nil {
		return 0, err
	}
	ad, err := parseAuthData(r.AuthenticatorData)
	if err != nil {
		return 0, err
	}
	if err := c.verifyClientData(cd, challenge, protocol.AssertCeremony); err != nil {
		return 0, err
	}
	if err := c.verifyRPAndPresence(ad); err != nil {
		return 0, err
	}
	if (ad.counter > 0 || storedSignCount > 0) && ad.counter <= storedSignCount {
		return 0, fail("potentialReplayAttack")
	}
	clientDataHash := sha256.Sum256(r.ClientDataJSON)
	signed := append(append([]byte{}, r.AuthenticatorData...), clientDataHash[:]...)
	key, err := parseCOSEKey(publicKey)
	if err != nil {
		return 0, err
	}
	if err := key.verify(r.Signature, signed); err != nil {
		return 0, err
	}
	return ad.counter, nil
}

type coseKey struct {
	kty  webauthncose.COSEKeyType
	alg  webauthncose.COSEAlgorithmIdentifier
	x, y []byte // EC2
	n, e []byte // RSA
}

func supported(alg webauthncose.COSEAlgorithmIdentifier) bool {
	for _, a := range SupportedAlgorithms {
		if a == alg {
			return true
		}
	}
	return false
}

// parseCOSEKey is CredentialPublicKey(publicKeyBytes:).
//
// Swift reads a key whose first byte is 0x04 (an uncompressed U2F point) with an
// off-by-one slice; from authenticator data that key is the one-byte CBOR item 4, and
// the slice traps, taking the Swift process down. Go refuses it like any other bad key.
func parseCOSEKey(b []byte) (coseKey, error) {
	obj, _, err := decodeFirst(b)
	if err != nil {
		return coseKey{}, fail("badPublicKeyBytes")
	}
	if b[0] == 0x04 {
		return coseKey{}, fail("u2fPublicKey")
	}
	m, _ := obj.(map[any]any)
	kty, ok := m[uint64(1)].(uint64)
	if !ok || kty < uint64(webauthncose.OctetKey) || kty > uint64(webauthncose.RSAKey) {
		return coseKey{}, fail("invalidKeyType")
	}
	algRaw, ok := m[uint64(3)].(int64)
	if !ok || algRaw >= 0 {
		return coseKey{}, fail("invalidAlgorithm")
	}
	key := coseKey{kty: webauthncose.COSEKeyType(kty), alg: webauthncose.COSEAlgorithmIdentifier(algRaw)}
	if !supported(key.alg) {
		return coseKey{}, fail("unsupportedCOSEAlgorithm")
	}
	switch key.kty {
	case webauthncose.EllipticKey:
		crv, ok := m[int64(-1)].(uint64)
		if !ok || crv < uint64(webauthncose.P256) || crv > uint64(webauthncose.P521) {
			return coseKey{}, fail("invalidCurve")
		}
		if key.x, ok = m[int64(-2)].([]byte); !ok {
			return coseKey{}, fail("invalidXCoordinate")
		}
		if key.y, ok = m[int64(-3)].([]byte); !ok {
			return coseKey{}, fail("invalidYCoordinate")
		}
	case webauthncose.RSAKey:
		if key.n, ok = m[int64(-1)].([]byte); !ok {
			return coseKey{}, fail("invalidModulus")
		}
		if key.e, ok = m[int64(-2)].([]byte); !ok {
			return coseKey{}, fail("invalidExponent")
		}
	default: // OKP: swift-webauthn has the type but throws
		return coseKey{}, fail("unsupported")
	}
	return key, nil
}

// verify is EC2PublicKey.verify and RSAPublicKeyData.verify.
//
// EC2: the algorithm picks the curve and the hash (the key's crv is not consulted), the
// point is x||y as swift-crypto's rawRepresentation, the signature DER.
//
// RSA: Swift's _RSA.Signing.PublicKey.isValidSignature(_:for:padding:) always hashes
// with SHA-256, so RS1, RS384, RS512, PS384 and PS512 verify only a SHA-256 signature.
// Kept. PKCS#1 v1.5 for the RS algorithms, PSS with a digest-length salt for the PS
// ones; swift-crypto refuses a modulus under 2048 bits.
func (k coseKey) verify(sig, data []byte) error {
	switch k.kty {
	case webauthncose.EllipticKey:
		var (
			curve elliptic.Curve
			h     hash.Hash
		)
		switch k.alg {
		case webauthncose.AlgES256:
			curve, h = elliptic.P256(), sha256.New()
		case webauthncose.AlgES384:
			curve, h = elliptic.P384(), sha512.New384()
		case webauthncose.AlgES512:
			curve, h = elliptic.P521(), sha512.New()
		default:
			return fail("unsupportedCredentialPublicKeyAlgorithm")
		}
		raw := append(append([]byte{}, k.x...), k.y...)
		if len(raw) != 2*((curve.Params().BitSize+7)/8) {
			return fail("invalidPublicKey")
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, append([]byte{4}, raw...))
		if err != nil {
			return fail("invalidPublicKey")
		}
		h.Write(data)
		if !ecdsa.VerifyASN1(pub, h.Sum(nil), sig) {
			return fail("invalidSignature")
		}
		return nil
	case webauthncose.RSAKey:
		pss := false
		switch k.alg {
		case webauthncose.AlgRS1, webauthncose.AlgRS256, webauthncose.AlgRS384, webauthncose.AlgRS512:
		case webauthncose.AlgPS256, webauthncose.AlgPS384, webauthncose.AlgPS512:
			pss = true
		default:
			return fail("unsupportedCOSEAlgorithmForRSAPublicKey")
		}
		n := new(big.Int).SetBytes(k.n)
		e := new(big.Int).SetBytes(k.e)
		if n.BitLen() < 2048 || !e.IsInt64() || e.Int64() > 1<<31-1 {
			return fail("invalidPublicKey")
		}
		pub := &rsa.PublicKey{N: n, E: int(e.Int64())}
		digest := sha256.Sum256(data)
		var err error
		if pss {
			err = rsa.VerifyPSS(pub, crypto.SHA256, digest[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		} else {
			err = rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
		}
		if err != nil {
			return fail("invalidSignature")
		}
		return nil
	}
	return fail("unsupported")
}
