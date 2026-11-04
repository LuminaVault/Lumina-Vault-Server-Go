package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func classify(status int, body *string) *ProviderError {
	if body == nil {
		return Classify(OpenAI, status, nil)
	}
	return Classify(OpenAI, status, []byte(*body))
}

// ProviderErrorClassifierTests.
func TestClassifier(t *testing.T) {
	t.Run("401 maps to permanent", func(t *testing.T) {
		err := classify(401, strPtr("{}"))
		assert.False(t, err.IsRecoverable())
		assert.Equal(t, "upstream_rejected", err.ReasonCode())
	})
	t.Run("402 maps to creditExhausted", func(t *testing.T) {
		err := classify(402, strPtr("{}"))
		assert.True(t, err.IsRecoverable())
		assert.Equal(t, "credit_exhausted", err.ReasonCode())
	})
	t.Run("403 plain maps to permanent", func(t *testing.T) {
		assert.False(t, classify(403, strPtr(`{"error":"forbidden"}`)).IsRecoverable())
	})
	for _, body := range []string{
		`{"error":{"code":"insufficient_quota"}}`, `{"message":"Out of credits"}`, `billing hard limit`,
		`Payment required`, `quota exceeded`, `insufficient balance`,
	} {
		t.Run("403 with credit marker "+body, func(t *testing.T) {
			err := classify(403, &body)
			assert.True(t, err.IsRecoverable())
			assert.Equal(t, "credit_exhausted", err.ReasonCode())
		})
	}
	for _, status := range []int{408, 425, 429, 500, 502, 503, 504, 599} {
		assert.True(t, classify(status, nil).IsRecoverable(), status)
		assert.Equal(t, KindTransient, classify(status, nil).Kind, status)
	}
	assert.Equal(t, "rate_limit", classify(429, nil).ReasonCode())
	assert.Equal(t, "upstream_error", classify(503, nil).ReasonCode())
	assert.False(t, classify(404, nil).IsRecoverable())
	assert.False(t, classify(418, nil).IsRecoverable())
	assert.Equal(t, KindPermanent, classify(403, nil).Kind, "nil body never matches credit marker")
	assert.Equal(t, KindPermanent, classify(204, nil).Kind, "a 2xx here is an adapter bug")
}

func TestClassifierPreview(t *testing.T) {
	long := strings.Repeat("a", 5000)
	assert.Len(t, *Classify(OpenAI, 500, []byte(long)).Body, 2048, "the stored preview is capped at 2 KB")
	cut := strings.Repeat("a", 2047) + "é"
	assert.Nil(t, Classify(OpenAI, 500, []byte(cut)).Body, "a character cut at 2048 makes Swift's preview nil")
}

func TestProviderErrorMessages(t *testing.T) {
	credit := CreditExhausted(XAI, 403, nil)
	assert.Contains(t, credit.UserMessage(), "Grok")
	assert.Contains(t, strings.ToLower(credit.UserMessage()), "credit")
	assert.Contains(t, Network(Anthropic, errors.New("x")).UserMessage(), "Anthropic")

	assert.Equal(t, "OpenAI is rate-limiting us.", Transient(OpenAI, 429, nil).UserMessage())
	assert.Equal(t, "OpenAI is having trouble responding. (500)", Transient(OpenAI, 500, nil).UserMessage())
	assert.Equal(t, "Gemini rejected the request. (404) models/x is not found",
		Permanent(Gemini, 404, strPtr(`{"error":{"message":"models/x is not found"}}`)).UserMessage())
	assert.Equal(t, "Groq rejected the request. (400) plain", Permanent(Groq, 400, strPtr(" plain ")).UserMessage())
	assert.Equal(t, "Groq rejected the request. (400) top", Permanent(Groq, 400, strPtr(`{"error":"top"}`)).UserMessage())
	assert.Equal(t, "Groq rejected the request. (400) msg", Permanent(Groq, 400, strPtr(`{"message":"msg"}`)).UserMessage())
	long := Permanent(Groq, 400, strPtr(strings.Repeat("x", 250))).UserMessage()
	assert.Equal(t, "Groq rejected the request. (400) "+strings.Repeat("x", 200)+"…", long)
}

// ProviderErrorReasonCodeTests: Go transport errors stand in for URLError codes.
func TestNetworkReasonCodes(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	for name, tc := range map[string]struct {
		err     error
		code    string
		message string
	}{
		"timedOut":              {context.DeadlineExceeded, "upstream_timeout", "Hermes timed out responding."},
		"net timeout":           {&net.DNSError{IsTimeout: true}, "upstream_timeout", "Hermes timed out responding."},
		"cannotConnectToHost":   {refused, "upstream_unreachable", "Couldn't reach Hermes."},
		"notConnected":          {&net.OpError{Err: syscall.ENETUNREACH}, "upstream_unreachable", "Couldn't reach Hermes."},
		"cannotFindHost":        {&net.DNSError{IsNotFound: true}, "upstream_unreachable", "Couldn't reach Hermes."},
		"networkConnectionLost": {&net.OpError{Err: syscall.ECONNRESET}, "upstream_unreachable", "Couldn't reach Hermes."},
		"unknown":               {errors.New("boom"), "network", "Couldn't reach Hermes."},
		"stream transport":      {opaqueNetworkError{"timed out"}, "network", "Couldn't reach Hermes."},
	} {
		t.Run(name, func(t *testing.T) {
			err := Network(HermesGateway, tc.err)
			assert.Equal(t, tc.code, err.ReasonCode())
			assert.Equal(t, tc.message, err.UserMessage())
		})
	}
}

// ProviderCredentialEvidenceTests.
func TestCredentialEvidence(t *testing.T) {
	for _, status := range []int{401, 403} {
		assert.True(t, Permanent(OpenAI, status, nil).IsCredentialRejection(), status)
	}
	assert.True(t, CreditExhausted(OpenRouter, 402, nil).IsCredentialRejection())
	for _, err := range []*ProviderError{
		Transient(OpenAI, 429, nil), Transient(OpenAI, 500, nil), Transient(OpenAI, 503, nil),
		Network(OpenAI, context.DeadlineExceeded), Network(OpenAI, &net.DNSError{}),
	} {
		assert.False(t, err.IsCredentialRejection(), err.Error())
	}
	for _, status := range []int{400, 404, 422} {
		assert.False(t, Permanent(Anthropic, status, nil).IsCredentialRejection(), status)
	}
}

func envelopeOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var outer map[string]map[string]any
	require.NoError(t, json.Unmarshal(body, &outer))
	return outer["error"]
}

// UpstreamErrorResponseTests.
func TestUpstreamErrorResponse(t *testing.T) {
	assert.Equal(t, http.StatusGatewayTimeout, (&UpstreamErrorResponse{Reason: "upstream_timeout"}).HTTPStatus())
	for _, reason := range []string{"upstream_unreachable", "network", "upstream_rejected"} {
		assert.Equal(t, http.StatusBadGateway, (&UpstreamErrorResponse{Reason: reason}).HTTPStatus())
	}
	hint := 2000
	withHint := &UpstreamErrorResponse{Reason: "upstream_timeout", Message: "Hermes timed out.", RetryAfterMs: &hint}
	assert.Equal(t, `{"error":{"code":"upstream_timeout","message":"Hermes timed out.","retry_after_ms":2000}}`, string(withHint.Body()))
	_, has := envelopeOf(t, (&UpstreamErrorResponse{Reason: "network", Message: "m"}).Body())["retry_after_ms"]
	assert.False(t, has)
	// JSONSerialization writes "/" as "\/".
	assert.Equal(t, `{"error":{"code":"x","message":"a\/b"}}`, string((&UpstreamErrorResponse{Reason: "x", Message: "a/b"}).Body()))
}

func TestRoutingErrorEnvelopes(t *testing.T) {
	byok := BYOKKeysRequiredError{}
	assert.Equal(t, http.StatusForbidden, byok.HTTPStatus())
	assert.Equal(t, `{"error":{"code":"byok_keys_required","cta":["add_key","switch_to_managed"],"message":"`+byokMessage+`"}}`, string(byok.Body()))

	lane := &FreeLaneExhaustedError{RetryAfterSeconds: 60}
	assert.Equal(t, http.StatusTooManyRequests, lane.HTTPStatus())
	assert.Equal(t, float64(60), envelopeOf(t, lane.Body())["retryAfterSeconds"])
	assert.Contains(t, lane.UserMessage(), "Upgrade for the full brain")
	assert.Contains(t, (&FreeLaneExhaustedError{Actions: []string{"add_key", "switch_to_managed"}}).UserMessage(), "Switch to Managed")
	assert.Equal(t, "You've used today's free messages. Add your own API key in Settings to keep going now.",
		(&FreeLaneExhaustedError{Actions: []string{"add_key"}}).UserMessage())

	unavailable := &FreeLaneUnavailableError{Actions: []string{"add_key"}}
	assert.Equal(t, http.StatusServiceUnavailable, unavailable.HTTPStatus())
	_, has := envelopeOf(t, unavailable.Body())["retryAfterSeconds"]
	assert.False(t, has, "nothing resets at midnight")

	capped := &UsageCapExceededError{RetryAfter: 1500 * time.Millisecond}
	assert.Equal(t, 2, capped.RetryAfterSeconds())
	assert.Equal(t, 1, (&UsageCapExceededError{}).RetryAfterSeconds())
}

// StreamErrorMessageTests.
func TestStreamErrorMessage(t *testing.T) {
	lane := &FreeLaneExhaustedError{RetryAfterSeconds: 60, Actions: []string{"add_key", "switch_to_managed"}}
	assert.Equal(t, byokMessage, StreamErrorMessage(BYOKKeysRequiredError{}))
	assert.Equal(t, lane.UserMessage(), StreamErrorMessage(lane))
	assert.Equal(t, usageCapMessage, StreamErrorMessage(&UsageCapExceededError{}))
	assert.Equal(t, "Your provider is out of credit.", StreamErrorMessage(&UpstreamErrorResponse{Reason: "credit_exhausted", Message: "Your provider is out of credit."}))

	assert.Equal(t, StreamErrorGeneric, StreamErrorMessage(errors.New("internal")))
	assert.Equal(t, StreamErrorGeneric, StreamErrorMessage(context.Canceled))

	assert.Equal(t, StreamErrorEvent{Message: lane.UserMessage(), Code: "free_lane_exhausted", CTA: []string{"add_key", "switch_to_managed"}}, StreamErrorFor(lane))
	assert.Equal(t, StreamErrorEvent{Message: byokMessage, Code: "byok_keys_required", CTA: []string{"add_key", "switch_to_managed"}}, StreamErrorFor(BYOKKeysRequiredError{}))
	assert.Equal(t, StreamErrorEvent{Message: StreamErrorGeneric}, StreamErrorFor(errors.New("internal")))
}
