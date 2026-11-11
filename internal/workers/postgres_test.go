package workers

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/dbtest"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/calendar"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/secretbox"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One container for the package's database tests; every subtest makes its own
// tenants. The name is J1's so a later group's file in this package can't collide.
func TestJ1JobsAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	for name, fn := range map[string]func(*testing.T, *pgxpool.Pool){
		// Tests/AppTests/Telemetry/RetrievalLeakReportWorkerTests.swift.
		"LeakReportAggregatesOncePerWindow":     testLeakReportAggregates,
		"LeakReportSkipsTenantWithoutTelemetry": testLeakReportEmpty,
		"LeakTickWrongWeekdayIsNoOp":            testLeakTickGate,
		"LeakReportSurfacesInsight":             testLeakReportInsight,
		// Tests/AppTests/Skills/ReminderSchedulerTests.swift.
		"ReminderOneShotFiresAndIsStamped":   testReminderOneShot,
		"ReminderRecurringAdvancesAndRearms": testReminderRecurring,
		"ReminderSkipsFiredAndFuture":        testReminderSkips,
		// Tests/AppTests/Telemetry/RetrievalTelemetryWorkerTests.swift and
		// RetrievalTelemetrySampleTests.swift, against Go's direct insert
		// (memoryagent.PGTelemetry), which stands in for the worker's inbox.
		"TelemetrySamplesArePersisted":    testTelemetryPersisted,
		"TelemetryRecordNeverFails":       testTelemetryNeverFails,
		"TelemetrySampleMinTopAndMean":    testTelemetrySampleMinMean,
		"TelemetryEmptyHitsAreZeroHit":    testTelemetryZeroHit,
		"TelemetryNonEmptyHitsCarrySpace": testTelemetryCarriesSpace,
		// Tests/AppTests/Telemetry/SpaceTopologyTests.swift (memorystore.SpaceTopology).
		"SpaceTopologySortsAndResolvesSlug": testSpaceTopologySorts,
		"SpaceTopologyIsTenantScoped":       testSpaceTopologyScoped,
		// Go only: the rest of the jobs' database effects.
		"GraphLayoutPersistsCoordinates":     testGraphLayoutPersists,
		"AnalyticsMaintenanceDeletesExpired": testAnalyticsMaintenance,
		"CalendarSyncCountsConnectedRows":    testCalendarSyncCounts,
	} {
		t.Run(name, func(t *testing.T) { fn(t, pool) })
	}
}

func newTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at) VALUES ($1, $2, 'x', true, $3, NOW(), NOW())`,
		id, id.String()+"@test.luminavault", "u"+id.String()[:8])
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', NOW(), NOW())`, id)
	require.NoError(t, err)
	return id
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// seedTelemetry is RetrievalTelemetryEvent(.from(...)).create, created now.
func seedTelemetry(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, source string, distances []float64) {
	t.Helper()
	var top, mean *float64
	if len(distances) > 0 {
		lo, sum := distances[0], 0.0
		for _, d := range distances {
			lo, sum = math.Min(lo, d), sum+d
		}
		avg := sum / float64(len(distances))
		top, mean = &lo, &avg
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO retrieval_telemetry_events (tenant_id, source_path, hit_count, zero_hit, top_distance, mean_distance, limit_requested)
		VALUES ($1, $2, $3, $4, $5, $6, 5)`, tenant, source, len(distances), len(distances) == 0, top, mean)
	require.NoError(t, err)
}

// nowWindow brackets rows just inserted with created_at = NOW().
func nowWindow() LeakPeriod {
	now := time.Now()
	return LeakPeriod{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

// Swift: "report aggregates zero-hit rate and mean, once per window".
func testLeakReportAggregates(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	// 4 retrievals: 1 zero-hit, 3 with hits (top distances .2/.4/.6).
	seedTelemetry(t, pool, tenant, "local_reply", nil)
	seedTelemetry(t, pool, tenant, "local_reply", []float64{0.2})
	seedTelemetry(t, pool, tenant, "query", []float64{0.4})
	seedTelemetry(t, pool, tenant, "query", []float64{0.6})

	w := &RetrievalLeakReportWorker{Pool: pool, Logger: quiet()}
	window := nowWindow()
	ok, err := w.RunLeakJob(ctx, tenant, window)
	require.NoError(t, err)
	assert.True(t, ok)

	var total, zeros int
	var rate float64
	var meanTop *float64
	var worst *string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT total_retrievals, zero_hit_count, zero_hit_rate, mean_top_distance, worst_source
		FROM retrieval_leak_reports WHERE tenant_id = $1`, tenant).Scan(&total, &zeros, &rate, &meanTop, &worst))
	assert.Equal(t, 4, total)
	assert.Equal(t, 1, zeros)
	assert.InDelta(t, 0.25, rate, 1e-9)
	require.NotNil(t, meanTop)
	assert.InDelta(t, 0.4, *meanTop, 1e-6)
	require.NotNil(t, worst)
	assert.Equal(t, "local_reply", *worst, "half of local_reply found nothing")

	// Idempotent: a second run in the same window inserts nothing.
	ok, err = w.RunLeakJob(ctx, tenant, window)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM retrieval_leak_reports WHERE tenant_id = $1`, tenant))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM insights WHERE tenant_id = $1`, tenant), "surfaceInsight defaults off")
}

// Swift: "tenant with no telemetry produces no report".
func testLeakReportEmpty(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	w := &RetrievalLeakReportWorker{Pool: pool, Logger: quiet()}
	ok, err := w.RunLeakJob(context.Background(), tenant, nowWindow())
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM retrieval_leak_reports WHERE tenant_id = $1`, tenant))
}

// Swift: "tick only fires at Sunday 05 UTC": a Monday 05:00 tick writes nothing.
func testLeakTickGate(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	seedTelemetry(t, pool, tenant, "query", nil)
	w := &RetrievalLeakReportWorker{Pool: pool, Logger: quiet()}
	n, err := w.Tick(context.Background(), time.Unix(4*86400+5*3600, 0))
	assert.Zero(t, n)
	require.ErrorIs(t, err, errNotDue)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM retrieval_leak_reports WHERE tenant_id = $1`, tenant))
}

// Go only: retrieval.leakReport.surfaceInsight writes the patterns insight.
func testLeakReportInsight(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	seedTelemetry(t, pool, tenant, "query", nil)
	seedTelemetry(t, pool, tenant, "query", []float64{0.3})
	w := &RetrievalLeakReportWorker{Pool: pool, SurfaceInsight: true, Logger: quiet()}
	ok, err := w.RunLeakJob(ctx, tenant, nowWindow())
	require.NoError(t, err)
	require.True(t, ok)
	var section, headline, summary string
	require.NoError(t, pool.QueryRow(ctx, `SELECT section, headline, summary FROM insights WHERE tenant_id = $1`, tenant).
		Scan(&section, &headline, &summary))
	assert.Equal(t, "patterns", section)
	assert.Equal(t, "Retrieval health this week", headline)
	assert.Equal(t, "2 retrievals; 50% found nothing; avg best match 0.30; weakest path: query.", summary)
}

// recordingSender is RecordingPushSender.
type recordingSender struct {
	mu   sync.Mutex
	sent []apns.Delivery
}

func (s *recordingSender) Send(_ context.Context, d apns.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, d)
	return nil
}

func (s *recordingSender) sends() []apns.Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]apns.Delivery(nil), s.sent...)
}

func scheduler(pool *pgxpool.Pool, sender *recordingSender) *ReminderScheduler {
	return &ReminderScheduler{Pool: pool, Push: apns.NewWithSender(pool, "com.luminavault.test", sender), Logger: quiet()}
}

func addDeviceToken(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO device_tokens (id, tenant_id, token, platform, created_at, updated_at)
		VALUES ($1, $2, $3, 'ios', now(), now())`, uuid.New(), tenant, uuid.NewString())
	require.NoError(t, err)
}

func addReminder(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, title, body string, fireAt time.Time, cron *string, firedAt *time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `INSERT INTO reminders (id, tenant_id, title, body, fire_at, recurrence_cron, fired_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, tenant, title, body, fireAt, cron, firedAt)
	require.NoError(t, err)
	return id
}

func reminderState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (fireAt time.Time, firedAt *time.Time) {
	t.Helper()
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT fire_at, fired_at FROM reminders WHERE id = $1`, id).Scan(&fireAt, &firedAt))
	return fireAt, firedAt
}

// Swift: "due one-shot reminder fires and is stamped".
func testReminderOneShot(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	addDeviceToken(t, pool, tenant)
	id := addReminder(t, pool, tenant, "Drink water", "Stay hydrated", time.Now().Add(-2*time.Minute), nil, nil)

	sender := &recordingSender{}
	fired, err := scheduler(pool, sender).Tick(context.Background(), time.Now())
	require.NoError(t, err)

	// Tick fires every tenant's due reminders, so count only this one's pushes.
	assert.GreaterOrEqual(t, fired, 1)
	var mine []apns.Delivery
	for _, d := range sender.sends() {
		if d.Message.Title == "Drink water" {
			mine = append(mine, d)
		}
	}
	require.Len(t, mine, 1)
	assert.Equal(t, apns.CategoryReminder, mine[0].Message.Category)
	assert.Equal(t, "Stay hydrated", mine[0].Message.Body)
	_, firedAt := reminderState(t, pool, id)
	assert.NotNil(t, firedAt)
}

// Swift: "recurring reminder advances fireAt and stays armed".
func testReminderRecurring(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	addDeviceToken(t, pool, tenant)
	every := "* * * * *" // every minute: the next match is imminent
	id := addReminder(t, pool, tenant, "Daily standup", "9am", time.Now().Add(-2*time.Minute), &every, nil)

	now := time.Now()
	_, err := scheduler(pool, &recordingSender{}).Tick(context.Background(), now)
	require.NoError(t, err)

	fireAt, firedAt := reminderState(t, pool, id)
	assert.Nil(t, firedAt, "re-armed, not stamped")
	assert.True(t, fireAt.After(now), "advanced into the future")
}

// Swift: "already-fired and future reminders are skipped".
func testReminderSkips(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	addDeviceToken(t, pool, tenant)
	firedAt := time.Now().Add(-4 * time.Minute)
	addReminder(t, pool, tenant, "old-"+tenant.String(), "x", time.Now().Add(-5*time.Minute), nil, &firedAt)
	addReminder(t, pool, tenant, "later-"+tenant.String(), "y", time.Now().Add(time.Hour), nil, nil)

	sender := &recordingSender{}
	_, err := scheduler(pool, sender).Tick(context.Background(), time.Now())
	require.NoError(t, err)
	for _, d := range sender.sends() {
		assert.NotContains(t, d.Message.Title, tenant.String())
	}
}

type telemetryRow struct {
	source    string
	spaceID   *uuid.UUID
	hits      int
	zeroHit   bool
	top, mean *float64
	limit     int
}

func telemetryRows(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) []telemetryRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT source_path, space_id, hit_count, zero_hit, top_distance, mean_distance, limit_requested
		FROM retrieval_telemetry_events WHERE tenant_id = $1`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	var out []telemetryRow
	for rows.Next() {
		var r telemetryRow
		require.NoError(t, rows.Scan(&r.source, &r.spaceID, &r.hits, &r.zeroHit, &r.top, &r.mean, &r.limit))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// waitRows waits for PGTelemetry's detached inserts.
func waitRows(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, n int) []telemetryRow {
	t.Helper()
	require.Eventually(t, func() bool { return len(telemetryRows(t, pool, tenant)) == n }, 5*time.Second, 20*time.Millisecond)
	return telemetryRows(t, pool, tenant)
}

func record(pool *pgxpool.Pool, tenant uuid.UUID, source string, space *uuid.UUID, distances []float32, limit int) {
	memoryagent.PGTelemetry{Pool: pool}.Record(context.Background(), memoryagent.TelemetrySample{
		Tenant: tenant, SourcePath: source, SpaceID: space, Distances: distances, Limit: limit,
	})
}

// RetrievalTelemetryWorkerTests: "enqueued samples are persisted".
func testTelemetryPersisted(t *testing.T, pool *pgxpool.Pool) {
	tenant := uuid.New()
	record(pool, tenant, "local_reply", nil, []float32{0.1, 0.2}, 5)
	record(pool, tenant, "query", nil, nil, 5)
	record(pool, tenant, "agentic_search", nil, []float32{0.3}, 3)
	rows := waitRows(t, pool, tenant, 3)
	zero := 0
	for _, r := range rows {
		if r.zeroHit {
			zero++
		}
		if r.source == "local_reply" {
			assert.Equal(t, 2, r.hits)
			require.NotNil(t, r.top)
			assert.InDelta(t, 0.1, *r.top, 1e-6)
		}
	}
	assert.Equal(t, 1, zero)
}

// RetrievalTelemetryWorkerTests: "enqueue after shutdown is a no-op and never
// throws". Go has no inbox to shut down; the equivalent is a sink whose database is
// gone: Record returns at once, logs, and nothing is written.
func testTelemetryNeverFails(t *testing.T, pool *pgxpool.Pool) {
	closed, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	require.NoError(t, err)
	closed.Close()
	tenant := uuid.New()
	assert.NotPanics(t, func() { record(closed, tenant, "local_reply", nil, []float32{0.1}, 5) })
	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, telemetryRows(t, pool, tenant))
}

// RetrievalTelemetrySampleTests: "from computes min top and mean over distances".
func testTelemetrySampleMinMean(t *testing.T, pool *pgxpool.Pool) {
	tenant := uuid.New()
	record(pool, tenant, "local_reply", nil, []float32{0.4, 0.1, 0.3}, 5)
	r := waitRows(t, pool, tenant, 1)[0]
	assert.Equal(t, 3, r.hits)
	require.NotNil(t, r.top)
	require.NotNil(t, r.mean)
	// Distances widen from float32, so compare with a tolerance.
	assert.InDelta(t, 0.1, *r.top, 1e-6)
	assert.InDelta(t, 0.8/3.0, *r.mean, 1e-6)
	assert.Equal(t, "local_reply", r.source)
	assert.Equal(t, 5, r.limit)
}

// RetrievalTelemetrySampleTests: "empty hits are zero-hit with nil distances".
func testTelemetryZeroHit(t *testing.T, pool *pgxpool.Pool) {
	tenant := uuid.New()
	record(pool, tenant, "query", nil, nil, 5)
	r := waitRows(t, pool, tenant, 1)[0]
	assert.Zero(t, r.hits)
	assert.Nil(t, r.top)
	assert.Nil(t, r.mean)
	assert.True(t, r.zeroHit)
}

// RetrievalTelemetrySampleTests: "non-empty hits are not zero-hit and carry space".
func testTelemetryCarriesSpace(t *testing.T, pool *pgxpool.Pool) {
	tenant, space := uuid.New(), uuid.New()
	record(pool, tenant, "agentic_search", &space, []float32{0.2}, 8)
	r := waitRows(t, pool, tenant, 1)[0]
	assert.False(t, r.zeroHit)
	require.NotNil(t, r.spaceID)
	assert.Equal(t, space, *r.spaceID)
	assert.Equal(t, "agentic_search", r.source)
	assert.Equal(t, 8, r.limit)
}

func addSpace(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, name, slug string, notes int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW())`, uuid.New(), tenant, name, slug, notes)
	require.NoError(t, err)
}

// SpaceTopologyTests: "spaceTopology sorts by note count desc and resolves slug".
func testSpaceTopologySorts(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	addSpace(t, pool, tenant, "AI", "ai", 42)
	addSpace(t, pool, tenant, "Stocks", "stocks", 17)
	addSpace(t, pool, tenant, "Health", "health", 9)

	topo, err := memorystore.New(pool, nil).SpaceTopology(context.Background(), tenant)
	require.NoError(t, err)
	slugs := make([]string, len(topo))
	bySlug := map[string]uuid.UUID{}
	for i, s := range topo {
		slugs[i] = s.Slug
		bySlug[s.Slug] = s.ID
	}
	assert.Equal(t, []string{"ai", "stocks", "health"}, slugs)
	assert.Equal(t, 42, topo[0].NoteCount)
	// slug → id resolution (the session_search scoping path).
	assert.NotEqual(t, uuid.Nil, bySlug["stocks"])
	_, found := bySlug["nonexistent"]
	assert.False(t, found)
}

// SpaceTopologyTests: "spaceTopology is tenant scoped".
func testSpaceTopologyScoped(t *testing.T, pool *pgxpool.Pool) {
	a, b := newTenant(t, pool), newTenant(t, pool)
	addSpace(t, pool, a, "AI", "ai", 1)
	repo := memorystore.New(pool, nil)
	topoA, err := repo.SpaceTopology(context.Background(), a)
	require.NoError(t, err)
	assert.Len(t, topoA, 1)
	topoB, err := repo.SpaceTopology(context.Background(), b)
	require.NoError(t, err)
	assert.Empty(t, topoB)
}

// vectorLiteral is a 1536-d pgvector literal with the first values set.
func vectorLiteral(head ...float64) string {
	parts := make([]string, 1536)
	for i := range parts {
		parts[i] = "0"
	}
	for i, v := range head {
		parts[i] = fmt.Sprint(v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func addMemory(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, embedding *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `INSERT INTO memories (id, tenant_id, content, embedding, created_at)
		VALUES ($1, $2, 'm', $3::vector, NOW())`, id, tenant, embedding)
	require.NoError(t, err)
	return id
}

// Go only: GraphLayoutWorker.tick lays out a tenant with unlaid embedded memories,
// writes the same coordinates ProjectTo3D gives, and leaves a tenant with one memory
// (nothing to reduce) and memories without vectors alone.
func testGraphLayoutPersists(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant, single := newTenant(t, pool), newTenant(t, pool)
	var embedded []uuid.UUID
	var literals []string
	for i := range 4 {
		lit := vectorLiteral(float64(i), float64(i*i)/2, 1-float64(i))
		literals = append(literals, lit)
		embedded = append(embedded, addMemory(t, pool, tenant, &lit))
	}
	bare := addMemory(t, pool, tenant, nil)
	lone := vectorLiteral(1)
	loneID := addMemory(t, pool, single, &lone)

	fixed := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	w := &GraphLayoutWorker{Pool: pool, Logger: quiet(), Now: func() time.Time { return fixed }}
	_, err := w.Tick(ctx)
	require.NoError(t, err)

	// Same order as the fetch: score ties, last_accessed_at null, so id ascending.
	vectors := map[uuid.UUID][]float64{}
	for i, id := range embedded {
		vectors[id] = ParsePgVector(literals[i])
	}
	rows, err := pool.Query(ctx, `SELECT id FROM memories WHERE tenant_id = $1 AND embedding IS NOT NULL ORDER BY score DESC, last_accessed_at DESC NULLS LAST, id ASC`, tenant)
	require.NoError(t, err)
	var order []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		order = append(order, id)
	}
	require.NoError(t, rows.Err())
	input := make([][]float64, len(order))
	for i, id := range order {
		input[i] = vectors[id]
	}
	want := ProjectTo3D(input)
	for i, id := range order {
		var x, y, z *float64
		var at *time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT graph_x, graph_y, graph_z, graph_layout_at FROM memories WHERE id = $1`, id).Scan(&x, &y, &z, &at))
		require.NotNil(t, x)
		assert.Equal(t, want[i], [3]float64{*x, *y, *z})
		require.NotNil(t, at)
		assert.True(t, at.Equal(fixed))
	}
	for _, id := range []uuid.UUID{bare, loneID} {
		var x *float64
		require.NoError(t, pool.QueryRow(ctx, `SELECT graph_x FROM memories WHERE id = $1`, id).Scan(&x))
		assert.Nil(t, x)
	}

	// Nothing new: the next tick lays out nobody (the one-memory tenant still has
	// nothing to reduce).
	relaid, err := w.Tick(ctx)
	require.NoError(t, err)
	assert.Zero(t, relaid)
}

// Go only: AnalyticsMaintenanceService.runOnce deletes past retention and keeps
// the rest.
func testAnalyticsMaintenance(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newTenant(t, pool)
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, q, args...)
		require.NoError(t, err)
	}
	for _, age := range []string{"91 days", "1 day"} {
		exec(`INSERT INTO analytics_events (vault_id, actor_user_id, event_name, source, occurred_at)
			VALUES ($1, $1, $2, 'server', NOW() - $3::interval)`, tenant, "e-"+age, age)
		exec(`INSERT INTO retrieval_telemetry_events (tenant_id, source_path, hit_count, zero_hit, limit_requested, created_at)
			VALUES ($1, $2, 0, true, 5, NOW() - $3::interval)`, tenant, age, age)
	}
	for _, age := range []string{"14 months", "1 month"} {
		exec(`INSERT INTO analytics_daily_rollups (day, vault_id, actor_user_id, metric)
			VALUES (CURRENT_DATE - $2::interval, $1, $1, $3)`, tenant, age, "m-"+age)
	}

	require.NoError(t, RunAnalyticsMaintenance(ctx, pool))

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM analytics_events WHERE vault_id = $1 AND event_name = 'e-1 day'`, tenant))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM analytics_events WHERE vault_id = $1`, tenant))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM analytics_daily_rollups WHERE vault_id = $1 AND metric = 'm-1 month'`, tenant))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM analytics_daily_rollups WHERE vault_id = $1`, tenant))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM retrieval_telemetry_events WHERE tenant_id = $1`, tenant))
}

// Go only: CalendarSyncWorker.tick counts every connected row, including ones its
// sync then skips (no Calendar scope), and never calls Google for them.
func testCalendarSyncCounts(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	before := countRows(t, pool, `SELECT count(*) FROM calendar_accounts WHERE status = 'connected'`)
	for _, status := range []string{"connected", "connected", "needs_reauth"} {
		tenant := newTenant(t, pool)
		_, err := pool.Exec(ctx, `INSERT INTO calendar_accounts (tenant_id, provider, scope, status) VALUES ($1, 'google', 'https://www.googleapis.com/auth/gmail.readonly', $2)`, tenant, status)
		require.NoError(t, err)
	}
	box, err := secretbox.New("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	require.NoError(t, err)
	google := calendar.NewGoogle(pool, calendar.GoogleOptions{
		Box: box, ClientID: "id", ClientSecret: "secret", RedirectURI: "https://example.invalid/cb",
		Endpoints: calendar.Endpoints{Token: "http://127.0.0.1:1/token", Revoke: "http://127.0.0.1:1/revoke", CalendarBase: "http://127.0.0.1:1"},
	})
	require.True(t, google.Configured())
	processed, err := google.SyncConnected(ctx, 4)
	require.NoError(t, err)
	assert.Equal(t, before+2, processed)
}
