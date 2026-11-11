package calendar_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/apns"
	"lumina/internal/embedding"
	"lumina/internal/modules/calendar"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// The scheduler half of Calendar/MeetingBriefTests.swift. Its matching, copy,
// attendee-sync and route tests are in logic_test.go and module_test.go.
// Embeddings are axis vectors: the stub answers axis 0 for every event, so a note
// on axis 0 is a perfect match and one on any other axis is past the floor.

type axisEmbedder struct {
	calls atomic.Int32
	fail  atomic.Bool
}

func axis(i int) []float32 {
	v := make([]float32, embedding.Dimensions)
	v[i] = 1
	return v
}

func (a *axisEmbedder) Embed(context.Context, string, uuid.UUID) ([]float32, error) {
	a.calls.Add(1)
	if a.fail.Load() {
		return nil, errors.New("embedding provider down")
	}
	return axis(0), nil
}

type pushes struct {
	mu   sync.Mutex
	sent []apns.Delivery
}

func (p *pushes) Send(_ context.Context, d apns.Delivery) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, d)
	return nil
}

func (p *pushes) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

var briefNow = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

type briefFixture struct {
	pool  *pgxpool.Pool
	emb   *axisEmbedder
	push  *pushes
	sched *calendar.MeetingBriefScheduler
}

func newBriefFixture(t *testing.T) briefFixture {
	t.Helper()
	pool := startDB(t)
	emb := &axisEmbedder{}
	push := &pushes{}
	return briefFixture{pool: pool, emb: emb, push: push, sched: calendar.NewMeetingBriefScheduler(pool,
		apns.NewWithSender(pool, "com.lumina.fernando", push),
		calendar.Options{Embeddings: emb, MaxDistance: calendar.DefaultBriefMaxDistance}, nil)}
}

// tenant makes a user (opted in unless optIn is false; with a device unless
// device is false) with one note on axis 0 naming Ana.
func (f briefFixture) tenant(t *testing.T, optIn, device bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	slug := "mb" + id.String()[:8]
	_, err := f.pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'stub', $3, now(), now())`, id, slug+"@test.luminavault", slug)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(t, err)
	if optIn {
		_, err = f.pool.Exec(ctx, `INSERT INTO apns_category_prefs (tenant_id, meeting_brief_enabled) VALUES ($1, true)`, id)
		require.NoError(t, err)
	}
	if device {
		_, err = f.pool.Exec(ctx, `INSERT INTO device_tokens (id, tenant_id, token, platform, created_at, updated_at) VALUES ($1, $2, $3, 'ios', now(), now())`,
			uuid.New(), id, "tok-"+id.String())
		require.NoError(t, err)
	}
	f.note(t, id, "Ana wants hiring in the Q4 plan", 0)
	return id
}

func (f briefFixture) note(t *testing.T, tenant uuid.UUID, content string, ax int) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), `INSERT INTO memories (id, tenant_id, content, created_at, embedding, review_state, kind, origin_kind)
		VALUES ($1, $2, $3, now(), $4::vector, 'auto', 'fact', 'manual')`, uuid.New(), tenant, content, embedding.FormatVector(axis(ax)))
	require.NoError(t, err)
}

type briefEv struct {
	title     string
	in        time.Duration
	allDay    bool
	status    string
	attendees string
}

func (f briefFixture) event(t *testing.T, tenant uuid.UUID, e briefEv) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if e.status == "" {
		e.status = "confirmed"
	}
	var attendees *string
	if e.attendees != "" {
		attendees = &e.attendees
	}
	start := briefNow.Add(e.in)
	_, err := f.pool.Exec(context.Background(), `INSERT INTO calendar_events
		(id, tenant_id, source, external_id, title, starts_at, ends_at, all_day, status, attendees, remote_updated_at, created_at, updated_at)
		VALUES ($1, $2, 'apple_eventkit', $3, $4, $5, $6, $7, $8, $9::jsonb, now(), now(), now())`,
		id, tenant, "x-"+id.String(), e.title, start, start.Add(30*time.Minute), e.allDay, e.status, attendees)
	require.NoError(t, err)
	return id
}

type briefRow struct {
	outcome  string
	memories string
	sentAt   *time.Time
}

func (f briefFixture) row(t *testing.T, tenant, event uuid.UUID) *briefRow {
	t.Helper()
	var r briefRow
	err := f.pool.QueryRow(context.Background(), `SELECT outcome, memories::text, sent_at FROM meeting_briefs WHERE tenant_id = $1 AND event_id = $2`,
		tenant, event).Scan(&r.outcome, &r.memories, &r.sentAt)
	if err != nil {
		return nil
	}
	return &r
}

func TestBriefOnlyTimedLiveEventsStartingIn25To35MinutesAreBriefed(t *testing.T) {
	f := newBriefFixture(t)
	u := f.tenant(t, true, true)
	early := f.event(t, u, briefEv{title: "Early", in: 20 * time.Minute})
	due := f.event(t, u, briefEv{title: "Due", in: 30 * time.Minute})
	late := f.event(t, u, briefEv{title: "Late", in: 40 * time.Minute})
	allDay := f.event(t, u, briefEv{title: "All day", in: 30 * time.Minute, allDay: true})
	cancelled := f.event(t, u, briefEv{title: "Cancelled", in: 30 * time.Minute, status: "cancelled"})
	sent, err := f.sched.Tick(context.Background(), briefNow)
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	require.Equal(t, "sent", f.row(t, u, due).outcome)
	for _, id := range []uuid.UUID{early, late, allDay, cancelled} {
		require.Nil(t, f.row(t, u, id))
	}
}

func TestBriefNobodyWhoHasNotOptedInIsBriefed(t *testing.T) {
	f := newBriefFixture(t)
	noPrefs := f.tenant(t, false, true)
	off := f.tenant(t, false, true)
	_, err := f.pool.Exec(context.Background(), `INSERT INTO apns_category_prefs (tenant_id, meeting_brief_enabled) VALUES ($1, false)`, off)
	require.NoError(t, err)
	a := f.event(t, noPrefs, briefEv{title: "Q4 plan", in: 30 * time.Minute})
	b := f.event(t, off, briefEv{title: "Q4 plan", in: 30 * time.Minute})
	sent, err := f.sched.Tick(context.Background(), briefNow)
	require.NoError(t, err)
	require.Zero(t, sent)
	require.Nil(t, f.row(t, noPrefs, a))
	require.Nil(t, f.row(t, off, b))
	require.Zero(t, f.emb.calls.Load())
}

func TestBriefThePushNamesAttendeesFromTheNotesQuietlyAndKeepsWhatItCounted(t *testing.T) {
	f := newBriefFixture(t)
	u := f.tenant(t, true, true)
	id := f.event(t, u, briefEv{title: "Q4 plan", in: 30 * time.Minute,
		attendees: `{"list":[{"email":"ana@x.test","displayName":"Ana Silva"},{"email":"rui@x.test","displayName":"Rui Costa"}]}`})
	sent, err := f.sched.Tick(context.Background(), briefNow)
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	require.Len(t, f.push.sent, 1)
	m := f.push.sent[0].Message
	require.Equal(t, "Before Q4 plan", m.Title)
	require.Equal(t, "1 note on Ana", m.Body)
	require.Equal(t, apns.CategoryMeeting, m.Category)
	require.Equal(t, "passive", m.InterruptionLevel)
	require.Equal(t, "meeting", m.ThreadID)
	require.Equal(t, "meeting", m.ActionCategory)
	require.Equal(t, "luminavault://meeting/"+strings.ToLower(id.String()), m.Payload["deepLink"])
	require.Equal(t, strings.ToUpper(id.String()), m.Payload["eventID"])
	row := f.row(t, u, id)
	require.Equal(t, "sent", row.outcome)
	require.NotNil(t, row.sentAt)
	var stored []struct {
		ID         string  `json:"id"`
		Similarity float64 `json:"similarity"`
	}
	require.NoError(t, json.Unmarshal([]byte(row.memories), &stored))
	require.Len(t, stored, 1)
	require.InDelta(t, 1.0, stored[0].Similarity, 1e-6)
}

func TestBriefTwoReplicasTickingTogetherSendOnePush(t *testing.T) {
	f := newBriefFixture(t)
	u := f.tenant(t, true, true)
	f.event(t, u, briefEv{title: "Q4 plan", in: 30 * time.Minute})
	other := calendar.NewMeetingBriefScheduler(f.pool, apns.NewWithSender(f.pool, "com.lumina.fernando", f.push),
		calendar.Options{Embeddings: f.emb, MaxDistance: calendar.DefaultBriefMaxDistance}, nil)
	var wg sync.WaitGroup
	for _, s := range []*calendar.MeetingBriefScheduler{f.sched, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Tick(context.Background(), briefNow)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, f.push.count())
}

func TestBriefNothingPastTheFloorMeansNoPushAndTheEventIsNotSearchedAgain(t *testing.T) {
	f := newBriefFixture(t)
	u := uuid.New()
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at) VALUES ($1, $2, 'stub', $3, now(), now())`,
		u, "far-"+u.String()[:8]+"@test.luminavault", "far"+u.String()[:8])
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, u)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO apns_category_prefs (tenant_id, meeting_brief_enabled) VALUES ($1, true)`, u)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO device_tokens (id, tenant_id, token, platform, created_at, updated_at) VALUES ($1, $2, 'tok-far', 'ios', now(), now())`, uuid.New(), u)
	require.NoError(t, err)
	f.note(t, u, "Unrelated", 5)
	id := f.event(t, u, briefEv{title: "Dentist", in: 30 * time.Minute})
	sent, err := f.sched.Tick(ctx, briefNow)
	require.NoError(t, err)
	require.Zero(t, sent)
	require.Equal(t, "empty", f.row(t, u, id).outcome)
	require.Equal(t, int32(1), f.emb.calls.Load())
	_, err = f.sched.Tick(ctx, briefNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int32(1), f.emb.calls.Load())
	require.Zero(t, f.push.count())
}

func TestBriefASpentProactiveSlotDoesNotBlockItAndItSpendsNone(t *testing.T) {
	f := newBriefFixture(t)
	u := f.tenant(t, true, true)
	ctx := context.Background()
	won, err := apns.ClaimProactivePush(ctx, f.pool, u, "brief", "UTC", briefNow.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, won)
	f.event(t, u, briefEv{title: "Q4 plan", in: 30 * time.Minute})
	sent, err := f.sched.Tick(ctx, briefNow)
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	var kind string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT kind FROM proactive_push_log WHERE tenant_id = $1`, u).Scan(&kind))
	require.Equal(t, "brief", kind)
}

func TestBriefNoDeviceIsRecordedBeforeAnyEmbeddingIsPaidFor(t *testing.T) {
	f := newBriefFixture(t)
	u := f.tenant(t, true, false)
	id := f.event(t, u, briefEv{title: "Q4 plan", in: 30 * time.Minute})
	sent, err := f.sched.Tick(context.Background(), briefNow)
	require.NoError(t, err)
	require.Zero(t, sent)
	require.Equal(t, "no_device", f.row(t, u, id).outcome)
	require.Zero(t, f.emb.calls.Load())
}

func TestBriefAnEmbeddingFailureReleasesTheClaimForTheNextTick(t *testing.T) {
	f := newBriefFixture(t)
	u := f.tenant(t, true, true)
	id := f.event(t, u, briefEv{title: "Q4 plan", in: 30 * time.Minute})
	f.emb.fail.Store(true)
	sent, err := f.sched.Tick(context.Background(), briefNow)
	require.NoError(t, err)
	require.Zero(t, sent)
	require.Nil(t, f.row(t, u, id))
	f.emb.fail.Store(false)
	sent, err = f.sched.Tick(context.Background(), briefNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	require.Equal(t, "sent", f.row(t, u, id).outcome)
}

// `push and why lines`: the push half (the why half is TestWhyLines).
func TestBriefPushLines(t *testing.T) {
	require.Equal(t, "Before Q4 plan", calendar.BriefPushTitle("  Q4\n plan "))
	require.Equal(t, "Before your meeting", calendar.BriefPushTitle("   "))
	require.Equal(t, "Before "+strings.Repeat("a", 79)+"…", calendar.BriefPushTitle(strings.Repeat("a", 81)))
	require.Equal(t, "3 notes for this meeting", calendar.BriefPushBody(3, nil))
	require.Equal(t, "1 note on Ana", calendar.BriefPushBody(1, []string{"Ana"}))
	require.Equal(t, "2 notes on Ana and Rui", calendar.BriefPushBody(2, []string{"Ana", "Rui", "Eva"}))
}
