package webauthnroutes

import (
	"encoding/json"
	"lumina/internal/passkey"
	"lumina/internal/wire"
	"reflect"
)

// The credential bodies are swift-webauthn's Decodable implementations, fields in
// their init(from:) order so wire.Bind reports the same first failure. Binary members
// are base64url strings (standard base64, padded or not, also passes, as Swift's
// urlDecoded makes no distinction); one that does not decode is Foundation's
// dataCorrupted with the library's own debugDescription.

// keyName gives a binary member's dataCorrupted text.
type keyName interface{ corrupted() string }

// codingKey is how swift-webauthn interpolates a CodingKey into its message:
// CodingKey's description.
func codingKey(name string) string {
	return `Failed to decode base64url encoded string at CodingKeys(stringValue: "` + name + `", intValue: nil) into bytes`
}

type (
	regRawID          struct{}
	assertRawID       struct{}
	clientDataJSONKey struct{}
	attestationObjKey struct{}
	authenticatorKey  struct{}
	signatureKey      struct{}
	userHandleKey     struct{}
)

// RegistrationCredential has its own message for rawId; every other member goes
// through decodeBytesFromURLEncodedBase64(forKey:).
func (regRawID) corrupted() string          { return "Failed to decode base64url encoded rawID into bytes" }
func (assertRawID) corrupted() string       { return codingKey("rawId") }
func (clientDataJSONKey) corrupted() string { return codingKey("clientDataJSON") }
func (attestationObjKey) corrupted() string { return codingKey("attestationObject") }
func (authenticatorKey) corrupted() string  { return codingKey("authenticatorData") }
func (signatureKey) corrupted() string      { return codingKey("signature") }
func (userHandleKey) corrupted() string     { return codingKey("userHandle") }

// b64 is a base64url member decoded to bytes.
type b64[K keyName] []byte

func (b *b64[K]) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	decoded, ok := passkey.DecodeBytes(s)
	if !ok {
		var k K
		return &wire.CorruptedError{Message: k.corrupted()}
	}
	*b = decoded
	return nil
}

// registrationCredential is RegistrationCredential: id, type, rawId, response.
type registrationCredential struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	RawID    b64[regRawID]       `json:"rawId"`
	Response attestationResponse `json:"response"`
}

// attestationResponse is AuthenticatorAttestationResponse.
type attestationResponse struct {
	ClientDataJSON    b64[clientDataJSONKey] `json:"clientDataJSON"`
	AttestationObject b64[attestationObjKey] `json:"attestationObject"`
}

// authenticationCredential is AuthenticationCredential: id, rawId, response,
// authenticatorAttachment (optional, any string), type.
type authenticationCredential struct {
	ID                      string            `json:"id"`
	RawID                   b64[assertRawID]  `json:"rawId"`
	Response                assertionResponse `json:"response"`
	AuthenticatorAttachment *string           `json:"authenticatorAttachment,omitempty"`
	Type                    string            `json:"type"`
}

// assertionResponse is AuthenticatorAssertionResponse. userHandle and
// attestationObject are decoded (a bad one is a 400) and never used.
type assertionResponse struct {
	ClientDataJSON    b64[clientDataJSONKey]  `json:"clientDataJSON"`
	AuthenticatorData b64[authenticatorKey]   `json:"authenticatorData"`
	Signature         b64[signatureKey]       `json:"signature"`
	UserHandle        *b64[userHandleKey]     `json:"userHandle,omitempty"`
	AttestationObject *b64[attestationObjKey] `json:"attestationObject,omitempty"`
}
