package embedding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"lumina/internal/dbtest"
	"lumina/internal/embedding/db"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// One container for everything that writes usage rows or reads installs. Every
// subtest uses its own tenants.
func TestAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	for name, fn := range map[string]func(*testing.T, *pgxpool.Pool){
		// EmbeddingProviderRegistryTests
		"RegistryOpenRouterCallsOpenRouter":      testRegistryOpenRouter,
		"RegistryEverySpellingSelectsOpenRouter": testRegistrySpellings,
		"RegistryPlatformFallbackKey":            testRegistryPlatformKey,
		"RegistryNoKeyFallsBackToDeterministic":  testRegistryMissingKey,
		"RegistryOverrides":                      testRegistryOverrides,
		"RegistryWrongWidthRejected":             testRegistryWrongWidth,
		// The rest of the registry, the tracker, BYOK.
		"RegistryDeterministicPath": testRegistryDeterministic,
		"RegistryFallbackChain":     testRegistryChain,
		"TrackerUsageRows":          testTrackerUsageRows,
		"TrackerCap":                testTrackerCap,
		"BYOKRouting":               testBYOKRouting,
		"BYOKOpenRouterNotKeyable":  testBYOKOpenRouter,
		"BYOKDeterministicPath":     testBYOKDeterministic,
		"BYOKUnreadableConfigs":     testBYOKUnreadable,
		"BYOKCacheTTL":              testBYOKCache,
	} {
		t.Run(name, func(t *testing.T) { fn(t, pool) })
	}
}

func tokensUsed(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COALESCE(sum(tokens_used), 0) FROM embedding_usage WHERE tenant_id = $1`, tenant).Scan(&n))
	return n
}

// MARK: EmbeddingProviderRegistryTests

func testRegistryOpenRouter(t *testing.T, pool *pgxpool.Pool) {
	rt := &roundTripper{status: http.StatusOK, body: okPayload(Dimensions, 3)}
	r := Bootstrap(pool, Config{Provider: "openrouter", OpenRouterAPIKeys: []string{"or-test-key"}, HTTPClient: &http.Client{Transport: rt}})
	require.Equal(t, KindOpenRouter, r.ActiveKind)
	v, err := r.Active.Embed(context.Background(), "hello", uuid.New())
	require.NoError(t, err)
	require.Len(t, v, Dimensions)
	require.Equal(t, "https://openrouter.ai/api/v1/embeddings", rt.url)
	require.Equal(t, "Bearer or-test-key", rt.c.auth)
	require.Equal(t, "openai/text-embedding-3-small", rt.c.body["model"])
	require.InDelta(t, Dimensions, rt.c.body["dimensions"], 0)
	require.Equal(t, "float", rt.c.body["encoding_format"])
}

func testRegistrySpellings(t *testing.T, pool *pgxpool.Pool) {
	for _, raw := range []string{"openrouter", "OpenRouter", "open_router", "open-router"} {
		require.Equal(t, KindOpenRouter, Bootstrap(pool, Config{Provider: raw, OpenRouterAPIKeys: []string{"or-test-key"}}).ActiveKind, raw)
	}
}

func testRegistryPlatformKey(t *testing.T, pool *pgxpool.Pool) {
	keys := []string{"", "", "", "platform-key"}
	require.Equal(t, KindOpenRouter, Bootstrap(pool, Config{Provider: "openrouter", OpenRouterAPIKeys: keys}).ActiveKind)
	require.Equal(t, "platform-key", openRouterAPIKey(keys))
	// First non-blank wins, in the LLM routing's order; blank doesn't shadow.
	require.Equal(t, "legacy", openRouterAPIKey([]string{"  ", " legacy ", "alias", "platform"}))
}

func testRegistryMissingKey(t *testing.T, pool *pgxpool.Pool) {
	require.Equal(t, KindDeterministic, Bootstrap(pool, Config{Provider: "openrouter", OpenRouterAPIKeys: []string{"   "}}).ActiveKind)
}

func testRegistryOverrides(t *testing.T, pool *pgxpool.Pool) {
	rt := &roundTripper{status: http.StatusOK, body: okPayload(Dimensions, 3)}
	r := Bootstrap(pool, Config{
		Provider: "openrouter", OpenRouterAPIKeys: []string{"or-test-key"},
		OpenRouterBaseURL: "https://gateway.example.com/openrouter", OpenRouterModel: "openai/text-embedding-3-large",
		HTTPClient: &http.Client{Transport: rt},
	})
	_, err := r.Active.Embed(context.Background(), "hello", uuid.New())
	require.NoError(t, err)
	require.Equal(t, "https://gateway.example.com/openrouter/v1/embeddings", rt.url)
	require.Equal(t, "openai/text-embedding-3-large", rt.c.body["model"])
}

func testRegistryWrongWidth(t *testing.T, pool *pgxpool.Pool) {
	rt := &roundTripper{status: http.StatusOK, body: okPayload(3072, 3)}
	r := Bootstrap(pool, Config{Provider: "openrouter", OpenRouterAPIKeys: []string{"or-test-key"}, HTTPClient: &http.Client{Transport: rt}})
	_, err := r.Active.Embed(context.Background(), "hello", uuid.New())
	require.Equal(t, &Error{Kind: FailureDimMismatch, Expected: 1536, Got: 3072}, asError(t, err))
}

// MARK: what production runs

func testRegistryDeterministic(t *testing.T, pool *pgxpool.Pool) {
	want, err := Deterministic{}.Embed(context.Background(), "hello", uuid.Nil)
	require.NoError(t, err)
	for _, raw := range []string{"", "deterministic", "stub", "dev", "not-a-provider"} {
		// Keys being present changes nothing: production seals the OpenRouter key.
		rt := &roundTripper{status: http.StatusInternalServerError}
		r := Bootstrap(pool, Config{Provider: raw, MonthlyCap: 2_000_000, OpenRouterAPIKeys: []string{"or-test-key"}, OpenAIAPIKey: "sk-test", HTTPClient: &http.Client{Transport: rt}})
		require.Equal(t, KindDeterministic, r.ActiveKind, raw)
		require.Empty(t, r.FallbackKinds, raw)
		tenant := uuid.New()
		got, err := r.Active.Embed(context.Background(), "hello", tenant)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
		require.Equal(t, int64(2), tokensUsed(t, pool, tenant), raw)
		require.Zero(t, rt.c.calls, raw)
	}
}

func testRegistryChain(t *testing.T, pool *pgxpool.Pool) {
	r := Bootstrap(pool, Config{Provider: "openai", FallbackChain: "openai, nomic ,bogus,stub,openrouter", OpenAIAPIKey: "sk-test"})
	require.Equal(t, KindOpenAI, r.ActiveKind)
	require.Equal(t, []Kind{KindDeterministic}, r.FallbackKinds, "the active kind, unknown names and unconfigured providers drop out")

	// The filter uses the requested kind, so a primary that fell back to
	// deterministic still keeps deterministic in its chain.
	r = Bootstrap(pool, Config{Provider: "openrouter", FallbackChain: "deterministic"})
	require.Equal(t, KindDeterministic, r.ActiveKind)
	require.Equal(t, []Kind{KindDeterministic}, r.FallbackKinds)

	// hermesLocal always registers and, with no resolver, stops the chain: it's a
	// permanent failure, whatever the Swift comment says about advancing.
	r = Bootstrap(pool, Config{Provider: "hermesLocal", FallbackChain: "deterministic"})
	require.Equal(t, KindHermesLocal, r.ActiveKind)
	_, err := r.Active.Embed(context.Background(), "x", uuid.New())
	require.Equal(t, ReasonEndpointMissing, asError(t, err).Permanent)

	// A 503 advances to the fallback, and the tracker still counts the embed.
	rt := &roundTripper{status: http.StatusServiceUnavailable, body: "down"}
	r = Bootstrap(pool, Config{Provider: "openai", FallbackChain: "deterministic", OpenAIAPIKey: "sk-test", HTTPClient: &http.Client{Transport: rt}})
	tenant := uuid.New()
	v, err := r.Active.Embed(context.Background(), "hello", tenant)
	require.NoError(t, err)
	require.Len(t, v, Dimensions)
	require.Equal(t, 1, rt.c.calls)
	require.Equal(t, int64(2), tokensUsed(t, pool, tenant))
}

// MARK: EmbeddingUsageTracker

func testTrackerUsageRows(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	at := time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC)
	tracker := &usageTracker{inner: Deterministic{}, q: db.New(pool), now: func() time.Time { return at }}
	tenant := uuid.New()

	_, err := tracker.Embed(ctx, "twelve chars", tenant)
	require.NoError(t, err)
	var ym string
	var used int64
	var created, updated time.Time
	row := `SELECT year_month, tokens_used, created_at, updated_at FROM embedding_usage WHERE tenant_id = $1 ORDER BY year_month DESC LIMIT 1`
	require.NoError(t, pool.QueryRow(ctx, row, tenant).Scan(&ym, &used, &created, &updated))
	require.Equal(t, "2026-01", ym)
	require.Equal(t, int64(3), used)
	require.True(t, created.Equal(at))
	require.True(t, updated.Equal(at))

	// Same month adds to the row and moves updated_at only.
	later := at.Add(time.Minute)
	tracker.now = func() time.Time { return later }
	_, err = tracker.Embed(ctx, "x", tenant)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, row, tenant).Scan(&ym, &used, &created, &updated))
	require.Equal(t, int64(4), used)
	require.True(t, created.Equal(at))
	require.True(t, updated.Equal(later))

	// The month is UTC, and a new one starts a new row.
	tracker.now = func() time.Time { return time.Date(2026, 2, 1, 1, 0, 0, 0, time.FixedZone("x", 3*3600)) }
	_, err = tracker.Embed(ctx, "x", tenant)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, row, tenant).Scan(&ym, &used, &created, &updated))
	require.Equal(t, "2026-01", ym, "01:00 at +03 is still January in UTC")
	require.Equal(t, int64(5), used)
	tracker.now = func() time.Time { return time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) }
	_, err = tracker.Embed(ctx, "x", tenant)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, row, tenant).Scan(&ym, &used, &created, &updated))
	require.Equal(t, "2026-02", ym)
	require.Equal(t, int64(1), used)

	// A failed embed costs nothing.
	other := uuid.New()
	tracker.inner = failing{permanent(ReasonAuthRejected)}
	_, err = tracker.Embed(ctx, "x", other)
	require.Equal(t, ReasonAuthRejected, asError(t, err).Permanent)
	require.Zero(t, tokensUsed(t, pool, other))
}

func testTrackerCap(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	now := time.Now
	inner := &scripted{vec: []float32{1}}
	tracker := &usageTracker{inner: inner, q: db.New(pool), cap: 2, now: now}
	tenant := uuid.New()

	_, err := tracker.Embed(ctx, "twelve chars", tenant) // 3 tokens: over, but allowed once
	require.NoError(t, err)
	require.Equal(t, 1, inner.calls)

	_, err = tracker.Embed(ctx, "x", tenant)
	require.Equal(t, &Error{Kind: FailureCapExceeded, Tenant: tenant, Used: 3, Cap: 2}, asError(t, err))
	require.Equal(t, 1, inner.calls, "refused before any provider call")
	require.False(t, asError(t, err).IsRecoverable())

	tracker.cap = 0
	_, err = tracker.Embed(ctx, "x", tenant)
	require.NoError(t, err, "cap 0 disables the guard")
	require.Equal(t, 2, inner.calls)
}

// MARK: TenantAwareEmbeddingService, PerTenantEmbeddingResolver

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	require.NoError(t, err)
	return box
}

func newUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at) VALUES ($1, $2, 'x', true, $3, NOW(), NOW())`,
		id, id.String()+"@test.luminavault", "u"+id.String()[:8])
	require.NoError(t, err)
	return id
}

func install(t *testing.T, pool *pgxpool.Pool, box *secretbox.Box, tenant uuid.UUID, status, config string) {
	t.Helper()
	sealed, err := box.Seal(config, tenant)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce) VALUES ($1, $2, 'byok-embeddings', $3, $4, $5)`,
		uuid.New(), tenant, status, sealed.Ciphertext, sealed.Nonce)
	require.NoError(t, err)
}

func tokenConfig(t *testing.T, key string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"access_token": key})
	require.NoError(t, err)
	return string(raw)
}

func testBYOKRouting(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	box := testBox(t)
	base, c := stub(t, http.StatusOK, okPayload(Dimensions, 3))
	svc := New(pool, Config{Provider: "openai", MonthlyCap: 1, OpenAIAPIKey: "sk-platform", OpenAIBaseURL: base.String()}, box)

	byo, disabled, plain := newUser(t, pool), newUser(t, pool), newUser(t, pool)
	install(t, pool, box, byo, "enabled", tokenConfig(t, "  sk-tenant\n"))
	install(t, pool, box, disabled, "disabled", tokenConfig(t, "sk-disabled"))

	_, err := svc.Embed(ctx, "hello", byo)
	require.NoError(t, err)
	require.Equal(t, "Bearer sk-tenant", c.auth, "their key, trimmed")
	require.Zero(t, tokensUsed(t, pool, byo), "a BYO embed skips the platform's usage row")
	_, err = svc.Embed(ctx, "hello", byo)
	require.NoError(t, err, "and its cap")

	for _, tenant := range []uuid.UUID{disabled, plain} {
		_, err = svc.Embed(ctx, "hello", tenant)
		require.NoError(t, err)
		require.Equal(t, "Bearer sk-platform", c.auth)
		require.Equal(t, int64(2), tokensUsed(t, pool, tenant))
	}
}

func testBYOKOpenRouter(t *testing.T, pool *pgxpool.Pool) {
	box := testBox(t)
	base, c := stub(t, http.StatusOK, okPayload(Dimensions, 3))
	svc := New(pool, Config{Provider: "openrouter", OpenRouterAPIKeys: []string{"or-platform"}, OpenRouterBaseURL: base.String()}, box)
	tenant := newUser(t, pool)
	install(t, pool, box, tenant, "enabled", tokenConfig(t, "sk-tenant"))
	_, err := svc.Embed(context.Background(), "hello", tenant)
	require.NoError(t, err)
	require.Equal(t, "Bearer or-platform", c.auth, "an OpenAI key sent to OpenRouter would be a 401 on every embed")
	require.Equal(t, int64(2), tokensUsed(t, pool, tenant))
}

// Production: master key set, deterministic active. An install changes nothing.
func testBYOKDeterministic(t *testing.T, pool *pgxpool.Pool) {
	box := testBox(t)
	rt := &roundTripper{status: http.StatusInternalServerError}
	svc := New(pool, Config{Provider: "deterministic", MonthlyCap: 2_000_000, HTTPClient: &http.Client{Transport: rt}}, box)
	tenant := newUser(t, pool)
	install(t, pool, box, tenant, "enabled", tokenConfig(t, "sk-tenant"))
	got, err := svc.Embed(context.Background(), "hello", tenant)
	require.NoError(t, err)
	want, err := Deterministic{}.Embed(context.Background(), "hello", tenant)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Zero(t, rt.c.calls)
	require.Equal(t, int64(2), tokensUsed(t, pool, tenant))
}

func testBYOKUnreadable(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	box := testBox(t)
	keyed := func(string) Service { return Deterministic{} }
	r := newPerTenantResolver(db.New(pool), box, keyed)

	for name, config := range map[string]string{
		"not json":    "nope",
		"no key":      `{"other":"x"}`,
		"blank key":   `{"access_token":"   "}`,
		"not strings": `{"access_token":"sk","n":1}`,
		"null value":  `{"access_token":"sk","n":null}`,
	} {
		tenant := newUser(t, pool)
		install(t, pool, box, tenant, "enabled", config)
		require.Nil(t, r.service(ctx, tenant), name)
	}

	// Sealed for another tenant, or a different master key: it won't open.
	wrong := newUser(t, pool)
	sealed, err := box.Seal(tokenConfig(t, "sk"), uuid.New())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO plugin_installs (id, tenant_id, plugin_slug, status, config_ciphertext, config_nonce) VALUES ($1, $2, 'byok-embeddings', 'enabled', $3, $4)`,
		uuid.New(), wrong, sealed.Ciphertext, sealed.Nonce)
	require.NoError(t, err)
	require.Nil(t, r.service(ctx, wrong))

	readable := newUser(t, pool)
	install(t, pool, box, readable, "enabled", tokenConfig(t, "sk"))
	require.NotNil(t, r.service(ctx, readable))
}

func testBYOKCache(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	box := testBox(t)
	var keys []string
	r := newPerTenantResolver(db.New(pool), box, func(key string) Service {
		keys = append(keys, key)
		return Deterministic{}
	})
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return at }
	tenant := newUser(t, pool)

	require.Nil(t, r.service(ctx, tenant))
	install(t, pool, box, tenant, "enabled", tokenConfig(t, "sk-new"))
	at = at.Add(299 * time.Second)
	require.Nil(t, r.service(ctx, tenant), `"none" is cached too`)

	at = at.Add(time.Second)
	require.NotNil(t, r.service(ctx, tenant), "the TTL expired")
	require.Equal(t, []string{"sk-new"}, keys)

	_, err := pool.Exec(ctx, `UPDATE plugin_installs SET status = 'disabled' WHERE tenant_id = $1`, tenant)
	require.NoError(t, err)
	at = at.Add(299 * time.Second)
	require.NotNil(t, r.service(ctx, tenant), "a change takes up to the TTL to show")
	require.Equal(t, []string{"sk-new"}, keys, "no rebuild inside the TTL")
	at = at.Add(time.Second)
	require.Nil(t, r.service(ctx, tenant))
}

type stubResolver struct{ svc Service }

func (s stubResolver) service(context.Context, uuid.UUID) Service { return s.svc }

func TestTenantAwareFallsThroughToGlobal(t *testing.T) {
	global := &scripted{vec: []float32{1}}
	own := &scripted{vec: []float32{2}}
	v, err := (&tenantAware{global: global, resolver: stubResolver{own}}).Embed(context.Background(), "x", uuid.New())
	require.NoError(t, err)
	require.Equal(t, []float32{2}, v)
	require.Zero(t, global.calls)

	v, err = (&tenantAware{global: global, resolver: stubResolver{nil}}).Embed(context.Background(), "x", uuid.New())
	require.NoError(t, err)
	require.Equal(t, []float32{1}, v)
}
