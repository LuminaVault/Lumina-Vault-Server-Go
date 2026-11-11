package me_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/me"
	"lumina/internal/ratelimit"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const day = 24 * time.Hour

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// startDB is a fresh Postgres with the schema and the seeds the scenarios use.
func startDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "me.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	return pool
}

type env struct {
	t    *testing.T
	e    *echo.Echo
	m    *me.Module
	pool *pgxpool.Pool
}

func newEnv(t *testing.T, pool *pgxpool.Pool, opts me.Options) env {
	t.Helper()
	m := me.New(pool, auth.Identify(keys, auth.PGUsers(pool)), ratelimit.NewStore(), opts)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	return env{t: t, e: e, m: m, pool: pool}
}

func setup(t *testing.T, pool *pgxpool.Pool) env {
	t.Helper()
	return newEnv(t, pool, me.Options{RecapThemesEnabled: true})
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func decode[T any](t *testing.T, r resp) T {
	t.Helper()
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var out T
	require.NoError(t, json.Unmarshal(r.body, &out), string(r.body))
	return out
}

func (v env) do(user uuid.UUID, method, path, body string, header ...string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func (v env) get(user uuid.UUID, path string, header ...string) resp {
	v.t.Helper()
	return v.do(user, http.MethodGet, path, "", header...)
}

func (v env) exec(sql string, args ...any) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(), sql, args...)
	require.NoError(v.t, err)
}

// user registers a tenant with its personal vault.
func (v env) user(tz string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at, timezone)
		 VALUES ($1, $2, 'stub-hash', true, $3, now(), now(), $4)`, id, id.String()+"@test.luminavault", "u"+id.String()[:8], tz)
	v.exec(`INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	return id
}

type mem struct {
	kind, state, origin      string
	lastReviewed, nextReview *time.Time
	muted                    bool
	tags                     []string
	space                    *uuid.UUID
}

func (v env) memory(tenant uuid.UUID, content string, at time.Time, o mem) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	if o.state == "" {
		o.state = "auto"
	}
	if o.origin == "" {
		o.origin = "legacy"
	}
	var kind *string
	if o.kind != "" {
		kind = &o.kind
	}
	v.exec(`INSERT INTO memories (id, tenant_id, content, created_at, review_state, origin_kind, kind, last_reviewed_at, next_review_at, resurface_muted, tags, space_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		id, tenant, content, at, o.state, o.origin, kind, o.lastReviewed, o.nextReview, o.muted, o.tags, o.space)
	return id
}

func (v env) soulChange(tenant uuid.UUID, title string, at time.Time, kind, state string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO self_improvement_changes (id, tenant_id, kind, state, trigger, title, summary, created_at)
		VALUES ($1, $2, $3, $4, 'weekly', $5, $6, $7)`, id, tenant, kind, state, title, "Summary of "+title, at)
	return id
}

func (v env) health(tenant uuid.UUID, kind string, value float64, unit string, at time.Time) {
	v.t.Helper()
	v.exec(`INSERT INTO health_events (id, tenant_id, event_type, value_numeric, unit, recorded_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())`, uuid.New(), tenant, kind, value, unit, at)
}

func (v env) message(conversation uuid.UUID, role string, at time.Time) {
	v.t.Helper()
	v.exec(`INSERT INTO conversation_messages (id, conversation_id, role, content, created_at) VALUES ($1, $2, $3, 'What did I decide?', $4)`,
		uuid.New(), conversation, role, at)
}

func (v env) conversation(tenant uuid.UUID, at time.Time) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 'Plans', $3, $3)`, id, tenant, at)
	return id
}

func (v env) brief(tenant uuid.UUID, date, cards string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO briefs (id, tenant_id, date, cards, end_note, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, 'That''s it for today.', now(), now())`, id, tenant, date, cards)
	return id
}

func date(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return v
}

func ptr[T any](v T) *T { return &v }

// localDay is the yyyy-MM-dd key and start of the day offset days from today.
func localDay(offset int, zone string) (string, time.Time) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		panic(err)
	}
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day()+offset, 0, 0, 0, 0, loc)
	return start.Format("2006-01-02"), start
}

// TestAgainstPostgres runs every database test on one container. The tests
// create their own tenants, so they do not see each other; the seeded fixture
// users are only read.
func TestAgainstPostgres(t *testing.T) {
	pool := startDB(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, env)
	}{
		{"ApnsDefaultsOn", testApnsDefaultsOn},
		{"ApnsTurningOffPersists", testApnsTurningOffPersists},
		{"ApnsSuppressesExactlyWhatWasTurnedOff", testApnsSuppressesExactly},
		{"ApnsRequiresAToken", testApnsRequiresToken},
		{"TodayForATenantWithNoData", testTodayNoData},
		{"TodaySumsStepsAndExcludesYesterday", testTodaySteps},
		{"TodayReportsSleepFromSleepSession", testTodaySleep},
		{"TodayCacheAndETag", testTodayCacheAndETag},
		{"TodayLastMemorySkipsRejected", testTodayLastMemory},
		{"CommitmentsTenantScopedAndPatchMovesState", testCommitments},
		{"CommitmentReopenClash", testCommitmentClash},
		{"BriefAndFeedbackAreTheCallersOwn", testBriefOwn},
		{"BriefDownOnToRevisitSnoozes30Days", testBriefSnooze},
		{"BriefDownOnOtherCardsDoesNotSnooze", testBriefNoSnooze},
		{"LearnedSoulChanges", testLearnedSoul},
		{"LearnedMemories", testLearnedMemories},
		{"LearnedCappedAtTwenty", testLearnedCap},
		{"LearnedBadSince", testLearnedBadSince},
		{"LearnedRequiresAToken", testLearnedToken},
		{"RecapCountsToday", testRecapToday},
		{"RecapBucketsByLocalDay", testRecapLocalDay},
		{"Recap30d", testRecap30d},
		{"RecapRejectsOtherRanges", testRecapBadRange},
		{"ResurfaceLanes", testResurfaceLanes},
		{"ResurfaceLimitAndDedupe", testResurfaceLimit},
		{"ResurfaceOnThisDayUsesLocalCalendar", testResurfaceTZ},
		{"ResurfaceSaysWhyInWords", testResurfaceWhy},
		{"ResurfaceSnippet", testResurfaceSnippet},
		{"PeriodBadParamsAndNewAccount", testPeriodBadParams},
		{"PeriodMonthCountsLocalDaysAndClaimsOnce", testPeriodMonth},
		{"PeriodReadyAndFallbackRows", testPeriodThemesRows},
		{"PeriodThemesDisabled", testPeriodDisabled},
		{"LocationCachedAndForgotten", testLocation},
		{"SharedRateLimit", testSharedRateLimit},
		{"SpecConformance", testSpecConformance},
		{"SpecTodaySchemaIsStale", testSpecTodayStale},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, setup(t, pool)) })
	}
}

// ─── ApnsCategoryPrefsControllerTests ──────────────────────────────────────

func testApnsDefaultsOn(t *testing.T, v env) {
	p := decode[me.APNSCategoryPrefsResponse](t, v.get(v.user("UTC"), "/v1/me/apns-categories"))
	require.True(t, p.ApprovalEnabled)
	require.True(t, p.RunCompletedEnabled)
	require.True(t, p.ChatEnabled)
	require.NotNil(t, p.MeetingBriefEnabled)
	require.False(t, *p.MeetingBriefEnabled, "the meeting brief is opt-in")
}

func testApnsTurningOffPersists(t *testing.T, v env) {
	u := v.user("UTC")
	p := decode[me.APNSCategoryPrefsResponse](t, v.do(u, http.MethodPut, "/v1/me/apns-categories", `{"approvalEnabled":false}`))
	require.False(t, p.ApprovalEnabled)
	// A sparse put must not reset what it did not mention.
	require.True(t, p.RunCompletedEnabled)
	require.True(t, p.ChatEnabled)
	// Read it back on a fresh request: the column, not just the echo.
	p = decode[me.APNSCategoryPrefsResponse](t, v.get(u, "/v1/me/apns-categories"))
	require.False(t, p.ApprovalEnabled)
}

func testApnsSuppressesExactly(t *testing.T, v env) {
	u := v.user("UTC")
	p := decode[me.APNSCategoryPrefsResponse](t, v.do(u, http.MethodPut, "/v1/me/apns-categories", `{"runCompletedEnabled":false,"approvalEnabled":true}`))
	require.False(t, p.RunCompletedEnabled)
	require.True(t, p.ApprovalEnabled)
}

func testApnsRequiresToken(t *testing.T, v env) {
	require.Equal(t, http.StatusUnauthorized, v.get(uuid.Nil, "/v1/me/apns-categories").code)
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPut, "/v1/me/apns-categories", `{}`).code)
}

// ─── MeTodayControllerTests ────────────────────────────────────────────────

// today is a local decoding of the server-local snake_case response.
type today struct {
	LastMemory *struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"last_memory"`
	OpenSpacesCount int `json:"open_spaces_count"`
	Unlocked        []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"unlocked_achievements_today"`
	Health *struct {
		Steps *int    `json:"steps_today"`
		Sleep *string `json:"sleep_last_night"`
	} `json:"health_summary"`
	Tier string `json:"tier"`
}

func startOfProcessDay() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
}

func testTodayNoData(t *testing.T, v env) {
	r := decode[today](t, v.get(v.user("UTC"), "/v1/me/today"))
	// COALESCE(SUM(...), 0) always returns a row: zero steps, not no summary.
	require.NotNil(t, r.Health)
	require.Equal(t, 0, *r.Health.Steps)
	require.Nil(t, r.Health.Sleep)
	require.Equal(t, 0, r.OpenSpacesCount)
	require.NotNil(t, r.Unlocked)
	require.Empty(t, r.Unlocked)
}

func testTodaySteps(t *testing.T, v env) {
	u := v.user("UTC")
	noon := startOfProcessDay().Add(12 * time.Hour)
	v.health(u, "steps", 1200, "count", noon)
	v.health(u, "steps", 800, "count", noon.Add(time.Hour))
	v.health(u, "steps", 9999, "count", noon.Add(-day))
	r := decode[today](t, v.get(u, "/v1/me/today"))
	require.Equal(t, 2000, *r.Health.Steps)
}

func testTodaySleep(t *testing.T, v env) {
	u := v.user("UTC")
	v.health(u, "sleep_session", 450, "minutes", startOfProcessDay().Add(-time.Hour))
	r := decode[today](t, v.get(u, "/v1/me/today"))
	require.Equal(t, "PT7H30M", *r.Health.Sleep)
}

func testTodayCacheAndETag(t *testing.T, v env) {
	u := v.user("UTC")
	first := v.get(u, "/v1/me/today")
	require.Equal(t, http.StatusOK, first.code)
	require.Equal(t, "private, max-age=300", first.header.Get("Cache-Control"))
	etag := first.header.Get("ETag")
	require.Regexp(t, `^"[0-9a-f]{16}"$`, etag)

	// A matching tag, quoted or not, is a 304 with both headers and no body.
	for _, tag := range []string{etag, strings.Trim(etag, `"`)} {
		r := v.get(u, "/v1/me/today", "If-None-Match", tag)
		require.Equal(t, http.StatusNotModified, r.code)
		require.Empty(t, r.body)
		require.Equal(t, etag, r.header.Get("ETag"))
		require.Equal(t, "private, max-age=300", r.header.Get("Cache-Control"))
	}
	require.Equal(t, http.StatusOK, v.get(u, "/v1/me/today", "If-None-Match", `"0000000000000000"`).code)

	// New data stays invisible for the TTL...
	v.memory(u, "Fresh", time.Now(), mem{})
	require.Nil(t, decode[today](t, v.get(u, "/v1/me/today")).LastMemory)
	// ...unless the entry is invalidated...
	v.m.InvalidateToday(u)
	require.Equal(t, "Fresh", decode[today](t, v.get(u, "/v1/me/today")).LastMemory.Title)
	// ...or 300 s have passed since it was generated.
	v.memory(u, "Fresher", time.Now().Add(time.Minute), mem{})
	v.m.SetClock(func() time.Time { return time.Now().Add(299 * time.Second) })
	require.Equal(t, "Fresh", decode[today](t, v.get(u, "/v1/me/today")).LastMemory.Title)
	v.m.SetClock(func() time.Time { return time.Now().Add(301 * time.Second) })
	require.Equal(t, "Fresher", decode[today](t, v.get(u, "/v1/me/today")).LastMemory.Title)
}

func testTodayLastMemory(t *testing.T, v env) {
	u := v.user("UTC")
	now := time.Now()
	v.memory(u, "Kept\nsecond line", now.Add(-time.Hour), mem{})
	v.memory(u, "Rejected", now, mem{state: "rejected"})
	require.Equal(t, "Kept", decode[today](t, v.get(u, "/v1/me/today")).LastMemory.Title)
}

// ─── CommitmentsTests (HTTP) ───────────────────────────────────────────────

func (v env) commitment(tenant uuid.UUID, text string, due *time.Time, state string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO commitments (id, tenant_id, text, normalized_text, due_at, state, surfaced_count, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 0, now())`, id, tenant, text, strings.ToLower(strings.TrimRight(text, ".")), due, state)
	return id
}

func patchCommitment(v env, u, id uuid.UUID, state string) resp {
	v.t.Helper()
	return v.do(u, http.MethodPatch, "/v1/me/commitments/"+strings.ToUpper(id.String()), fmt.Sprintf(`{"state":%q}`, state))
}

func commitmentTexts(t *testing.T, v env, u uuid.UUID, query string) []string {
	t.Helper()
	items := decode[me.CommitmentsResponse](t, v.get(u, "/v1/me/commitments"+query)).Items
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.Text)
	}
	return out
}

func testCommitments(t *testing.T, v env) {
	mine, other := v.user("UTC"), v.user("UTC")
	v.commitment(mine, "Later one", nil, "open")
	due := v.commitment(mine, "Due one", ptr(time.Now().Add(day)), "open")
	foreign := v.commitment(other, "Not mine", nil, "open")

	require.Equal(t, []string{"Due one", "Later one"}, commitmentTexts(t, v, mine, ""), "due first, then oldest")

	// Another tenant's id is a 404, and it is untouched.
	require.Equal(t, http.StatusNotFound, patchCommitment(v, mine, foreign, "done").code)
	require.Equal(t, []string{"Not mine"}, commitmentTexts(t, v, other, ""))

	done := decode[me.CommitmentDTO](t, patchCommitment(v, mine, due, "done"))
	require.Equal(t, me.CommitmentState("done"), done.State)
	require.NotNil(t, done.DoneAt)

	require.Equal(t, []string{"Later one"}, commitmentTexts(t, v, mine, ""))
	require.Equal(t, []string{"Due one"}, commitmentTexts(t, v, mine, "?state=done"))
	require.Len(t, commitmentTexts(t, v, mine, "?state=all"), 2)
	require.Empty(t, commitmentTexts(t, v, mine, "?state=dropped"))
	require.Equal(t, http.StatusBadRequest, v.get(mine, "/v1/me/commitments?state=snoozed").code)

	reopened := decode[me.CommitmentDTO](t, patchCommitment(v, mine, due, "open"))
	require.Equal(t, me.CommitmentState("open"), reopened.State)
	require.Nil(t, reopened.DoneAt)
	require.Equal(t, http.StatusBadRequest, patchCommitment(v, mine, due, "snoozed").code)
}

func testCommitmentClash(t *testing.T, v env) {
	u := v.user("UTC")
	v.commitment(u, "Renew passport", nil, "open")
	dup := v.commitment(u, "Renew passport.", nil, "done")
	r := patchCommitment(v, u, dup, "open")
	require.Equal(t, http.StatusConflict, r.code)
	require.JSONEq(t, `{"error":{"message":"an open commitment with the same text already exists"}}`, string(r.body))
}

// ─── MeBriefTests (HTTP) ───────────────────────────────────────────────────

func feedback(v env, u, brief uuid.UUID, card, action string) int {
	v.t.Helper()
	return v.do(u, http.MethodPost, "/v1/me/brief/"+strings.ToUpper(brief.String())+"/cards/"+card+"/feedback", fmt.Sprintf(`{"action":%q}`, action)).code
}

func testBriefOwn(t *testing.T, v env) {
	mine, other := v.user("UTC"), v.user("UTC")
	r := v.get(mine, "/v1/me/brief")
	require.Equal(t, http.StatusOK, r.code)
	require.JSONEq(t, `{}`, string(r.body), "no brief yet is empty, not 404")

	today, _ := localDay(0, "UTC")
	brief := v.brief(mine, today, `[{"id":"weather","kind":"weather","title":"Weather","body":"Dry.","refs":[]}]`)
	got := decode[me.MeBriefResponse](t, v.get(mine, "/v1/me/brief?date="+today))
	require.Equal(t, brief, got.Brief.Id.UUID)
	require.Equal(t, brief, decode[me.MeBriefResponse](t, v.get(mine, "/v1/me/brief")).Brief.Id.UUID, "default is today")
	require.Nil(t, decode[me.MeBriefResponse](t, v.get(other, "/v1/me/brief?date="+today)).Brief)
	require.Equal(t, http.StatusBadRequest, v.get(mine, "/v1/me/brief?date=2026-13-01").code)
	require.Equal(t, http.StatusBadRequest, v.get(mine, "/v1/me/brief?date=yesterday").code)

	require.Equal(t, http.StatusNotFound, feedback(v, other, brief, "weather", "down"))
	require.Equal(t, http.StatusNotFound, feedback(v, mine, brief, "nope", "down"))
	require.Equal(t, http.StatusBadRequest, feedback(v, mine, brief, "weather", "meh"))
	require.Equal(t, http.StatusNoContent, feedback(v, mine, brief, "weather", "up"))

	var tenant uuid.UUID
	var action, kind string
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT tenant_id, action, card_kind, count(*) OVER () FROM brief_feedback WHERE brief_id = $1`, brief).Scan(&tenant, &action, &kind, &n))
	require.Equal(t, 1, n)
	require.Equal(t, mine, tenant)
	require.Equal(t, "up", action)
	require.Equal(t, "weather", kind)
}

func testBriefSnooze(t *testing.T, v env) {
	u := v.user("UTC")
	now := time.Now()
	id := v.memory(u, "Revisit me", now.Add(-40*day), mem{nextReview: ptr(now.Add(-time.Minute))})
	today, _ := localDay(0, "UTC")
	brief := v.brief(u, today, fmt.Sprintf(`[{"id":"toRevisit","kind":"toRevisit","title":"To revisit","body":"Revisit me","refs":[{"kind":"memory","id":%q},{"kind":"memory","id":"not-a-uuid"}]}]`, strings.ToUpper(id.String())))
	require.Equal(t, http.StatusNoContent, feedback(v, u, brief, "toRevisit", "down"))

	var next time.Time
	var muted bool
	var count int
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT next_review_at, resurface_muted, review_count FROM memories WHERE id = $1`, id).Scan(&next, &muted, &count))
	require.InDelta(t, (30 * day).Seconds(), next.Sub(now).Seconds(), 5)
	require.False(t, muted)
	require.Equal(t, 1, count)
}

func testBriefNoSnooze(t *testing.T, v env) {
	u := v.user("UTC")
	next := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	id := v.memory(u, "Revisit me", next.Add(-40*day), mem{nextReview: &next})
	today, _ := localDay(0, "UTC")
	refs := fmt.Sprintf(`[{"kind":"memory","id":%q}]`, id)
	brief := v.brief(u, today, `[{"id":"a","kind":"weather","title":"t","body":"b","refs":`+refs+`},{"id":"b","kind":"toRevisit","title":"t","body":"b","refs":`+refs+`}]`)
	require.Equal(t, http.StatusNoContent, feedback(v, u, brief, "a", "down"), "another kind")
	require.Equal(t, http.StatusNoContent, feedback(v, u, brief, "b", "up"), "another action")

	var got time.Time
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT next_review_at FROM memories WHERE id = $1`, id).Scan(&got))
	require.True(t, got.Equal(next), "%v != %v", got, next)
}

// ─── MeLearnedTests ────────────────────────────────────────────────────────

func testLearnedSoul(t *testing.T, v env) {
	mine, other := v.user("UTC"), v.user("UTC")
	now := time.Now()
	older := v.soulChange(mine, "Prefers mornings", now.Add(-2*day), "soul", "pending")
	newer := v.soulChange(mine, "Runs on Sundays", now.Add(-day), "soul", "pending")
	v.soulChange(mine, "Approved", now.Add(-day), "soul", "approved")
	v.soulChange(mine, "Rejected", now.Add(-day), "soul", "rejected")
	v.soulChange(mine, "Curator", now.Add(-day), "curator", "pending")
	v.soulChange(mine, "Too old", now.Add(-10*day), "soul", "pending")
	v.soulChange(other, "Not mine", now.Add(-day), "soul", "pending")

	digest := decode[me.LearnedDigestResponse](t, v.get(mine, "/v1/me/learned"))
	require.Len(t, digest.SoulChanges, 2)
	require.Equal(t, newer, digest.SoulChanges[0].Id.UUID)
	require.Equal(t, older, digest.SoulChanges[1].Id.UUID)
	require.Equal(t, "Runs on Sundays", digest.SoulChanges[0].Title)
	require.Equal(t, "Summary of Runs on Sundays", digest.SoulChanges[0].Summary)
	require.Empty(t, digest.Memories)

	// A narrower window drops the older proposal.
	since := now.Add(-36 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	narrow := decode[me.LearnedDigestResponse](t, v.get(mine, "/v1/me/learned?since="+url.QueryEscape(since)))
	require.Len(t, narrow.SoulChanges, 1)
	require.Equal(t, newer, narrow.SoulChanges[0].Id.UUID)
}

func testLearnedMemories(t *testing.T, v env) {
	mine, other := v.user("UTC"), v.user("UTC")
	now := time.Now()
	long := "Prefers\n  quiet   mornings " + strings.Repeat("and long walks ", 20)
	preference := v.memory(mine, long, now.Add(-3*day), mem{kind: "preference"})
	decision := v.memory(mine, "Chose the cheaper flat", now.Add(-day), mem{kind: "decision", state: "pending"})
	v.memory(mine, "A fact", now.Add(-day), mem{kind: "fact"})
	v.memory(mine, "No kind", now.Add(-day), mem{})
	v.memory(mine, "Already kept", now.Add(-day), mem{kind: "preference", state: "approved"})
	v.memory(mine, "Forgotten", now.Add(-day), mem{kind: "decision", state: "rejected"})
	v.memory(mine, "Too old", now.Add(-8*day), mem{kind: "decision"})
	v.memory(other, "Not mine", now.Add(-day), mem{kind: "preference"})

	digest := decode[me.LearnedDigestResponse](t, v.get(mine, "/v1/me/learned"))
	require.Len(t, digest.Memories, 2)
	first, second := digest.Memories[0], digest.Memories[1]
	require.Equal(t, decision, first.Id.UUID)
	require.Equal(t, preference, second.Id.UUID)
	require.Equal(t, "decision", first.Kind)
	require.Equal(t, "preference", second.Kind)
	require.Equal(t, "pending", first.ReviewState)
	require.Equal(t, "auto", second.ReviewState)
	require.LessOrEqual(t, len([]rune(second.Snippet)), 160)
	require.NotContains(t, second.Snippet, "\n")
	require.True(t, strings.HasPrefix(second.Snippet, "Prefers quiet mornings and long walks"))
	require.Empty(t, digest.SoulChanges)
	// Default window is seven days.
	require.InDelta(t, (7 * day).Seconds(), digest.Until.Sub(digest.Since.Time).Seconds(), 1)
}

func testLearnedCap(t *testing.T, v env) {
	u := v.user("UTC")
	now := time.Now()
	ids := make([]uuid.UUID, 0, 25)
	for i := range 25 {
		ids = append(ids, v.memory(u, fmt.Sprintf("Preference %d", i), now.Add(-time.Duration(i+1)*10*time.Minute), mem{kind: "preference"}))
	}
	got := decode[me.LearnedDigestResponse](t, v.get(u, "/v1/me/learned")).Memories
	require.Len(t, got, 20)
	for i, m := range got {
		require.Equal(t, ids[i], m.Id.UUID, i)
	}
}

func testLearnedBadSince(t *testing.T, v env) {
	u := v.user("UTC")
	const layout = "2006-01-02T15:04:05.000Z"
	for _, q := range []string{"yesterday", time.Now().Add(day).UTC().Format(layout), time.Now().Add(-91 * day).UTC().Format(layout)} {
		require.Equal(t, http.StatusBadRequest, v.get(u, "/v1/me/learned?since="+q).code, q)
	}
	digest := decode[me.LearnedDigestResponse](t, v.get(u, "/v1/me/learned?since="+time.Now().Add(-89*day).UTC().Format(time.RFC3339)))
	require.Empty(t, digest.Memories)
	require.Empty(t, digest.SoulChanges)
}

func testLearnedToken(t *testing.T, v env) {
	require.Equal(t, http.StatusUnauthorized, v.get(uuid.Nil, "/v1/me/learned").code)
}

// ─── MeRecapControllerTests ────────────────────────────────────────────────

func testRecapToday(t *testing.T, v env) {
	u := v.user("UTC")
	now := time.Now()
	v.memory(u, "Moving the launch to Lisbon", now, mem{})
	decision := v.memory(u, "We ship on Tuesdays.\nBecause QA is Monday.", now, mem{kind: "decision", state: "pending", origin: "knowledge_compile"})
	conv := v.conversation(u, now)
	v.message(conv, "user", now)
	v.message(conv, "assistant", now)

	body := decode[me.MeRecapResponse](t, v.get(u, "/v1/me/recap"))
	todayKey, _ := localDay(0, "UTC")
	startKey, _ := localDay(-6, "UTC")
	require.Equal(t, me.MeRecapResponseRange("7d"), body.Range)
	require.Equal(t, "UTC", body.Timezone)
	require.Len(t, body.Daily, 7)
	require.Equal(t, todayKey, body.End)
	require.Equal(t, startKey, body.Start)
	for i, d := range body.Daily {
		key, _ := localDay(i-6, "UTC")
		require.Equal(t, key, d.Date)
	}
	require.Equal(t, me.RecapDayDTO{Date: todayKey, Captures: 1, Learned: 1, Asked: 1}, body.Daily[6])
	for _, d := range body.Daily[:6] {
		require.Equal(t, me.RecapDayDTO{Date: d.Date}, d)
	}
	require.Equal(t, me.RecapTotalsDTO{Captures: 1, Learned: 1, Asked: 1, ActiveDays: 1, StreakDays: 1}, body.Totals)
	require.Equal(t, me.RecapTotalsDTO{}, body.PreviousTotals)
	require.Equal(t, me.RecapKindsDTO{Decision: 1}, body.Kinds)
	var found bool
	for _, h := range body.Highlights {
		if h.Kind == "decision" {
			found = true
			require.Equal(t, strings.ToUpper(decision.String()), h.RefId)
			require.Equal(t, "We ship on Tuesdays.", h.Title)
		}
	}
	require.True(t, found)
	require.Equal(t, "My last 7 days on LuminaVault: 1 capture, 1 thing learned, 1 question asked, a 1-day streak.", body.ShareText)
}

func testRecapLocalDay(t *testing.T, v env) {
	// UTC+14: 00:30 local is 10:30 the previous UTC day.
	kiri := v.user("Pacific/Kiritimati")
	kiriKey, kiriStart := localDay(-2, "Pacific/Kiritimati")
	v.memory(kiri, "early", kiriStart.Add(30*time.Minute), mem{})
	// UTC-7/-8: 23:30 local is 06:30/07:30 the next UTC day.
	la := v.user("America/Los_Angeles")
	laKey, laStart := localDay(-2, "America/Los_Angeles")
	v.memory(la, "late", laStart.Add(23*time.Hour+30*time.Minute), mem{})

	check := func(u uuid.UUID, zone, key string) {
		body := decode[me.MeRecapResponse](t, v.get(u, "/v1/me/recap"))
		end, _ := localDay(0, zone)
		require.Equal(t, zone, body.Timezone)
		require.Equal(t, end, body.End)
		require.Equal(t, 1, body.Totals.Captures)
		for _, d := range body.Daily {
			if d.Date == key {
				require.Equal(t, 1, d.Captures)
				return
			}
		}
		t.Fatalf("no day %s", key)
	}
	check(kiri, "Pacific/Kiritimati", kiriKey)
	check(la, "America/Los_Angeles", laKey)
}

func testRecap30d(t *testing.T, v env) {
	u := v.user("UTC")
	// Eight days ago: outside this week, inside the week before.
	_, start := localDay(-8, "UTC")
	v.memory(u, "last week", start.Add(12*time.Hour), mem{})

	week := decode[me.MeRecapResponse](t, v.get(u, "/v1/me/recap?range=7d"))
	require.Len(t, week.Daily, 7)
	require.Equal(t, 0, week.Totals.Captures)
	require.Equal(t, 1, week.PreviousTotals.Captures)
	require.Equal(t, 1, week.PreviousTotals.ActiveDays)
	require.Equal(t, "My last 7 days on LuminaVault: 0 captures.", week.ShareText)

	month := decode[me.MeRecapResponse](t, v.get(u, "/v1/me/recap?range=30d"))
	key, _ := localDay(-29, "UTC")
	require.Equal(t, me.MeRecapResponseRange("30d"), month.Range)
	require.Len(t, month.Daily, 30)
	require.Equal(t, key, month.Start)
	require.Equal(t, 1, month.Totals.Captures)
	require.Equal(t, 0, month.PreviousTotals.Captures)
	require.True(t, strings.HasPrefix(month.ShareText, "My last 30 days on LuminaVault: 1 capture"))
}

func testRecapBadRange(t *testing.T, v env) {
	u := v.user("UTC")
	for _, q := range []string{"90d", "week", "1d"} {
		require.Equal(t, http.StatusBadRequest, v.get(u, "/v1/me/recap?range="+q).code, q)
	}
}

// ─── MeResurfaceTests ──────────────────────────────────────────────────────

func resurface(t *testing.T, v env, u uuid.UUID, query string) []me.ResurfaceItemDTO {
	t.Helper()
	return decode[me.ResurfaceResponse](t, v.get(u, "/v1/me/resurface"+query)).Items
}

func testResurfaceLanes(t *testing.T, v env) {
	u, stranger := v.user("UTC"), v.user("UTC")
	now := time.Now()
	old := now.Add(-90 * day)
	due := v.memory(u, "Call the accountant", now.Add(-5*day), mem{lastReviewed: ptr(now.Add(-4 * day)), nextReview: ptr(now.Add(-day))})
	fading := v.memory(u, "We pay suppliers net 30", old, mem{kind: "decision"})
	// Excluded, each for one reason.
	v.memory(u, "rejected", old, mem{kind: "decision", state: "rejected"})
	v.memory(u, "muted", old, mem{kind: "decision", muted: true})
	v.memory(u, "just reviewed", old, mem{kind: "decision", lastReviewed: ptr(now.Add(-time.Hour)), nextReview: ptr(now.Add(-time.Minute))})
	v.memory(u, "scheduled for later", old, mem{kind: "decision", nextReview: ptr(now.Add(10 * day))})
	v.memory(u, "unapproved chat memory", old, mem{kind: "decision", state: "pending", origin: "chat"})
	v.memory(u, "a fact, not fading", old, mem{kind: "fact"})
	v.memory(stranger, "someone else's", old, mem{kind: "decision", nextReview: ptr(now.Add(-day))})

	items := resurface(t, v, u, "?limit=10")
	require.Len(t, items, 2)
	require.Equal(t, due, items[0].MemoryId.UUID)
	require.Equal(t, fading, items[1].MemoryId.UUID)
	require.Equal(t, me.ResurfaceReason("due"), items[0].Reason)
	require.Equal(t, "You asked to see this again", items[0].Why)
	require.Equal(t, me.ResurfaceReason("fading"), items[1].Reason)
	require.Equal(t, "decision", *items[1].Kind)
	require.True(t, strings.HasPrefix(items[1].Why, "A decision you haven't revisited since "), items[1].Why)

	one := resurface(t, v, u, "?limit=1")
	require.Len(t, one, 1)
	require.Equal(t, due, one[0].MemoryId.UUID)
	require.Equal(t, http.StatusBadRequest, v.get(u, "/v1/me/resurface?limit=0").code)
	require.Equal(t, http.StatusBadRequest, v.get(u, "/v1/me/resurface?limit=11").code)
}

func testResurfaceLimit(t *testing.T, v env) {
	u := v.user("UTC")
	now := time.Now()
	for n := range 5 {
		// Due *and* fading: must appear once, as due.
		v.memory(u, fmt.Sprintf("Preference %d", n), now.Add(-time.Duration(100+n)*day), mem{kind: "preference", nextReview: ptr(now.Add(-time.Duration(n+1) * time.Hour))})
	}
	items := resurface(t, v, u, "")
	require.Len(t, items, 3)
	snippets := make([]string, 0, 3)
	for _, i := range items {
		require.Equal(t, me.ResurfaceReason("due"), i.Reason)
		snippets = append(snippets, i.Snippet)
	}
	// Oldest due first.
	require.Equal(t, []string{"Preference 4", "Preference 3", "Preference 2"}, snippets)
}

func testResurfaceTZ(t *testing.T, v env) {
	// 12:00 UTC on 16 Oct is already 01:00 on 17 Oct in Auckland (NZDT, +13).
	v.m.SetClock(func() time.Time { return date(t, "2026-10-16T12:00:00Z") })
	u := v.user("Pacific/Auckland")
	// 00:30 on 17 Oct 2025 in Auckland: a year ago today, locally.
	local := v.memory(u, "Signed the lease", date(t, "2025-10-16T11:30:00Z"), mem{})
	// 18:00 on 16 Oct 2025 in Auckland: a year ago on the UTC day only.
	v.memory(u, "Not today", date(t, "2025-10-16T05:00:00Z"), mem{})

	items := resurface(t, v, u, "?limit=10")
	require.Len(t, items, 1)
	require.Equal(t, local, items[0].MemoryId.UUID)
	require.Equal(t, me.ResurfaceReason("onThisDay"), items[0].Reason)
	require.Equal(t, "Saved a year ago today", items[0].Why)
}

func testResurfaceWhy(t *testing.T, v env) {
	v.m.SetClock(func() time.Time { return date(t, "2026-10-16T12:00:00Z") })
	u := v.user("UTC")
	want := []uuid.UUID{
		v.memory(u, "two years", date(t, "2024-10-16T09:00:00Z"), mem{}),
		v.memory(u, "six months", date(t, "2026-04-16T09:00:00Z"), mem{}),
		v.memory(u, "a month", date(t, "2026-09-16T09:00:00Z"), mem{}),
		v.memory(u, "Aisle seats", date(t, "2026-01-02T09:00:00Z"), mem{kind: "preference", lastReviewed: ptr(date(t, "2026-03-14T09:00:00Z"))}),
	}
	items := resurface(t, v, u, "?limit=10")
	require.Len(t, items, 4)
	whys := make([]string, 0, 4)
	for i, item := range items {
		require.Equal(t, want[i], item.MemoryId.UUID)
		whys = append(whys, item.Why)
	}
	require.Equal(t, []string{
		"Saved 2 years ago today",
		"Saved 6 months ago today",
		"Saved a month ago today",
		"A preference you haven't revisited since March 2026",
	}, whys)
	require.Equal(t, me.ResurfaceReason("fading"), items[3].Reason)
	require.Equal(t, "preference", *items[3].Kind)
}

func testResurfaceSnippet(t *testing.T, v env) {
	v.m.SetClock(func() time.Time { return date(t, "2026-10-16T12:00:00Z") })
	u := v.user("UTC")
	v.memory(u, "First line\nsecond "+strings.Repeat("word ", 80), date(t, "2025-10-16T09:00:00Z"), mem{})
	items := resurface(t, v, u, "")
	require.Len(t, items, 1)
	require.LessOrEqual(t, len([]rune(items[0].Snippet)), 160)
	require.NotContains(t, items[0].Snippet, "\n")
	require.True(t, strings.HasPrefix(items[0].Snippet, "First line second"))
}

// ─── PeriodRecapControllerTests ────────────────────────────────────────────

// seedMarch makes u a Los Angeles user with a busy March 2026, created in 2025.
func seedMarch(t *testing.T, v env, u uuid.UUID) {
	t.Helper()
	v.exec(`UPDATE users SET timezone = 'America/Los_Angeles', created_at = '2025-06-01T00:00:00Z' WHERE id = $1`, u)
	space := uuid.New()
	v.exec(`INSERT INTO spaces (id, tenant_id, name, slug, created_at, updated_at) VALUES ($1, $2, 'Work', $3, now(), now())`, space, u, "work-"+space.String()[:6])
	// 2–4 March, three captures each (PST is UTC-8).
	for d := 2; d <= 4; d++ {
		tags := []string{"swift", "side_project"}
		if d == 2 {
			tags = []string{"Swift"}
		}
		for _, h := range []int{16, 17, 18} {
			v.memory(u, fmt.Sprintf("Note %d-%d\nbody", d, h), time.Date(2026, 3, d, h, 0, 0, 0, time.UTC), mem{tags: tags, space: &space})
		}
	}
	// 23:30 on 31 March in Los Angeles is 1 April in UTC: still March.
	v.memory(u, "Late night", date(t, "2026-04-01T06:30:00Z"), mem{})
	// 23:30 on 28 February in Los Angeles is 1 March in UTC: February.
	v.memory(u, "February", date(t, "2026-03-01T07:30:00Z"), mem{})
	// A distilled memory counts as learned.
	v.memory(u, "Learned", date(t, "2026-03-03T20:00:00Z"), mem{origin: "knowledge_compile"})
	conv := v.conversation(u, date(t, "2026-03-03T18:00:00Z"))
	v.message(conv, "user", date(t, "2026-03-03T18:05:00Z"))
	v.message(conv, "user", date(t, "2026-03-04T18:05:00Z"))
	for range 2 {
		v.exec(`INSERT INTO skill_run_log (id, tenant_id, source, name, started_at, status)
			VALUES ($1, $2, 'cron', 'daily-brief', '2026-03-10T12:00:00Z', 'ok')`, uuid.New(), u)
	}
}

func testPeriodBadParams(t *testing.T, v env) {
	u := v.user("UTC")
	require.Equal(t, http.StatusBadRequest, v.get(u, "/v1/me/recap/period?period=week").code)
	require.Equal(t, http.StatusBadRequest, v.get(u, "/v1/me/recap/period?period=month&month=13").code)
	body := decode[me.PeriodRecapResponse](t, v.get(u, "/v1/me/recap/period?period=year"))
	require.False(t, body.Available)
	require.Equal(t, me.RecapUnavailableReason("notEnoughActivity"), *body.UnavailableReason)
	require.Empty(t, body.Order)
	old := decode[me.PeriodRecapResponse](t, v.get(u, "/v1/me/recap/period?period=year&year=2020"))
	require.Equal(t, me.RecapUnavailableReason("beforeAccount"), *old.UnavailableReason)
}

func themesAttempts(t *testing.T, v env, u uuid.UUID, key string) int {
	t.Helper()
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT attempts FROM recap_themes WHERE user_id = $1 AND period_key = $2`, u, key).Scan(&n))
	return n
}

func testPeriodMonth(t *testing.T, v env) {
	u := v.user("UTC")
	seedMarch(t, v, u)
	const path = "/v1/me/recap/period?period=month&year=2026&month=3"
	r := v.get(u, path)
	body := decode[me.PeriodRecapResponse](t, r)
	require.True(t, body.Available)
	require.Equal(t, "America/Los_Angeles", body.Timezone)
	require.True(t, body.IsComplete)
	require.Equal(t, "2026-03-01", body.Start)
	require.Equal(t, "2026-03-31", body.End)

	c := body.Captures
	require.Equal(t, 10, c.Captures)
	require.Equal(t, 1, c.Learned)
	require.Equal(t, 1, c.PreviousCaptures)
	require.Len(t, c.Series, 31)
	by := map[string]int{}
	for _, b := range c.Series {
		by[b.Key] = b.Count
	}
	require.Equal(t, 1, by["2026-03-31"])
	require.Equal(t, 4, by["2026-03-03"])
	require.Equal(t, "2026-03-02", c.FirstCapture.Date)
	require.Equal(t, "Note 2-16", c.FirstCapture.Title)
	require.Equal(t, me.RecapBucketDTO{Key: "2026-03-03", Count: 4}, body.Peak.Busiest)
	require.Equal(t, 3, body.Streak.LongestDays)
	require.Equal(t, "2026-03-02", *body.Streak.LongestStart)
	require.Equal(t, 4, body.Streak.ActiveDays)
	require.Equal(t, me.RecapCountDTO{Label: "swift", Count: 9}, body.Topics.TopTags[0])
	require.Contains(t, body.Topics.TopTags, me.RecapCountDTO{Label: "side_project", Count: 6})
	require.Equal(t, me.RecapCountDTO{Label: "Work", Count: 9}, *body.Topics.TopSpace)
	require.Equal(t, me.RecapChatsCardDTO{Conversations: 1, MessagesSent: 2}, *body.Chats)
	require.Equal(t, 2, body.Skills.Runs)
	require.Equal(t, me.RecapCountDTO{Label: "daily-brief", Count: 2}, *body.Skills.TopSkill)
	// 08:00–10:00 local, plus 10:05 messages.
	require.Equal(t, me.RecapPersona("earlyBird"), body.Rhythm.Persona)
	require.Contains(t, body.Order, me.RecapCardKind("themes"))
	require.Equal(t, me.RecapThemesStatus("pending"), body.Themes.Status)
	require.Equal(t, "A month of swift and side_project.", body.Themes.FallbackSentence)
	require.Equal(t, "private, no-cache", r.header.Get("Cache-Control"))
	require.Equal(t, "My March 2026 on LuminaVault: 11 memories, 2 questions asked, a 3-day streak.", body.ShareText)

	// A second view does not claim again. This env has no worker, so the row
	// stays in flight.
	again := decode[me.PeriodRecapResponse](t, v.get(u, path))
	require.Equal(t, me.RecapThemesStatus("pending"), again.Themes.Status)
	require.Equal(t, 1, themesAttempts(t, v, u, "2026-03"))
}

// The rows the worker writes are seeded directly here; themes_test.go runs it.
func testPeriodThemesRows(t *testing.T, v env) {
	u := v.user("UTC")
	seedMarch(t, v, u)
	v.exec(`INSERT INTO recap_themes (user_id, period_key, status, headline, sentence, themes, attempts, generated_at)
		VALUES ($1, '2026-03', 'ready', 'Early bird builder', 'You wrote about Swift before breakfast.', '{swift}', 1, now())`, u)
	r := v.get(u, "/v1/me/recap/period?period=month&year=2026&month=3")
	themes := decode[me.PeriodRecapResponse](t, r).Themes
	require.Equal(t, me.RecapThemesStatus("ready"), themes.Status)
	require.Equal(t, "Early bird builder", *themes.Headline)
	require.Equal(t, []string{"swift"}, themes.Themes)
	require.Equal(t, "private, max-age=3600", r.header.Get("Cache-Control"))
	require.Equal(t, 1, themesAttempts(t, v, u, "2026-03"))

	// A write that failed shows the counted sentence and is not retried yet.
	failed := v.user("UTC")
	seedMarch(t, v, failed)
	v.exec(`INSERT INTO recap_themes (user_id, period_key, status, themes, attempts, generated_at)
		VALUES ($1, '2026-03', 'fallback', '{}', 1, now())`, failed)
	themes = decode[me.PeriodRecapResponse](t, v.get(failed, "/v1/me/recap/period?period=month&year=2026&month=3")).Themes
	require.Equal(t, me.RecapThemesStatus("fallback"), themes.Status)
	require.Equal(t, "A month of swift and side_project.", themes.FallbackSentence)
	require.Equal(t, 1, themesAttempts(t, v, failed, "2026-03"))
}

func testPeriodDisabled(t *testing.T, v env) {
	off := newEnv(t, v.pool, me.Options{RecapThemesEnabled: false})
	u := off.user("UTC")
	seedMarch(t, off, u)
	body := decode[me.PeriodRecapResponse](t, off.get(u, "/v1/me/recap/period?period=month&year=2026&month=3"))
	require.Equal(t, me.RecapThemesStatus("disabled"), body.Themes.Status)
	var n int
	require.NoError(t, off.pool.QueryRow(context.Background(), `SELECT count(*) FROM recap_themes WHERE user_id = $1`, u).Scan(&n))
	require.Zero(t, n, "disabled themes claim nothing")
}

// ─── Go-only ───────────────────────────────────────────────────────────────

func testLocation(t *testing.T, v env) {
	u := v.user("UTC")
	require.JSONEq(t, `{"cached":false}`, string(v.get(u, "/v1/me/location").body))

	v.exec(`UPDATE users SET last_location_lat = 38.5, last_location_lng = -9.25, last_location_place = 'Lisbon', last_location_at = '2026-10-30T07:00:00Z' WHERE id = $1`, u)
	require.JSONEq(t, `{"cached":true,"place":"Lisbon","latitude":38.5,"longitude":-9.25,"capturedAt":"2026-10-30T07:00:00Z"}`, string(v.get(u, "/v1/me/location").body))

	v.exec(`UPDATE users SET last_location_place = NULL, last_location_at = NULL WHERE id = $1`, u)
	require.JSONEq(t, `{"cached":true,"latitude":38.5,"longitude":-9.25,"capturedAt":"1970-01-01T00:00:00Z"}`, string(v.get(u, "/v1/me/location").body))

	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodDelete, "/v1/me/location", "").code)
	require.JSONEq(t, `{"cached":false}`, string(v.get(u, "/v1/me/location").body))
}

// The chain is identify → meTodayByUser (60/60 s) → requireIdentity, so the
// bucket fills without a token, is shared by every limited route, and the
// JWT-only apns routes stay outside it.
func testSharedRateLimit(t *testing.T, v env) {
	for i := range 60 {
		require.Equal(t, http.StatusUnauthorized, v.get(uuid.Nil, "/v1/me/today").code, i)
	}
	for _, path := range []string{"/v1/me/location", "/v1/me/recap", "/v1/me/recap/period", "/v1/me/brief", "/v1/me/commitments", "/v1/me/learned", "/v1/me/resurface"} {
		require.Equal(t, http.StatusTooManyRequests, v.get(uuid.Nil, path).code, path)
	}
	require.Equal(t, http.StatusUnauthorized, v.get(uuid.Nil, "/v1/me/apns-categories").code)
	require.Equal(t, http.StatusOK, v.get(v.user("UTC"), "/v1/me/location").code, "users have their own bucket")
}

func specRouterAndValidate(t *testing.T) func(method, path string, r resp) error {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	return func(method, path string, r resp) error {
		req := httptest.NewRequest(method, path, nil)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, path)
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			},
			Status: r.code, Header: http.Header{"Content-Type": {r.header.Get("Content-Type")}},
		}
		out.SetBodyBytes(r.body)
		return openapi3filter.ValidateResponse(context.Background(), out)
	}
}

var (
	seedAnn = uuid.MustParse("AE000000-0000-4000-8000-0000000000A1")
	seedPer = uuid.MustParse("AE000000-0000-4000-8000-0000000000E5")
	seedCmt = uuid.MustParse("AE000000-0000-4000-8000-0000000000F6")
)

func testSpecConformance(t *testing.T, v env) {
	validate := specRouterAndValidate(t)
	u := v.user("UTC")
	// Known kinds only: the "unknown" Swift sends for a future kind is outside
	// the spec's enums (the seeded brief has one on purpose; see the scenarios).
	today, _ := localDay(0, "UTC")
	v.brief(u, today, `[{"id":"weather","kind":"weather","title":"Weather","body":"Dry.","why":"For planning your day","refs":[{"kind":"memory","id":"x"}]}]`)
	for _, c := range []struct {
		user         uuid.UUID
		method, path string
		body         string
	}{
		{u, http.MethodGet, "/v1/me/brief", ""},
		{v.user("UTC"), http.MethodGet, "/v1/me/brief", ""},
		{seedAnn, http.MethodGet, "/v1/me/recap", ""},
		{seedPer, http.MethodGet, "/v1/me/recap/period?period=month&year=2026&month=3", ""},
		{seedPer, http.MethodGet, "/v1/me/recap/period?period=month&year=2026&month=4", ""},
		{seedAnn, http.MethodGet, "/v1/me/resurface?limit=10", ""},
		{seedAnn, http.MethodGet, "/v1/me/learned", ""},
		{seedCmt, http.MethodGet, "/v1/me/commitments?state=all", ""},
		{seedCmt, http.MethodPatch, "/v1/me/commitments/AE0C0331-0000-4000-8000-0000000000A4", `{"state":"dropped"}`},
		{seedCmt, http.MethodPatch, "/v1/me/commitments/00000000-0000-4000-8000-000000000000", `{"state":"done"}`},
		{seedAnn, http.MethodGet, "/v1/me/location", ""},
		{u, http.MethodGet, "/v1/me/location", ""},
		{u, http.MethodDelete, "/v1/me/location", ""},
		{u, http.MethodGet, "/v1/me/apns-categories", ""},
		{u, http.MethodPut, "/v1/me/apns-categories", `{"chatEnabled":false}`},
		{u, http.MethodGet, "/v1/me/commitments?state=bogus", ""},
	} {
		r := v.do(c.user, c.method, c.path, c.body)
		require.NoError(t, validate(c.method, strings.Split(c.path, "?")[0], r), "%s %s -> %d %s", c.method, c.path, r.code, r.body)
	}
}

// The spec's MeTodayResponse (date/memories/achievements/...) has no required
// properties, so any object validates and says nothing about what Swift sends;
// the response is checked against Swift's shape above instead. This canary
// fails once the vendored spec describes the real keys.
func testSpecTodayStale(t *testing.T, _ env) {
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	schema := doc.Components.Schemas["MeTodayResponse"].Value
	require.Nil(t, schema.Properties["generated_at"], "the spec was fixed: check /v1/me/today against it")
	require.NotNil(t, schema.Properties["soulExcerpt"], "the spec was fixed: check /v1/me/today against it")
}

// TestParityScenariosRunOnGo replays parity/scenarios/me against Go alone on
// a fresh database: no step may fail or return a 5xx. Set ME_TRANSCRIPT to a
// file path to keep the transcript for review.
func TestParityScenariosRunOnGo(t *testing.T) {
	ctx := context.Background()
	pool := startDB(t)
	v := setup(t, pool)
	srv := httptest.NewServer(v.e)
	t.Cleanup(srv.Close)

	rawUsers, err := os.ReadFile("../../../parity/seed/users.yaml")
	require.NoError(t, err)
	var names map[string]string
	require.NoError(t, yaml.Unmarshal(rawUsers, &names))
	users := map[string]uuid.UUID{}
	for k, id := range names {
		users[k] = uuid.MustParse(id)
	}
	scs, err := scenario.Load("../../../parity/scenarios/me")
	require.NoError(t, err)
	require.NotEmpty(t, scs)
	target := scenario.Target{Name: "go", BaseURL: srv.URL, Keys: keys, Users: users, Snapshot: scenario.PGSnapshotter(pool)}
	var all []scenario.Transcript
	for _, sc := range scs {
		tr, err := scenario.Run(ctx, sc, target)
		require.NoError(t, err, sc.File)
		for _, st := range tr.Steps {
			require.Less(t, st.Status, 500, "%s/%s", sc.File, st.Name)
		}
		all = append(all, tr)
	}
	if out := os.Getenv("ME_TRANSCRIPT"); out != "" {
		raw, err := json.MarshalIndent(all, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(out, raw, 0o600))
	}
}
