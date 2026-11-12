// Package passkeytest is a software authenticator for tests: an ES256 key made with
// Go's crypto that answers registration with a "none" attestation and sign-in with a
// signed assertion, the bytes a browser or iOS would send. No device, no real key.
package passkeytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// Authenticator holds one credential. Fields are plain so a test can bend any of them.
type Authenticator struct {
	Key          *ecdsa.PrivateKey
	CredentialID []byte
	RPID         string
	Origin       string
	Counter      uint32 // the next ceremony's counter
	Flags        protocol.AuthenticatorFlags
	// Format and AttStmt are the attestation statement; "none" and {} by default.
	Format  string
	AttStmt map[string]any
}

// New makes an ES256 authenticator for rpID and origin with a 16-byte credential id.
func New(rpID, origin string) *Authenticator {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &Authenticator{
		Key: key, CredentialID: id, RPID: rpID, Origin: origin,
		Flags:  protocol.FlagUserPresent | protocol.FlagUserVerified,
		Format: "none", AttStmt: map[string]any{},
	}
}

// B64URL is base64url without padding, what clients send.
func B64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// COSEKey is the credential public key as a COSE_Key (EC2, ES256, P-256).
func (a *Authenticator) COSEKey() []byte {
	pub, err := a.Key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		panic(err)
	}
	b, err := webauthncbor.Marshal(map[int]any{
		1:  int(webauthncose.EllipticKey),
		3:  int(webauthncose.AlgES256),
		-1: int(webauthncose.P256),
		-2: pub[1:33],
		-3: pub[33:65],
	})
	if err != nil {
		panic(err)
	}
	return b
}

// ClientData is clientDataJSON for a ceremony type and a base64url challenge.
func (a *Authenticator) ClientData(ceremony protocol.CeremonyType, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": string(ceremony), "challenge": challenge, "origin": a.Origin, "crossOrigin": false})
	return b
}

func (a *Authenticator) authDataHead(flags protocol.AuthenticatorFlags) []byte {
	rp := sha256.Sum256([]byte(a.RPID))
	out := append([]byte{}, rp[:]...)
	out = append(out, byte(flags))
	return binary.BigEndian.AppendUint32(out, a.Counter)
}

// RegistrationAuthData is authenticator data with the AT flag and the attested credential.
func (a *Authenticator) RegistrationAuthData(publicKey []byte) []byte {
	out := a.authDataHead(a.Flags | protocol.FlagAttestedCredentialData)
	out = append(out, make([]byte, 16)...) // AAGUID: zeros, as passkey providers send
	out = binary.BigEndian.AppendUint16(out, uint16(len(a.CredentialID)))
	out = append(out, a.CredentialID...)
	return append(out, publicKey...)
}

// AttestationObject wraps authData with the configured format and statement.
func (a *Authenticator) AttestationObject(authData []byte) []byte {
	b, err := webauthncbor.Marshal(map[string]any{"fmt": a.Format, "attStmt": a.AttStmt, "authData": authData})
	if err != nil {
		panic(err)
	}
	return b
}

// Registration is a RegistrationCredential for a base64url challenge, as JSON.
func (a *Authenticator) Registration(challenge string) map[string]any {
	clientData := a.ClientData(protocol.CreateCeremony, challenge)
	att := a.AttestationObject(a.RegistrationAuthData(a.COSEKey()))
	id := B64URL(a.CredentialID)
	return map[string]any{
		"id": id, "rawId": id, "type": "public-key",
		"response": map[string]any{"clientDataJSON": B64URL(clientData), "attestationObject": B64URL(att)},
	}
}

// Assertion is an AuthenticationCredential for a base64url challenge, as JSON, signed
// over authData || SHA-256(clientDataJSON). id is the "id" string to send.
func (a *Authenticator) Assertion(challenge, id string) map[string]any {
	clientData := a.ClientData(protocol.AssertCeremony, challenge)
	authData := a.authDataHead(a.Flags)
	sig := a.Sign(authData, clientData)
	return map[string]any{
		"id": id, "rawId": B64URL(a.CredentialID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": B64URL(clientData), "authenticatorData": B64URL(authData),
			"signature": B64URL(sig), "userHandle": B64URL([]byte("ignored")),
		},
	}
}

// Sign is the assertion signature: ECDSA (DER) over SHA-256(authData || SHA-256(clientData)).
func (a *Authenticator) Sign(authData, clientData []byte) []byte {
	cdHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.Key, digest[:])
	if err != nil {
		panic(err)
	}
	return sig
}
