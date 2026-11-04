package wire

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"
)

// Error is an HTTP error with a client-visible message. It renders as
// Hummingbird's HTTPError does: {"error":{"message":"..."}} with Content-Type
// application/json; charset=utf-8 (golden: testdata/golden/errors-http.json).
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string   { return e.Message }
func (e *Error) StatusCode() int { return e.Status }

// ErrUnauthenticated is Hummingbird's requireIdentity() failure: every
// rejected or missing session token on a protected route.
var ErrUnauthenticated = NewError(http.StatusUnauthorized, "Authenticated identity is unavailable")

// NewError returns an *Error for status with a client-visible message.
func NewError(status int, message string) *Error {
	return &Error{Status: status, Message: message}
}

// ResponseError is an error that writes its own response, for a body that isn't
// the {"error":{"message":...}} envelope. A plain status error would lose it:
// HTTPErrorHandler writes unknown errors with an empty body.
type ResponseError interface {
	error
	WriteResponse(c *echo.Context) error
}

type errorBody struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// HTTPErrorHandler is installed as echo.Echo.HTTPErrorHandler. An *Error
// writes its message; any other error (echo sentinels such as echo.ErrNotFound,
// and plain errors) writes the status with an empty body and no Content-Type,
// which is what Hummingbird does for an unknown route. Unknown errors are 500
// and logged, never echoed to the client.
func HTTPErrorHandler(c *echo.Context, err error) {
	if resp, unwrapErr := echo.UnwrapResponse(c.Response()); unwrapErr == nil && resp.Committed {
		return
	}
	var re ResponseError
	if errors.As(err, &re) {
		if writeErr := re.WriteResponse(c); writeErr != nil {
			slog.ErrorContext(c.Request().Context(), "error response failed", "error", writeErr, "path", c.Path())
		}
		return
	}
	var we *Error
	if errors.As(err, &we) {
		var body errorBody
		body.Error.Message = we.Message
		_ = JSON(c, we.Status, body)
		return
	}
	status := echo.StatusCode(err)
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(c.Request().Context(), "request failed", "error", err, "path", c.Path())
	}
	_ = c.NoContent(status)
}
