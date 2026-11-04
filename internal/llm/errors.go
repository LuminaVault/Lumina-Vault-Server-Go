package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"lumina/internal/swiftstr"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// ErrorKind is the case of Swift's ProviderError.
type ErrorKind int

const (
	KindTransient ErrorKind = iota
	KindPermanent
	KindNetwork
	KindCreditExhausted
)

// ProviderError is what every adapter returns on a failed upstream call. The
// transport decides from Kind whether to try the next candidate. Body is nil
// where Swift's is nil, and Underlying is set only for KindNetwork.
type ProviderError struct {
	Kind       ErrorKind
	Provider   ProviderKind
	Status     int
	Body       *string
	Underlying error
}

func Transient(p ProviderKind, status int, body *string) *ProviderError {
	return &ProviderError{Kind: KindTransient, Provider: p, Status: status, Body: body}
}

func Permanent(p ProviderKind, status int, body *string) *ProviderError {
	return &ProviderError{Kind: KindPermanent, Provider: p, Status: status, Body: body}
}

func CreditExhausted(p ProviderKind, status int, body *string) *ProviderError {
	return &ProviderError{Kind: KindCreditExhausted, Provider: p, Status: status, Body: body}
}

func Network(p ProviderKind, underlying error) *ProviderError {
	return &ProviderError{Kind: KindNetwork, Provider: p, Underlying: underlying}
}

func strPtr(s string) *string { return &s }

func (e *ProviderError) Error() string {
	body := ""
	if e.Body != nil {
		body = *e.Body
	}
	if e.Kind == KindNetwork {
		return fmt.Sprintf("%s network: %v", e.Provider, e.Underlying)
	}
	return fmt.Sprintf("%s %s status=%d body=%q", e.Provider, e.ReasonCode(), e.Status, body)
}

func (e *ProviderError) Unwrap() error { return e.Underlying }

// IsRecoverable reports whether the transport should try the next candidate.
func (e *ProviderError) IsRecoverable() bool { return e.Kind != KindPermanent }

// IsCredentialRejection reports whether the failure says something about the
// key rather than about the provider's health: an explicit auth rejection, or
// a key that authenticated but can no longer buy anything.
func (e *ProviderError) IsCredentialRejection() bool {
	switch e.Kind {
	case KindPermanent:
		return e.Status == 401 || e.Status == 403
	case KindCreditExhausted:
		return true
	}
	return false
}

// ReasonCode is the stable tag on telemetry rows and SSE fallback notices.
func (e *ProviderError) ReasonCode() string {
	switch e.Kind {
	case KindCreditExhausted:
		return "credit_exhausted"
	case KindTransient:
		if e.Status == 429 {
			return "rate_limit"
		}
		return "upstream_error"
	case KindNetwork:
		switch networkKindOf(e.Underlying) {
		case netTimeout:
			return "upstream_timeout"
		case netUnreachable:
			return "upstream_unreachable"
		}
		return "network"
	}
	return "upstream_rejected"
}

// UserMessage is the short sentence surfaced verbatim to users.
func (e *ProviderError) UserMessage() string {
	name := e.Provider.UserFacingName()
	switch e.Kind {
	case KindCreditExhausted:
		return "Your " + name + " credits are exhausted."
	case KindTransient:
		if e.Status == 429 {
			return name + " is rate-limiting us."
		}
		return name + " is having trouble responding." + detailSuffix(e.Status, e.Body)
	case KindNetwork:
		if networkKindOf(e.Underlying) == netTimeout {
			return name + " timed out responding."
		}
		return "Couldn't reach " + name + "."
	}
	return name + " rejected the request." + detailSuffix(e.Status, e.Body)
}

func detailSuffix(status int, body *string) string {
	if message := extractUpstreamMessage(body); message != "" {
		return fmt.Sprintf(" (%d) %s", status, message)
	}
	return fmt.Sprintf(" (%d)", status)
}

func extractUpstreamMessage(body *string) string {
	if body == nil {
		return ""
	}
	if obj, ok := decodeObject([]byte(*body)); ok {
		if inner, ok := obj["error"].(map[string]any); ok {
			if m, ok := inner["message"].(string); ok {
				return clip(m)
			}
		}
		if s, ok := obj["error"].(string); ok {
			return clip(s)
		}
		if m, ok := obj["message"].(string); ok {
			return clip(m)
		}
	}
	return clip(*body)
}

func clip(s string) string {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(s)
	if swiftstr.Count(trimmed) > 200 {
		return swiftstr.Prefix(trimmed, 200) + "…"
	}
	return trimmed
}

type networkKind int

const (
	netOther networkKind = iota
	netTimeout
	netUnreachable
)

// networkKindOf maps a Go transport error onto Swift's URLError grouping:
// timedOut, then cannotConnectToHost, notConnectedToInternet, cannotFindHost,
// dnsLookupFailed and networkConnectionLost, then everything else.
func networkKindOf(err error) networkKind {
	if err == nil {
		return netOther
	}
	var timeout interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return netTimeout
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return netUnreachable
	}
	for _, target := range []error{
		syscall.ECONNREFUSED, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
		syscall.ECONNRESET, syscall.EPIPE, io.ErrUnexpectedEOF, io.EOF,
	} {
		if errors.Is(err, target) {
			return netUnreachable
		}
	}
	return netOther
}

// opaqueNetworkError hides a stream's transport failure from networkKindOf.
// Swift's streams run on AsyncHTTPClient, whose errors are not URLErrors, so
// even a stream timeout has reason code "network", never "upstream_timeout".
type opaqueNetworkError struct{ msg string }

func (e opaqueNetworkError) Error() string { return e.msg }

// creditMarkers flip a 403 into CreditExhausted. Matched against the lowercased
// body preview.
var creditMarkers = []string{
	"credit", "insufficient_quota", "insufficient quota", "quota_exceeded", "quota exceeded",
	"out_of_credits", "out of credits", "billing", "payment_required", "payment required",
	"insufficient_balance", "insufficient balance",
}

func containsCreditMarker(lowered string) bool {
	return slices.ContainsFunc(creditMarkers, func(m string) bool { return strings.Contains(lowered, m) })
}

// bodyPreview is String(data: body.prefix(2048), encoding: .utf8): nil when the
// bytes are not valid UTF-8, which includes a multibyte character cut at 2048.
func bodyPreview(body []byte) *string {
	if body == nil {
		return nil
	}
	if len(body) > 2048 {
		body = body[:2048]
	}
	if !utf8.Valid(body) {
		return nil
	}
	return strPtr(string(body))
}

// Classify is ProviderErrorClassifier.classify: a non-2xx response to a
// ProviderError. A nil body never matches a credit marker.
func Classify(provider ProviderKind, status int, body []byte) *ProviderError {
	preview := bodyPreview(body)
	switch {
	case status == 402:
		return CreditExhausted(provider, status, preview)
	case status == 403:
		if preview != nil && containsCreditMarker(strings.ToLower(*preview)) {
			return CreditExhausted(provider, status, preview)
		}
		return Permanent(provider, status, preview)
	case status == 408, status == 425, status == 429, status >= 500 && status < 600:
		return Transient(provider, status, preview)
	}
	return Permanent(provider, status, preview)
}

// UserFacingError is an error that can explain itself to the person who hit it:
// its message, the code of its HTTP envelope and the CTAs that are real for
// this caller. HTTP handlers render it from Status and Body.
type UserFacingError interface {
	error
	UserMessage() string
	ReasonCode() string
	RecoveryActions() []string
	HTTPStatus() int
	Body() []byte
}

// TimeoutRetryHintMs is the retry_after_ms on upstream_timeout envelopes.
const TimeoutRetryHintMs = 2000

// UpstreamErrorResponse is the structured envelope for LLM upstream failures:
// {"error":{"code","message","retry_after_ms"}}, 504 for upstream_timeout and
// 502 for everything else.
type UpstreamErrorResponse struct {
	Reason       string
	Message      string
	RetryAfterMs *int
}

func (e *UpstreamErrorResponse) Error() string       { return e.Reason + ": " + e.Message }
func (e *UpstreamErrorResponse) UserMessage() string { return e.Message }
func (e *UpstreamErrorResponse) ReasonCode() string  { return e.Reason }
func (e *UpstreamErrorResponse) RecoveryActions() []string {
	return nil
}

func (e *UpstreamErrorResponse) HTTPStatus() int {
	if e.Reason == "upstream_timeout" {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func (e *UpstreamErrorResponse) Body() []byte {
	inner := map[string]any{"code": e.Reason, "message": e.Message}
	if e.RetryAfterMs != nil {
		inner["retry_after_ms"] = *e.RetryAfterMs
	}
	return envelope(inner)
}

func envelope(inner map[string]any) []byte {
	b, err := swiftJSON(map[string]any{"error": inner})
	if err != nil {
		return nil // try? in Swift; every value here is a string, int or []string.
	}
	return b
}

// BYOKKeysRequiredError: BYOK selected but the tenant has no usable provider
// key. Always fatal to the routing walk: every candidate lacks the same key.
type BYOKKeysRequiredError struct{}

const byokMessage = "Add an LLM API key in Settings (OpenRouter recommended for Auto) or switch to Managed mode."

func (BYOKKeysRequiredError) Error() string       { return "byok_keys_required" }
func (BYOKKeysRequiredError) UserMessage() string { return byokMessage }
func (BYOKKeysRequiredError) ReasonCode() string  { return "byok_keys_required" }
func (BYOKKeysRequiredError) RecoveryActions() []string {
	return []string{"add_key", "switch_to_managed"}
}
func (BYOKKeysRequiredError) HTTPStatus() int { return http.StatusForbidden }
func (e BYOKKeysRequiredError) Body() []byte {
	return envelope(map[string]any{"code": e.ReasonCode(), "message": byokMessage, "cta": e.RecoveryActions()})
}

// FreeLaneExhaustedError: the free lane was the only legal route and today's
// allowance is spent. 429 with Retry-After; set Actions empty for the default.
type FreeLaneExhaustedError struct {
	RetryAfterSeconds int
	Actions           []string
}

func (e *FreeLaneExhaustedError) actions() []string {
	if len(e.Actions) == 0 {
		return []string{"upgrade", "add_key"}
	}
	return e.Actions
}

func (e *FreeLaneExhaustedError) Error() string             { return "free_lane_exhausted" }
func (e *FreeLaneExhaustedError) ReasonCode() string        { return "free_lane_exhausted" }
func (e *FreeLaneExhaustedError) RecoveryActions() []string { return e.actions() }
func (e *FreeLaneExhaustedError) HTTPStatus() int           { return http.StatusTooManyRequests }

func (e *FreeLaneExhaustedError) UserMessage() string {
	const spent = "You've used today's free messages."
	switch {
	case slices.Contains(e.actions(), "upgrade"):
		return spent + " Upgrade for the full brain, or add your own API key in Settings to keep going now."
	case slices.Contains(e.actions(), "switch_to_managed"):
		return spent + " Switch to Managed, or add your own API key in Settings to keep going now."
	}
	return spent + " Add your own API key in Settings to keep going now."
}

func (e *FreeLaneExhaustedError) Body() []byte {
	return envelope(map[string]any{
		"code": e.ReasonCode(), "message": e.UserMessage(), "cta": e.actions(),
		"retryAfterSeconds": e.RetryAfterSeconds,
	})
}

// FreeLaneUnavailableError: the free lane was the only legal route but no key
// for any leg loaded. 503, with no reset time to promise.
type FreeLaneUnavailableError struct{ Actions []string }

const freeLaneUnavailableMessage = "Free messages are unavailable right now. Add your own API key in Settings to keep going."

func (e *FreeLaneUnavailableError) Error() string             { return "free_lane_unavailable" }
func (e *FreeLaneUnavailableError) UserMessage() string       { return freeLaneUnavailableMessage }
func (e *FreeLaneUnavailableError) ReasonCode() string        { return "free_lane_unavailable" }
func (e *FreeLaneUnavailableError) RecoveryActions() []string { return e.Actions }
func (e *FreeLaneUnavailableError) HTTPStatus() int           { return http.StatusServiceUnavailable }
func (e *FreeLaneUnavailableError) Body() []byte {
	actions := e.Actions
	if actions == nil {
		actions = []string{}
	}
	return envelope(map[string]any{"code": e.ReasonCode(), "message": freeLaneUnavailableMessage, "cta": actions})
}

// UsageCapExceededError: today's budget is spent. 429 with Retry-After.
type UsageCapExceededError struct{ RetryAfter time.Duration }

const usageCapMessage = "You've reached today's usage limit. Upgrade for more, or add your own API key in Settings to keep going now."

func (e *UsageCapExceededError) Error() string             { return "usage_cap_exceeded" }
func (e *UsageCapExceededError) UserMessage() string       { return usageCapMessage }
func (e *UsageCapExceededError) ReasonCode() string        { return "usage_cap_exceeded" }
func (e *UsageCapExceededError) RecoveryActions() []string { return []string{"upgrade", "add_key"} }
func (e *UsageCapExceededError) HTTPStatus() int           { return http.StatusTooManyRequests }

// RetryAfterSeconds is max(1, ceil(retryAfter)).
func (e *UsageCapExceededError) RetryAfterSeconds() int {
	return max(1, int(math.Ceil(e.RetryAfter.Seconds())))
}

func (e *UsageCapExceededError) Body() []byte {
	return envelope(map[string]any{
		"code": e.ReasonCode(), "message": usageCapMessage, "cta": e.RecoveryActions(),
		"retryAfterSeconds": e.RetryAfterSeconds(),
	})
}

// StreamErrorGeneric is what an SSE error says for an internal failure.
const StreamErrorGeneric = "upstream failure"

// StreamErrorEvent is the payload that ends a stream failed by err. Code and
// CTA are empty for an internal failure, whose details must not reach a client.
type StreamErrorEvent struct {
	Message string   `json:"message"`
	Code    string   `json:"code,omitempty"`
	CTA     []string `json:"cta,omitempty"`
}

// StreamErrorMessage is StreamErrorMessage.forClient.
func StreamErrorMessage(err error) string { return StreamErrorFor(err).Message }

// StreamErrorFor is StreamErrorMessage.event(for:).
func StreamErrorFor(err error) StreamErrorEvent {
	var known UserFacingError
	if errors.As(err, &known) {
		return StreamErrorEvent{Message: known.UserMessage(), Code: known.ReasonCode(), CTA: known.RecoveryActions()}
	}
	return StreamErrorEvent{Message: StreamErrorGeneric}
}
