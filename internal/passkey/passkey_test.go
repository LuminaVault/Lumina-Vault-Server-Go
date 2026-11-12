package passkey_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"lumina/internal/passkey"
	"lumina/internal/passkey/passkeytest"
	"lumina/internal/wire"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseOrigins ports WebAuthnOriginsTests (Tests/AppTests/Auth/WebAuthnOriginsTests.swift),
// all 5 cases: one origin, an ordered list, blanks and padding dropped, empty
// configurations, duplicates collapsing in first-seen order.
func TestParseOrigins(t *testing.T) {
	t.Run("a single origin parses to one entry", func(t *testing.T) {
		assert.Equal(t, []string{"https://api.luminavault.fyi"}, passkey.ParseOrigins("https://api.luminavault.fyi"))
	})
	t.Run("a comma separated list parses in order", func(t *testing.T) {
		assert.Equal(t, []string{"https://app.luminavault.fyi", "https://luminavault.fyi"},
			passkey.ParseOrigins("https://app.luminavault.fyi,https://luminavault.fyi"))
	})
	t.Run("blank and whitespace-padded entries are dropped", func(t *testing.T) {
		assert.Equal(t, []string{"https://a.example", "https://b.example"},
			passkey.ParseOrigins(" https://a.example , ,https://b.example, "))
	})
	t.Run("an empty configuration parses to no origins", func(t *testing.T) {
		assert.Equal(t, []string{}, passkey.ParseOrigins(""))
		assert.Equal(t, []string{}, passkey.ParseOrigins("   "))
		assert.Equal(t, []string{}, passkey.ParseOrigins(",,"))
	})
	t.Run("duplicates collapse while preserving first-seen order", func(t *testing.T) {
		assert.Equal(t, []string{"https://b.example", "https://a.example"},
			passkey.ParseOrigins("https://b.example,https://a.example,https://b.example"))
	})
	t.Run("newlines are trimmed too", func(t *testing.T) {
		assert.Equal(t, []string{"https://a.example"}, passkey.ParseOrigins("\nhttps://a.example\t\r\n"))
	})
}

func TestConfigured(t *testing.T) {
	assert.True(t, passkey.Config{RPID: "x", Origins: []string{"https://x"}}.Configured())
	assert.False(t, passkey.Config{RPID: "", Origins: []string{"https://x"}}.Configured())
	assert.False(t, passkey.Config{RPID: "x"}.Configured())
	assert.False(t, passkey.Config{RPID: "x", Origins: []string{""}}.Configured())
}

// URLEncodedBase64.urlDecoded and decodedBytes.
func TestBase64(t *testing.T) {
	assert.Equal(t, "ab+/", passkey.URLDecoded("ab-_"))
	assert.Equal(t, "abc=", passkey.URLDecoded("abc"))
	assert.Equal(t, "ab==", passkey.URLDecoded("ab"))
	assert.Equal(t, "abcd", passkey.URLDecoded("abcd"))
	// Padding already there is padded again until the length is a multiple of four.
	assert.Equal(t, "ab==", passkey.URLDecoded("ab="))
	b, ok := passkey.DecodeBytes("-_8")
	require.True(t, ok)
	assert.Equal(t, []byte{0xfb, 0xff}, b)
	b, ok = passkey.DecodeBytes("+/8=") // standard base64 passes too
	require.True(t, ok)
	assert.Equal(t, []byte{0xfb, 0xff}, b)
	_, ok = passkey.DecodeBytes("a")
	assert.False(t, ok)
	_, ok = passkey.DecodeBytes("ab\ncd")
	assert.False(t, ok, "Foundation refuses a newline Go's decoder would skip")
	_, ok = passkey.DecodeBytes("a*b=")
	assert.False(t, ok)
	assert.Equal(t, "-_8", passkey.URLEncode([]byte{0xfb, 0xff}))
}

func TestStore(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := passkey.NewStoreWithClock(func() time.Time { return now })
	user := uuid.New()
	s.StoreRegistration(user, []byte{1})
	got, ok := s.Registration(user)
	require.True(t, ok)
	assert.Equal(t, []byte{1}, got)

	// A second begin replaces the challenge.
	s.StoreRegistration(user, []byte{2})
	got, _ = s.Registration(user)
	assert.Equal(t, []byte{2}, got)

	// 300 s for both ceremonies, whatever the options' timeout says.
	s.StoreAuthentication("Alice", []byte{3})
	now = now.Add(passkey.ChallengeTTL - time.Second)
	_, ok = s.Authentication("Alice")
	assert.True(t, ok)
	_, ok = s.Authentication("alice")
	assert.False(t, ok, "sign-in challenges are keyed by the username verbatim")
	now = now.Add(time.Second)
	_, ok = s.Authentication("Alice")
	assert.False(t, ok, "expired at exactly the TTL")
	_, ok = s.Registration(user)
	assert.False(t, ok)

	s.StoreAuthentication("bob", []byte{4})
	s.ClearAuthentication("bob")
	_, ok = s.Authentication("bob")
	assert.False(t, ok)
	s.StoreRegistration(user, []byte{5})
	s.ClearRegistration(user)
	_, ok = s.Registration(user)
	assert.False(t, ok)
}

// The options as swift-webauthn encodes them, keys in its order.
func TestOptionsWire(t *testing.T) {
	cfg := passkey.Config{RPID: "luminavault.test", RPName: "LuminaVault Test", Origins: []string{"https://luminavault.test"}}
	user := []byte("6F1C1A4C-0000-4000-8000-000000000001")
	opts, challenge := cfg.BeginRegistration(user, "alice", "Alice/A")
	require.Len(t, challenge, 32)
	assert.Equal(t, passkey.URLEncode(challenge), opts.Challenge)
	b, err := wire.MarshalFoundation(opts)
	require.NoError(t, err)
	want := `{"challenge":"` + opts.Challenge + `","user":{"id":"` + base64.RawURLEncoding.EncodeToString(user) + `","name":"alice","displayName":"Alice\/A"},` +
		`"rp":{"id":"luminavault.test","name":"LuminaVault Test"},"pubKeyCredParams":[` +
		`{"type":"public-key","alg":-7},{"type":"public-key","alg":-35},{"type":"public-key","alg":-36},` +
		`{"type":"public-key","alg":-65535},{"type":"public-key","alg":-257},{"type":"public-key","alg":-258},` +
		`{"type":"public-key","alg":-259},{"type":"public-key","alg":-37},{"type":"public-key","alg":-38},` +
		`{"type":"public-key","alg":-39}],"timeout":300000,"attestation":"none"}`
	assert.Equal(t, want, string(b))

	req, challenge := cfg.BeginAuthentication()
	require.Len(t, challenge, 32)
	b, err = wire.MarshalFoundation(req)
	require.NoError(t, err)
	assert.Equal(t, `{"challenge":"`+passkey.URLEncode(challenge)+`","timeout":60000,"rpId":"luminavault.test","userVerification":"preferred"}`, string(b))
}

const (
	rpID   = "luminavault.test"
	origin = "https://luminavault.test"
)

var cfg = passkey.Config{RPID: rpID, RPName: "LuminaVault Test", Origins: []string{"https://other.test", origin}}

func registration(a *passkeytest.Authenticator, challenge []byte) passkey.RegistrationResponse {
	clientData := a.ClientData(protocol.CreateCeremony, passkey.URLEncode(challenge))
	return passkey.RegistrationResponse{
		ID: passkeytest.B64URL(a.CredentialID), Type: "public-key", RawID: a.CredentialID,
		ClientDataJSON: clientData, AttestationObject: a.AttestationObject(a.RegistrationAuthData(a.COSEKey())),
	}
}

func assertion(a *passkeytest.Authenticator, challenge []byte) passkey.AssertionResponse {
	clientData := a.ClientData(protocol.AssertCeremony, passkey.URLEncode(challenge))
	head := authHead(a, a.Flags)
	return passkey.AssertionResponse{
		ID: passkeytest.B64URL(a.CredentialID), Type: "public-key",
		ClientDataJSON: clientData, AuthenticatorData: head, Signature: a.Sign(head, clientData),
	}
}

func authHead(a *passkeytest.Authenticator, flags protocol.AuthenticatorFlags) []byte {
	rp := sha256.Sum256([]byte(a.RPID))
	out := append(append([]byte{}, rp[:]...), byte(flags))
	return binary.BigEndian.AppendUint32(out, a.Counter)
}

func reason(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	var v *passkey.VerifyError
	require.ErrorAs(t, err, &v)
	return v.Reason
}

func TestVerifyRegistration(t *testing.T) {
	challenge := []byte(strings.Repeat("c", 32))

	t.Run("a none attestation from the second origin registers", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		a.CredentialID = []byte{0xfb, 0xff, 0x01} // base64url "-_8B", standard "+/8B"
		a.Counter = 7
		reg, err := cfg.VerifyRegistration(challenge, registration(a, challenge))
		require.NoError(t, err)
		assert.Equal(t, "+/8B", reg.ID, "stored as padded standard base64")
		assert.Equal(t, a.COSEKey(), reg.PublicKey, "the COSE_Key bytes as the authenticator sent them")
		assert.Equal(t, uint32(7), reg.SignCount)
	})

	t.Run("user verification is not required", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		a.Flags = protocol.FlagUserPresent
		_, err := cfg.VerifyRegistration(challenge, registration(a, challenge))
		require.NoError(t, err)
	})

	t.Run("the id is stored as sent, unchecked against rawId", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		r := registration(a, challenge)
		r.ID = "not-the-credential"
		reg, err := cfg.VerifyRegistration(challenge, r)
		require.NoError(t, err)
		assert.Equal(t, "not+the+credential==", reg.ID)
	})

	cases := []struct {
		name, reason string
		mutate       func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse)
	}{
		{"wrong type", "invalidCredentialCreationType", func(_ *passkeytest.Authenticator, r *passkey.RegistrationResponse) { r.Type = "password" }},
		{"client data not json", "clientDataJSON", func(_ *passkeytest.Authenticator, r *passkey.RegistrationResponse) { r.ClientDataJSON = []byte("nope") }},
		{"client data missing origin", "clientDataJSON", func(_ *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			r.ClientDataJSON = []byte(`{"type":"webauthn.create","challenge":"x"}`)
		}},
		{"attestation object not cbor", "invalidAttestationObject", func(_ *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			r.AttestationObject = []byte{0xff}
		}},
		{"assert ceremony", "ceremonyTypeDoesNotMatch", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			r.ClientDataJSON = a.ClientData(protocol.AssertCeremony, passkey.URLEncode(challenge))
		}},
		{"other challenge", "challengeDoesNotMatch", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			r.ClientDataJSON = a.ClientData(protocol.CreateCeremony, passkey.URLEncode([]byte("other")))
		}},
		{"padded challenge", "challengeDoesNotMatch", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			r.ClientDataJSON = a.ClientData(protocol.CreateCeremony, base64.URLEncoding.EncodeToString(challenge))
		}},
		{"foreign origin", "originDoesNotMatch", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			a.Origin = "https://evil.test"
			r.ClientDataJSON = a.ClientData(protocol.CreateCeremony, passkey.URLEncode(challenge))
		}},
		{"other rp id", "relyingPartyIDHashDoesNotMatch", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			a.RPID = "evil.test"
			r.AttestationObject = a.AttestationObject(a.RegistrationAuthData(a.COSEKey()))
		}},
		{"no user presence", "userPresentFlagNotSet", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			a.Flags = protocol.FlagUserVerified
			r.AttestationObject = a.AttestationObject(a.RegistrationAuthData(a.COSEKey()))
		}},
		{"no attested data", "attestedCredentialDataMissing", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			r.AttestationObject = a.AttestationObject(authHead(a, a.Flags))
		}},
		{"packed is not supported", "attestationVerificationNotSupported", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			a.Format = "packed"
			r.AttestationObject = a.AttestationObject(a.RegistrationAuthData(a.COSEKey()))
		}},
		{"none with a statement", "attestationStatementMustBeEmpty", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			a.AttStmt = map[string]any{"alg": -7}
			r.AttestationObject = a.AttestationObject(a.RegistrationAuthData(a.COSEKey()))
		}},
		{"okp keys are refused", "unsupported", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			key, _ := webauthncbor.Marshal(map[int]any{1: 1, 3: -7, -1: 6, -2: make([]byte, 32)})
			r.AttestationObject = a.AttestationObject(a.RegistrationAuthData(key))
		}},
		{"eddsa is not an algorithm Swift lists", "unsupportedCOSEAlgorithm", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			key, _ := webauthncbor.Marshal(map[int]any{1: 1, 3: -8, -1: 6, -2: make([]byte, 32)})
			r.AttestationObject = a.AttestationObject(a.RegistrationAuthData(key))
		}},
		{"unknown algorithm", "unsupportedCOSEAlgorithm", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			key, _ := webauthncbor.Marshal(map[int]any{1: 2, 3: -47, -1: 1, -2: make([]byte, 32), -3: make([]byte, 32)})
			r.AttestationObject = a.AttestationObject(a.RegistrationAuthData(key))
		}},
		{"u2f key byte", "u2fPublicKey", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			r.AttestationObject = a.AttestationObject(a.RegistrationAuthData([]byte{0x04}))
		}},
		{"leftover bytes", "leftOverBytesInAuthenticatorData", func(a *passkeytest.Authenticator, r *passkey.RegistrationResponse) {
			r.AttestationObject = a.AttestationObject(append(a.RegistrationAuthData(a.COSEKey()), 0))
		}},
		{"raw id too long", "credentialRawIDTooLong", func(_ *passkeytest.Authenticator, r *passkey.RegistrationResponse) { r.RawID = make([]byte, 1024) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := passkeytest.New(rpID, origin)
			r := registration(a, challenge)
			tc.mutate(a, &r)
			_, err := cfg.VerifyRegistration(challenge, r)
			assert.Equal(t, tc.reason, reason(t, err))
		})
	}

	t.Run("no origins accepts nothing", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		_, err := passkey.Config{RPID: rpID}.VerifyRegistration(challenge, registration(a, challenge))
		assert.Equal(t, "originDoesNotMatch", reason(t, err))
	})
}

func TestVerifyAssertion(t *testing.T) {
	challenge := []byte(strings.Repeat("a", 32))

	t.Run("a signed assertion verifies and returns the counter", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		a.Counter = 5
		n, err := cfg.VerifyAssertion(challenge, assertion(a, challenge), a.COSEKey(), 4)
		require.NoError(t, err)
		assert.Equal(t, uint32(5), n)
	})

	t.Run("zero counters on both sides pass", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		n, err := cfg.VerifyAssertion(challenge, assertion(a, challenge), a.COSEKey(), 0)
		require.NoError(t, err)
		assert.Equal(t, uint32(0), n)
	})

	t.Run("a counter that does not grow is a replay", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		a.Counter = 4
		_, err := cfg.VerifyAssertion(challenge, assertion(a, challenge), a.COSEKey(), 4)
		assert.Equal(t, "potentialReplayAttack", reason(t, err))
		a.Counter = 0
		_, err = cfg.VerifyAssertion(challenge, assertion(a, challenge), a.COSEKey(), 4)
		assert.Equal(t, "potentialReplayAttack", reason(t, err))
	})

	t.Run("another key's signature fails", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		other := passkeytest.New(rpID, origin)
		_, err := cfg.VerifyAssertion(challenge, assertion(a, challenge), other.COSEKey(), 0)
		assert.Equal(t, "invalidSignature", reason(t, err))
	})

	t.Run("checks in Swift's order", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		r := assertion(a, challenge)
		r.Type = "x"
		_, err := cfg.VerifyAssertion(challenge, r, a.COSEKey(), 0)
		assert.Equal(t, "invalidAssertionCredentialType", reason(t, err))

		r = assertion(a, []byte("stale"))
		_, err = cfg.VerifyAssertion(challenge, r, a.COSEKey(), 0)
		assert.Equal(t, "challengeDoesNotMatch", reason(t, err))

		r = assertion(a, challenge)
		r.ClientDataJSON = a.ClientData(protocol.CreateCeremony, passkey.URLEncode(challenge))
		_, err = cfg.VerifyAssertion(challenge, r, a.COSEKey(), 0)
		assert.Equal(t, "ceremonyTypeDoesNotMatch", reason(t, err))

		a.Flags = protocol.FlagUserVerified
		_, err = cfg.VerifyAssertion(challenge, assertion(a, challenge), a.COSEKey(), 0)
		assert.Equal(t, "userPresentFlagNotSet", reason(t, err))

		r = assertion(a, challenge)
		r.AuthenticatorData = r.AuthenticatorData[:36]
		_, err = cfg.VerifyAssertion(challenge, r, a.COSEKey(), 0)
		assert.Equal(t, "authDataTooShort", reason(t, err))
	})

	t.Run("extension data is skipped, not parsed", func(t *testing.T) {
		a := passkeytest.New(rpID, origin)
		clientData := a.ClientData(protocol.AssertCeremony, passkey.URLEncode(challenge))
		authData := append(authHead(a, a.Flags|protocol.FlagHasExtensions), 0xff, 0xff)
		r := passkey.AssertionResponse{Type: "public-key", ClientDataJSON: clientData, AuthenticatorData: authData, Signature: a.Sign(authData, clientData)}
		_, err := cfg.VerifyAssertion(challenge, r, a.COSEKey(), 0)
		require.NoError(t, err)
	})
}

// EC2: the algorithm, not crv, picks the curve and hash.
func TestVerifyAssertionES384(t *testing.T) {
	challenge := []byte("es384")
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	pub, _ := key.PublicKey.Bytes()
	cose, _ := webauthncbor.Marshal(map[int]any{1: 2, 3: int(webauthncose.AlgES384), -1: int(webauthncose.P256), -2: pub[1:49], -3: pub[49:]})
	a := passkeytest.New(rpID, origin)
	r := assertion(a, challenge)
	cdHash := sha256.Sum256(r.ClientDataJSON)
	digest := sha512.Sum384(append(append([]byte{}, r.AuthenticatorData...), cdHash[:]...))
	r.Signature, _ = ecdsa.SignASN1(rand.Reader, key, digest[:])
	_, err = cfg.VerifyAssertion(challenge, r, cose, 0)
	require.NoError(t, err)
}

func rsaKey(t *testing.T, bits int, alg webauthncose.COSEAlgorithmIdentifier) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	require.NoError(t, err)
	e := make([]byte, 4)
	binary.BigEndian.PutUint32(e, uint32(key.E))
	cose, err := webauthncbor.Marshal(map[int]any{1: 3, 3: int(alg), -1: key.N.Bytes(), -2: e[1:]})
	require.NoError(t, err)
	return key, cose
}

// Swift: _RSA isValidSignature(_:for:padding:) hashes with SHA-256 whatever the
// algorithm, so an RS384 credential verifies a SHA-256 signature and refuses a SHA-384 one.
func TestSwiftWeaknessRSAAlwaysSHA256(t *testing.T) {
	challenge := []byte("rsa")
	a := passkeytest.New(rpID, origin)
	r := assertion(a, challenge)
	cdHash := sha256.Sum256(r.ClientDataJSON)
	signed := append(append([]byte{}, r.AuthenticatorData...), cdHash[:]...)

	key, cose := rsaKey(t, 2048, webauthncose.AlgRS384)
	d256 := sha256.Sum256(signed)
	r.Signature, _ = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d256[:])
	_, err := cfg.VerifyAssertion(challenge, r, cose, 0)
	require.NoError(t, err)

	d384 := sha512.Sum384(signed)
	r.Signature, _ = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA384, d384[:])
	_, err = cfg.VerifyAssertion(challenge, r, cose, 0)
	assert.Equal(t, "invalidSignature", reason(t, err))

	key, cose = rsaKey(t, 2048, webauthncose.AlgPS256)
	r.Signature, _ = rsa.SignPSS(rand.Reader, key, crypto.SHA256, d256[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	_, err = cfg.VerifyAssertion(challenge, r, cose, 0)
	require.NoError(t, err)

	// swift-crypto refuses a modulus under 2048 bits.
	key, cose = rsaKey(t, 1536, webauthncose.AlgRS256)
	r.Signature, _ = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d256[:])
	_, err = cfg.VerifyAssertion(challenge, r, cose, 0)
	assert.Equal(t, "invalidPublicKey", reason(t, err))
}
