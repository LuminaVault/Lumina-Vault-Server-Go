package httpserver_test

import (
	"context"
	"io"
	"lumina/internal/config"
	"lumina/internal/httpserver"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

const testReadTimeout = 150 * time.Millisecond

type slowRoute struct{ mw []echo.MiddlewareFunc }

// Register mounts a route that reads the body, then waits past the read timeout and
// reports whether its context survived.
func (m slowRoute) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.POST("/slow", func(c *echo.Context) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return c.String(http.StatusTeapot, "read: "+err.Error())
		}
		select {
		case <-time.After(3 * testReadTimeout):
			return c.String(http.StatusOK, "survived "+string(body))
		case <-c.Request().Context().Done():
			return c.String(http.StatusConflict, "canceled")
		}
	}, m.mw...)
}

// serve runs the route as production does, otelhttp included, with a short ReadTimeout.
func serve(t *testing.T, mw ...echo.MiddlewareFunc) *httptest.Server {
	t.Helper()
	cfg := config.Config{OTelEnabled: true}
	e := httpserver.New(cfg, httpserver.Deps{Modules: []httpserver.Module{slowRoute{mw}}})
	srv := httptest.NewUnstartedServer(httpserver.Handler(cfg, e))
	srv.Config.ReadTimeout = testReadTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, body io.Reader) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/slow", body)
	require.NoError(t, err)
	// httpx and URLSession both ask for gzip, and gzip wraps the response writer.
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, string(raw)
}

// slowBody hands out its bytes across more than the read timeout.
type slowBody struct{ left int }

func (b *slowBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(testReadTimeout)
	b.left--
	p[0] = 'y'
	return 1, nil
}

// The hazard itself: if this ever passes, the exemption can go.
func TestReadTimeoutFailsASlowUpload(t *testing.T) {
	code, body := post(t, serve(t), &slowBody{left: 3})
	require.Equal(t, http.StatusTeapot, code, body)
}

func TestLongUploadLetsASlowUploadFinish(t *testing.T) {
	code, body := post(t, serve(t, httpserver.LongUpload()), &slowBody{left: 3})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "survived yyy", body)
}

// Waiting on whisper needs no exemption: the deadline is gone once the body is read.
func TestReadTimeoutDoesNotLimitAHandlerAfterTheBody(t *testing.T) {
	code, body := post(t, serve(t), strings.NewReader("x"))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "survived x", body)
}

type okRoute struct{}

func (okRoute) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.POST("/ok", func(c *echo.Context) error { return c.NoContent(http.StatusOK) }, httpserver.LongUpload())
}

// A test recorder has no connection. The request must still run.
func TestLongUploadWithoutAConnection(t *testing.T) {
	e := httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{okRoute{}}})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ok", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}
