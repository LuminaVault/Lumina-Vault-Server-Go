package ws_test

import (
	"strings"
	"testing"

	"lumina/internal/ws"

	"github.com/stretchr/testify/require"
)

// MARK: WebSocketBroadcastGuardTests (9)

func TestGuardValidJSONWithTypeFieldIsAllowed(t *testing.T) {
	require.Equal(t, ws.Decision{Reason: ws.Allow}, ws.Evaluate(`{"type":"presence","userId":"abc"}`))
}

func TestGuardEmptyStringIsRejected(t *testing.T) {
	require.Equal(t, ws.Decision{Reason: ws.RejectEmpty}, ws.Evaluate(""))
}

func TestGuardOversizeMessageIsRejected(t *testing.T) {
	oversize := strings.Repeat("a", ws.MaxMessageBytes+1)
	require.Equal(t, ws.Decision{Reason: ws.RejectOversize, ByteCount: ws.MaxMessageBytes + 1}, ws.Evaluate(oversize))
}

func TestGuardMessageAtExactSizeCapIsAllowedIfJSONIsValid(t *testing.T) {
	typeKey := `{"type":"x","pad":"`
	suffix := `"}`
	payload := typeKey + strings.Repeat("a", ws.MaxMessageBytes-len(typeKey)-len(suffix)) + suffix
	require.Len(t, payload, ws.MaxMessageBytes)
	require.Equal(t, ws.Decision{Reason: ws.Allow}, ws.Evaluate(payload))
}

func TestGuardPlainTextIsRejectedAsInvalidJSON(t *testing.T) {
	require.Equal(t, ws.Decision{Reason: ws.RejectInvalidJSON}, ws.Evaluate("hello world"))
}

func TestGuardJSONArrayAtTopLevelIsRejected(t *testing.T) {
	require.Equal(t, ws.Decision{Reason: ws.RejectInvalidJSON}, ws.Evaluate(`[1,2,3]`))
}

func TestGuardJSONObjectMissingTypeFieldIsRejected(t *testing.T) {
	require.Equal(t, ws.Decision{Reason: ws.RejectMissingType}, ws.Evaluate(`{"foo":"bar"}`))
}

func TestGuardJSONObjectWithNonStringTypeIsRejected(t *testing.T) {
	require.Equal(t, ws.Decision{Reason: ws.RejectMissingType}, ws.Evaluate(`{"type":42}`))
	require.Equal(t, ws.Decision{Reason: ws.RejectMissingType}, ws.Evaluate(`{"type":null}`))
}

func TestGuardExtraFieldsAreTolerated(t *testing.T) {
	require.Equal(t, ws.Decision{Reason: ws.Allow}, ws.Evaluate(`{"type":"chat","sender":"alice","body":"hi","ts":1700000000}`))
}

// Go-only: top-level scalars and null are not objects, as with
// JSONSerialization's `as? [String: Any]`.
func TestGuardTopLevelNonObjectsAreInvalidJSON(t *testing.T) {
	for _, s := range []string{`null`, `42`, `"type"`, `true`, `{"type":"x"} trailing`} {
		require.Equal(t, ws.Decision{Reason: ws.RejectInvalidJSON}, ws.Evaluate(s), s)
	}
	require.Equal(t, ws.Decision{Reason: ws.RejectMissingType}, ws.Evaluate(`{}`))
}
