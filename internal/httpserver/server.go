// Package httpserver builds the Echo instance: the middleware stack in the
// Swift server's order (Sources/App/App+build.swift buildRouter), the
// root-mounted routes, and module registration.
package httpserver

import (
	"lumina/internal/config"
	"lumina/internal/wire"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// Module is one migrated Swift module. Register adds its routes; protected
// routes use auth.
type Module interface {
	Register(r Router, auth echo.MiddlewareFunc)
}

// Router is what modules see: Echo's route methods plus Streaming, which
// exempts an SSE route from gzip.
type Router interface {
	GET(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	POST(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	PUT(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	PATCH(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	DELETE(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) echo.RouteInfo
	Streaming(method, path string)
}

type Deps struct {
	// Auth protects module routes. Nil only in tests that register no module.
	Auth    echo.MiddlewareFunc
	Modules []Module
}

// AASA backs Sign in with Apple and passkeys for com.lumina.fernando (team
// 84X9WYBF36); byte-identical to the Swift constant.
const AASA = `{"webcredentials":{"apps":["84X9WYBF36.com.lumina.fernando"]}}`

type server struct {
	*echo.Echo
	streaming map[string]bool // "METHOD path"
}

func (s *server) Streaming(method, path string) { s.streaming[method+" "+path] = true }

// New returns the configured Echo instance.
func New(cfg config.Config, deps Deps) *echo.Echo {
	s := &server{Echo: echo.New(), streaming: map[string]bool{}}
	e := s.Echo
	e.HTTPErrorHandler = wire.HTTPErrorHandler

	e.Use(spanName())
	e.Use(middleware.RequestID())
	e.Use(requestLogger())
	e.Use(middleware.Recover())
	e.Use(middleware.Decompress())
	e.Use(middleware.GzipWithConfig(middleware.GzipConfig{
		MinLength: 512,
		// Skipping by route, not by response Content-Type: Echo's gzip writer
		// buffers until MinLength, which would hold SSE frames back exactly
		// like the bug SSEAwareResponseCompressionMiddleware fixed in Swift.
		Skipper: func(c *echo.Context) bool {
			return s.streaming[c.Request().Method+" "+c.Path()] ||
				strings.Contains(c.Request().Header.Get(echo.HeaderAccept), "text/event-stream")
		},
	}))
	e.Use(cors(cfg.CORSAllowedOrigins))

	e.GET("/health", func(c *echo.Context) error { return wire.Text(c, http.StatusOK, "ok") })
	e.GET("/", func(c *echo.Context) error { return wire.Text(c, http.StatusOK, "Hello!") })
	e.GET("/.well-known/apple-app-site-association", func(c *echo.Context) error {
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, []byte(AASA))
	})

	for _, m := range deps.Modules {
		m.Register(s, deps.Auth)
	}
	return e
}

// Handler wraps e with otelhttp server instrumentation when OTel is enabled.
func Handler(cfg config.Config, e *echo.Echo) http.Handler {
	if !cfg.OTelEnabled {
		return e
	}
	return otelhttp.NewHandler(e, "http.server")
}

// spanName renames the otelhttp server span to "METHOD /route/:param" once
// Echo has matched the route (otelecho has no Echo v5 release).
func spanName() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if span := trace.SpanFromContext(c.Request().Context()); span.IsRecording() && c.Path() != "" {
				span.SetName(c.Request().Method + " " + c.Path())
			}
			return next(c)
		}
	}
}

// cors mirrors Swift: an empty origin list allows all origins without
// credentials (dev only); otherwise only listed origins, with credentials.
func cors(origins []string) echo.MiddlewareFunc {
	if len(origins) == 0 {
		return middleware.CORSWithConfig(middleware.CORSConfig{AllowOrigins: []string{"*"}})
	}
	return middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     origins,
		AllowHeaders:     []string{echo.HeaderContentType, echo.HeaderAuthorization},
		AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch},
		AllowCredentials: true,
		MaxAge:           600,
	})
}
