package hermesdash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Caps and timeout of HermesDashboardClient (and the gateway read client).
const (
	ListBodyCap = 8 * 1024 * 1024
	FileBodyCap = 2 * 1024 * 1024
	Timeout     = 15 * time.Second
	MaxRetries  = 3
)

// Doer is HermesHTTPExecuting: the seam tests replace.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewClient is BYOHTTP.httpClient: no redirects (a 3xx comes back as itself).
func NewClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Response is HermesHTTPResponse.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r Response) IsSuccess() bool  { return r.Status >= 200 && r.Status < 300 }
func (r Response) IsRedirect() bool { return r.Status >= 300 && r.Status < 400 }

// JSON is json(): nil when the body is not JSON.
func (r Response) JSON() any { return decodeAny(r.Body) }

// Object is jsonObject().
func (r Response) Object() (map[string]any, bool) { return asObject(r.JSON()) }

// errTooLarge marks a body over the cap (NIOTooManyBytesError).

// execute is AsyncHTTPClientHermesHTTP.execute: one request with a timeout and a
// body cap. A body over the cap is bodyTooLarge(url, cap).
func execute(ctx context.Context, doer Doer, method, url string, header http.Header, body any, timeout time.Duration, maxBody int) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return Response{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return Response{}, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := doer.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)+1))
	if err != nil {
		return Response{}, err
	}
	if len(data) > maxBody {
		return Response{}, BodyTooLarge(url, maxBody)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// asTransportError passes a transport error through and reports anything else
// as unreachable (the debug-logged catch-all in both clients).
func asTransportError(err error, path string) error {
	var te *Error
	if errors.As(err, &te) {
		return te
	}
	return DashboardUnreachable(path)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
