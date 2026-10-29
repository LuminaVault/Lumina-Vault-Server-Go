package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"lumina/internal/config"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var prodCfg = config.Config{CORSAllowedOrigins: []string{"https://app.luminavault.fyi", "https://luminavault.fyi"}}

// testModule registers the routes the tests below exercise.
type testModule struct{}

func (testModule) Register(r Router, auth echo.MiddlewareFunc) {
	big := strings.Repeat("x", 2048)
	r.GET("/v1/test/json", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"data": big})
	})
	r.GET("/v1/test/sse", func(c *echo.Context) error {
		c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
		c.Response().WriteHeader(http.StatusOK)
		_, err := c.Response().Write([]byte("data: " + big + "\n\n"))
		return err
	})
	r.Streaming(http.MethodGet, "/v1/test/sse")
	r.POST("/v1/test/protected", func(c *echo.Context) error { return c.NoContent(http.StatusOK) }, auth)
	r.GET("/v1/test/message", func(*echo.Context) error {
		return wire.NewError(http.StatusBadRequest, "title must not be empty")
	})
}

func denyAll(echo.HandlerFunc) echo.HandlerFunc {
	return func(*echo.Context) error { return echo.ErrUnauthorized }
}

func do(t *testing.T, e *echo.Echo, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func newTestServer() *echo.Echo {
	return New(prodCfg, Deps{Auth: denyAll, Modules: []Module{testModule{}}})
}

func TestBaseRoutes(t *testing.T) {
	e := newTestServer()
	rec := do(t, e, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", rec.Body.String())

	rec = do(t, e, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, "Hello!", rec.Body.String())

	rec = do(t, e, httptest.NewRequest(http.MethodGet, "/.well-known/apple-app-site-association", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, AASA, rec.Body.String())
	require.Equal(t, "application/json", rec.Header().Get(echo.HeaderContentType))
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/v1/test/protected", nil)
	req.Header.Set("Origin", "https://app.luminavault.fyi")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := do(t, newTestServer(), req)
	require.Less(t, rec.Code, 300)
	require.Equal(t, "https://app.luminavault.fyi", rec.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"))
	require.Equal(t, "600", rec.Header().Get("Access-Control-Max-Age"))
	require.Contains(t, strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")), "authorization")
}

func TestCORSDisallowedOriginGetsNoAllowHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := do(t, newTestServer(), req)
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestGzipSkipsEventStream(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/test/sse", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := do(t, newTestServer(), req)
	require.Empty(t, rec.Header().Get("Content-Encoding"))
	require.True(t, strings.HasPrefix(rec.Body.String(), "data: "))
}

func TestGzipCompressesLargeJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/test/json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := do(t, newTestServer(), req)
	require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
}

func TestDecompressesGzipRequestBodies(t *testing.T) {
	e := echo.New()
	var got string
	e.Use(New(prodCfg, Deps{}).Middlewares()...)
	e.POST("/echo", func(c *echo.Context) error {
		var body map[string]string
		if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
			return err
		}
		got = body["a"]
		return c.NoContent(http.StatusOK)
	})
	var buf bytes.Buffer
	gz := newGzipWriter(&buf)
	_, _ = gz.Write([]byte(`{"a":"b"}`))
	_ = gz.Close()
	req := httptest.NewRequest(http.MethodPost, "/echo", &buf)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, e, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "b", got)
}

func TestErrorShapes(t *testing.T) {
	e := newTestServer()
	rec := do(t, e, httptest.NewRequest(http.MethodGet, "/v1/test/message", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, `{"error":{"message":"title must not be empty"}}`, rec.Body.String())

	rec = do(t, e, httptest.NewRequest(http.MethodPost, "/v1/test/protected", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// errors-http.json is captured from the Swift binary (Task 0.3). Each case
// whose route exists in this test server must match status and body.
func TestErrorBodiesMatchGolden(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/errors-http.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("golden fixture not captured yet (Phase 0 Task 0.3)")
	}
	require.NoError(t, err)
	var cases []struct {
		Name   string `json:"name"`
		Method string `json:"method"`
		Path   string `json:"path"`
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	require.NoError(t, json.Unmarshal(raw, &cases))
	e := New(prodCfg, Deps{Auth: denyAll})
	e.GET("/v1/todos", func(c *echo.Context) error { return c.NoContent(http.StatusOK) }, denyAll)
	for _, tc := range cases {
		if tc.Name != "no-token" && tc.Name != "unknown-route" {
			continue // the rest need the Todos module (Task 4.3)
		}
		rec := do(t, e, httptest.NewRequest(tc.Method, tc.Path, nil))
		require.Equal(t, tc.Status, rec.Code, tc.Name)
		if tc.Body == "" {
			require.Empty(t, rec.Body.String(), tc.Name)
		} else {
			require.JSONEq(t, tc.Body, rec.Body.String(), tc.Name)
		}
	}
}
