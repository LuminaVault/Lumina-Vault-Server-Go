package llm

import (
	"lumina/internal/wire"
	"strconv"

	"github.com/labstack/echo/v5"
)

// The user-facing errors are Swift's HTTPResponseError types: each writes its own
// envelope, so a handler can simply return one. Retry-After goes on the two that
// promise a time.

var (
	_ wire.ResponseError = (*UpstreamErrorResponse)(nil)
	_ wire.ResponseError = BYOKKeysRequiredError{}
	_ wire.ResponseError = (*FreeLaneExhaustedError)(nil)
	_ wire.ResponseError = (*FreeLaneUnavailableError)(nil)
	_ wire.ResponseError = (*UsageCapExceededError)(nil)
)

func writeEnvelope(c *echo.Context, e UserFacingError) error {
	return c.Blob(e.HTTPStatus(), "application/json", e.Body())
}

// WriteResponse implements wire.ResponseError.
func (e *UpstreamErrorResponse) WriteResponse(c *echo.Context) error { return writeEnvelope(c, e) }

// WriteResponse implements wire.ResponseError.
func (e BYOKKeysRequiredError) WriteResponse(c *echo.Context) error { return writeEnvelope(c, e) }

// WriteResponse implements wire.ResponseError.
func (e *FreeLaneExhaustedError) WriteResponse(c *echo.Context) error {
	c.Response().Header().Set("Retry-After", strconv.Itoa(e.RetryAfterSeconds))
	return writeEnvelope(c, e)
}

// WriteResponse implements wire.ResponseError.
func (e *FreeLaneUnavailableError) WriteResponse(c *echo.Context) error { return writeEnvelope(c, e) }

// WriteResponse implements wire.ResponseError.
func (e *UsageCapExceededError) WriteResponse(c *echo.Context) error {
	c.Response().Header().Set("Retry-After", strconv.Itoa(e.RetryAfterSeconds()))
	return writeEnvelope(c, e)
}
