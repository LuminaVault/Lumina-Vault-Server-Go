package ws_test

import (
	"context"
	"errors"
	"lumina/internal/ws"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeWriter struct {
	mu   sync.Mutex
	got  []string
	fail error
	ctxs []context.Context
	errs []error // ctx.Err() at write time
}

func (f *fakeWriter) Write(ctx context.Context, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxs = append(f.ctxs, ctx)
	f.errs = append(f.errs, ctx.Err())
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, message)
	return nil
}

func (f *fakeWriter) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

const (
	alice = "A0000000-0000-4000-8000-000000000001"
	bob   = "B0000000-0000-4000-8000-000000000002"
)

func TestBroadcastReachesEverySocketOfTheTenantOnly(t *testing.T) {
	m := ws.NewManager()
	a1, a2, b1 := &fakeWriter{}, &fakeWriter{}, &fakeWriter{}
	m.Register(alice, "alice", a1)
	m.Register(alice, "alice", a2)
	m.Register(bob, "bob", b1)

	m.Broadcast(context.Background(), alice, `{"type":"x"}`)
	require.Equal(t, []string{`{"type":"x"}`}, a1.messages())
	require.Equal(t, []string{`{"type":"x"}`}, a2.messages())
	require.Empty(t, b1.messages())
	require.Len(t, m.Connections(alice), 2)
	require.Len(t, m.Connections(bob), 1)
}

func TestBroadcastToTenantWithoutSocketsIsANoop(t *testing.T) {
	m := ws.NewManager()
	m.Broadcast(context.Background(), alice, "anything")
	require.Empty(t, m.Connections(alice))
}

func TestTenantKeysAreCaseInsensitive(t *testing.T) {
	m := ws.NewManager()
	w := &fakeWriter{}
	m.Register(strings.ToLower(alice), "alice", w)
	m.Broadcast(context.Background(), alice, "m")
	require.Equal(t, []string{"m"}, w.messages())
	c := m.Connections(alice)
	require.Len(t, c, 1)
	require.Equal(t, alice, c[0].TenantID)
	require.Equal(t, "alice", c[0].Username)
	require.False(t, c[0].ConnectedAt.IsZero())
}

func TestRemoveDropsTheSocketAndUnknownRemoveIsANoop(t *testing.T) {
	m := ws.NewManager()
	w := &fakeWriter{}
	id := m.Register(alice, "alice", w)
	m.Remove(alice, uuid.New())
	require.Len(t, m.Connections(alice), 1)
	m.Remove(alice, id)
	require.Empty(t, m.Connections(alice))
	m.Broadcast(context.Background(), alice, "m")
	require.Empty(t, w.messages())
}

func TestFailedWriteDropsOnlyThatSocket(t *testing.T) {
	m := ws.NewManager()
	bad, good := &fakeWriter{fail: errors.New("broken pipe")}, &fakeWriter{}
	m.Register(alice, "alice", bad)
	goodID := m.Register(alice, "alice", good)
	m.Broadcast(context.Background(), alice, "one")
	m.Broadcast(context.Background(), alice, "two")
	require.Equal(t, []string{"one", "two"}, good.messages())
	conns := m.Connections(alice)
	require.Len(t, conns, 1)
	require.Equal(t, goodID, conns[0].ID)
	bad.mu.Lock()
	require.Len(t, bad.ctxs, 1, "a dropped socket is not written again")
	bad.mu.Unlock()
}

func TestBroadcastWritesOutliveTheCallersCancellation(t *testing.T) {
	m := ws.NewManager()
	w := &fakeWriter{}
	m.Register(alice, "alice", w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Broadcast(ctx, alice, "m")
	require.Equal(t, []string{"m"}, w.messages())
	w.mu.Lock()
	defer w.mu.Unlock()
	require.NoError(t, w.errs[0], "the write's context was live")
	deadline, ok := w.ctxs[0].Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(ws.WriteTimeout), deadline, 2*time.Second)
}

// MARK: Serve over a real socket

type server struct {
	m   *ws.Manager
	url string
}

func newServer(t *testing.T) server {
	t.Helper()
	m := ws.NewManager()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := uuid.MustParse(r.URL.Query().Get("tenant"))
		conn, err := ws.Accept(w, r)
		if err != nil {
			return
		}
		m.Serve(r.Context(), conn, tenant, r.URL.Query().Get("user"))
	}))
	t.Cleanup(srv.Close)
	return server{m: m, url: "ws" + strings.TrimPrefix(srv.URL, "http")}
}

func (s server) dial(t *testing.T, tenant string, header http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, s.url+"?tenant="+tenant+"&user=u", &websocket.DialOptions{HTTPHeader: header})
	require.NoError(t, err)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	require.Eventually(t, func() bool { return len(s.m.Connections(tenant)) > 0 }, 2*time.Second, 10*time.Millisecond)
	return c
}

func read(t *testing.T, c *websocket.Conn) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		return "", err
	}
	require.Equal(t, websocket.MessageText, typ)
	return string(data), nil
}

func write(t *testing.T, c *websocket.Conn, typ websocket.MessageType, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, typ, []byte(s)))
}

func TestServeRelaysValidFramesToTheTenantsSocketsIncludingTheSender(t *testing.T) {
	s := newServer(t)
	a1 := s.dial(t, alice, nil)
	a2 := s.dial(t, alice, nil)
	require.Eventually(t, func() bool { return len(s.m.Connections(alice)) == 2 }, 2*time.Second, 10*time.Millisecond)
	b := s.dial(t, bob, nil)

	// Rejected frames are not relayed; the next valid one is the first to arrive.
	write(t, a1, websocket.MessageText, "hello world")
	write(t, a1, websocket.MessageText, `{"foo":"bar"}`)
	write(t, a1, websocket.MessageBinary, `{"type":"binary"}`)
	write(t, a1, websocket.MessageText, `{"type":"presence"}`)

	for _, c := range []*websocket.Conn{a1, a2} {
		got, err := read(t, c)
		require.NoError(t, err)
		require.Equal(t, `{"type":"presence"}`, got)
	}

	// Bob's socket got nothing: its first message is its own.
	write(t, b, websocket.MessageText, `{"type":"mine"}`)
	got, err := read(t, b)
	require.NoError(t, err)
	require.Equal(t, `{"type":"mine"}`, got)

	// A publisher reaches the sockets too.
	s.m.Broadcast(context.Background(), alice, `{"type":"memory_compile"}`)
	got, err = read(t, a2)
	require.NoError(t, err)
	require.Equal(t, `{"type":"memory_compile"}`, got)
}

func TestServeAcceptsAnyOrigin(t *testing.T) {
	s := newServer(t)
	s.dial(t, alice, http.Header{"Origin": []string{"https://elsewhere.example"}})
}

func TestServeClosesOversizeMessagesWith1009AndUnregisters(t *testing.T) {
	s := newServer(t)
	c := s.dial(t, alice, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, []byte(strings.Repeat("a", ws.MaxMessageBytes+1)))
	_, err := read(t, c)
	require.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(err))
	require.Eventually(t, func() bool { return len(s.m.Connections(alice)) == 0 }, 2*time.Second, 10*time.Millisecond)
}

func TestServeAcceptsAMessageAtTheCap(t *testing.T) {
	s := newServer(t)
	c := s.dial(t, alice, nil)
	typeKey, suffix := `{"type":"x","pad":"`, `"}`
	msg := typeKey + strings.Repeat("a", ws.MaxMessageBytes-len(typeKey)-len(suffix)) + suffix
	write(t, c, websocket.MessageText, msg)
	got, err := read(t, c)
	require.NoError(t, err)
	require.Equal(t, msg, got)
}

func TestServeUnregistersWhenThePeerCloses(t *testing.T) {
	s := newServer(t)
	c := s.dial(t, alice, nil)
	require.NoError(t, c.Close(websocket.StatusNormalClosure, ""))
	require.Eventually(t, func() bool { return len(s.m.Connections(alice)) == 0 }, 2*time.Second, 10*time.Millisecond)
}
