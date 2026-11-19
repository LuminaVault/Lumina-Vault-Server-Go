package connectedapps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type allowAll struct{}

func (allowAll) Validate(_ context.Context, raw string) (*url.URL, error) { return url.Parse(raw) }

type denyAll struct{}

func (denyAll) Validate(context.Context, string) (*url.URL, error) {
	return nil, errors.New("blocked")
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type seen struct {
	method  string
	session string
	auth    string
	accept  string
	hasID   bool
	params  string
}

type mcpStub struct {
	mu    sync.Mutex
	seen  []seen
	reqFn func(w http.ResponseWriter, id int, r rpcReq)
	srv   *httptest.Server
}

func newStub(t *testing.T, reqFn func(w http.ResponseWriter, id int, r rpcReq)) *mcpStub {
	t.Helper()
	s := &mcpStub{reqFn: reqFn}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.seen = append(s.seen, seen{
			req.Method, r.Header.Get("Mcp-Session-Id"), r.Header.Get("Authorization"),
			r.Header.Get("Accept"), req.ID != nil, string(req.Params),
		})
		s.mu.Unlock()
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*req.ID) + `,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			s.reqFn(w, *req.ID, req)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func newClient(s *mcpStub) *Client {
	return &Client{HTTP: s.srv.Client(), Guard: allowAll{}}
}

func jsonReply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestClientInitializesThenLists(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, id int, r rpcReq) {
		jsonReply(w, `{"jsonrpc":"2.0","id":`+itoa(id)+`,"result":{"tools":[{"name":"read_library","description":"d","inputSchema":{"type":"object"}}]}}`)
	})
	tools, err := newClient(s).ListTools(context.Background(), s.srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "read_library" || tools[0].Description != "d" || string(tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools = %+v", tools)
	}
	want := []string{"initialize", "notifications/initialized", "tools/list"}
	if len(s.seen) != len(want) {
		t.Fatalf("seen %d requests", len(s.seen))
	}
	for i, m := range want {
		if s.seen[i].method != m {
			t.Fatalf("req %d = %s want %s", i, s.seen[i].method, m)
		}
		if s.seen[i].auth != "Bearer tok" {
			t.Fatalf("req %d auth = %q", i, s.seen[i].auth)
		}
		if s.seen[i].accept != "application/json, text/event-stream" {
			t.Fatalf("req %d accept = %q", i, s.seen[i].accept)
		}
	}
	if s.seen[0].session != "" || s.seen[1].session != "sess-1" || s.seen[2].session != "sess-1" {
		t.Fatalf("session ids: %q %q %q", s.seen[0].session, s.seen[1].session, s.seen[2].session)
	}
	if s.seen[1].hasID {
		t.Fatal("notification must not carry an id")
	}
	if !strings.Contains(s.seen[0].params, `"2025-06-18"`) || !strings.Contains(s.seen[0].params, `"LuminaVault"`) {
		t.Fatalf("initialize params = %s", s.seen[0].params)
	}
}

func TestClientParsesSSEResponse(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, id int, r rpcReq) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A non-matching event first, then the real response.
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"))
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":" + itoa(id) + ",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"foo\"},{\"type\":\"image\",\"data\":\"x\"},{\"type\":\"text\",\"text\":\"bar\"}]}}\n\n"))
	})
	got, err := newClient(s).CallTool(context.Background(), s.srv.URL, "tok", "read_library", map[string]any{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != "foobar" {
		t.Fatalf("got %q", got)
	}
	last := s.seen[len(s.seen)-1]
	if last.method != "tools/call" || !strings.Contains(last.params, `"read_library"`) || !strings.Contains(last.params, `"arguments"`) {
		t.Fatalf("call = %+v", last)
	}
}

func TestClientCallReturnsToolErrorText(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, id int, r rpcReq) {
		jsonReply(w, `{"jsonrpc":"2.0","id":`+itoa(id)+`,"result":{"isError":true,"content":[{"type":"text","text":"no such item"}]}}`)
	})
	got, err := newClient(s).CallTool(context.Background(), s.srv.URL, "tok", "x", nil)
	if err != nil || got != "no such item" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestClientJSONRPCErrorNeverLeaksBearer(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, id int, r rpcReq) {
		jsonReply(w, `{"jsonrpc":"2.0","id":`+itoa(id)+`,"error":{"code":-32602,"message":"bad params"}}`)
	})
	_, err := newClient(s).CallTool(context.Background(), s.srv.URL, "sekrit-token", "x", nil)
	if err == nil || !strings.Contains(err.Error(), "bad params") || strings.Contains(err.Error(), "sekrit-token") {
		t.Fatalf("err = %v", err)
	}
}

func TestClient401IsErrUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Guard: allowAll{}}
	if _, err := c.ListTools(context.Background(), srv.URL, "tok"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("list err = %v", err)
	}
	if _, err := c.CallTool(context.Background(), srv.URL, "tok", "x", nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("call err = %v", err)
	}
}

func TestClientTimeout(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, id int, r rpcReq) {
		time.Sleep(500 * time.Millisecond)
		jsonReply(w, `{"jsonrpc":"2.0","id":`+itoa(id)+`,"result":{"tools":[]}}`)
	})
	c := newClient(s)
	c.Timeout = 50 * time.Millisecond
	_, err := c.ListTools(context.Background(), s.srv.URL, "tok")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientCapsResponse(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	s := newStub(t, func(w http.ResponseWriter, id int, r rpcReq) {
		jsonReply(w, `{"jsonrpc":"2.0","id":`+itoa(id)+`,"result":{"content":[{"type":"text","text":"`+big+`"}]}}`)
	})
	c := newClient(s)
	c.MaxBytes = 1024
	got, err := c.CallTool(context.Background(), s.srv.URL, "tok", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 1024 || !strings.HasSuffix(got, "\n[truncated]") || !strings.HasPrefix(got, "aaaa") {
		t.Fatalf("len %d suffix %q", len(got), got[max(0, len(got)-20):])
	}
}

func TestClientRejectsGuardFailure(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, id int, r rpcReq) {})
	c := &Client{HTTP: s.srv.Client(), Guard: denyAll{}}
	if _, err := c.ListTools(context.Background(), s.srv.URL, "tok"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := c.CallTool(context.Background(), s.srv.URL, "tok", "x", nil); err == nil {
		t.Fatal("expected error")
	}
	if len(s.seen) != 0 {
		t.Fatalf("%d requests reached the server", len(s.seen))
	}
}

func TestClientNilHTTPFailsClosed(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, id int, r rpcReq) {})
	c := &Client{Guard: allowAll{}}
	if _, err := c.ListTools(context.Background(), s.srv.URL, "tok"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := c.CallTool(context.Background(), s.srv.URL, "tok", "x", nil); err == nil {
		t.Fatal("expected error")
	}
	if len(s.seen) != 0 {
		t.Fatalf("%d requests reached the server", len(s.seen))
	}
}

func TestClientPartialText(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"compact cut", `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hello wor`, "hello wor"},
		{"spaced", `{"jsonrpc": "2.0", "id": 2, "result": {"content": [{"type": "text", "text": "hello wor`, "hello wor"},
		{"pretty", "{\n  \"result\": {\n    \"content\": [\n      {\n        \"type\": \"text\",\n        \"text\": \"a\\nb cut", "a\nb cut"},
		{"ignores structuredContent and resource.text", `{"result":{"structuredContent":{"text":"NO"},"content":[{"type":"resource","resource":{"text":"NO2"}},{"type":"text","text":"yes`, "yes"},
		{"two text parts", `{"result":{"content":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"thr`, "onetwothr"},
		{"text before type", `{"result":{"content":[{"text":"abc","type":"text"},{"text":"img","type":"image"},{"type":"text","text":"d`, "abcd"},
		{"garbage", `\x00\xff not json at all }}}{{`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		if got := partialText([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestClientCapTextInvalidUTF8(t *testing.T) {
	s := "ab\xffcd" + strings.Repeat("é", 5000) // bad byte early, many multibyte runes
	got := capText(s, 1001, true)
	if len(got) > 1001 || !strings.HasSuffix(got, "\n[truncated]") || !utf8.ValidString(got) {
		t.Fatalf("len %d valid %v", len(got), utf8.ValidString(got))
	}
	if !strings.HasPrefix(got, "abcdéé") {
		t.Fatalf("content after bad byte lost: %q", got[:12])
	}
}

// R17: a redirected tools/call would carry the bearer and arguments to a host
// the guard never saw.
func TestClientRefusesRedirects(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		jsonReply(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer other.Close()
	// initialize succeeds; only the tools/call POST is redirected.
	s := newStub(t, func(w http.ResponseWriter, _ int, r rpcReq) {
		if r.Method != "tools/call" {
			t.Errorf("unexpected method %s", r.Method)
		}
		w.Header().Set("Location", other.URL+"/mcp")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	caller := s.srv.Client()
	c := &Client{HTTP: caller, Guard: allowAll{}}
	_, err := c.CallTool(context.Background(), s.srv.URL, "secret-bearer", "read_library", map[string]any{"q": "x"})
	if err == nil {
		t.Fatal("want an error for a redirected call")
	}
	s.mu.Lock()
	last := s.seen[len(s.seen)-1].method
	s.mu.Unlock()
	if last != "tools/call" {
		t.Fatalf("the redirected request was %s, want tools/call", last)
	}
	for _, leak := range []string{other.URL, s.srv.URL, "secret-bearer"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("redirect target got %d requests", hits)
	}
	if caller.CheckRedirect != nil {
		t.Fatal("the caller's client was mutated")
	}
}
