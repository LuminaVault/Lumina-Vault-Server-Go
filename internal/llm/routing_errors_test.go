package llm

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cases from ProviderErrorClassifierTests with no exact Go counterpart: the
// Swift fixture bodies verbatim, and the case each status lands in.
func TestAuditDClassifierSwiftFixtures(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"insufficient_quota"}}`,
		"Quota exceeded for organization xyz",
		"Out of credits — top up your balance",
		"Your billing subscription has lapsed",
		"Payment required to continue",
		"insufficient_balance",
	} {
		t.Run("403 with credit marker maps to creditExhausted: "+body, func(t *testing.T) {
			err := Classify(OpenAI, 403, []byte(body))
			assert.Equal(t, KindCreditExhausted, err.Kind)
			assert.True(t, err.IsRecoverable())
			assert.Equal(t, "credit_exhausted", err.ReasonCode())
		})
	}
	t.Run("each status lands in Swift's case", func(t *testing.T) {
		assert.Equal(t, KindPermanent, Classify(OpenAI, 401, nil).Kind)
		assert.Equal(t, "upstream_rejected", Classify(OpenAI, 401, nil).ReasonCode())
		assert.Equal(t, KindCreditExhausted, Classify(OpenAI, 402, nil).Kind)
		assert.Equal(t, KindPermanent, Classify(OpenAI, 403, []byte(`{"error":"forbidden"}`)).Kind)
		assert.Equal(t, KindPermanent, Classify(OpenAI, 404, nil).Kind)
		assert.Equal(t, KindPermanent, Classify(OpenAI, 418, nil).Kind)
	})
}

type auditDOtherError struct{}

func (auditDOtherError) Error() string { return "some other error" }

// ProviderErrorReasonCodeTests: dnsLookupFailed, a non-URLError, and the
// errnos FoundationNetworking folds into cannotConnectToHost or
// networkConnectionLost.
func TestAuditDNetworkReasonCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"dnsLookupFailed":                   {&net.DNSError{Err: "server misbehaving", IsTemporary: true}, "upstream_unreachable"},
		"non-URLError underlying":           {auditDOtherError{}, "network"},
		"connect with the network down":     {&net.OpError{Op: "dial", Err: syscall.ENETDOWN}, "upstream_unreachable"},
		"connect with the host down":        {&net.OpError{Op: "dial", Err: syscall.EHOSTDOWN}, "upstream_unreachable"},
		"connection aborted mid-read":       {&net.OpError{Op: "read", Err: syscall.ECONNABORTED}, "upstream_unreachable"},
		"a TLS alert is not unreachable":    {&net.OpError{Op: "remote error", Err: tls.AlertError(40)}, "network"},
		"a dial timeout is still a timeout": {&net.OpError{Op: "dial", Err: syscall.ETIMEDOUT}, "upstream_timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Network(HermesGateway, tc.err)
			assert.Equal(t, tc.code, err.ReasonCode())
			want := "Couldn't reach Hermes."
			if tc.code == "upstream_timeout" {
				want = "Hermes timed out responding."
			}
			assert.Equal(t, want, err.UserMessage())
		})
	}
}

// StreamErrorMessageTests: every UserFacingError becomes an errorDetail, and
// StreamErrorDTO's cta is never missing, so an error with no actions says [].
func TestAuditDStreamErrorEvent(t *testing.T) {
	upstream := &UpstreamErrorResponse{Reason: "credit_exhausted", Message: "Your provider is out of credit."}
	event := StreamErrorFor(upstream)
	assert.Equal(t, StreamErrorEvent{Message: "Your provider is out of credit.", Code: "credit_exhausted", CTA: []string{}}, event)
	require.NotNil(t, event.CTA)

	capped := &UsageCapExceededError{RetryAfter: 60}
	assert.Equal(t, capped.UserMessage(), StreamErrorFor(capped).Message)
	assert.Equal(t, []string{"upgrade", "add_key"}, StreamErrorFor(capped).CTA)

	internal := StreamErrorFor(errors.New(`PSQLError: relation "users" does not exist`))
	assert.Equal(t, StreamErrorEvent{Message: StreamErrorGeneric}, internal)
	b, err := json.Marshal(internal)
	require.NoError(t, err)
	assert.JSONEq(t, `{"message":"upstream failure"}`, string(b), "no code, no detail")
}

// Swift's only production caller (CerberusRouterService.swift:158) builds the
// default actions when freeLaneActions is empty; Go folds that rule into the
// type, so empty and nil Actions both mean the default.
func TestAuditDFreeLaneExhaustedEmptyActions(t *testing.T) {
	for _, actions := range [][]string{nil, {}} {
		lane := &FreeLaneExhaustedError{RetryAfterSeconds: 60, Actions: actions}
		assert.Equal(t, []string{"upgrade", "add_key"}, lane.RecoveryActions())
		assert.Equal(t, []any{"upgrade", "add_key"}, envelopeOf(t, lane.Body())["cta"])
	}
}
