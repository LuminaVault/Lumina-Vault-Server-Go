package llm

import (
	"fmt"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserFacingErrorsWriteTheirOwnEnvelope(t *testing.T) {
	retry := TimeoutRetryHintMs
	for name, c := range map[string]struct {
		err    error
		status int
		body   string
		after  string
	}{
		"unreachable": {
			err:    &UpstreamErrorResponse{Reason: "upstream_unreachable", Message: "Couldn't reach Hermes."},
			status: http.StatusBadGateway,
			body:   `{"error":{"code":"upstream_unreachable","message":"Couldn't reach Hermes."}}`,
		},
		"timeout": {
			err:    &UpstreamErrorResponse{Reason: "upstream_timeout", Message: "Hermes timed out responding.", RetryAfterMs: &retry},
			status: http.StatusGatewayTimeout,
			body:   `{"error":{"code":"upstream_timeout","message":"Hermes timed out responding.","retry_after_ms":2000}}`,
		},
		"byok": {
			err:    BYOKKeysRequiredError{},
			status: http.StatusForbidden,
			body:   `{"error":{"code":"byok_keys_required","cta":["add_key","switch_to_managed"],"message":"` + byokMessage + `"}}`,
		},
		"free lane spent": {
			err:    &FreeLaneExhaustedError{RetryAfterSeconds: 60},
			status: http.StatusTooManyRequests,
			after:  "60",
		},
		"free lane down": {
			err:    &FreeLaneUnavailableError{},
			status: http.StatusServiceUnavailable,
		},
		"usage cap": {
			err:    &UsageCapExceededError{RetryAfter: 90 * time.Second},
			status: http.StatusTooManyRequests,
			after:  "90",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := echo.New()
			e.HTTPErrorHandler = wire.HTTPErrorHandler
			// Wrapped, as a caller adding context would: errors.As still finds it.
			e.GET("/", func(*echo.Context) error { return fmt.Errorf("agent: %w", c.err) })
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			assert.Equal(t, c.status, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, c.after, rec.Header().Get("Retry-After"))
			require.NotEmpty(t, rec.Body.String())
			if c.body != "" {
				assert.JSONEq(t, c.body, rec.Body.String())
			}
		})
	}
}
