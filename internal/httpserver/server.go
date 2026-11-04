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
	// Hummingbird has no 405 and no automatic OPTIONS: a path registered for
	// another method is a plain 404.
	notFound := func(*echo.Context) error { return echo.ErrNotFound }
	s := &server{Echo: echo.NewWithConfig(echo.Config{Router: echo.NewRouter(echo.RouterConfig{
		AllowOverwritingRoute:   true,
		MethodNotAllowedHandler: notFound,
		OptionsMethodHandler:    notFound,
		AllowOverwritingRoute:   true, // Echo's own default router sets it
		MethodNotAllowedHandler: func(*echo.Context) error { return echo.ErrNotFound },
	})}), streaming: map[string]bool{}}
	e := s.Echo
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	e.Pre(normalizePath())
	// Echo's router leaves the Allow value here and its CORS middleware sends
	// it; Hummingbird never sends Allow.
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set(echo.ContextKeyHeaderAllow, nil)
			return next(c)
		}
	})

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
	if len(cfg.CORSAllowedOrigins) > 0 {
		e.Use(allowedOrigins(cfg.CORSAllowedOrigins))
	}
	e.Use(cors(cfg.CORSAllowedOrigins))
	e.Use(oneSegmentParams())

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

// normalizePath drops empty path segments before routing, because Hummingbird
// splits on "/" and ignores them: /v1/projects/ and //v1//projects are both
// /v1/projects.
func normalizePath() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			u := c.Request().URL
			if strings.Contains(u.Path, "//") || (len(u.Path) > 1 && strings.HasSuffix(u.Path, "/")) {
				segments := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
				u.Path = "/" + strings.Join(segments, "/")
				u.RawPath = ""
			}
			return next(c)
		}
	}
}

// oneSegmentParams turns a match whose path parameter holds a slash into a 404.
// Echo lets a trailing :param swallow the rest of the path, so /v1/cards/:id
// matches /v1/cards/1/x; Hummingbird's parameters are one segment. The "*"
// wildcard is exempt, since it is meant to span segments.
func oneSegmentParams() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			for _, p := range c.PathValues() {
				if p.Name != "*" && strings.Contains(p.Value, "/") {
					return echo.ErrNotFound
				}
			}
			return next(c)
		}
	}
}

// allowedOrigins is Swift's AllowedOriginsMiddleware: a request whose Origin
// is not on the list gets a 403 before anything else runs. A request with no
// Origin (same-origin, server-to-server) passes.
func allowedOrigins(origins []string) echo.MiddlewareFunc {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if v := c.Request().Header.Values(echo.HeaderOrigin); len(v) > 0 && !allowed[v[0]] {
				return wire.NewError(http.StatusForbidden, "origin not allowed")
			}
			return next(c)
		}
	}
}

// cors mirrors Swift: an empty origin list allows all origins without
// credentials (dev only); otherwise only listed origins, with credentials.
// Hummingbird treats every OPTIONS with an Origin as a preflight, while Echo
// also wants Access-Control-Request-Method, so that header is filled in.
func cors(origins []string) echo.MiddlewareFunc {
	conf := middleware.CORSConfig{AllowOrigins: []string{"*"}}
	if len(origins) > 0 {
		conf = middleware.CORSConfig{
			AllowOrigins:     origins,
			AllowHeaders:     []string{echo.HeaderContentType, echo.HeaderAuthorization},
			AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch},
			AllowCredentials: true,
			MaxAge:           600,
		}
	}
	mw := middleware.CORSWithConfig(conf)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		h := mw(next)
		return func(c *echo.Context) error {
			r := c.Request()
			if r.Method == http.MethodOptions && r.Header.Get(echo.HeaderOrigin) != "" && r.Header.Get(echo.HeaderAccessControlRequestMethod) == "" {
				r.Header.Set(echo.HeaderAccessControlRequestMethod, http.MethodOptions)
			}
			return h(c)
		}
	}
}
