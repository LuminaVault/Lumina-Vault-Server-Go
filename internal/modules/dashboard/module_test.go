package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/db"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/dashboard"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
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
	ann  = uuid.MustParse("DA5B0000-0000-4000-8000-0000000000A1")
	ben  = uuid.MustParse("DA5B0000-0000-4000-8000-0000000000B2")
	cat  = uuid.MustParse("DA5B0000-0000-4000-8000-0000000000C3")
	dan  = uuid.MustParse("DA5B0000-0000-4000-8000-0000000000D4")
	eli  = uuid.MustParse("DA5B0000-0000-4000-8000-0000000000E5")
	wri  = uuid.MustParse("DA5B0000-0000-4000-8000-0000000000F6")
	ins  = uuid.MustParse("DA5B0000-0000-4000-8000-0000000000A7")
	ovf  = uuid.MustParse("DA5B0000-0000-4000-8000-0000000000B8")
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

const teamVault = "DA5BC000-0000-4000-8000-0000000000C1"

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

// TestModule runs every case on its own database, cloned from one migrated and
// seeded template: one container per case costs a minute on a busy machine.
func TestModule(t *testing.T) {
	ctx := context.Background()
	base := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, base, migrations.SQL()))
	for _, f := range []string{"base.sql", "dashboard.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = base.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	dsn := base.Config().ConnString()
	template := base.Config().ConnConfig.Database
	base.Close() // CREATE DATABASE ... TEMPLATE needs the template idle.
	admin, err := db.Open(ctx, strings.Replace(dsn, "/"+template, "/postgres", 1))
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	for i, c := range []struct {
		name string
		run  func(*testing.T, env)
	}{
		{"EveryReadRequiresAuthentication", testEveryReadRequiresAuthentication},
		{"FreshRetrievalHealth", testFreshRetrievalHealth},
		{"FreshOverview", testFreshOverview},
		{"ManagedModelsHideUpstreamIdentity", testManagedModelsHideUpstreamIdentity},
		{"BYOKModelsPreserveUpstreamIdentity", testBYOKModelsPreserveUpstreamIdentity},
		{"RejectsCrossVaultRead", testRejectsCrossVaultRead},
		{"ValidatesClientEvents", testValidatesClientEvents},
		{"ValidatesModelFeedback", testValidatesModelFeedback},
		{"FreshStats", testFreshStats},
		{"FreshProfile", testFreshProfile},
		{"FreshTasks", testFreshTasks},
		{"FreshHome", testFreshHome},
		{"FreshHomePeriod", testFreshHomePeriod},
		{"FreshActivity", testFreshActivity},
		{"FreshInsights", testFreshInsights},
		{"Streak", testStreak},
		{"SeededStats", testSeededStats},
		{"SeededProfileAndPowerLevel", testSeededProfileAndPowerLevel},
		{"SeededHome", testSeededHome},
		{"HomeModelDisclosure", testHomeModelDisclosure},
		{"HomeCockpitSwallowsInt32Overflow", testHomeCockpitSwallowsInt32Overflow},
		{"ActivityFeed", testActivityFeed},
		{"Tasks", testTasks},
		{"Insights", testInsights},
		{"DismissInsight", testDismissInsight},
		{"RetrievalHealthTrends", testRetrievalHealthTrends},
		{"UsageSummary", testUsageSummary},
		{"OverviewAggregates", testOverviewAggregates},
		{"ModelsFeedback", testModelsFeedback},
		{"Team", testTeam},
		{"RecordEventWrites", testRecordEventWrites},
		{"RecommendationStates", testRecommendationStates},
		{"ResponsesConformToSpec", testResponsesConformToSpec},
	} {
		t.Run(c.name, func(t *testing.T) {
			name := fmt.Sprintf("case_%d", i)
			_, err := admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE "+template)
			require.NoError(t, err)
			pool, err := db.Open(ctx, strings.Replace(dsn, "/"+template, "/"+name, 1))
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			users := auth.PGUsers(pool)
			e := httpserver.New(config.Config{}, httpserver.Deps{
				Auth:    auth.Middleware(keys, users),
				Modules: []httpserver.Module{dashboard.New(pool, auth.Identify(keys, users))},
			})
			c.run(t, env{t: t, e: e, pool: pool})
		})
	}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.body, &m), string(r.body))
	return m
}

func (v env) req(user uuid.UUID, method, path, body string, headers map[string]string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

func (v env) get(user uuid.UUID, path string) map[string]any {
	v.t.Helper()
	r := v.req(user, http.MethodGet, path, "", nil)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	return r.json(v.t)
}

func (v env) errMessage(r resp, status int, msg string) {
	v.t.Helper()
	require.Equal(v.t, status, r.code, string(r.body))
	body, err := json.Marshal(msg)
	require.NoError(v.t, err)
	require.JSONEq(v.t, `{"error":{"message":`+string(body)+`}}`, string(r.body))
}

// num reads a JSON number as an int.
func num(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	n, ok := m[key].(float64)
	require.True(t, ok, "%s: %v", key, m[key])
	return int(n)
}

// freshUser adds a user and personal vault with no other rows.
func (v env) freshUser() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at)
		 VALUES ($1, $2, 'x', true, $3, now(), now())`, id, id.String()+"@parity.test", "u"+id.String()[:8])
	require.NoError(v.t, err)
	_, err = v.pool.Exec(context.Background(),
		`INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(v.t, err)
	return id
}

func (v env) memoryOn(tenant uuid.UUID, day string) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO memories (id, tenant_id, content, created_at) VALUES ($1, $2, 'activity', $3::timestamptz)`, uuid.New(), tenant, day+"T12:00:00Z")
	require.NoError(v.t, err)
}

func (v env) conversationOn(tenant uuid.UUID, day string) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 'session', $3::timestamptz, $3::timestamptz)`, uuid.New(), tenant, day+"T12:00:00Z")
	require.NoError(v.t, err)
}

func (v env) routerExecution(tenant uuid.UUID, provider, model string, tokensIn, tokensOut, cost int) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO router_executions (id, tenant_id, actor_user_id, vault_id, surface, task_type, strategy, status,
		   selected_provider, selected_model, tokens_in, tokens_out, estimated_cost_usd_micros, latency_ms, usage_estimated, fallback_count, occurred_at)
		 VALUES ($1, $2, $2, $2, 'chat', 'general', 'single', 'ok', $3, $4, $5, $6, $7, 250, false, 0, now())`,
		uuid.New(), tenant, provider, model, tokensIn, tokensOut, cost)
	require.NoError(v.t, err)
}

// Swift AnalyticsEndpointsTests "Analytics endpoints require authentication" and
// the "requires auth" tests of DashboardEndpointsTests.
func testEveryReadRequiresAuthentication(t *testing.T, v env) {
	for _, p := range []string{
		"/v1/analytics/overview", "/v1/analytics/models", "/v1/analytics/team", "/v1/analytics/retrieval-health",
		"/v1/analytics/usage-summary", "/v1/dashboard/stats", "/v1/dashboard/profile", "/v1/dashboard/home",
		"/v1/dashboard/activity", "/v1/tasks", "/v1/insights", "/v1/insights/synthesis/latest",
	} {
		v.errMessage(v.req(uuid.Nil, http.MethodGet, p, "", nil), http.StatusUnauthorized, "Authenticated identity is unavailable")
	}
}

// Swift "Fresh retrieval health returns empty-window defaults".
func testFreshRetrievalHealth(t *testing.T, v env) {
	require.Equal(t, map[string]any{"eventsCount": 0.0, "leakCount": 0.0, "trend": "steady"}, v.get(cat, "/v1/analytics/retrieval-health"))
}

// Swift "Fresh personal analytics return the complete contract".
func testFreshOverview(t *testing.T, v env) {
	body := v.get(cat, "/v1/analytics/overview?range=7d&scope=personal")
	require.Equal(t, "personal", body["scope"])
	require.Equal(t, "7d", body["range"])
	require.Equal(t, strings.ToUpper(cat.String()), body["vaultId"])
	require.Len(t, body["daily"], 7)
	require.Equal(t, 0, num(t, body["memoryHealth"].(map[string]any), "totalMemories"))
	require.Empty(t, body["recommendations"])
}

// Swift "Managed model analytics hide upstream provider and model".
func testManagedModelsHideUpstreamIdentity(t *testing.T, v env) {
	u := v.freshUser()
	v.routerExecution(u, "openrouter", "anthropic/claude-opus-4-1", 200, 75, 5000)
	v.routerExecution(u, "anthropic", "claude-sonnet-4-6", 25, 10, 1000)
	body := v.get(u, "/v1/analytics/models?scope=personal")
	models := body["models"].([]any)
	require.Len(t, models, 1)
	m := models[0].(map[string]any)
	require.Equal(t, "LuminaVault", m["provider"])
	require.Equal(t, "auto", m["model"])
	require.Equal(t, 2, num(t, m, "requests"))
	require.Equal(t, 310, num(t, m, "tokens"))
	require.Equal(t, 6000, num(t, m, "estimatedCostUsdMicros"))
	require.NotContains(t, m, "satisfactionRate", "no feedback: nil is omitted")
}

// Swift "BYOK model analytics preserve user-selected provider and model". The
// Swift test saves the preference through /v1/me/preferences/llm (wave 5); here
// the row is inserted directly.
func testBYOKModelsPreserveUpstreamIdentity(t *testing.T, v env) {
	u := v.freshUser()
	_, err := v.pool.Exec(context.Background(),
		`INSERT INTO user_llm_preferences (id, tenant_id, primary_provider, primary_model, fallback_chain, mode, created_at, updated_at)
		 VALUES ($1, $2, 'anthropic', 'claude-opus-4-1', '{"steps":[]}', 'byok', now(), now())`, uuid.New(), u)
	require.NoError(t, err)
	v.routerExecution(u, "anthropic", "claude-opus-4-1", 100, 50, 1234)
	m := v.get(u, "/v1/analytics/models?scope=personal")["models"].([]any)[0].(map[string]any)
	require.Equal(t, "anthropic", m["provider"])
	require.Equal(t, "claude-opus-4-1", m["model"])
}

// Swift "Analytics reject cross-vault reads".
func testRejectsCrossVaultRead(t *testing.T, v env) {
	r := v.req(cat, http.MethodGet, "/v1/analytics/overview", "", map[string]string{"X-Vault-ID": strings.ToUpper(ann.String())})
	v.errMessage(r, http.StatusForbidden, "vault access denied")
}

// Swift "Client event validation is content-free and idempotency-safe".
func testValidatesClientEvents(t *testing.T, v env) {
	r := v.req(wri, http.MethodPost, "/v1/analytics/events", `{"name":"analytics_dashboard_viewed","source":"web","range":"7d","idempotencyKey":"safe-key-1"}`, nil)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.JSONEq(t, `{"accepted":true}`, string(r.body))
	v.errMessage(v.req(wri, http.MethodPost, "/v1/analytics/events", `{"name":"analytics_dashboard_viewed","source":"web","idempotencyKey":"unsafe key"}`, nil),
		http.StatusBadRequest, "invalid idempotencyKey")
}

// Swift "Model feedback accepts only content-free model identities".
func testValidatesModelFeedback(t *testing.T, v env) {
	r := v.req(wri, http.MethodPost, "/v1/analytics/model-feedback", `{"provider":"openai","model":"gpt-test","rating":"positive","idempotencyKey":"feedback-1"}`, nil)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	v.errMessage(v.req(wri, http.MethodPost, "/v1/analytics/model-feedback", `{"provider":"openai","model":"prompt content is forbidden","rating":"negative"}`, nil),
		http.StatusBadRequest, "invalid provider or model")
}

// Swift "dashboard stats returns zero counters for a fresh tenant".
func testFreshStats(t *testing.T, v env) {
	require.Equal(t, map[string]any{"memoriesToday": 0.0, "memoriesTotal": 0.0}, v.get(cat, "/v1/dashboard/stats"))
}

// Swift "dashboard profile returns zero counters and level one for a fresh tenant".
func testFreshProfile(t *testing.T, v env) {
	require.Equal(t, map[string]any{
		"skillsCount": 0.0, "jobsCount": 0.0, "sessionsCount": 0.0, "badgesEarned": 0.0, "powerXP": 0.0, "powerLevel": 1.0,
	}, v.get(cat, "/v1/dashboard/profile"))
}

// Swift "tasks list returns empty list and accepts state and limit query params".
func testFreshTasks(t *testing.T, v env) {
	require.Equal(t, map[string]any{"tasks": []any{}}, v.get(cat, "/v1/tasks?state=running&limit=10"))
}

// Swift "dashboard home returns command center defaults for a fresh tenant".
func testFreshHome(t *testing.T, v env) {
	home := v.get(cat, "/v1/dashboard/home")
	for _, key := range []string{"skillsCount", "jobsCount", "activeJobsCount", "memoriesTotal", "sessionsCount", "powerXP", "cronJobsCount", "toolsCount"} {
		require.Equal(t, 0, num(t, home, key), key)
	}
	for _, key := range []string{"activeJobs", "skills", "cronJobs", "tools", "periodSeries"} {
		require.Equal(t, []any{}, home[key], key)
	}
	require.Equal(t, 1, num(t, home, "powerLevel"))
	require.Equal(t, true, home["agentOnline"])
	require.Equal(t, "LuminaVault Brain · Auto", home["primaryModel"])
	require.NotContains(t, home, "graphPreview")
	require.Equal(t, "today", home["period"])
}

// Swift "dashboard home accepts period query and still returns defaults".
func testFreshHomePeriod(t *testing.T, v env) {
	home := v.get(cat, "/v1/dashboard/home?period=week")
	require.Equal(t, "week", home["period"])
	require.Equal(t, 0, num(t, home["periodStats"].(map[string]any), "captures"))
}

// Swift "dashboard activity returns empty feed for a fresh tenant and accepts limit".
func testFreshActivity(t *testing.T, v env) {
	require.Equal(t, map[string]any{"items": []any{}}, v.get(cat, "/v1/dashboard/activity?limit=5"))
}

// Swift "insights list returns empty list and accepts section and limit query params".
func testFreshInsights(t *testing.T, v env) {
	require.Equal(t, map[string]any{"insights": []any{}}, v.get(cat, "/v1/insights?section=this_week&limit=20"))
}

func testStreak(t *testing.T, v env) {
	streak := func(u uuid.UUID) int { return num(t, v.get(u, "/v1/dashboard/home"), "streakDays") }

	// Swift DashboardStreakSQLTests: counts distinct recent days, stops at gaps.
	gaps := v.freshUser()
	for _, d := range []string{"2026-07-01", "2026-07-01", "2026-06-30", "2026-06-29", "2026-06-27"} {
		v.memoryOn(gaps, d)
	}
	require.Equal(t, 3, streak(gaps))

	// Swift DashboardStreakSQLTests: combines memory and conversation activity.
	mixed := v.freshUser()
	v.memoryOn(mixed, "2026-07-01")
	v.conversationOn(mixed, "2026-06-30")
	v.memoryOn(mixed, "2026-06-29")
	require.Equal(t, 3, streak(mixed))

	// Swift PowerLevelTests, whose streak cases exercise the test-only
	// consecutiveStreak; the production path is this SQL.
	run := v.freshUser()
	for _, d := range []string{"2026-06-01", "2026-06-03", "2026-06-04", "2026-06-05"} {
		v.memoryOn(run, d)
	}
	require.Equal(t, 3, streak(run), "most recent unbroken run")
	dedupe := v.freshUser()
	for _, d := range []string{"2026-06-05", "2026-06-04", "2026-06-04", "2026-06-03"} {
		v.memoryOn(dedupe, d)
	}
	require.Equal(t, 3, streak(dedupe), "order-independent, dedupes")
	single := v.freshUser()
	require.Equal(t, 0, streak(single), "no activity")
	v.memoryOn(single, "2026-06-05")
	require.Equal(t, 1, streak(single), "a single day")
	month := v.freshUser()
	for _, d := range []string{"2026-01-31", "2026-02-01", "2026-02-02"} {
		v.memoryOn(month, d)
	}
	require.Equal(t, 3, streak(month), "spans month boundaries")
}

func testSeededStats(t *testing.T, v env) {
	stats := v.get(ann, "/v1/dashboard/stats")
	require.Equal(t, 2, num(t, stats, "memoriesToday"))
	require.Equal(t, 10, num(t, stats, "memoriesTotal"))
	require.Contains(t, stats, "lastCompileAt")
}

func testSeededProfileAndPowerLevel(t *testing.T, v env) {
	profile := v.get(ann, "/v1/dashboard/profile")
	require.Equal(t, 9, num(t, profile, "skillsCount"))
	require.Equal(t, 5, num(t, profile, "jobsCount"))
	require.Equal(t, 4, num(t, profile, "sessionsCount"))
	require.Equal(t, 2, num(t, profile, "badgesEarned"))
	// memories 10 + sessions 4×3 + jobs 5×2 + badges 2×10 + connections 1 + spaces 2×5 + streak 5×3.
	require.Equal(t, 78, num(t, profile, "powerXP"))
	require.Equal(t, 9, num(t, profile, "powerLevel"))
}

func testSeededHome(t *testing.T, v env) {
	home := v.get(ann, "/v1/dashboard/home")
	require.Equal(t, 2, num(t, home, "remindersCount"))
	require.Equal(t, 2, num(t, home, "todosCount"))
	require.Equal(t, 1, num(t, home, "projectsCount"))
	require.Equal(t, 2, num(t, home, "insightsCount"))
	require.Equal(t, "Coach", home["activeProfileName"])
	require.Equal(t, "coach", home["activeProfileSlug"])
	require.Equal(t, "LuminaVault", home["primaryProvider"])
	require.Equal(t, "LuminaVault Brain · Auto", home["primaryModel"])
	require.Equal(t, 6, num(t, home, "activeJobsCount"), "5 active runs + 1 running apply; paused and finished are out")
	require.Len(t, home["activeJobs"], 5)
	require.Len(t, home["skills"], 8)
	require.Equal(t, 8, num(t, home, "toolsCount"), "toolsCount is the capped preview, as in Swift")
	require.Equal(t, 5, num(t, home, "streakDays"))
	require.Equal(t, 0, num(t, home, "cronJobsCount"))

	graph := home["graphPreview"].([]any)
	require.Len(t, graph, 3, "the partially laid-out row is excluded")
	hot := graph[0].(map[string]any)
	require.Equal(t, "Hot node", hot["label"])
	require.InDelta(t, 0.5, hot["x"], 0)
	require.InDelta(t, 1.0, hot["activity"], 1e-12)
	require.Equal(t, "memory", hot["kind"])
	require.Contains(t, string(v.req(ann, http.MethodGet, "/v1/dashboard/home", "", nil).body), `"z":9.999999999999999e-06`, "the small coordinate takes Swift's exponent form")

	stats := home["periodStats"].(map[string]any)
	require.Equal(t, 2, num(t, stats, "captures"))
	require.Equal(t, 3, num(t, stats, "skillRuns"))
	require.Equal(t, 1, num(t, stats, "done"), "OK counts case-insensitively; failed and running do not")
	require.Equal(t, 150, num(t, stats, "tokens"), "the day column is compared as a timestamptz, so today counts")
	require.Equal(t, 30, num(t, stats, "previousTokens"))
	require.Equal(t, 1, num(t, stats, "previousDone"))
	mix := home["periodMix"].(map[string]any)
	require.Equal(t, 1, num(t, mix, "chats"))
	require.Equal(t, 2, num(t, mix, "jobs"))
	require.NotEmpty(t, home["periodSeries"])
}

func testHomeModelDisclosure(t *testing.T, v env) {
	byok := v.get(ben, "/v1/dashboard/home")
	require.Equal(t, "anthropic", byok["primaryProvider"])
	require.Equal(t, "claude-opus-4-1", byok["primaryModel"])
	empty := v.get(eli, "/v1/dashboard/home")
	require.Equal(t, "openai", empty["primaryProvider"])
	require.Equal(t, "LuminaVault Brain · Auto", empty["primaryModel"], "BYOK with an empty model")
	managed := v.get(ins, "/v1/dashboard/home")
	require.Equal(t, "LuminaVault", managed["primaryProvider"], "a managed row never leaks its provider")
	require.Equal(t, "LuminaVault Brain · Auto", managed["primaryModel"])
}

func testHomeCockpitSwallowsInt32Overflow(t *testing.T, v env) {
	home := v.get(ovf, "/v1/dashboard/home")
	require.Equal(t, 1, num(t, home, "memoriesTotal"))
	require.Equal(t, 0, num(t, home["periodStats"].(map[string]any), "captures"), "the whole cockpit falls back")
	require.Equal(t, []any{}, home["periodSeries"])
	require.Equal(t, 3000000000, num(t, v.get(ovf, "/v1/analytics/usage-summary"), "llmTokensIn"))
}

func testActivityFeed(t *testing.T, v env) {
	items := v.get(ann, "/v1/dashboard/activity?limit=50")["items"].([]any)
	byTitle := map[string]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		byTitle[m["title"].(string)] = m
	}
	require.Equal(t, "skillRun", byTitle["scrape"]["kind"])
	require.Equal(t, "running", byTitle["scrape"]["subtitle"])
	require.Equal(t, "memory", byTitle["Morning plan"]["kind"])
	require.Equal(t, "memory", byTitle["Quoted title"]["kind"])
	require.Equal(t, "conversation", byTitle["Conversation"]["kind"], "an empty title reads Conversation")
	require.Equal(t, "achievement", byTitle["first_capture"]["kind"])
	require.NotContains(t, byTitle["Conversation"], "subtitle")
	require.Len(t, v.get(ann, "/v1/dashboard/activity?limit=0")["items"], 1)
	require.Len(t, v.get(ann, "/v1/dashboard/activity?limit=abc")["items"], 20)
}

func testTasks(t *testing.T, v env) {
	tasks := v.get(ann, "/v1/tasks")["tasks"].([]any)
	require.Len(t, tasks, 6)
	var labels []string
	for _, x := range tasks {
		m := x.(map[string]any)
		labels = append(labels, m["kind"].(string)+"/"+m["state"].(string))
	}
	require.Equal(t, []string{
		"gateway_apply/running", "workflow/running", "workflow/running",
		"workflow/queued", "workflow/queued", "workflow/queued",
	}, labels, "running first, newest first within each band")
	require.Equal(t, "partial failure", tasks[1].(map[string]any)["error"])
	require.Equal(t, "Daily digest", tasks[1].(map[string]any)["label"])
	require.Equal(t, "Applying gateway config", tasks[0].(map[string]any)["label"])
	require.GreaterOrEqual(t, num(t, tasks[0].(map[string]any), "elapsedSeconds"), 500)
	require.Len(t, v.get(ann, "/v1/tasks?state=queued")["tasks"], 3)
	require.Empty(t, v.get(ann, "/v1/tasks?state=failed")["tasks"])
	require.Len(t, v.get(ann, "/v1/tasks?state=nope")["tasks"], 6)
	require.Len(t, v.get(ann, "/v1/tasks?limit=3")["tasks"], 3)
	require.Empty(t, v.get(ann, "/v1/tasks?state=queued&limit=2")["tasks"], "each source is capped before the state filter")
	require.Len(t, v.get(ben, "/v1/tasks")["tasks"], 1)
}

func testInsights(t *testing.T, v env) {
	list := v.get(ins, "/v1/insights")["insights"].([]any)
	require.Len(t, list, 5, "the dismissed row is out")
	sections := map[string]int{}
	for _, x := range list {
		sections[x.(map[string]any)["section"].(string)]++
	}
	require.Equal(t, map[string]int{"this_week": 1, "patterns": 2, "contradictions": 1, "connections": 1}, sections, "unknown sections read as patterns")
	require.Len(t, v.get(ins, "/v1/insights?section=patterns")["insights"], 1, "the raw column is filtered, so the unknown row stays out")
	require.Len(t, v.get(ins, "/v1/insights?section=retired_section")["insights"], 5, "an unknown section means no filter")
	require.Len(t, v.get(ins, "/v1/insights?limit=0")["insights"], 1)

	latest := v.get(ins, "/v1/insights/synthesis/latest")
	require.Equal(t, "Month dismissed", latest["headline"], "a dismissed snapshot still counts")
	require.Equal(t, true, latest["dismissed"])
	require.Contains(t, latest, "periodStart")
	v.errMessage(v.req(ann, http.MethodGet, "/v1/insights/synthesis/latest", "", nil), http.StatusNotFound, "no synthesis available yet")

	week := list[len(list)-2].(map[string]any)
	require.Equal(t, "Week", week["headline"])
	require.Equal(t, []any{"DA5BE000-0000-4000-8000-000000000001", "DA5BE000-0000-4000-8000-000000000002"}, week["sourceMemoryIDs"])
}

func testDismissInsight(t *testing.T, v env) {
	const id = "DA5B8100-0000-4000-8000-000000000003"
	path := "/v1/insights/" + id + "/dismiss"
	v.errMessage(v.req(uuid.Nil, http.MethodPost, path, "", nil), http.StatusUnauthorized, "Authenticated identity is unavailable")
	v.errMessage(v.req(ins, http.MethodPost, "/v1/insights/nope/dismiss", "", nil), http.StatusBadRequest, "invalid insight id")
	v.errMessage(v.req(ben, http.MethodPost, path, "", nil), http.StatusNotFound, "insight not found")

	r := v.req(ins, http.MethodPost, strings.ToLower(path), "", nil)
	require.Equal(t, http.StatusNoContent, r.code, string(r.body))
	require.Empty(t, r.body)
	var first string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT dismissed_at::text FROM insights WHERE id = $1`, id).Scan(&first))
	require.Equal(t, http.StatusNoContent, v.req(ins, http.MethodPost, path, "", nil).code)
	var second string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT dismissed_at::text FROM insights WHERE id = $1`, id).Scan(&second))
	require.Equal(t, first, second, "dismissing twice keeps the first timestamp")
	require.Len(t, v.get(ins, "/v1/insights")["insights"], 4)
}

func testRetrievalHealthTrends(t *testing.T, v env) {
	improving := v.get(ann, "/v1/analytics/retrieval-health")
	require.Equal(t, "improving", improving["trend"])
	require.InDelta(t, 2.0/3, improving["hitRate"], 1e-12)
	require.InDelta(t, 0.375, improving["meanTopDistance"], 1e-12)
	require.Equal(t, 3, num(t, improving, "eventsCount"))
	require.Equal(t, 4, num(t, improving, "leakCount"), "the latest report by period end")
	require.Equal(t, "declining", v.get(ben, "/v1/analytics/retrieval-health")["trend"])
	require.Equal(t, "steady", v.get(eli, "/v1/analytics/retrieval-health")["trend"])
	noPrior := v.get(ins, "/v1/analytics/retrieval-health")
	require.Equal(t, "steady", noPrior["trend"])
	require.InDelta(t, 0.0, noPrior["hitRate"], 0)
	require.NotContains(t, noPrior, "meanTopDistance", "AVG over NULLs stays nil")
}

func testUsageSummary(t *testing.T, v env) {
	s := v.get(ann, "/v1/analytics/usage-summary")
	require.Equal(t, 120, num(t, s, "llmTokensIn"))
	require.Equal(t, 60, num(t, s, "llmTokensOut"))
	require.Equal(t, 1234, num(t, s, "embeddingTokens"))
	require.Equal(t, 3, num(t, s, "sessionsCount"))
	require.Equal(t, 96, num(t, s, "estimatedCostCents"))
	voice := s["voice"].(map[string]any)
	require.Equal(t, 4, num(t, voice, "calls"))
	require.Equal(t, 1, num(t, voice, "failedCalls"))
	require.InDelta(t, 2.2, voice["audioMinutes"], 0)
	require.Equal(t, 3, num(t, voice, "realCostCents"), "raw micros summed, not rounded channels")
	channels := voice["byChannel"].([]any)
	require.Len(t, channels, 3)
	require.Equal(t, "telegram", channels[0].(map[string]any)["channel"])
	require.Equal(t, 2, num(t, channels[0].(map[string]any), "calls"))
	require.Equal(t, "unknown", channels[2].(map[string]any)["channel"])
	require.NotContains(t, v.get(cat, "/v1/analytics/usage-summary"), "voice", "no rows: the block is absent")
}

func testOverviewAggregates(t *testing.T, v env) {
	o := v.get(ann, "/v1/analytics/overview?range=7d")
	require.Equal(t, "active", o["scope"])
	require.Len(t, o["daily"], 7)
	s := o["summary"].(map[string]any)
	require.Equal(t, 3, num(t, s, "sessions"), "personal: conversations in the window")
	require.Equal(t, 12, num(t, s, "aiRequests"))
	require.Equal(t, 7800, num(t, s, "tokensIn"))
	require.Equal(t, 780, num(t, s, "tokensOut"))
	require.Equal(t, 962910, num(t, s, "estimatedCostUsdMicros"))
	require.Equal(t, 1, num(t, s, "retrievals"))
	require.Equal(t, 4, num(t, s, "captures"), "the rejected memory is out")
	health := o["memoryHealth"].(map[string]any)
	require.Equal(t, 9, num(t, health, "totalMemories"))
	require.Equal(t, 1, num(t, health, "pendingReviewCount"))
	require.Len(t, health["components"], 4)
	ids := []string{}
	for _, x := range o["recommendations"].([]any) {
		ids = append(ids, x.(map[string]any)["id"].(string))
	}
	require.Equal(t, []string{"memory-review-overdue", "memory-moderation-pending", "memory-organization", "memory-never-retrieved", "model-reliability"}, ids)

	team := v.req(ben, http.MethodGet, "/v1/analytics/overview?range=7d", "", map[string]string{"X-Vault-ID": teamVault})
	require.Equal(t, http.StatusOK, team.code, string(team.body))
	require.Equal(t, 1, num(t, team.json(t)["summary"].(map[string]any), "sessions"), "shared vault: conversation_created events")

	v.errMessage(v.req(dan, http.MethodGet, "/v1/analytics/overview?scope=personal", "", nil), http.StatusNotFound, "vault not found")
}

func testModelsFeedback(t *testing.T, v env) {
	models := v.get(ben, "/v1/analytics/models")["models"].([]any)
	require.Len(t, models, 3)
	first := models[0].(map[string]any)
	require.Equal(t, "claude-opus-4-1", first["model"])
	require.Equal(t, 3, num(t, first, "requests"))
	require.InDelta(t, 2.0/3, first["satisfactionRate"], 1e-12)
	require.Equal(t, 700, num(t, first, "p95LatencyMs"))
	require.InDelta(t, 2.0/3, first["successRate"], 1e-12)
	require.InDelta(t, 1.0/3, first["fallbackRate"], 1e-12)
	require.Equal(t, "unknown", models[2].(map[string]any)["provider"])
}

func testTeam(t *testing.T, v env) {
	header := map[string]string{"X-Vault-ID": teamVault}
	v.errMessage(v.req(ann, http.MethodGet, "/v1/analytics/team", "", nil), http.StatusUnprocessableEntity, "team analytics requires a shared vault")
	v.errMessage(v.req(cat, http.MethodGet, "/v1/analytics/team", "", header), http.StatusForbidden, "vault access denied")

	admin := v.req(ann, http.MethodGet, "/v1/analytics/team", "", header)
	require.Equal(t, http.StatusOK, admin.code, string(admin.body))
	body := admin.json(t)
	members := body["members"].([]any)
	require.Len(t, members, 2, "eli holds a vault membership but no team membership")
	// ben has no captures, so captures + requests is NULL: first under DESC.
	require.Equal(t, "dash_ben", members[0].(map[string]any)["displayName"])
	require.Equal(t, 2, num(t, members[0].(map[string]any), "aiRequests"))
	require.Equal(t, 2, num(t, members[0].(map[string]any), "retrievals"))
	require.Equal(t, 2, num(t, members[1].(map[string]any), "captures"))
	require.Equal(t, 3, num(t, body["summary"].(map[string]any), "captures"))

	viewer := v.req(ben, http.MethodGet, "/v1/analytics/team", "", header)
	require.Equal(t, http.StatusOK, viewer.code)
	require.NotContains(t, viewer.json(t), "members")
}

func testRecordEventWrites(t *testing.T, v env) {
	ctx := context.Background()
	body := `{"name":"analytics_dashboard_viewed","source":"web","range":"7d","idempotencyKey":"k1"}`
	require.Equal(t, http.StatusOK, v.req(wri, http.MethodPost, "/v1/analytics/events", body, nil).code)
	require.Equal(t, http.StatusOK, v.req(wri, http.MethodPost, "/v1/analytics/events", body, nil).code)
	var events, rollup int
	var dims string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*), max(dimensions::text) FROM analytics_events WHERE actor_user_id = $1`, wri).Scan(&events, &dims))
	require.Equal(t, 1, events, "the idempotency key dedupes the event")
	require.JSONEq(t, `{"range":"7d"}`, dims)
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT value FROM analytics_daily_rollups WHERE actor_user_id = $1 AND metric = 'analytics_dashboard_viewed'`, wri).Scan(&rollup))
	require.Equal(t, 2, rollup, "the rollup still bumps")

	v.errMessage(v.req(wri, http.MethodPost, "/v1/analytics/events", `{"name":"analytics_recommendation_opened","source":"web"}`, nil), http.StatusBadRequest, "recommendationId required")
	v.errMessage(v.req(uuid.Nil, http.MethodPost, "/v1/analytics/events", `{"name":"analytics_recommendation_opened","source":"web"}`, nil), http.StatusUnauthorized, "Authenticated identity is unavailable")
	v.errMessage(v.req(uuid.Nil, http.MethodPost, "/v1/analytics/events", `{"name":"nope","source":"web"}`, nil), http.StatusBadRequest,
		"The given data was not valid input: Cannot initialize AnalyticsClientEventName from invalid String value nope")
	v.errMessage(v.req(wri, http.MethodPost, "/v1/analytics/events", `{"name":"analytics_dashboard_viewed","source":"web","range":5}`, nil), http.StatusBadRequest,
		"Type mismatch for `range` key, expected `String` type.")
	r := v.req(wri, http.MethodPost, "/v1/analytics/events", `{"name":"analytics_recommendation_snoozed","source":"ios","recommendationId":"a_b-1"}`, nil)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT dimensions::text FROM analytics_events WHERE event_name = 'analytics_recommendation_snoozed'`).Scan(&dims))
	require.JSONEq(t, `{"recommendation_id":"a_b-1"}`, dims)
}

func testRecommendationStates(t *testing.T, v env) {
	ctx := context.Background()
	ids := func() []string {
		out := []string{}
		for _, x := range v.get(wri, "/v1/analytics/overview?range=90d")["recommendations"].([]any) {
			out = append(out, x.(map[string]any)["id"].(string))
		}
		return out
	}
	require.Equal(t, []string{"memory-review-overdue", "memory-moderation-pending", "memory-organization", "memory-never-retrieved"}, ids(), "an expired snooze hides nothing")
	patch := func(id, disposition string) resp {
		return v.req(wri, http.MethodPatch, "/v1/analytics/recommendations", fmt.Sprintf(`{"recommendationId":%q,"disposition":%q}`, id, disposition), nil)
	}
	require.Equal(t, http.StatusOK, patch("memory-review-overdue", "dismiss").code)
	require.Equal(t, http.StatusOK, patch("memory-moderation-pending", "snooze7").code)
	require.Equal(t, http.StatusOK, patch("memory-organization", "snooze30").code)
	require.Equal(t, []string{"memory-never-retrieved"}, ids())
	days := func(id string) (d int, dismissed bool) {
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT coalesce(snooze_days, 0), dismissed FROM dashboard_recommendation_states_view WHERE recommendation_id = $1`, id).Scan(&d, &dismissed))
		return
	}
	d, dismissed := days("memory-moderation-pending")
	require.Equal(t, 7, d)
	require.False(t, dismissed)
	d, _ = days("memory-organization")
	require.Equal(t, 30, d)
	d, dismissed = days("memory-review-overdue")
	require.Equal(t, 0, d)
	require.True(t, dismissed)

	require.Equal(t, http.StatusOK, patch("memory-review-overdue", "snooze7").code)
	d, dismissed = days("memory-review-overdue")
	require.Equal(t, 7, d)
	require.False(t, dismissed, "the upsert replaces the dismissal")
	v.errMessage(patch("bad id", "dismiss"), http.StatusBadRequest, "invalid recommendationId")
}

// Every success body must validate against api/openapi.yaml (decisions.md D2).
func testResponsesConformToSpec(t *testing.T, v env) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	header := map[string]string{"X-Vault-ID": teamVault}
	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
		headers            map[string]string
	}{
		{ann, http.MethodGet, "/v1/dashboard/stats", "", nil},
		{ann, http.MethodGet, "/v1/dashboard/profile", "", nil},
		{ann, http.MethodGet, "/v1/dashboard/home", "", nil},
		{cat, http.MethodGet, "/v1/dashboard/home", "", nil},
		{ann, http.MethodGet, "/v1/dashboard/activity", "", nil},
		{ann, http.MethodGet, "/v1/tasks", "", nil},
		{ins, http.MethodGet, "/v1/insights", "", nil},
		{ins, http.MethodGet, "/v1/insights/synthesis/latest", "", nil},
		{ann, http.MethodGet, "/v1/analytics/usage-summary", "", nil},
		{ann, http.MethodGet, "/v1/analytics/retrieval-health", "", nil},
		{ann, http.MethodGet, "/v1/analytics/overview", "", nil},
		{ben, http.MethodGet, "/v1/analytics/models", "", nil},
		{ann, http.MethodGet, "/v1/analytics/team", "", header},
		{ben, http.MethodGet, "/v1/analytics/team", "", header},
		{wri, http.MethodPost, "/v1/analytics/events", `{"name":"analytics_dashboard_viewed","source":"web"}`, nil},
		{wri, http.MethodPost, "/v1/analytics/model-feedback", `{"provider":"p","model":"m","rating":"positive"}`, nil},
		{wri, http.MethodPatch, "/v1/analytics/recommendations", `{"recommendationId":"r","disposition":"dismiss"}`, nil},
		{ins, http.MethodPost, "/v1/insights/DA5B8100-0000-4000-8000-000000000003/dismiss", "", nil},
	} {
		r := v.req(c.user, c.method, c.path, c.body, c.headers)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}
