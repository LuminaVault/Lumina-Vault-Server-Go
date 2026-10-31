package health

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
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
)

var (
	hal  = uuid.MustParse("4EA10000-0000-4000-8000-0000000000A1")
	bea  = uuid.MustParse("4EA10000-0000-4000-8000-0000000000B2")
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	// A fixed clock mid-day, so "today" and "yesterday" never straddle midnight.
	clock = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setupWith(t *testing.T, gate func(*pgxpool.Pool) echo.MiddlewareFunc) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at) VALUES
		($1, 'hal@test', 'x', 'hal', now(), now()), ($2, 'bea@test', 'x', 'bea', now(), now())`, hal, bea)
	require.NoError(t, err)
	var g echo.MiddlewareFunc
	if gate != nil {
		g = gate(pool)
	}
	m := New(pool, g)
	m.now = func() time.Time { return clock }
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	return env{t: t, e: e, pool: pool}
}

func setup(t *testing.T) env { return setupWith(t, nil) }

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
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
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

type sample struct {
	typ   string
	at    time.Time
	value float64
}

func (v env) ingest(user uuid.UUID, samples ...sample) ingestResponseWire {
	v.t.Helper()
	events := make([]map[string]any, 0, len(samples))
	for _, s := range samples {
		events = append(events, map[string]any{"type": s.typ, "recordedAt": s.at.Format(time.RFC3339Nano), "valueNumeric": s.value, "unit": "count"})
	}
	b, _ := json.Marshal(map[string]any{"events": events})
	r := v.do(user, http.MethodPost, "/v1/health", string(b))
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out ingestResponseWire
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out
}

type ingestResponseWire struct {
	Inserted, Skipped int
	Events            []struct{ ID, Type, RecordedAt string }
}

type listWire struct {
	Events []struct {
		ID, Type, RecordedAt string
		ValueNumeric         *float64
	}
	Limit, Offset int
}

func (v env) list(user uuid.UUID, query string) listWire {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/health"+query, "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out listWire
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out
}

type dailyWire struct {
	Type string
	Days []struct {
		Date        string
		Type        string
		Value       float64
		SampleCount int
	}
}

func (v env) daily(user uuid.UUID, query string) dailyWire {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/health/daily"+query, "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out dailyWire
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out
}

func descending(l listWire) bool {
	for i := 1; i < len(l.Events); i++ {
		if l.Events[i-1].RecordedAt < l.Events[i].RecordedAt {
			return false
		}
	}
	return true
}

// Swift HealthReadTests "lists most recent events within default 7 day window".
func TestListDefaultWindow(t *testing.T) {
	v := setup(t)
	v.ingest(hal, sample{"steps", clock, 1200}, sample{"steps", clock.Add(-time.Hour), 800}, sample{"steps", clock.Add(-3 * 24 * time.Hour), 4000},
		sample{"steps", clock.Add(-8 * 24 * time.Hour), 1})
	l := v.list(hal, "")
	// [from, to) with to = now: the sample at exactly now is excluded.
	require.Len(t, l.Events, 2)
	require.True(t, descending(l))
	require.Equal(t, 100, l.Limit)
	require.Equal(t, 0, l.Offset)
}

// Swift "filters by type".
func TestListFiltersByType(t *testing.T) {
	v := setup(t)
	v.ingest(hal, sample{"steps", clock.Add(-time.Minute), 1200}, sample{"hr_bpm", clock.Add(-2 * time.Minute), 72})
	for _, q := range []string{"?type=steps", "?type=%20STEPS%20", "?type=steps&type=hr_bpm"} {
		l := v.list(hal, q)
		require.Len(t, l.Events, 1, q)
		require.Equal(t, "steps", l.Events[0].Type)
	}
	require.Len(t, v.list(hal, "?type=%20").Events, 2, "a blank type is no filter")
}

// Swift "applies from to window".
func TestListAppliesFromTo(t *testing.T) {
	v := setup(t)
	thirty := clock.Add(-30 * 24 * time.Hour)
	v.ingest(hal, sample{"weight_kg", thirty, 80})
	require.Empty(t, v.list(hal, "?type=weight_kg").Events)
	from := clock.Add(-40 * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z07:00")
	l := v.list(hal, "?type=weight_kg&from="+from+"&to="+clock.Format(time.RFC3339))
	require.Len(t, l.Events, 1)
	require.Equal(t, 80.0, *l.Events[0].ValueNumeric)
	// An unencoded "+" offset is an offset, not a space (Hummingbird query parsing).
	off := thirty.In(time.FixedZone("", 2*3600)).Format(time.RFC3339)
	require.Contains(t, off, "+02:00")
	require.Len(t, v.list(hal, "?from="+off+"&to="+clock.Format(time.RFC3339)).Events, 1)
	// Unparseable bounds fall back to the default window.
	require.Empty(t, v.list(hal, "?type=weight_kg&from=yesterday").Events)
}

// Swift "clamps limit to bounds", plus the Int(String) fallbacks.
func TestListClampsLimit(t *testing.T) {
	v := setup(t)
	for q, want := range map[string]int{"?limit=10000": 200, "?limit=0": 1, "?limit=-5": 1, "?limit=abc": 100, "?limit=+2": 2, "?limit=": 100} {
		require.Equal(t, want, v.list(hal, q).Limit, q)
	}
	for q, want := range map[string]int{"?offset=-3": 0, "?offset=abc": 0, "?offset=4": 4} {
		require.Equal(t, want, v.list(hal, q).Offset, q)
	}
}

// Swift "offset paginates stably".
func TestListOffsetPaginates(t *testing.T) {
	v := setup(t)
	var samples []sample
	for i := range 10 {
		samples = append(samples, sample{"steps", clock.Add(-time.Duration(i+1) * time.Minute), float64(1000 + i)})
	}
	v.ingest(hal, samples...)
	p1, p2 := v.list(hal, "?type=steps&limit=4&offset=0"), v.list(hal, "?type=steps&limit=4&offset=4")
	require.Len(t, p1.Events, 4)
	require.Len(t, p2.Events, 4)
	require.Equal(t, 4, p2.Offset)
	seen := map[string]bool{}
	for _, e := range append(p1.Events, p2.Events...) {
		require.False(t, seen[e.ID])
		seen[e.ID] = true
	}
	require.True(t, descending(p1) && descending(p2))
}

func dayAt(n int, hour int) time.Time {
	return time.Date(clock.Year(), clock.Month(), clock.Day()-n, hour, 0, 0, 0, time.UTC)
}

// Swift HealthDailyAggregateTests "steps aggregation sums daily values".
func TestDailySums(t *testing.T) {
	v := setup(t)
	v.ingest(hal, sample{"steps", dayAt(0, 11), 1200}, sample{"steps", dayAt(0, 10), 800}, sample{"steps", dayAt(0, 9), 400},
		sample{"steps", dayAt(1, 11), 5000}, sample{"steps", dayAt(1, 10), 3000})
	d := v.daily(hal, "?type=steps")
	require.Equal(t, "steps", d.Type)
	require.Len(t, d.Days, 7)
	today, yesterday := d.Days[6], d.Days[5]
	require.Equal(t, 2400.0, today.Value)
	require.Equal(t, 3, today.SampleCount)
	require.Equal(t, 8000.0, yesterday.Value)
	require.Equal(t, 2, yesterday.SampleCount)
	require.Equal(t, dayAt(0, 0).Format(time.RFC3339), today.Date)
	require.Equal(t, dayAt(6, 0).Format(time.RFC3339), d.Days[0].Date)
}

// Swift "hr aggregation averages daily values".
func TestDailyAverages(t *testing.T) {
	v := setup(t)
	v.ingest(hal, sample{"hr_bpm", dayAt(0, 11), 60}, sample{"hr_bpm", dayAt(0, 10), 80}, sample{"hr_bpm", dayAt(0, 9), 100})
	today := v.daily(hal, "?type=hr_bpm").Days[6]
	require.InDelta(t, 80, today.Value, 0.001)
	require.Equal(t, 3, today.SampleCount)
}

// Swift "gap days return zero value and zero sample count".
func TestDailyGapDays(t *testing.T) {
	v := setup(t)
	v.ingest(hal, sample{"steps", dayAt(0, 11), 500})
	d := v.daily(hal, "?type=steps")
	require.Len(t, d.Days, 7)
	nonZero := 0
	for _, x := range d.Days {
		if x.SampleCount > 0 {
			nonZero++
		} else {
			require.Zero(t, x.Value)
			require.Equal(t, "steps", x.Type)
		}
	}
	require.Equal(t, 1, nonZero)
}

// Swift "custom days parameter returns matching window length".
func TestDailyCustomDays(t *testing.T) {
	v := setup(t)
	for q, n := range map[string]int{"?type=steps&days=14": 14, "?type=steps&days=1": 1, "?type=steps&days=90": 90, "?type=steps&days=abc": 7, "?type=steps&days=+3": 3} {
		require.Len(t, v.daily(hal, q).Days, n, q)
	}
}

// Swift "days out of range returns 400" and "missing type query parameter returns 400".
func TestDailyErrors(t *testing.T) {
	v := setup(t)
	for q, msg := range map[string]string{
		"?type=steps&days=0":  "days must be between 1 and 90",
		"?type=steps&days=91": "days must be between 1 and 90",
		"?type=steps&days=-1": "days must be between 1 and 90",
		"":                    "type query parameter required",
		"?type=":              "type query parameter required",
		"?type=%20%09":        "type query parameter required",
		"?type":               "type query parameter required",
		"?days=0":             "type query parameter required",
	} {
		r := v.do(hal, http.MethodGet, "/v1/health/daily"+q, "")
		require.Equal(t, http.StatusBadRequest, r.code, q)
		require.JSONEq(t, `{"error":{"message":"`+msg+`"}}`, string(r.body), q)
	}
}

// Swift "tenant isolation other-user samples not returned".
func TestDailyTenantIsolation(t *testing.T) {
	v := setup(t)
	v.ingest(hal, sample{"steps", dayAt(0, 11), 9999})
	for _, d := range v.daily(bea, "?type=steps").Days {
		require.Zero(t, d.Value)
		require.Zero(t, d.SampleCount)
	}
	require.Empty(t, v.list(bea, "?from=2000-01-01T00:00:00Z&to=2100-01-01T00:00:00Z").Events)
}

func TestIngestValidatesAndSkips(t *testing.T) {
	v := setup(t)
	r := v.do(bea, http.MethodPost, "/v1/health", `{"events":[
		{"type":"  Steps ","recordedAt":"2026-10-01T10:00:00.5Z","valueNumeric":100,"metadata":{"device":"watch"}},
		{"type":"sleep_quality","recordedAt":"2026-10-01T09:00:00+02:00","valueText":"deep"},
		{"type":"   ","recordedAt":"2026-10-01T11:00:00Z","valueNumeric":1},
		{"type":"steps","recordedAt":"2026-10-01T12:00:00Z","unit":"count"},
		{"type":"`+strings.Repeat("b", 65)+`","recordedAt":"2026-10-01T13:00:00Z","valueNumeric":1},
		{"type":"`+strings.Repeat("a", 64)+`","recordedAt":"2026-10-01T14:00:00Z","valueNumeric":1},
		{"type":"ÉTAPES","recordedAt":"2026-10-01T16:00:00Z","valueNumeric":-2.25,"metadata":{}}]}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var out ingestResponseWire
	require.NoError(t, json.Unmarshal(r.body, &out))
	require.Equal(t, 4, out.Inserted)
	require.Equal(t, 3, out.Skipped)
	require.Equal(t, "steps", out.Events[0].Type)
	require.Equal(t, "2026-10-01T10:00:00Z", out.Events[0].RecordedAt)
	require.Equal(t, "étapes", out.Events[3].Type)
	require.Equal(t, strings.ToUpper(out.Events[0].ID), out.Events[0].ID)

	var md, empty *string
	var at time.Time
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT metadata::text, recorded_at FROM health_events WHERE tenant_id = $1 AND event_type = 'steps'`, bea).Scan(&md, &at))
	require.JSONEq(t, `{"device":"watch"}`, *md)
	require.Equal(t, time.Date(2026, 10, 1, 10, 0, 0, 5e8, time.UTC), at.UTC())
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT metadata::text FROM health_events WHERE tenant_id = $1 AND event_type = 'sleep_quality'`, bea).Scan(&empty))
	require.Nil(t, empty)
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT metadata::text FROM health_events WHERE tenant_id = $1 AND event_type = 'étapes'`, bea).Scan(&md))
	require.Equal(t, "{}", *md)

	// A batch that skips everything inserts nothing and still answers 200.
	r = v.do(bea, http.MethodPost, "/v1/health", `{"events":[{"type":" ","recordedAt":"2026-10-02T10:00:00Z","valueNumeric":1}]}`)
	require.JSONEq(t, `{"inserted":0,"skipped":1,"events":[]}`, string(r.body))
}

func TestIngestErrors(t *testing.T) {
	v := setup(t)
	notJSON := "The given data was not valid input: The given data was not valid JSON."
	for body, msg := range map[string]string{
		``:                notJSON,
		`{"events":`:      notJSON,
		`[]`:              "Type mismatch for `` key, expected `Dictionary<String, Any>` type.",
		`{}`:              "Coding key `events` not found.",
		`{"events":null}`: "Value not found for `events` key.",
		`{"events":"x"}`:  "Type mismatch for `events` key, expected `Array<Any>` type.",
		`{"events":[]}`:   "events array required",
		`{"events":[{"recordedAt":"2026-10-01T10:00:00Z"}]}`:                               "Coding key `type` not found.",
		`{"events":[{"type":"steps"}]}`:                                                    "Coding key `recordedAt` not found.",
		`{"events":[{"type":5,"recordedAt":"2026-10-01T10:00:00Z"}]}`:                      "Type mismatch for `events.0.type` key, expected `String` type.",
		`{"events":[{"type":"s","recordedAt":"2026-10-01T10:00:00Z","valueNumeric":"1"}]}`: "Type mismatch for `events.0.valueNumeric` key, expected `Double` type.",
		`{"events":[{"type":"s","recordedAt":"2026-10-01T10:00:00Z","metadata":{"a":1}}]}`: "Type mismatch for `events.0.metadata.a` key, expected `String` type.",
	} {
		r := v.do(bea, http.MethodPost, "/v1/health", body)
		require.Equal(t, http.StatusBadRequest, r.code, body)
		b, _ := json.Marshal(map[string]map[string]string{"error": {"message": msg}})
		require.JSONEq(t, string(b), string(r.body), body)
	}
	ev := `{"type":"steps","recordedAt":"2026-10-03T10:00:00Z","valueNumeric":1}`
	r := v.do(bea, http.MethodPost, "/v1/health", `{"events":[`+strings.Repeat(ev+",", 1000)+ev+`]}`)
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code)
	require.JSONEq(t, `{"error":{"message":"batch exceeds 1000 events"}}`, string(r.body))
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		r := v.do(uuid.Nil, m, "/v1/health", `nope`)
		require.Equal(t, http.StatusUnauthorized, r.code)
	}
}

// EntitlementMiddleware(.healthIngest) with enforcement on: trial, pro and
// ultimate ingest; free and lapsed get the 402 paywall unless they hold a
// spendable key; reads are never gated.
func TestIngestPaywall(t *testing.T) {
	hasKey := map[uuid.UUID]bool{}
	v := setupWith(t, func(pool *pgxpool.Pool) echo.MiddlewareFunc {
		return entitlement.Middleware(pool, entitlement.HealthIngest, true, func(_ context.Context, id uuid.UUID) bool { return hasKey[id] })
	})
	body := `{"events":[{"type":"steps","recordedAt":"2026-10-01T10:00:00Z","valueNumeric":1}]}`
	for _, c := range []struct {
		tier, override string
		key            bool
		want           int
	}{
		{"trial", "none", false, 200},
		{"pro", "none", false, 200},
		{"ultimate", "none", false, 200},
		{"free", "none", false, 402},
		{"lapsed", "none", false, 402},
		{"archived", "none", false, 402},
		{"free", "pro", false, 200},
		{"archived", "ultimate", false, 200},
		{"free", "none", true, 200},
		{"lapsed", "none", true, 200},
		{"archived", "none", true, 402},
	} {
		_, err := v.pool.Exec(context.Background(), `UPDATE users SET tier = $2, tier_override = $3 WHERE id = $1`, bea, c.tier, c.override)
		require.NoError(t, err)
		hasKey[bea] = c.key
		r := v.do(bea, http.MethodPost, "/v1/health", body)
		require.Equal(t, c.want, r.code, fmt.Sprintf("%+v", c))
		if c.want == http.StatusPaymentRequired {
			require.Equal(t, `{"paywall":true,"paywallId":"default","requiredTier":"pro"}`, string(r.body))
			require.Equal(t, "application/json; charset=utf-8", r.ct)
		}
		require.Equal(t, http.StatusOK, v.do(bea, http.MethodGet, "/v1/health", "").code)
	}
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, "/v1/health", body).code)
}

func TestQueryParam(t *testing.T) {
	for _, c := range []struct {
		raw, key, want string
		ok             bool
	}{
		{"a=1&b=2", "b", "2", true},
		{"a=1&a=2", "a", "1", true},
		{"a", "a", "", true},
		{"a=", "a", "", true},
		{"a=x+y", "a", "x+y", true},
		{"a=x%2By%20z", "a", "x+y z", true},
		{"a=100%", "a", "100%", true},
		{"a=%zz", "a", "%zz", true},
		{"%61=1", "a", "", false},
		{"", "a", "", false},
		{"&&a=3", "a", "3", true},
		{"b=1=2", "b", "1=2", true},
	} {
		got, ok := queryParam(c.raw, c.key)
		require.Equal(t, c.ok, ok, c.raw)
		require.Equal(t, c.want, got, c.raw)
	}
	require.Equal(t, "steps", trimWhitespaces("\t\u00a0 steps\u200b "))
	require.Equal(t, "\nsteps", trimWhitespaces("\nsteps"))
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	v.ingest(hal, sample{"steps", dayAt(0, 11), 1})
	// The spec's HealthEventDTO (eventType, metadata) and HealthIngestResponse
	// ({ingested}) do not describe what Swift sends (type; inserted, skipped,
	// events), so only the daily route and an empty list are validated.
	for _, path := range []string{"/v1/health/daily?type=steps&days=3", "/v1/health?type=nope"} {
		r := v.do(hal, http.MethodGet, path, "")
		req := httptest.NewRequest(http.MethodGet, path, nil)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: r.code, Header: http.Header{"Content-Type": {r.ct}}}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s: %s", path, r.body)
	}
}

// parity/seed/health.sql loads on the migrated schema and dates its relative
// rows where the scenarios expect them.
func TestParitySeedLoads(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "health.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	var recent int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM health_events WHERE tenant_id = $1 AND recorded_at >= now() - interval '7 days'`, hal).Scan(&recent))
	require.Equal(t, 6, recent)
}
