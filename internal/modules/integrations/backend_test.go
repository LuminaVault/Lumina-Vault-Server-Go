package integrations

import (
	"context"
	"lumina/internal/hermescontainers"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stubStream is StubStreamingHandle.
type stubStream struct {
	lines     chan string
	exit      int
	finished  chan struct{}
	once      sync.Once
	cancelled bool
}

func newStubStream(lines []string, exit int) *stubStream {
	s := &stubStream{lines: make(chan string, len(lines)+1), exit: exit, finished: make(chan struct{})}
	for _, l := range lines {
		s.lines <- l
	}
	return s
}

func (s *stubStream) finish() { s.once.Do(func() { close(s.lines); close(s.finished) }) }

func (s *stubStream) Lines() <-chan string { return s.lines }
func (s *stubStream) Wait(ctx context.Context) (int, error) {
	select {
	case <-s.finished:
		return s.exit, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (s *stubStream) Cancel() { s.cancelled = true; s.finish() }

type invocation struct {
	kind string
	args []string
}

// stubDocker is StubDockerExec.
type stubDocker struct {
	mu          sync.Mutex
	next        hermescontainers.Streaming
	invocations []invocation
}

func (d *stubDocker) record(kind string, args []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.invocations = append(d.invocations, invocation{kind, args})
}

func (d *stubDocker) Run(_ context.Context, args []string) (hermescontainers.Result, error) {
	d.record("run", args)
	return hermescontainers.Result{}, nil
}

func (d *stubDocker) Exec(_ context.Context, _ string, cmd []string, _ []byte) (hermescontainers.Result, error) {
	d.record("exec", cmd)
	return hermescontainers.Result{}, nil
}

func (d *stubDocker) ExecStreaming(_ context.Context, _ string, cmd []string) (hermescontainers.Streaming, error) {
	d.record("stream", cmd)
	return d.next, nil
}

func (d *stubDocker) IsRunning(context.Context, string) (bool, error)   { return true, nil }
func (d *stubDocker) EnsureNetworkExists(context.Context, string) error { return nil }

func testHandle() *hermescontainers.Handle {
	return &hermescontainers.Handle{TenantID: uuid.New(), ContainerName: "hermes-tenant-test", Port: 9000, APIServerKey: "stub-key"}
}

func xaiBackend(d *stubDocker) *LiveXaiBackend {
	b := NewLiveXaiBackend(d)
	b.startTimeout, b.completeTimeout = 5*time.Second, 5*time.Second
	return b
}

// Ported from LiveXaiOAuthBackendTests: requestAuthorizeURL extracts URL from Authorize prefix line.
func TestRequestAuthorizeURLFromPrefixLine(t *testing.T) {
	d := &stubDocker{next: newStubStream([]string{
		"Hermes CLI v0.14", "Starting xai-oauth flow",
		"Authorize at: https://accounts.x.ai/oauth/authorize?client_id=hermes&state=abc",
	}, 0)}
	b := xaiBackend(d)
	u, err := b.RequestAuthorizeURL(context.Background(), testHandle(), "sess-1")
	require.NoError(t, err)
	require.Equal(t, "https://accounts.x.ai/oauth/authorize?client_id=hermes&state=abc", u)
	require.Equal(t, 1, b.registry.count())
}

// Ported from LiveXaiOAuthBackendTests: requestAuthorizeURL accepts bare https x_ai URL on its own line.
func TestRequestAuthorizeURLBareLine(t *testing.T) {
	d := &stubDocker{next: newStubStream([]string{"Hermes CLI v0.14", "https://accounts.x.ai/oauth/authorize?xyz=1"}, 0)}
	u, err := xaiBackend(d).RequestAuthorizeURL(context.Background(), testHandle(), "sess-2")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(u, "https://accounts.x.ai/"))
}

// Ported from LiveXaiOAuthBackendTests: requestAuthorizeURL throws when no matching line is emitted.
func TestRequestAuthorizeURLMissing(t *testing.T) {
	s := newStubStream([]string{"unrelated banner"}, 0)
	s.finish()
	_, err := xaiBackend(&stubDocker{next: s}).RequestAuthorizeURL(context.Background(), testHandle(), "sess-3")
	require.ErrorIs(t, err, ErrAuthorizeURLMissing)
}

// Ported from LiveXaiOAuthBackendTests: submitCallback forwards captured query to loopback and awaits exit.
func TestSubmitCallbackForwardsQuery(t *testing.T) {
	s := newStubStream([]string{"Authorize at: https://accounts.x.ai/oauth/authorize"}, 0)
	d := &stubDocker{next: s}
	b := xaiBackend(d)
	_, err := b.RequestAuthorizeURL(context.Background(), testHandle(), "sess-4")
	require.NoError(t, err)
	time.AfterFunc(100*time.Millisecond, s.finish)
	ok, err := b.SubmitCallback(context.Background(), testHandle(), "sess-4", "http://127.0.0.1:56121/callback?code=abc&state=xyz")
	require.NoError(t, err)
	require.True(t, ok)
	var curl []string
	for _, inv := range d.invocations {
		if inv.kind == "exec" && inv.args[0] == "curl" {
			curl = inv.args
		}
	}
	require.NotNil(t, curl, "submitCallback must run a curl exec to forward the callback")
	last := curl[len(curl)-1]
	require.Contains(t, last, "code=abc")
	require.Contains(t, last, "state=xyz")
	require.True(t, strings.HasPrefix(last, "http://127.0.0.1:56121/callback"))
}

// Ported from LiveXaiOAuthBackendTests: submitCallback throws sessionNotFound when registry is empty.
func TestSubmitCallbackUnknownSession(t *testing.T) {
	_, err := xaiBackend(&stubDocker{}).SubmitCallback(context.Background(), testHandle(), "missing", "http://127.0.0.1:56121/callback?code=x")
	require.ErrorIs(t, err, ErrSessionNotFound)
}

// Ported from LiveXaiOAuthBackendTests: cancel removes registry entry.
func TestCancelRemovesRegistryEntry(t *testing.T) {
	b := xaiBackend(&stubDocker{next: newStubStream([]string{"Authorize at: https://accounts.x.ai/foo"}, 0)})
	_, err := b.RequestAuthorizeURL(context.Background(), testHandle(), "sess-5")
	require.NoError(t, err)
	require.Equal(t, 1, b.registry.count())
	b.Cancel("sess-5")
	require.Equal(t, 0, b.registry.count())
}

// Ported from LiveXaiOAuthBackendTests: revoke shells out to hermes auth remove.
func TestRevokeShellsOut(t *testing.T) {
	d := &stubDocker{}
	ok, err := xaiBackend(d).Revoke(context.Background(), testHandle())
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, d.invocations[0].args, "remove")
	require.Contains(t, d.invocations[0].args, "xai-oauth")
}

// Ported from LiveXaiOAuthBackendTests: forwardURL preserves query string from captured URL.
func TestForwardURL(t *testing.T) {
	require.Equal(t, "http://127.0.0.1:56121/callback?code=abc&state=xyz",
		forwardURL("http://127.0.0.1:56121/callback", "http://127.0.0.1:56121/callback?code=abc&state=xyz"))
	require.Equal(t, "http://127.0.0.1:56121/callback", forwardURL("http://127.0.0.1:56121/callback", "http://127.0.0.1:56121/callback"))
	require.Equal(t, "http://x/cb?a=1&code=z", forwardURL("http://x/cb?a=1", "http://y/?code=z"))
}

func TestNousParsers(t *testing.T) {
	s := newStubStream([]string{"Requesting device code", "To authorize, visit https://portal.nousresearch.com/device and enter code ABCD-1234."}, 0)
	v, err := consumeVerification(context.Background(), s.Lines())
	require.NoError(t, err)
	require.Equal(t, "https://portal.nousresearch.com/device", v.url)
	require.Equal(t, "ABCD-1234", *v.code)
	require.Equal(t, "Pro", *extractPlan("Status: ok\nPlan: Pro\n"))
	require.Nil(t, extractPlan("nothing here"))
	require.Equal(t, "Q1", *embeddedUserCode("https://portal.nousresearch.com/device?user_code=Q1"))
}
