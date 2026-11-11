package ws

import "encoding/json"

// MaxMessageBytes is WebSocketBroadcastGuard.maxMessageBytes, and also the read
// limit Swift puts on inbound messages (`inbound.messages(maxSize:)`).
const MaxMessageBytes = 16 * 1024

// Reason is a WebSocketBroadcastGuard.Decision case.
type Reason int

const (
	Allow Reason = iota
	RejectEmpty
	RejectOversize
	RejectInvalidJSON
	RejectMissingType
)

func (r Reason) String() string {
	switch r {
	case Allow:
		return "allow"
	case RejectEmpty:
		return "empty"
	case RejectOversize:
		return "oversize"
	case RejectInvalidJSON:
		return "non-JSON"
	case RejectMissingType:
		return "missing type field"
	}
	return "unknown"
}

// Decision is what Evaluate returns. ByteCount is set only for RejectOversize,
// like Swift's `.rejectOversize(byteCount:)`.
type Decision struct {
	Reason    Reason
	ByteCount int
}

// Evaluate is WebSocketBroadcastGuard.evaluate (HER-200 L1): an inbound text
// frame is fanned out to the tenant's sockets only when it is non-empty, at most
// MaxMessageBytes of UTF-8, a JSON object, and has a string "type" field.
func Evaluate(message string) Decision {
	if message == "" {
		return Decision{Reason: RejectEmpty}
	}
	if n := len(message); n > MaxMessageBytes {
		return Decision{Reason: RejectOversize, ByteCount: n}
	}
	// JSONSerialization.jsonObject(with:) as? [String: Any]: anything but a
	// top-level object is invalid JSON here.
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(message), &object); err != nil || object == nil {
		return Decision{Reason: RejectInvalidJSON}
	}
	// `json["type"] is String`: null and every other kind fail.
	if raw := object["type"]; len(raw) == 0 || raw[0] != '"' {
		return Decision{Reason: RejectMissingType}
	}
	return Decision{Reason: Allow}
}
