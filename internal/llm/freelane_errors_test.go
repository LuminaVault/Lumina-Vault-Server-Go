package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditBLaneBody is FreeLaneExhaustedErrorTests' body(_:): the "error" object.
func auditBLaneBody(t *testing.T, err *FreeLaneExhaustedError) map[string]any {
	t.Helper()
	body := envelopeOf(t, err.Body())
	require.NotNil(t, body)
	return body
}

func auditBStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	require.True(t, ok, "cta is not an array: %#v", v)
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		str, ok := s.(string)
		require.True(t, ok)
		out = append(out, str)
	}
	return out
}

// Ports FreeLaneExhaustedErrorTests: what an exhausted lane tells the user
// depends on who they are.
func TestAuditBFreeLaneExhaustedError(t *testing.T) {
	t.Run("an unpaid user is offered an upgrade or a key", func(t *testing.T) {
		err := &FreeLaneExhaustedError{RetryAfterSeconds: 60, Actions: []string{"upgrade", "add_key"}}
		assert.Equal(t, []string{"upgrade", "add_key"}, auditBStrings(t, auditBLaneBody(t, err)["cta"]))
		assert.Contains(t, err.UserMessage(), "Upgrade")
	})
	t.Run("a paying byok user is offered managed or a key, never an upgrade", func(t *testing.T) {
		err := &FreeLaneExhaustedError{RetryAfterSeconds: 60, Actions: []string{"add_key", "switch_to_managed"}}
		assert.Equal(t, []string{"add_key", "switch_to_managed"}, auditBStrings(t, auditBLaneBody(t, err)["cta"]))
		assert.NotContains(t, err.UserMessage(), "Upgrade")
		assert.Contains(t, err.UserMessage(), "Managed")
	})
	t.Run("a paying user in an outage is offered a key only", func(t *testing.T) {
		err := &FreeLaneExhaustedError{RetryAfterSeconds: 60, Actions: []string{"add_key"}}
		assert.Equal(t, []string{"add_key"}, auditBStrings(t, auditBLaneBody(t, err)["cta"]))
		assert.NotContains(t, err.UserMessage(), "Upgrade")
		assert.NotContains(t, err.UserMessage(), "Managed")
	})
	t.Run("the retry hint is unchanged", func(t *testing.T) {
		body := auditBLaneBody(t, &FreeLaneExhaustedError{RetryAfterSeconds: 60, Actions: []string{"add_key"}})
		assert.Equal(t, float64(60), body["retryAfterSeconds"])
		assert.Equal(t, "free_lane_exhausted", body["code"])
	})
}

// BYOKConsistencyTests' "byok chat without provider keys reaches the free lane,
// not a 403" asserts the 503 body; this is that body, below the HTTP route.
func TestAuditBFreeLaneUnavailableBody(t *testing.T) {
	err := &FreeLaneUnavailableError{Actions: FreeLaneRecoveryActions("trial", ModeBYOK, TriggerNotEntitled)}
	assert.Equal(t, 503, err.HTTPStatus())
	body := envelopeOf(t, err.Body())
	assert.NotEqual(t, "byok_keys_required", body["code"])
	assert.Equal(t, "free_lane_unavailable", body["code"])
	_, hasRetry := body["retryAfterSeconds"]
	assert.False(t, hasRetry)
	message, _ := body["message"].(string)
	assert.NotEmpty(t, message)
	cta := auditBStrings(t, body["cta"])
	assert.Contains(t, cta, "add_key")
	assert.Contains(t, cta, "switch_to_managed")
	assert.NotContains(t, cta, "upgrade")
}
