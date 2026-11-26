package githubsync_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"lumina/internal/githubsync"
	"lumina/internal/ratelimit"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	module "lumina/internal/modules/githubsync"
)

const webhookSecret = "whsec-for-tests-only"

// parkedSyncer stands in for the sync worker: it records what the webhook
// hands it and holds each delivery until the test lets go.
type parkedSyncer struct {
	mu        sync.Mutex
	delivered []githubsync.Delivery
	release   chan struct{}
}

func (p *parkedSyncer) Notify(uuid.UUID)                         {}
func (p *parkedSyncer) SyncNow(context.Context, uuid.UUID) error { return nil }
func (p *parkedSyncer) MergeExisting(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}
func (p *parkedSyncer) Syncing(uuid.UUID) bool                  { return false }
func (p *parkedSyncer) Disconnected(context.Context, uuid.UUID) {}
func (p *parkedSyncer) Deliver(_ context.Context, d githubsync.Delivery) string {
	<-p.release
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delivered = append(p.delivered, d)
	return githubsync.DeliveryDone
}

func (p *parkedSyncer) seen() []githubsync.Delivery {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]githubsync.Delivery(nil), p.delivered...)
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// hook posts a webhook as GitHub does. An empty signature sends none.
func (v *env) hook(id, event, signature string, body []byte) int {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if id != "" {
		req.Header.Set("X-GitHub-Delivery", id)
	}
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec.Code
}

func (v *env) deliveries() int {
	v.t.Helper()
	var n int
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM github_webhook_deliveries`).Scan(&n))
	return n
}

// D9, D17: the webhook. A bad or missing signature is 401 and leaves no
// row; a body over 5 MiB is 413; a delivery id is taken once, so a replay
// answers 200 and starts nothing; and the handler answers before the pull
// runs.
func TestWebhook(t *testing.T) {
	worker := &parkedSyncer{release: make(chan struct{})}
	v := newEnvWith(t, []string{webOrigin}, func(o *module.Options) {
		o.WebhookSecret = webhookSecret
		o.Sync = worker
	})
	push := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("a", 40) + `","repository":{"id":456,"full_name":"octocat/notes"},` +
		`"installation":{"id":7},"head_commit":{"message":"a secret plan","author":{"email":"someone@example.com"}}}`)

	// Refused, without a trace.
	require.Equal(t, http.StatusUnauthorized, v.hook("d-1", "push", "", push), "unsigned")
	require.Equal(t, http.StatusUnauthorized, v.hook("d-1", "push", sign("another secret", push), push), "signed with another secret")
	require.Equal(t, http.StatusUnauthorized, v.hook("d-1", "push", sign(webhookSecret, append([]byte(" "), push...)), push), "signature of other bytes")
	require.Equal(t, http.StatusUnauthorized, v.hook("d-1", "push", "sha1="+strings.Repeat("0", 40), push), "the old sha1 header's form")
	require.Equal(t, http.StatusUnauthorized, v.hook("d-1", "push", "sha256=not-hex", push))
	big := append([]byte(`{"ref":"`), bytes.Repeat([]byte("x"), 5<<20)...)
	big = append(big, `"}`...)
	require.Equal(t, http.StatusRequestEntityTooLarge, v.hook("d-1", "push", sign(webhookSecret, big), big))
	require.Equal(t, http.StatusBadRequest, v.hook("", "push", sign(webhookSecret, push), push), "no delivery id")
	require.Equal(t, http.StatusBadRequest, v.hook("d-1", "", sign(webhookSecret, push), push), "no event")
	require.Equal(t, http.StatusBadRequest, v.hook("d-1", "push", sign(webhookSecret, []byte("not json")), []byte("not json")))
	require.Zero(t, v.deliveries())
	require.Empty(t, worker.seen())

	// Accepted: 202 while the worker has not run yet.
	require.Equal(t, http.StatusAccepted, v.hook("d-1", "push", sign(webhookSecret, push), push))
	require.Empty(t, worker.seen(), "the handler answered before the pull")
	require.Equal(t, 1, v.deliveries())
	// Replays, during and after: 200, one row, one pull.
	require.Equal(t, http.StatusOK, v.hook("d-1", "push", sign(webhookSecret, push), push))
	close(worker.release)
	v.module.Drain()
	require.Equal(t, http.StatusOK, v.hook("d-1", "push", sign(webhookSecret, push), push))
	v.module.Drain()
	require.Equal(t, []githubsync.Delivery{{
		ID: "d-1", Event: "push", InstallationID: 7, RepoID: 456, Ref: "refs/heads/main", After: strings.Repeat("a", 40),
	}}, worker.seen())
	require.Equal(t, 1, v.deliveries())

	// An installation event carries its action and the repositories removed.
	removed := []byte(`{"action":"removed","installation":{"id":7},"repositories_removed":[{"id":456},{"id":457}]}`)
	require.Equal(t, http.StatusAccepted, v.hook("d-2", "installation_repositories", sign(webhookSecret, removed), removed))
	v.module.Drain()
	require.Equal(t, githubsync.Delivery{
		ID: "d-2", Event: "installation_repositories", Action: "removed", InstallationID: 7, Removed: []int64{456, 457},
	}, worker.seen()[1])

	// D2: nothing of the payload but ids and the ref is kept or logged.
	var stored string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT string_agg(d::text, ' ') FROM github_webhook_deliveries d`).Scan(&stored))
	for _, private := range []string{"a secret plan", "someone@example.com", "octocat/notes", webhookSecret} {
		require.NotContains(t, stored, private)
		require.NotContains(t, v.logs.String(), private)
	}

	// A header that is no signature is refused before a byte of the body
	// is read.
	for _, header := range []string{"", "sha256=", "sha256=not-hex", "sha1=" + strings.Repeat("0", 40), "sha256=" + strings.Repeat("0", 63), strings.Repeat("0", 64)} {
		body := &countingBody{}
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", body)
		req.Header.Set("X-GitHub-Delivery", "d-unread")
		req.Header.Set("X-GitHub-Event", "push")
		if header != "" {
			req.Header.Set("X-Hub-Signature-256", header)
		}
		rec := httptest.NewRecorder()
		v.e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, header)
		require.Zero(t, body.read.Load(), "the body of %q was read", header)
	}

	// Public, so limited, but only what fails its signature counts: GitHub's
	// own deliveries are never turned away, however many there are, and a
	// flood of forgeries from an address does not shut GitHub out.
	for i := range ratelimit.GitHubWebhookByIP.Max + 20 {
		require.Equal(t, http.StatusOK, v.hook("d-1", "push", sign(webhookSecret, push), push), "signed delivery %d", i)
	}
	refused := 0
	for range ratelimit.GitHubWebhookByIP.Max {
		code := v.hook("x", "push", sign("another secret", push), push)
		if code == http.StatusTooManyRequests {
			break
		}
		require.Equal(t, http.StatusUnauthorized, code)
		refused++
	}
	require.Greater(t, refused, ratelimit.GitHubWebhookByIP.Max-30, "the refusals above count too")
	require.Less(t, refused, ratelimit.GitHubWebhookByIP.Max)
	require.Equal(t, http.StatusTooManyRequests, v.hook("x", "push", "", push), "a missing signature is a failure like a wrong one")
	require.Equal(t, http.StatusAccepted, v.hook("d-3", "push", sign(webhookSecret, push), push), "a signed delivery is still taken")
	v.clock.advance(61 * time.Second)
	require.Equal(t, http.StatusUnauthorized, v.hook("x", "push", "", push), "a new window")
	v.module.Drain()
	require.Equal(t, 3, v.deliveries())
}

// countingBody is a request body that counts what is read of it.
type countingBody struct{ read atomic.Int64 }

func (b *countingBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read.Add(int64(len(p)))
	if b.read.Load() > 1<<20 {
		return len(p), io.EOF
	}
	return len(p), nil
}

// A server with no webhook secret refuses every delivery: it cannot tell
// GitHub from anyone else.
func TestWebhookWithoutASecret(t *testing.T) {
	v := newEnv(t)
	body := []byte(`{}`)
	require.Equal(t, http.StatusInternalServerError, v.hook("d-1", "ping", sign("", body), body))
	require.Zero(t, v.deliveries())
}

// D14: the status shows a pull in progress and the files a pull kept.
func TestStatusShowsProgressAndKept(t *testing.T) {
	var mu sync.Mutex
	applying := false
	v := newEnvWith(t, []string{webOrigin}, func(o *module.Options) {
		o.Progress = func(uuid.UUID) (int, int, bool) {
			mu.Lock()
			defer mu.Unlock()
			return 3, 12, applying
		}
	})
	alice := v.user()
	v.connectVault(alice, nil)
	v.settled(alice, func(c *githubsync.Connection) bool { return c.State == githubsync.StateActive })
	read := func() map[string]any {
		r := v.do(alice, nil, http.MethodGet, "/v1/vault/github-sync", nil)
		require.Equal(t, http.StatusOK, r.code, r.String())
		var out struct {
			Connection map[string]any `json:"connection"`
		}
		require.NoError(t, json.Unmarshal(r.body, &out))
		return out.Connection
	}
	c := read()
	require.Nil(t, c["progress"])
	require.Equal(t, []any{}, c["kept"])

	mu.Lock()
	applying = true
	mu.Unlock()
	store := githubsync.NewStore(v.pool)
	conn := v.connection(alice)
	ok, err := store.MarkPulled(context.Background(), conn.Key(), strings.Repeat("b", 40), v.clock.now(), githubsync.Report{Kept: []string{"rowless.md"}})
	require.NoError(t, err)
	require.True(t, ok)
	c = read()
	require.Equal(t, map[string]any{"done": float64(3), "total": float64(12)}, c["progress"])
	require.Equal(t, true, c["syncing"])
	require.Equal(t, []any{"rowless.md"}, c["kept"])
}
