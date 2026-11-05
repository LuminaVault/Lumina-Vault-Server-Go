package apns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"lumina/internal/dbtest"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const bundle = "com.luminavault.app"

type stubSender struct {
	mu   sync.Mutex
	sent []Delivery
	fail map[string]error
}

func (s *stubSender) Send(_ context.Context, d Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail[d.DeviceToken]; err != nil {
		return err
	}
	s.sent = append(s.sent, d)
	return nil
}

func TestService(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	newTenant := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
			VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", id.String())
		require.NoError(t, err)
		return id
	}
	addToken := func(t *testing.T, tenant uuid.UUID, token string, bundleID *string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO device_tokens (id, tenant_id, token, platform, bundle_id, created_at, updated_at)
			VALUES ($1, $2, $3, 'ios', $4, now(), now())`, uuid.New(), tenant, token, bundleID)
		require.NoError(t, err)
	}
	unique := func(prefix string) string { return prefix + "-" + uuid.NewString() }
	exists := func(t *testing.T, token string) bool {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM device_tokens WHERE token = $1`, token).Scan(&n))
		return n == 1
	}

	// Swift "notify ... fans out to every registered token", through the one surface ported.
	t.Run("achievement fans out with Swift's text", func(t *testing.T) {
		tenant := newTenant(t)
		a, b := unique("a"), unique("b")
		addToken(t, tenant, a, nil)
		addToken(t, tenant, b, nil)
		addToken(t, newTenant(t), unique("other"), nil)
		sender := &stubSender{}
		s := NewWithSender(pool, bundle, sender)
		require.NoError(t, s.NotifyAchievement(ctx, tenant, "a.key", "First Steps"))
		require.Len(t, sender.sent, 2)
		tokens := []string{sender.sent[0].DeviceToken, sender.sent[1].DeviceToken}
		require.ElementsMatch(t, []string{a, b}, tokens)
		m := sender.sent[0].Message
		require.Equal(t, "Achievement unlocked", m.Title)
		require.Equal(t, "First Steps", *m.Subtitle)
		require.Equal(t, "You evolved one step closer to your true form.", m.Body)
		require.Equal(t, CategoryAchievement, m.Category)
		// A delivered token is stamped.
		var seen *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT last_seen_at::text FROM device_tokens WHERE token = $1`, a).Scan(&seen))
		require.NotNil(t, seen)
	})

	// Swift "ingestion notifications include terminal state and batch deep link payload".
	t.Run("ingestion notifications carry the state and the batch deep link", func(t *testing.T) {
		tenant := newTenant(t)
		addToken(t, tenant, unique("ingest"), nil)
		sender := &stubSender{}
		s := NewWithSender(pool, bundle, sender)
		batchID, itemID := uuid.New(), uuid.New()
		name := "source.pdf"

		require.NoError(t, s.NotifyIngestion(ctx, tenant, batchID, itemID, true, &name))
		require.Len(t, sender.sent, 1)
		m := sender.sent[0].Message
		require.Equal(t, CategoryIngestion, m.Category)
		require.Equal(t, "Capture ready", m.Title)
		require.Equal(t, "source.pdf", *m.Subtitle)
		require.Equal(t, "Hermes finished processing and linked the new memory.", m.Body)
		require.Equal(t, ids.Upper(batchID), m.Payload["batchID"])
		require.Equal(t, ids.Upper(itemID), m.Payload["itemID"])
		require.Equal(t, "completed", m.Payload["state"])

		require.NoError(t, s.NotifyIngestion(ctx, tenant, batchID, itemID, false, &name))
		require.Len(t, sender.sent, 2)
		failure := sender.sent[1].Message
		require.Equal(t, "Capture needs attention", failure.Title)
		require.Equal(t, "Open LuminaVault to review the ingestion status.", failure.Body)
		require.Equal(t, "failed", failure.Payload["state"])
	})

	// Swift "dead tokens are reaped on bad device token error".
	t.Run("dead tokens are reaped and the rest still go", func(t *testing.T) {
		tenant := newTenant(t)
		dead, live := unique("dead"), unique("live")
		addToken(t, tenant, dead, nil)
		addToken(t, tenant, live, nil)
		sender := &stubSender{fail: map[string]error{dead: &PushError{Status: 410, Reason: "BadDeviceToken"}}}
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyAchievement(ctx, tenant, "k", "L"))
		require.False(t, exists(t, dead))
		require.True(t, exists(t, live))
		require.Len(t, sender.sent, 1)
	})

	// Swift "transient errors do not reap the token".
	t.Run("transient errors keep the token", func(t *testing.T) {
		tenant := newTenant(t)
		flaky := unique("flaky")
		addToken(t, tenant, flaky, nil)
		for _, failure := range []error{&PushError{Status: 500, Reason: "InternalServerError"}, errors.New("network")} {
			sender := &stubSender{fail: map[string]error{flaky: failure}}
			require.NoError(t, NewWithSender(pool, bundle, sender).NotifyAchievement(ctx, tenant, "k", "L"))
			require.True(t, exists(t, flaky))
		}
	})

	// Swift "disabled service is no op".
	t.Run("disabled is a no-op", func(t *testing.T) {
		tenant := newTenant(t)
		addToken(t, tenant, unique("off"), nil)
		for _, cfg := range []Config{
			{Enabled: "false", BundleID: bundle, PrivateKeyPath: "/x"},
			{Enabled: "true", BundleID: bundle},
			{Enabled: "true", BundleID: bundle, PrivateKeyPath: filepath.Join(t.TempDir(), "missing.p8")},
		} {
			s := New(pool, cfg)
			require.Nil(t, s.sender)
			require.NoError(t, s.NotifyAchievement(ctx, tenant, "k", "L"))
		}
		sender := &stubSender{}
		require.NoError(t, NewWithSender(pool, "", sender).NotifyAchievement(ctx, tenant, "k", "L"), "no bundle id")
		require.Empty(t, sender.sent)
		var none *Service
		require.NoError(t, none.NotifyAchievement(ctx, tenant, "k", "L"))
	})

	t.Run("each device uses its own app's topic", func(t *testing.T) {
		tenant := newTenant(t)
		beta, other, plain := bundle+".beta", "com.evil.app", (*string)(nil)
		ta, tb, tc := unique("beta"), unique("other"), unique("plain")
		addToken(t, tenant, ta, &beta)
		addToken(t, tenant, tb, &other)
		addToken(t, tenant, tc, plain)
		sender := &stubSender{}
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyAchievement(ctx, tenant, "k", "L"))
		topics := map[string]string{}
		for _, d := range sender.sent {
			topics[d.DeviceToken] = d.Topic
		}
		require.Equal(t, map[string]string{ta: beta, tb: bundle, tc: bundle}, topics)
	})

	t.Run("no tokens", func(t *testing.T) {
		sender := &stubSender{}
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyAchievement(ctx, newTenant(t), "k", "L"))
		require.Empty(t, sender.sent)
	})

	// Swift ApnsCategoryPrefs: gated categories read the row, the rest never do.
	t.Run("category prefs", func(t *testing.T) {
		tenant := newTenant(t)
		s := NewWithSender(pool, bundle, &stubSender{})
		suppressed := func(c Category) bool {
			t.Helper()
			got, err := s.isSuppressed(ctx, c, tenant)
			require.NoError(t, err)
			return got
		}
		for _, c := range []Category{CategoryChat, CategoryNudge, CategoryDigest, CategoryApproval, CategoryRunCompleted, CategoryAchievement} {
			require.False(t, suppressed(c), "no row: %s is on", c)
		}
		require.True(t, suppressed(CategoryMeeting), "no row: meeting is opt-in")

		_, err := pool.Exec(ctx, `INSERT INTO apns_category_prefs (tenant_id, chat_enabled, nudge_enabled, digest_enabled, approval_enabled, run_completed_enabled, meeting_brief_enabled)
			VALUES ($1, false, false, false, false, false, true)`, tenant)
		require.NoError(t, err)
		for _, c := range []Category{CategoryChat, CategoryNudge, CategoryDigest, CategoryApproval, CategoryRunCompleted} {
			require.True(t, suppressed(c), "opted out of %s", c)
		}
		require.False(t, suppressed(CategoryMeeting))
		for _, c := range []Category{CategoryAchievement, CategoryReminder, CategoryCron, CategoryIngestion, CategoryWorkflow, CategoryDeviceCommand} {
			require.False(t, suppressed(c), "%s is never gated", c)
		}

		// A suppressed category sends nothing.
		addToken(t, tenant, unique("muted"), nil)
		sender := &stubSender{}
		require.NoError(t, NewWithSender(pool, bundle, sender).Notify(ctx, tenant, Message{Title: "t", Body: "b", Category: CategoryChat}))
		require.Empty(t, sender.sent)
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyAchievement(ctx, tenant, "k", "L"))
		require.Len(t, sender.sent, 1, "achievements ignore the opt-out table")
	})

	t.Run("a lookup failure is returned", func(t *testing.T) {
		closed, err := pgxpool.New(ctx, pool.Config().ConnString())
		require.NoError(t, err)
		closed.Close()
		require.Error(t, NewWithSender(closed, bundle, &stubSender{}).NotifyAchievement(ctx, uuid.New(), "k", "L"))
	})
}

// Swift "should reap classifies every dead reason".
func TestShouldReap(t *testing.T) {
	for _, reason := range []string{"BadDeviceToken", "Unregistered", "DeviceTokenNotForTopic", "MissingDeviceToken"} {
		require.True(t, ShouldReap(&PushError{Status: 410, Reason: reason}), reason)
	}
	require.False(t, ShouldReap(&PushError{Status: 500, Reason: "InternalServerError"}))
	require.False(t, ShouldReap(errors.New("other")))
}

func TestPayload(t *testing.T) {
	sub := "Sub"
	got, err := json.Marshal(payload(Message{Title: "T", Subtitle: &sub, Body: "B", Category: CategoryAchievement}))
	require.NoError(t, err)
	require.JSONEq(t, `{"category":"achievement","aps":{"alert":{"title":"T","subtitle":"Sub","body":"B"},"sound":"default","category":"achievement"}}`, string(got))

	// A custom key, the optional aps fields, an action category that replaces
	// only aps.category, and a payload "category" that wins over the default.
	got, err = json.Marshal(payload(Message{
		Title: "T", Body: "B", Category: CategoryApproval, ActionCategory: "approval.actions", ThreadID: "run-1", InterruptionLevel: "time-sensitive",
		Payload: map[string]string{"runId": "1", "category": "mine", "aps": "ignored"},
	}))
	require.NoError(t, err)
	require.JSONEq(t, `{"runId":"1","category":"mine","aps":{"alert":{"title":"T","body":"B"},"sound":"default","category":"approval.actions","thread-id":"run-1","interruption-level":"time-sensitive"}}`, string(got))
}

// The live sender against a stub server standing in for Apple: the request
// Swift's APNSClient would make, and a throwaway key made here.
func TestLiveSenderRequest(t *testing.T) {
	type seen struct {
		path   string
		header http.Header
		body   []byte
	}
	got := make(chan seen, 2)
	status, reason := http.StatusOK, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.URL.Path, r.Header.Clone(), b}
		w.WriteHeader(status)
		if reason != "" {
			_, _ = w.Write([]byte(`{"reason":"` + reason + `"}`))
		}
	}))
	t.Cleanup(srv.Close)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "AuthKey_TEST.p8")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))

	sender, err := newLiveSender(Config{BundleID: bundle, TeamID: "TEAM", KeyID: "KEY", PrivateKeyPath: path})
	require.NoError(t, err)
	require.Contains(t, sender.client.Host, "sandbox", "anything but production is the sandbox")
	production, err := newLiveSender(Config{TeamID: "TEAM", KeyID: "KEY", PrivateKeyPath: path, Environment: "Production"})
	require.NoError(t, err)
	require.NotContains(t, production.client.Host, "sandbox")

	// Point it at the stub. The transport wrapper stays; only the host and the
	// inner HTTP/2 transport change.
	sender.client.Host = srv.URL
	sender.client.HTTPClient.Transport = expireNow{srv.Client().Transport}

	sub := "L"
	d := Delivery{DeviceToken: "abc123", Topic: bundle + ".beta", Message: Message{Title: "T", Subtitle: &sub, Body: "B", Category: CategoryAchievement}}
	require.NoError(t, sender.Send(context.Background(), d))
	req := <-got
	require.Equal(t, "/3/device/abc123", req.path)
	require.Equal(t, bundle+".beta", req.header.Get("apns-topic"))
	require.Equal(t, "alert", req.header.Get("apns-push-type"))
	require.Equal(t, "10", req.header.Get("apns-priority"))
	require.Equal(t, "0", req.header.Get("apns-expiration"), "Swift's .none: don't store for an offline device")
	require.True(t, strings.HasPrefix(req.header.Get("Authorization"), "bearer "), "token auth")
	require.JSONEq(t, `{"category":"achievement","aps":{"alert":{"title":"T","subtitle":"L","body":"B"},"sound":"default","category":"achievement"}}`, string(req.body))

	// A refusal comes back as a PushError that ShouldReap can read.
	status, reason = http.StatusGone, "Unregistered"
	err = sender.Send(context.Background(), d)
	<-got
	var pe *PushError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, http.StatusGone, pe.Status)
	require.True(t, ShouldReap(err))
}
