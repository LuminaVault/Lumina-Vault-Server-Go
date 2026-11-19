package connectedapps

import (
	"errors"
	"net/http"
)

// errRedirect is fixed text: it names no URL and carries no body.
var errRedirect = errors.New("connectedapps: redirects are not allowed")

// noRedirects returns a shallow copy of c that refuses every redirect; c is
// not changed. Go re-sends a POST body on a 307 or 308, so following one would
// hand tokens to a host the guard never checked, and a first-party endpoint
// has no reason to redirect.
func noRedirects(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &cp
}
