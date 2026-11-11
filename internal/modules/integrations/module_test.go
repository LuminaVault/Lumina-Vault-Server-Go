package integrations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// spawning stands in for a docker that can start containers: EnsureRunning
// inserts the row Swift's dockerRun path would.
type spawning struct {
	*hermescontainers.Lifecycle
	pool *pgxpool.Pool
	box  *secretbox.Box
}

func (s *spawning) EnsureRunning(ctx context.Context, tenant uuid.UUID) (*hermescontainers.Handle, error) {
	if h, err := s.Handle(ctx, tenant); err != nil || h != nil {
		return h, err
	}
	sealed, err := s.box.Seal("key", tenant)
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO hermes_tenant_containers (id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, last_used_at)
		VALUES ($1, $2, $3, 9100, $4, $5, now())`, uuid.New(), tenant, "hermes-tenant-"+tenant.String(), sealed.Ciphertext, sealed.Nonce); err != nil {
		return nil, err
	}
	return s.Handle(ctx, tenant)
}

// stubXai is StubXaiOAuthBackend.
type stubXai struct {
	completeOK   bool
	startCalls   []string
	completeURLs []string
	revokeCalls  int
}

func (b *stubXai) RequestAuthorizeURL(_ context.Context, _ *hermescontainers.Handle, sessionID string) (string, error) {
	b.startCalls = append(b.startCalls, sessionID)
	return "https://accounts.x.ai/oauth/authorize?stub=1", nil
}

func (b *stubXai) SubmitCallback(_ context.Context, _ *hermescontainers.Handle, _, callbackURL string) (bool, error) {
	b.completeURLs = append(b.completeURLs, callbackURL)
	return b.completeOK, nil
}
func (b *stubXai) Cancel(string) {}
func (b *stubXai) Revoke(context.Context, *hermescontainers.Handle) (bool, error) {
	b.revokeCalls++
	return true, nil
}

// stubNous is StubNousOAuthBackend.
type stubNous struct {
	completeOK  bool
	calls       []string
	revokeCalls int
}

func (b *stubNous) RequestVerification(context.Context, *hermescontainers.Handle, string) (string, *string, error) {
	code := "STUB-CODE"
	return "https://portal.nousresearch.com/device", &code, nil
}

func (b *stubNous) AwaitCompletion(_ context.Context, _ *hermescontainers.Handle, sessionID string) (bool, error) {
	b.calls = append(b.calls, sessionID)
	return b.completeOK, nil
}
func (b *stubNous) Cancel(string) {}
func (b *stubNous) Revoke(context.Context, *hermescontainers.Handle) (bool, error) {
	b.revokeCalls++
	return true, nil
}

func (b *stubNous) SubscriptionPlan(context.Context, *hermescontainers.Handle) *string { return nil }

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	m    *Module
	srv  http.Handler
	xai  *stubXai
	nous *stubNous
}

func setup(t *testing.T, spawn bool) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	master := make([]byte, 32)
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(master))
	require.NoError(t, err)
	life := &hermescontainers.Lifecycle{Manager: hermescontainers.New(pool, box), Docker: hermescontainers.ProcessDocker{BinaryPath: "/nonexistent/docker"}, Config: hermescontainers.Config{Network: "n"}}
	var containers hermescontainers.Containers = life
	if spawn {
		containers = &spawning{Lifecycle: life, pool: pool, box: box}
	}
	xai, nous := &stubXai{completeOK: true}, &stubNous{completeOK: true}
	m := New(pool, Options{Containers: containers, Xai: xai, Nous: nous})
	srv := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{m}})
	return env{t: t, pool: pool, m: m, srv: srv, xai: xai, nous: nous}
}

func (v env) user() uuid.UUID {
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, username, tier, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, 'trial', now(), now())`, id, id.String()+"@t", "u"+strings.ReplaceAll(id.String(), "-", "")[:12])
	require.NoError(v.t, err)
	return id
}

func (v env) do(user uuid.UUID, method, path, body string) (int, []byte) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (v env) tier(u uuid.UUID) string {
	var tier string
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT tier FROM users WHERE id = $1`, u).Scan(&tier))
	return tier
}

// Ported from XaiOAuthServiceTests: start returns authorize URL and session.
func TestXaiStart(t *testing.T) {
	v := setup(t, true)
	res, err := v.m.XaiStart(context.Background(), v.user())
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)
	require.True(t, strings.HasPrefix(res.AuthorizeURL, "https://accounts.x.ai/"))
	require.Equal(t, []string{res.SessionID}, v.xai.startCalls)
}

// Ported from XaiOAuthServiceTests: complete forwards callback and promotes user to premium.
func TestXaiCompletePromotes(t *testing.T) {
	v := setup(t, true)
	u := v.user()
	ctx := context.Background()
	start, err := v.m.XaiStart(ctx, u)
	require.NoError(t, err)
	cb := "http://127.0.0.1:56121/callback?code=c&state=s"
	s, err := v.m.XaiComplete(ctx, start.SessionID, cb)
	require.NoError(t, err)
	require.True(t, s.Connected)
	require.Equal(t, "pro", s.Tier)
	require.Equal(t, "pro", v.tier(u))
	require.Equal(t, []string{cb}, v.xai.completeURLs)
	var kind string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT credential_kind FROM user_provider_credentials WHERE tenant_id = $1 AND provider = 'xai'`, u).Scan(&kind))
	require.Equal(t, "oauth", kind)
}

// Ported from XaiOAuthServiceTests: complete fails cleanly when backend rejects.
func TestXaiCompleteBackendRejects(t *testing.T) {
	v := setup(t, true)
	v.xai.completeOK = false
	u := v.user()
	start, err := v.m.XaiStart(context.Background(), u)
	require.NoError(t, err)
	_, err = v.m.XaiComplete(context.Background(), start.SessionID, "http://127.0.0.1:56121/callback?code=x")
	var failed *BackendFailedError
	require.ErrorAs(t, err, &failed)
	require.NotEqual(t, "pro", v.tier(u))
}

// Ported from XaiOAuthServiceTests: complete with unknown session throws sessionNotFound.
func TestXaiCompleteUnknownSession(t *testing.T) {
	v := setup(t, true)
	_, err := v.m.XaiComplete(context.Background(), "nope", "http://x/?code=1")
	require.ErrorIs(t, err, ErrSessionNotFound)
}

// Ported from XaiOAuthServiceTests: revoke clears xai connection and demotes tier to free.
func TestXaiRevoke(t *testing.T) {
	v := setup(t, true)
	u := v.user()
	ctx := context.Background()
	start, err := v.m.XaiStart(ctx, u)
	require.NoError(t, err)
	_, err = v.m.XaiComplete(ctx, start.SessionID, "http://127.0.0.1:56121/callback?code=c")
	require.NoError(t, err)
	s, err := v.m.XaiRevoke(ctx, u)
	require.NoError(t, err)
	require.False(t, s.Connected)
	require.Equal(t, "trial", s.Tier)
	var at *time.Time
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT xai_connected_at FROM hermes_tenant_containers WHERE tenant_id = $1`, u).Scan(&at))
	require.Nil(t, at)
	require.Equal(t, 1, v.xai.revokeCalls)
}

// Ported from NousOAuthServiceTests: start returns verify URL and session.
func TestNousStart(t *testing.T) {
	v := setup(t, true)
	res, err := v.m.NousStart(context.Background(), v.user())
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)
	require.Contains(t, res.VerifyURL, "nousresearch.com")
	require.Equal(t, "STUB-CODE", *res.UserCode)
}

// Ported from NousOAuthServiceTests: complete stamps nousConnectedAt without changing tier.
func TestNousComplete(t *testing.T) {
	v := setup(t, true)
	u := v.user()
	ctx := context.Background()
	start, err := v.m.NousStart(ctx, u)
	require.NoError(t, err)
	s, err := v.m.NousComplete(ctx, start.SessionID)
	require.NoError(t, err)
	require.True(t, s.Connected)
	require.NotNil(t, s.NousConnectedAt)
	require.Equal(t, "trial", v.tier(u))
	require.Equal(t, []string{start.SessionID}, v.nous.calls)
}

// Ported from NousOAuthServiceTests: complete fails cleanly when backend rejects.
func TestNousCompleteBackendRejects(t *testing.T) {
	v := setup(t, true)
	v.nous.completeOK = false
	u := v.user()
	start, err := v.m.NousStart(context.Background(), u)
	require.NoError(t, err)
	_, err = v.m.NousComplete(context.Background(), start.SessionID)
	var failed *BackendFailedError
	require.ErrorAs(t, err, &failed)
	s, err := v.m.NousStatus(context.Background(), u)
	require.NoError(t, err)
	require.False(t, s.Connected)
}

// Ported from NousOAuthServiceTests: complete with unknown session throws sessionNotFound.
func TestNousCompleteUnknownSession(t *testing.T) {
	v := setup(t, true)
	_, err := v.m.NousComplete(context.Background(), "nope")
	require.ErrorIs(t, err, ErrSessionNotFound)
}

// Ported from NousOAuthServiceTests: revoke clears nous connection.
func TestNousRevoke(t *testing.T) {
	v := setup(t, true)
	u := v.user()
	ctx := context.Background()
	start, err := v.m.NousStart(ctx, u)
	require.NoError(t, err)
	_, err = v.m.NousComplete(ctx, start.SessionID)
	require.NoError(t, err)
	s, err := v.m.NousRevoke(ctx, u)
	require.NoError(t, err)
	require.False(t, s.Connected)
	require.Equal(t, 1, v.nous.revokeCalls)
}

// Ported from XaiOAuthControllerTests (4): status default, no bearer, unknown
// session, empty body. The server has no docker, as production.
func TestXaiController(t *testing.T) {
	v := setup(t, false)
	u := v.user()
	code, body := v.do(u, http.MethodGet, "/v1/integrations/xai", "")
	require.Equal(t, http.StatusOK, code)
	require.JSONEq(t, `{"connected":false,"tier":"trial"}`, string(body))

	code, _ = v.do(uuid.Nil, http.MethodGet, "/v1/integrations/xai", "")
	require.Equal(t, http.StatusUnauthorized, code)

	code, body = v.do(u, http.MethodPost, "/v1/integrations/xai/complete", `{"sessionID":"nope","callbackURL":"http://127.0.0.1:56121/callback?code=x"}`)
	require.Equal(t, http.StatusNotFound, code)
	require.Contains(t, string(body), "session not found or expired")

	code, _ = v.do(u, http.MethodPost, "/v1/integrations/xai/complete", `{}`)
	require.Equal(t, http.StatusBadRequest, code)

	code, body = v.do(u, http.MethodPost, "/v1/integrations/xai/complete", `{"sessionID":"","callbackURL":"x"}`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, string(body), "sessionID required")

	// No docker: start is a 500, as Swift's spawnFailed.
	code, _ = v.do(u, http.MethodPost, "/v1/integrations/xai/start", "")
	require.Equal(t, http.StatusInternalServerError, code)

	code, body = v.do(u, http.MethodDelete, "/v1/integrations/xai", "")
	require.Equal(t, http.StatusOK, code)
	var s XaiStatusResponse
	require.NoError(t, json.Unmarshal(body, &s))
	require.Equal(t, "trial", s.Tier)

	code, body = v.do(u, http.MethodGet, "/v1/integrations/nous", "")
	require.Equal(t, http.StatusOK, code)
	require.JSONEq(t, `{"connected":false}`, string(body))
}
