package httpserver

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

// LongUploadBudget is how long a LongUpload route may take to receive its body.
const LongUploadBudget = 10 * time.Minute

// LongUpload exempts a route from echo.StartConfig's 30 s http.Server.ReadTimeout,
// which is one deadline for the whole request, body included. Mount it first on the
// route, and on any route that takes a body too large to send in 30 s on a bad
// network: past that, reading the body fails. Swift has no such limit.
//
// net/http clears the deadline once the body has been read, so it never limits the
// time a handler spends after that, such as waiting on an upstream. Router.Streaming
// has nothing to do with it: it only exempts a route from gzip.
func LongUpload() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			deadline := time.Now().Add(LongUploadBudget)
			if err := http.NewResponseController(c.Response()).SetReadDeadline(deadline); err != nil {
				slog.WarnContext(c.Request().Context(), "long upload: read deadline not extended", "path", c.Path(), "error", err)
			}
			return next(c)
		}
	}
}
