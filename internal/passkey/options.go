package passkey

import (
	"crypto/rand"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// The options are swift-webauthn's Encodable implementations, keys in the order their
// encode(to:) writes them. There is no authenticatorSelection, excludeCredentials or
// allowCredentials: WebAuthnService passes none, and the library writes only what is set.

// CreationOptions is PublicKeyCredentialCreationOptions.
type CreationOptions struct {
	Challenge        string      `json:"challenge"`
	User             UserEntity  `json:"user"`
	RP               RPEntity    `json:"rp"`
	PubKeyCredParams []CredParam `json:"pubKeyCredParams"`
	Timeout          int64       `json:"timeout"`
	Attestation      string      `json:"attestation"`
}

// UserEntity is PublicKeyCredentialUserEntity; ID is base64url.
type UserEntity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// RPEntity is PublicKeyCredentialRelyingPartyEntity.
type RPEntity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CredParam is PublicKeyCredentialParameters.
type CredParam struct {
	Type string `json:"type"`
	Alg  int64  `json:"alg"`
}

// RequestOptions is PublicKeyCredentialRequestOptions.
type RequestOptions struct {
	Challenge        string `json:"challenge"`
	Timeout          int64  `json:"timeout"`
	RPID             string `json:"rpId"`
	UserVerification string `json:"userVerification"`
}

// SupportedAlgorithms is `[PublicKeyCredentialParameters].supported`:
// COSEAlgorithmIdentifier.allCases in declaration order.
var SupportedAlgorithms = []webauthncose.COSEAlgorithmIdentifier{
	webauthncose.AlgES256, webauthncose.AlgES384, webauthncose.AlgES512,
	webauthncose.AlgRS1, webauthncose.AlgRS256, webauthncose.AlgRS384, webauthncose.AlgRS512,
	webauthncose.AlgPS256, webauthncose.AlgPS384, webauthncose.AlgPS512,
}

const (
	registrationTimeoutMS   = 5 * 60 * 1000 // beginRegistration's default timeout
	authenticationTimeoutMS = 60 * 1000     // beginAuthentication's default timeout
)

// newChallenge is ChallengeGenerator.live: 32 random bytes.
func newChallenge() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

// BeginRegistration is WebAuthnManager.beginRegistration(user:) with its defaults:
// attestation none, every supported algorithm, a five-minute timeout. userID is the raw
// user handle (WebAuthnService passes the uuidString's UTF-8 bytes).
func (c Config) BeginRegistration(userID []byte, name, displayName string) (CreationOptions, []byte) {
	challenge := newChallenge()
	params := make([]CredParam, 0, len(SupportedAlgorithms))
	for _, alg := range SupportedAlgorithms {
		params = append(params, CredParam{Type: string(protocol.PublicKeyCredentialType), Alg: int64(alg)})
	}
	return CreationOptions{
		Challenge:        URLEncode(challenge),
		User:             UserEntity{ID: URLEncode(userID), Name: name, DisplayName: displayName},
		RP:               RPEntity{ID: c.RPID, Name: c.RPName},
		PubKeyCredParams: params,
		Timeout:          registrationTimeoutMS,
		Attestation:      string(protocol.PreferNoAttestation),
	}, challenge
}

// BeginAuthentication is WebAuthnManager.beginAuthentication() with its defaults: a
// one-minute timeout, no allowCredentials, userVerification preferred.
func (c Config) BeginAuthentication() (RequestOptions, []byte) {
	challenge := newChallenge()
	return RequestOptions{
		Challenge:        URLEncode(challenge),
		Timeout:          authenticationTimeoutMS,
		RPID:             c.RPID,
		UserVerification: string(protocol.VerificationPreferred),
	}, challenge
}
