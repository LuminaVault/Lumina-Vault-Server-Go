package achievements_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/ratelimit"
	"lumina/internal/modules/achievements"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

// setup loads the baseline schema plus the parity seeds the scenarios use.
func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "achievements.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{achievements.New(pool, auth.Identify(keys, auth.PGUsers(pool)), ratelimit.NewStore())},
	})
	return env{t: t, e: e, pool: pool}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) get(user uuid.UUID, path string) resp {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

type subDTO struct {
	Key        string  `json:"key"`
	Label      string  `json:"label"`
	Target     int64   `json:"target"`
	Progress   int64   `json:"progress"`
	UnlockedAt *string `json:"unlockedAt"`
}

type listDTO struct {
	CatalogVersion int `json:"catalogVersion"`
	Archetypes     []struct {
		Key   string   `json:"key"`
		Label string   `json:"label"`
		Sub   []subDTO `json:"sub"`
	} `json:"archetypes"`
}

type unlockDTO struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	UnlockedAt string `json:"unlockedAt"`
}

func list(t *testing.T, v env, user uuid.UUID) listDTO {
	t.Helper()
	r := v.get(user, "/v1/achievements")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Equal(t, "application/json; charset=utf-8", r.ct)
	var out listDTO
	require.NoError(t, json.Unmarshal(r.body, &out))
	return out
}

func recent(t *testing.T, v env, user uuid.UUID, query string) []unlockDTO {
	t.Helper()
	r := v.get(user, "/v1/achievements/recent"+query)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var out struct {
		Unlocks []unlockDTO `json:"unlocks"`
	}
	require.NoError(t, json.Unmarshal(r.body, &out))
	require.NotNil(t, out.Unlocks, "unlocks is [] when empty, never null")
	return out.Unlocks
}

// Swift AchievementsFlowTests: both routes reject unauthenticated requests.
func TestRoutesRejectUnauthenticated(t *testing.T) {
	v := setup(t)
	for _, p := range []string{"/v1/achievements", "/v1/achievements/recent"} {
		r := v.get(uuid.Nil, p)
		require.Equal(t, http.StatusUnauthorized, r.code, p)
		require.JSONEq(t, `{"error":{"message":"Authenticated identity is unavailable"}}`, string(r.body))
	}
}

func TestListJoinsCatalogAndProgress(t *testing.T) {
	v := setup(t)
	got := list(t, v, alice)
	require.Equal(t, 1, got.CatalogVersion)
	var keys []string
	subs := map[string]subDTO{}
	for _, a := range got.Archetypes {
		keys = append(keys, a.Key)
		for _, s := range a.Sub {
			subs[s.Key] = s
		}
	}
	require.Equal(t, []string{"lightbringer", "shadowlord", "reignmaker", "soulseeker"}, keys, "catalog order, not sorted")
	require.Len(t, subs, 16, "retired keys in achievement_progress are not listed")
	fs := subs["lightbringer.first-spark"]
	require.Equal(t, subDTO{Key: "lightbringer.first-spark", Label: "First Spark", Target: 1, Progress: 3, UnlockedAt: ptr("2026-02-01T10:00:00Z")}, fs)
	require.Equal(t, int64(4), subs["lightbringer.kindled-mind"].Progress)
	require.Nil(t, subs["lightbringer.kindled-mind"].UnlockedAt)
	require.Equal(t, "2026-02-03T08:30:00Z", *subs["shadowlord.shadow-touched"].UnlockedAt, "whole seconds on the wire")
	require.Equal(t, int64(0), subs["soulseeker.first-relic"].Progress, "bob's row is not alice's")

	r := v.get(alice, "/v1/achievements")
	require.NotContains(t, string(r.body), `"unlockedAt":null`, "nil unlockedAt is omitted, as Swift does")
}

func TestListForTenantWithoutRows(t *testing.T) {
	v := setup(t)
	_, err := v.pool.Exec(context.Background(), `DELETE FROM achievement_progress WHERE tenant_id = $1`, bob)
	require.NoError(t, err)
	got := list(t, v, bob)
	for _, a := range got.Archetypes {
		require.Len(t, a.Sub, 4)
		for _, s := range a.Sub {
			require.Zero(t, s.Progress)
			require.Nil(t, s.UnlockedAt)
		}
	}
	require.Empty(t, recent(t, v, bob, ""))
}

func TestRecentNewestFirstWithLabelFallback(t *testing.T) {
	v := setup(t)
	got := recent(t, v, alice, "")
	require.Equal(t, []unlockDTO{
		{"legacy.retired-badge", "legacy.retired-badge", "2026-02-04T00:00:00Z"},
		{"shadowlord.shadow-touched", "Shadow-touched", "2026-02-03T08:30:00Z"},
		{"reignmaker.first-edict", "First Edict", "2026-02-02T00:00:00Z"},
		{"lightbringer.first-spark", "First Spark", "2026-02-01T10:00:00Z"},
	}, got)
}

func TestRecentLimit(t *testing.T) {
	v := setup(t)
	for q, n := range map[string]int{
		"?limit=1": 1, "?limit=2": 2, "?limit=0": 1, "?limit=-3": 1, "?limit=999": 4,
		"?limit=abc": 4, "?limit=": 4, "?limit=99999999999999999999": 4, "?limit=%2B2": 2,
	} {
		require.Len(t, recent(t, v, alice, q), n, q)
	}
	// Default 10 and max 50, observable with more unlocks than either.
	_, err := v.pool.Exec(context.Background(), `
		INSERT INTO achievement_progress (tenant_id, achievement_key, progress_count, unlocked_at)
		SELECT $1, 'bulk.' || g, 1, '2025-01-01T00:00:00Z'::timestamptz + g * interval '1 minute'
		FROM generate_series(1, 60) g`, alice)
	require.NoError(t, err)
	require.Len(t, recent(t, v, alice, ""), 10)
	require.Len(t, recent(t, v, alice, "?limit=999"), 50)
	require.Len(t, recent(t, v, alice, "?limit=50"), 50)
}

func TestTenantIsolation(t *testing.T) {
	v := setup(t)
	got := recent(t, v, bob, "?limit=50")
	require.Equal(t, []unlockDTO{{"soulseeker.first-relic", "First Relic", "2026-03-01T00:00:00Z"}}, got)
	for _, u := range recent(t, v, alice, "?limit=50") {
		require.NotEqual(t, "soulseeker.first-relic", u.Key)
	}
}

// Swift AchievementsServiceTests "catalog renders deterministic JSON across runs".
func TestCatalogDeterministicAndWellFormed(t *testing.T) {
	first, err := json.Marshal(achievements.Current.Archetypes)
	require.NoError(t, err)
	second, err := json.Marshal(achievements.Current.Archetypes)
	require.NoError(t, err)
	require.Equal(t, first, second)

	c := achievements.Current
	require.Len(t, c.Archetypes, 4)
	require.GreaterOrEqual(t, c.CatalogVersion, 1)
	var keys []string
	covered := map[achievements.Event]bool{}
	for _, a := range c.Archetypes {
		keys = append(keys, a.Key)
		require.True(t, len(a.Subs) >= 3 && len(a.Subs) <= 5, "%s sub count must be 3-5", a.Key)
		for _, s := range a.Subs {
			require.True(t, strings.HasPrefix(s.Key, a.Key+"."), "sub key must namespace its archetype: %s", s.Key)
			require.GreaterOrEqual(t, s.Target, int64(1))
			covered[s.Event] = true
		}
	}
	slices.Sort(keys)
	require.Equal(t, []string{"lightbringer", "reignmaker", "shadowlord", "soulseeker"}, keys)
	require.Len(t, covered, len(achievements.AllEvents), "every AchievementEvent reaches at least one sub")
	for _, e := range achievements.AllEvents {
		require.True(t, covered[e], e)
	}
}

// The catalog is copied from Swift by hand: pin every field so an edit on one
// side only shows up here.
func TestCatalogMatchesSwiftVerbatim(t *testing.T) {
	raw, err := json.Marshal(achievements.Current.Archetypes)
	require.NoError(t, err)
	const want = `[` +
		`{"key":"lightbringer","label":"Lightbringer","subs":[` +
		`{"key":"lightbringer.first-spark","label":"First Spark","event":"memory_upserted","target":1},` +
		`{"key":"lightbringer.kindled-mind","label":"Kindled Mind","event":"memory_upserted","target":10},` +
		`{"key":"lightbringer.illuminator","label":"Illuminator","event":"memory_upserted","target":50},` +
		`{"key":"lightbringer.lightbearer","label":"Lightbearer","event":"memory_upserted","target":200}]},` +
		`{"key":"shadowlord","label":"Shadowlord","subs":[` +
		`{"key":"shadowlord.shadow-touched","label":"Shadow-touched","event":"soul_configured","target":1},` +
		`{"key":"shadowlord.deep-listener","label":"Deep Listener","event":"chat_completed","target":5},` +
		`{"key":"shadowlord.night-walker","label":"Night Walker","event":"chat_completed","target":25},` +
		`{"key":"shadowlord.umbral-sovereign","label":"Umbral Sovereign","event":"chat_completed","target":100}]},` +
		`{"key":"reignmaker","label":"Reignmaker","subs":[` +
		`{"key":"reignmaker.first-edict","label":"First Edict","event":"query_ran","target":1},` +
		`{"key":"reignmaker.tactician","label":"Tactician","event":"query_ran","target":10},` +
		`{"key":"reignmaker.strategist","label":"Strategist","event":"kb_compiled","target":5},` +
		`{"key":"reignmaker.regent","label":"Regent","event":"query_ran","target":100}]},` +
		`{"key":"soulseeker","label":"Soulseeker","subs":[` +
		`{"key":"soulseeker.first-relic","label":"First Relic","event":"vault_uploaded","target":1},` +
		`{"key":"soulseeker.collector","label":"Collector","event":"vault_uploaded","target":10},` +
		`{"key":"soulseeker.cartographer","label":"Cartographer","event":"space_created","target":3},` +
		`{"key":"soulseeker.soulkeeper","label":"Soulkeeper","event":"vault_uploaded","target":100}]}]`
	require.JSONEq(t, want, string(raw))
	sm := achievements.Current.SubsMatching(achievements.EventQueryRan)
	require.Equal(t, []string{"reignmaker.first-edict", "reignmaker.tactician", "reignmaker.regent"}, subKeys(sm), "order-preserving")
}

// Swift AchievementsServiceTests "record is idempotent on unlocked_at after threshold".
func TestRecordIdempotentUnlock(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	rec := achievements.NewRecorder(v.pool)
	tenant := newUser(t, v, "ach1")

	first, err := rec.Record(ctx, tenant, achievements.EventMemoryUpserted)
	require.NoError(t, err)
	require.Contains(t, subKeys(first), "lightbringer.first-spark")
	count, unlockedAt := progressRow(t, v, tenant, "lightbringer.first-spark")
	require.NotNil(t, unlockedAt)

	second, err := rec.Record(ctx, tenant, achievements.EventMemoryUpserted)
	require.NoError(t, err)
	require.NotContains(t, subKeys(second), "lightbringer.first-spark", "re-fire must not re-emit")
	count2, unlockedAt2 := progressRow(t, v, tenant, "lightbringer.first-spark")
	require.Equal(t, int64(1), count)
	require.Equal(t, int64(2), count2)
	require.Equal(t, *unlockedAt, *unlockedAt2, "unlocked_at must be sticky")
}

// Swift AchievementsServiceTests "record partitions counters by tenant".
func TestRecordPartitionsByTenant(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	rec := achievements.NewRecorder(v.pool)
	t1, t2 := newUser(t, v, "ach2a"), newUser(t, v, "ach2b")
	for _, id := range []uuid.UUID{t1, t1, t2} {
		_, err := rec.Record(ctx, id, achievements.EventMemoryUpserted)
		require.NoError(t, err)
	}
	c1, _ := progressRow(t, v, t1, "lightbringer.first-spark")
	c2, _ := progressRow(t, v, t2, "lightbringer.first-spark")
	require.Equal(t, int64(2), c1)
	require.Equal(t, int64(1), c2)
}

// Swift AchievementsServiceTests "record returns multi-step unlocks when crossing several thresholds".
func TestRecordMultiStepUnlocks(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	rec := achievements.NewRecorder(v.pool)
	tenant := newUser(t, v, "ach3")
	var unlocked []string
	for range 10 {
		got, err := rec.Record(ctx, tenant, achievements.EventMemoryUpserted)
		require.NoError(t, err)
		unlocked = append(unlocked, subKeys(got)...)
	}
	require.Contains(t, unlocked, "lightbringer.first-spark")
	require.Contains(t, unlocked, "lightbringer.kindled-mind")
	require.NotContains(t, unlocked, "lightbringer.illuminator")
}

// Swift PowerLevelTests (the five PowerLevel assertions; the streak tests
// exercise DashboardController.consecutiveStreak and move with Dashboard).
func TestPowerLevel(t *testing.T) {
	base := achievements.XPInput{MemoriesTotal: 10, SessionsCount: 4, JobsCount: 5, BadgesEarned: 2}
	require.Equal(t, 52, achievements.PowerLevelXP(base))
	full := base
	full.GraphConnections, full.ActiveSpaces, full.StreakDays = 7, 3, 4
	require.Equal(t, 86, achievements.PowerLevelXP(full))
	explicit := base
	explicit.GraphConnections, explicit.ActiveSpaces, explicit.StreakDays = 0, 0, 0
	require.Equal(t, achievements.PowerLevelXP(base), achievements.PowerLevelXP(explicit))

	require.Equal(t, 1, achievements.PowerLevel(0))
	require.Equal(t, 1, achievements.PowerLevel(-5))
	for xp, lvl := range map[int]int{1: 2, 3: 2, 4: 3, 99: 10, 100: 11} {
		require.Equal(t, lvl, achievements.PowerLevel(xp), xp)
	}
}

// The spec's achievements schemas ({achievements:[AchievementDTO]} with
// key/title) do not describe what Swift sends, so the responses are checked
// against Swift's shapes above instead. This canary fails once the vendored
// spec is fixed, as a reminder to validate against it like projects does.
func TestSpecAchievementsSchemasAreStale(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	for _, p := range []string{"/v1/achievements", "/v1/achievements/recent"} {
		r := v.get(alice, p)
		req := httptest.NewRequest(http.MethodGet, p, nil)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, p)
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			},
			Status: r.code, Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes(r.body)
		require.Error(t, openapi3filter.ValidateResponse(ctx, out),
			"%s now conforms to api/openapi.yaml: the spec was fixed; replace this canary with a conformance test", p)
	}
}

func newUser(t *testing.T, v env, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
		 VALUES ($1, $2, 'stub-hash', true, $3, now(), now())`, id, slug+"@test.luminavault", slug)
	require.NoError(t, err)
	return id
}

func progressRow(t *testing.T, v env, tenant uuid.UUID, key string) (count int64, unlockedAt *string) {
	t.Helper()
	err := v.pool.QueryRow(context.Background(),
		`SELECT progress_count, to_char(unlocked_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')
		 FROM achievement_progress WHERE tenant_id = $1 AND achievement_key = $2`, tenant, key).Scan(&count, &unlockedAt)
	require.NoError(t, err)
	return count, unlockedAt
}

func subKeys(subs []achievements.Sub) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.Key)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// Swift's chain is authenticator → achievementsByUser (60/60 s) → requireIdentity,
// so the 61st request in a window is a 429 even without a token.
func TestRateLimitBeforeIdentity(t *testing.T) {
	v := setup(t)
	for i := range 60 {
		require.Equal(t, http.StatusUnauthorized, v.get(uuid.Nil, "/v1/achievements").code, i)
	}
	r := v.get(uuid.Nil, "/v1/achievements/recent")
	require.Equal(t, http.StatusTooManyRequests, r.code, "both routes share the bucket")
	require.Equal(t, http.StatusOK, v.get(alice, "/v1/achievements").code, "users have their own bucket")
}
