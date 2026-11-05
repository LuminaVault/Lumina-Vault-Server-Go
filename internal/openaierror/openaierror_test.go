package openaierror_test

import (
	"errors"
	"lumina/internal/openaierror"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// paywallError stands in for a gate that writes its own body through wire.ResponseError.
type paywallError struct{}

func (paywallError) Error() string { return "paywall" }

func (paywallError) WriteResponse(c *echo.Context) error {
	return wire.JSON(c, http.StatusPaymentRequired, map[string]bool{"paywall": true})
}

func run(t *testing.T, h echo.HandlerFunc, mw ...echo.MiddlewareFunc) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	all := append([]echo.MiddlewareFunc{func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Response().Header().Set("X-Outer", "kept") // set by a middleware outside it
			return next(c)
		}
	}, openaierror.Middleware()}, mw...)
	e.POST("/v1/audio/transcriptions", h, all...)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil))
	return rec
}

func TestEnvelopeForEveryStatus(t *testing.T) {
	msg := "custom"
	cases := []struct {
		status  int
		message *string
		want    string
	}{
		{401, nil, `{"error":{"message":"Not authorized for this audio endpoint.","type":"authentication_error","code":"invalid_api_key"}}`},
		{403, &msg, `{"error":{"message":"custom","type":"authentication_error","code":"invalid_api_key"}}`},
		{402, nil, `{"error":{"message":"Voice is not available on this plan.","type":"insufficient_quota","code":"insufficient_quota"}}`},
		{413, nil, `{"error":{"message":"Audio file is too large.","type":"invalid_request_error","code":"file_too_large"}}`},
		{415, nil, `{"error":{"message":"Unsupported audio format.","type":"invalid_request_error","code":"unsupported_format"}}`},
		{429, nil, `{"error":{"message":"You have reached the voice limit for now. Try again later.","type":"rate_limit_error","code":"rate_limit_exceeded"}}`},
		{400, nil, `{"error":{"message":"Bad Request","type":"invalid_request_error"}}`},
		{404, &msg, `{"error":{"message":"custom","type":"invalid_request_error"}}`},
		{422, nil, `{"error":{"message":"Unprocessable Content","type":"invalid_request_error"}}`},
		{416, nil, `{"error":{"message":"Range Not Satisfiable","type":"invalid_request_error"}}`},
		{501, nil, `{"error":{"message":"This audio capability is not enabled.","type":"server_error","code":"not_implemented"}}`},
		{502, nil, `{"error":{"message":"The speech service is temporarily unavailable.","type":"server_error","code":"upstream_error"}}`},
		{503, &msg, `{"error":{"message":"custom","type":"server_error","code":"upstream_error"}}`},
		{504, nil, `{"error":{"message":"The speech service is temporarily unavailable.","type":"server_error","code":"upstream_error"}}`},
		{500, nil, `{"error":{"message":"Internal Server Error","type":"server_error"}}`},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		require.NoError(t, openaierror.Write(rec, tc.status, openaierror.ForStatus(tc.status, tc.message)))
		require.Equal(t, tc.status, rec.Code)
		require.Equal(t, wire.ContentTypeJSON, rec.Header().Get("Content-Type"))
		require.Equal(t, tc.want, rec.Body.String())
	}
}

func TestSlashIsEscapedAsFoundationDoes(t *testing.T) {
	msg := "use audio/ogg"
	rec := httptest.NewRecorder()
	require.NoError(t, openaierror.Write(rec, 415, openaierror.ForStatus(415, &msg)))
	require.Contains(t, rec.Body.String(), `"use audio\/ogg"`)
}

func TestThrownErrorKeepsMessageAndRetryAfter(t *testing.T) {
	store := ratelimit.NewStore()
	policy := ratelimit.Policy{Name: "p", Max: 0, Window: time.Minute}
	limit := ratelimit.Middleware(store, policy, func(*echo.Context) string { return "k" })
	rec := run(t, func(*echo.Context) error { return nil }, limit)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, `{"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`, rec.Body.String())
	require.Equal(t, "60", rec.Header().Get("Retry-After"))
	require.Equal(t, wire.ContentTypeJSON, rec.Header().Get("Content-Type"))
	require.Equal(t, "kept", rec.Header().Get("X-Outer"))
}

func TestWrittenFailureLosesItsBodyAndMessage(t *testing.T) {
	rec := run(t, func(c *echo.Context) error {
		c.Response().Header().Set("X-Inner", "dropped")
		return wire.JSON(c, http.StatusPaymentRequired, map[string]any{"paywall": true, "paywallId": "default"})
	})
	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	require.Equal(t, `{"error":{"message":"Voice is not available on this plan.","type":"insufficient_quota","code":"insufficient_quota"}}`, rec.Body.String())
	require.Empty(t, rec.Header().Get("X-Inner"), "only Retry-After survives from below")
	require.Equal(t, "kept", rec.Header().Get("X-Outer"))
}

func TestResponseErrorCountsAsAWrittenFailure(t *testing.T) {
	rec := run(t, func(*echo.Context) error { return paywallError{} })
	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	require.Contains(t, rec.Body.String(), "insufficient_quota")
	require.NotContains(t, rec.Body.String(), "paywall")
}

func TestStatusErrorHasNoMessage(t *testing.T) {
	rec := run(t, func(*echo.Context) error { return wire.ErrBodyTooLarge })
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "Audio file is too large.")
}

func TestUnauthenticatedKeepsRequireIdentitysMessage(t *testing.T) {
	rec := run(t, func(*echo.Context) error { return wire.ErrUnauthenticated })
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `{"error":{"message":"Authenticated identity is unavailable","type":"authentication_error","code":"invalid_api_key"}}`, rec.Body.String())
}

func TestUnknownErrorStaysOpaque(t *testing.T) {
	rec := run(t, func(*echo.Context) error { return errors.New("sql: secret detail") })
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Empty(t, rec.Body.String())
}

func TestSuccessPassesThrough(t *testing.T) {
	rec := run(t, func(c *echo.Context) error {
		c.Response().Header().Set("X-Inner", "kept")
		return c.String(http.StatusCreated, "hello")
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "hello", rec.Body.String())
	require.Equal(t, "kept", rec.Header().Get("X-Inner"))
}

func TestFlushDoesNotCommitAHeldBackFailure(t *testing.T) {
	rec := run(t, func(c *echo.Context) error {
		c.Response().WriteHeader(http.StatusBadGateway)
		return http.NewResponseController(c.Response()).Flush()
	})
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "upstream_error")
}
