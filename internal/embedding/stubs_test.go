package embedding

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// captured is the one request a stub saw. Stubs only: no network, no real key.
type captured struct {
	mu          sync.Mutex
	calls       int
	method      string
	path        string
	auth        string
	contentType string
	body        map[string]any
}

func (c *captured) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	c.calls++
	c.method, c.path = r.Method, r.URL.Path
	c.auth, c.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
	c.body = nil
	_ = json.Unmarshal(raw, &c.body)
}

func stub(t *testing.T, status int, body string) (*url.URL, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u, c
}

// roundTripper answers without a socket, for assertions on a default base URL.
type roundTripper struct {
	status int
	body   string
	url    string
	c      captured
}

func (rt *roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.url = r.URL.String()
	rt.c.record(r)
	return &http.Response{
		StatusCode: rt.status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Request:    r,
	}, nil
}

func floats(n int, v string) string {
	if n == 0 {
		return ""
	}
	return v + strings.Repeat(","+v, n-1)
}

// okPayload is the OpenAI-wire answer with dim copies of 0.01.
func okPayload(dim, tokens int) string {
	return `{"data":[{"embedding":[` + floats(dim, "0.01") + `]}],"usage":{"total_tokens":` + strconv.Itoa(tokens) + `}}`
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	require.Error(t, err)
	var e *Error
	require.ErrorAs(t, err, &e)
	return e
}
