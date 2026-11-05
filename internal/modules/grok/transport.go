package grok

import (
	"bytes"
	"context"
	"errors"
	"io"
	"lumina/internal/hermescontainers"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/labstack/echo/v5"
)

// maxRedirects is HTTPClient.shared's .follow(max: 20, allowCycles: false), from
// AsyncHTTPClient's Configuration+BrowserLike.swift.
const maxRedirects = 20

var (
	errRedirectLimit = errors.New("grok: redirect limit reached")
	errRedirectCycle = errors.New("grok: redirect cycle detected")
)

// brokenReply is what Hummingbird does with an HTTPParserError, which AsyncHTTPClient
// throws on a reply that isn't valid HTTP, including a connection closed before the
// reply ends. mapProxyError passes it through, and Hummingbird's server rethrows that
// type from the responder as if the client's request had been unparseable: a bare 400
// that closes the client's connection (HTTPChannelHandler.swift).
type brokenReply struct{ cause error }

func (e brokenReply) Error() string { return "grok: broken reply: " + e.cause.Error() }
func (e brokenReply) Unwrap() error { return e.cause }

func (brokenReply) WriteResponse(c *echo.Context) error {
	c.Response().Header().Set("Connection", "close")
	return c.NoContent(http.StatusBadRequest)
}

// parserOr turns the failures AsyncHTTPClient's parser reports into brokenReply. A
// clean close mid-reply is EOF. net/http says "malformed" for bad status lines, headers
// and chunks, with no type to match. Anything else, such as a reset, stays a 500.
func parserOr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "malformed") {
		return brokenReply{err}
	}
	return err
}

// execute is AsyncHTTPClient's executeAndFollowRedirectsIfNeeded, because net/http's
// own following differs: it treats 304 and 305 as final and retries a cycle.
func (m *Module) execute(ctx context.Context, h *hermescontainers.Handle, path string, payload []byte) (*http.Response, error) {
	method, target := http.MethodPost, h.BaseURL()+path
	contentType, bearer := "application/json", "Bearer "+h.APIServerKey
	visited := []string{target}
	for {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if bearer != "" {
			req.Header.Set("Authorization", bearer)
		}
		resp, err := m.client.Do(req)
		if err != nil {
			return nil, err
		}
		next := redirectTarget(resp, req.URL)
		if next == nil {
			return resp, nil
		}
		_ = resp.Body.Close()
		// RedirectState.redirect: visited starts with the first URL.
		if len(visited) > maxRedirects {
			return nil, errRedirectLimit
		}
		if slices.Contains(visited, next.String()) {
			return nil, errRedirectCycle
		}
		visited = append(visited, next.String())
		// transformRequestForRedirect.
		if convertsToGet(resp.StatusCode, method) {
			method, payload, contentType = http.MethodGet, nil, ""
		}
		if !sameOrigin(req.URL, next) {
			bearer = ""
		}
		target = next.String()
	}
}

// redirectTarget is extractRedirectTarget, nil when the reply is final. 304 and 305
// count, and a Location it can't follow leaves the 3xx as the answer.
func redirectTarget(resp *http.Response, from *url.URL) *url.URL {
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusNotModified,
		http.StatusUseProxy, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return nil
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return nil
	}
	next, err := from.Parse(location)
	if err != nil {
		return nil
	}
	if scheme := strings.ToLower(next.Scheme); scheme != "http" && scheme != "https" {
		return nil
	}
	return next
}

func convertsToGet(status int, method string) bool {
	switch status {
	case http.StatusSeeOther:
		return method != http.MethodHead
	case http.StatusMovedPermanently, http.StatusFound:
		return method == http.MethodPost
	}
	return false
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Host == b.Host
}
