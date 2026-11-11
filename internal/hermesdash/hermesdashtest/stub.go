// Package hermesdashtest is StubHermesHTTP: a canned-response hermesdash.Doer
// for tests. It imports nothing from hermesdash so internal tests can use it.
package hermesdashtest

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Recorded is one request the stub saw.
type Recorded struct {
	Method string
	URL    string
	Header http.Header
	Body   string
}

type canned struct {
	status int
	body   string
	header http.Header
}

// Stub answers "METHOD path" from a queue of canned responses. The last
// response for a key repeats; an unknown key is a FastAPI-shaped 404.
type Stub struct {
	mu        sync.Mutex
	responses map[string][]canned
	recorded  []Recorded
	// Failure, when set, is returned from every Do before anything is recorded.
	Failure error
}

// New returns an empty stub.
func New() *Stub { return &Stub{responses: map[string][]canned{}} }

// Respond queues a response. headers are name/value pairs.
func (s *Stub) Respond(method, path string, status int, body string, headers ...string) {
	h := http.Header{}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Add(headers[i], headers[i+1])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := method + " " + path
	s.responses[key] = append(s.responses[key], canned{status: status, body: body, header: h})
}

// Requests returns every request seen so far.
func (s *Stub) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.recorded...)
}

// Paths returns "METHOD path" for every request seen so far.
func (s *Stub) Paths() []string {
	var out []string
	for _, r := range s.Requests() {
		path := r.URL
		if i := strings.Index(path, "://"); i >= 0 {
			path = path[i+3:]
			if j := strings.Index(path, "/"); j >= 0 {
				path = path[j:]
			}
		}
		if i := strings.Index(path, "?"); i >= 0 {
			path = path[:i]
		}
		out = append(out, r.Method+" "+path)
	}
	return out
}

// Do implements hermesdash.Doer.
func (s *Stub) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	failure := s.Failure
	s.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	var body string
	if req.Body != nil {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = string(data)
	}
	key := req.Method + " " + req.URL.Path
	s.mu.Lock()
	s.recorded = append(s.recorded, Recorded{Method: req.Method, URL: req.URL.String(), Header: req.Header.Clone(), Body: body})
	queue := s.responses[key]
	var next *canned
	if len(queue) > 0 {
		c := queue[0]
		next = &c
		if len(queue) > 1 {
			s.responses[key] = queue[1:]
		}
	}
	s.mu.Unlock()
	if next == nil {
		next = &canned{status: http.StatusNotFound, body: `{"detail":"Not Found"}`, header: http.Header{}}
	}
	return &http.Response{
		StatusCode: next.status,
		Header:     next.header.Clone(),
		Body:       io.NopCloser(bytes.NewReader([]byte(next.body))),
		Request:    req,
	}, nil
}
