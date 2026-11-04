package embedding

import (
	"encoding/json"
	"fmt"
	"lumina/internal/swiftstr"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// FailureKind is the case of Swift's EmbeddingProviderError.
type FailureKind int

const (
	FailureTransient FailureKind = iota
	FailureNetwork
	FailurePermanent
	FailureCapExceeded
	FailureDimMismatch
)

// PermanentReason is EmbeddingProviderError.PermanentReason.
type PermanentReason string

const (
	ReasonMissingAPIKey         PermanentReason = "missingAPIKey"
	ReasonAuthRejected          PermanentReason = "authRejected"
	ReasonPaymentRequired       PermanentReason = "paymentRequired"
	ReasonRequestRejected       PermanentReason = "requestRejected"
	ReasonDecodeFailed          PermanentReason = "decodeFailed"
	ReasonEndpointMissing       PermanentReason = "endpointMissing"
	ReasonProviderNotRegistered PermanentReason = "providerNotRegistered"
	ReasonAllProvidersFailed    PermanentReason = "allProvidersFailed"
)

// Error is EmbeddingProviderError. Which fields mean anything depends on Kind:
// Reason (transient, network), Permanent (permanent), Response (transient,
// permanent), Tenant, Used and Cap (capExceeded), Expected and Got (dimMismatch).
type Error struct {
	Kind      FailureKind
	Reason    string
	Permanent PermanentReason
	Response  *ProviderResponse
	Tenant    uuid.UUID
	Used, Cap int64
	Expected  int
	Got       int
}

// Error is Swift's description, one log-friendly line such as
// `reason=paymentRequired http=402 msg="Insufficient credits"`.
func (e *Error) Error() string {
	suffix := ""
	if e.Response != nil {
		suffix = " " + e.Response.String()
	}
	switch e.Kind {
	case FailureTransient:
		return `transient reason="` + e.Reason + `"` + suffix
	case FailureNetwork:
		return `network reason="` + e.Reason + `"`
	case FailurePermanent:
		return "reason=" + string(e.Permanent) + suffix
	case FailureCapExceeded:
		return fmt.Sprintf("capExceeded tenant=%s used=%d cap=%d", e.Tenant, e.Used, e.Cap)
	default:
		return fmt.Sprintf("dimMismatch expected=%d got=%d", e.Expected, e.Got)
	}
}

// IsRecoverable reports whether a fallback chain advances to the next provider.
func (e *Error) IsRecoverable() bool {
	return e.Kind == FailureTransient || e.Kind == FailureNetwork
}

// IsProviderUnavailable is true when the provider cannot serve any row right
// now (outage, rate limit, bad key, empty balance) rather than this one input
// being bad. Swift's heal pass stops and backs off on these.
func (e *Error) IsProviderUnavailable() bool {
	switch e.Kind {
	case FailureTransient, FailureNetwork:
		return true
	case FailurePermanent:
		return e.Permanent != ReasonRequestRejected && e.Permanent != ReasonDecodeFailed
	default:
		return false
	}
}

// maxMessageLength caps ProviderResponse.Message, in characters.
const maxMessageLength = 200

// ProviderResponse is the HTTP status and error message a provider answered
// with. Message is empty when the body carried nothing readable.
type ProviderResponse struct {
	Status  int
	Message string
}

func (r *ProviderResponse) String() string {
	if r.Message == "" {
		return fmt.Sprintf("http=%d", r.Status)
	}
	return fmt.Sprintf(`http=%d msg="%s"`, r.Status, r.Message)
}

func newProviderResponse(status int, body []byte) *ProviderResponse {
	return &ProviderResponse{Status: status, Message: sanitizeMessage(extractMessage(body))}
}

// extractMessage reads `{"error":{"message":…}}`, `{"error":"…"}` or a
// top-level `{"message":…}`. Any other JSON object has no message; a body that
// is not a JSON object is the message itself.
func extractMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err == nil && object != nil {
		switch e := object["error"].(type) {
		case map[string]any:
			if m, ok := e["message"].(string); ok {
				return m
			}
		case string:
			return e
		}
		m, _ := object["message"].(string)
		return m
	}
	if !utf8.Valid(body) {
		return ""
	}
	return string(body)
}

// sanitizeMessage collapses whitespace, redacts key-shaped tokens and truncates.
// Providers echo a masked key on auth errors; masked or not, it stays out of logs.
func sanitizeMessage(raw string) string {
	words := strings.FieldsFunc(raw, unicode.IsSpace)
	afterBearer := false
	for i, word := range words {
		lowered := strings.ToLower(word)
		if afterBearer || strings.HasPrefix(lowered, "sk-") || strings.HasPrefix(lowered, "sk_") || lowered == "bearer" {
			words[i] = "[redacted]"
		}
		afterBearer = lowered == "bearer"
	}
	collapsed := strings.Join(words, " ")
	if swiftstr.Count(collapsed) <= maxMessageLength {
		return collapsed
	}
	return swiftstr.Prefix(collapsed, maxMessageLength) + "…"
}

// errorFromHTTP is EmbeddingProviderError.fromHTTP: nil for 2xx.
func errorFromHTTP(status int, body []byte, label string) *Error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 401 || status == 403:
		return &Error{Kind: FailurePermanent, Permanent: ReasonAuthRejected, Response: newProviderResponse(status, body)}
	case status == 402:
		return &Error{Kind: FailurePermanent, Permanent: ReasonPaymentRequired, Response: newProviderResponse(status, body)}
	case status == 408 || status == 429 || (status >= 500 && status < 600):
		return &Error{Kind: FailureTransient, Reason: fmt.Sprintf("%s http %d", label, status), Response: newProviderResponse(status, body)}
	default:
		return &Error{Kind: FailurePermanent, Permanent: ReasonRequestRejected, Response: newProviderResponse(status, body)}
	}
}
