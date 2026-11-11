package me_test

import (
	"context"
	"errors"
	"lumina/internal/apns"
	"lumina/internal/modules/me"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Ported from Services/LearnedDigestSchedulerTests.swift. Its `M145 turns off
// untouched placeholder jobs only` checks a Swift data migration that Go has no
// counterpart for (the baseline is the post-migration schema). Sunday 2026-10-18 closes the week that started Monday 2026-10-12.

const (
	kathmandu = "Asia/Kathmandu"
	stJohns   = "America/St_Johns"
)

var (
	sundayKathmandu = mustISO("2026-10-18T12:45:00Z") // 18:30 in Kathmandu
	sundayStJohns   = mustISO("2026-10-18T21:00:00Z") // 18:30 in St John's (NDT)
)

func mustISO(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

type recordingSender struct {
	mu   sync.Mutex
	sent []apns.Delivery
}

func (r *recordingSender) Send(_ context.Context, d apns.Delivery) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, d)
	return nil
}

func (r *recordingSender) to(token string) []apns.Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []apns.Delivery{}
	for _, d := range r.sent {
		if d.DeviceToken == token {
			out = append(out, d)
		}
	}
	return out
}

func learnedFixture(t *testing.T) (*pgxpool.Pool, *me.LearnedDigestScheduler, *recordingSender) {
	t.Helper()
	pool := startDB(t)
	push := &recordingSender{}
	return pool, &me.LearnedDigestScheduler{Pool: pool, Push: apns.NewWithSender(pool, "com.lumina.fernando", push)}, push
}

// learnedUser has one device and, unless learned is false, two preferences and one
// decision saved in the week before now.
func learnedUser(t *testing.T, pool *pgxpool.Pool, timezone string, learned bool, now time.Time) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	slug := "ld" + id.String()[:8]
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, timezone, created_at, updated_at)
		VALUES ($1, $2, 'stub', $3, $4, now(), now())`, id, slug+"@test.luminavault", slug, timezone)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(t, err)
	token := "tok-" + uuid.NewString()
	_, err = pool.Exec(ctx, `INSERT INTO device_tokens (id, tenant_id, token, platform, created_at, updated_at) VALUES ($1, $2, $3, 'ios', now(), now())`,
		uuid.New(), id, token)
	require.NoError(t, err)
	if learned {
		for i, m := range []struct{ content, kind string }{
			{"Likes window seats", "preference"}, {"Tea after 3pm", "preference"}, {"Booked the October trip", "decision"},
		} {
			at := now.Add(-time.Duration(i+1) * day)
			_, err = pool.Exec(ctx, `INSERT INTO memories (id, tenant_id, content, kind, review_state, origin_kind, created_at, updated_at)
				VALUES ($1, $2, $3, $4, 'auto', 'manual', $5, $5)`, uuid.New(), id, m.content, m.kind, at)
			require.NoError(t, err)
		}
	}
	return id, token
}

func learnedOutcome(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, weekStart string) *string {
	t.Helper()
	var outcome string
	err := pool.QueryRow(context.Background(), `SELECT outcome FROM learned_digest_log WHERE tenant_id = $1 AND week_start = $2::date`, tenant, weekStart).Scan(&outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	require.NoError(t, err)
	return &outcome
}

func budgetKind(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, dayStr string) *string {
	t.Helper()
	var kind string
	err := pool.QueryRow(context.Background(), `SELECT kind FROM proactive_push_log WHERE tenant_id = $1 AND day = $2::date`, tenant, dayStr).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	require.NoError(t, err)
	return &kind
}

func TestLearnedOnlyUsersWhoseClockReadsSunday18AreSent(t *testing.T) {
	pool, s, push := learnedFixture(t)
	ktm, ktmTok := learnedUser(t, pool, kathmandu, true, sundayKathmandu)
	stj, stjTok := learnedUser(t, pool, stJohns, true, sundayKathmandu)
	_, err := s.Tick(context.Background(), sundayKathmandu)
	require.NoError(t, err)
	require.Len(t, push.to(ktmTok), 1)
	require.Empty(t, push.to(stjTok))
	require.Equal(t, "sent", *learnedOutcome(t, pool, ktm, "2026-10-12"))
	require.Nil(t, learnedOutcome(t, pool, stj, "2026-10-12"))
	_, err = s.Tick(context.Background(), sundayStJohns)
	require.NoError(t, err)
	require.Len(t, push.to(stjTok), 1)
}

func TestLearnedPushCarriesCountsOnlyQuietlyToTheLearnedScreen(t *testing.T) {
	pool, s, push := learnedFixture(t)
	_, tok := learnedUser(t, pool, kathmandu, true, sundayKathmandu)
	_, err := s.Tick(context.Background(), sundayKathmandu)
	require.NoError(t, err)
	got := push.to(tok)
	require.Len(t, got, 1)
	m := got[0].Message
	require.Equal(t, "What I learned this week", m.Title)
	require.Equal(t, "2 things about you and 1 decision — keep, edit or forget them.", m.Body)
	require.Equal(t, apns.CategoryDigest, m.Category)
	require.Equal(t, "passive", m.InterruptionLevel)
	require.Equal(t, "learned", m.ThreadID)
	require.Equal(t, "learned", m.ActionCategory)
	require.Equal(t, "luminavault://learned", m.Payload["deepLink"])
}

func TestLearnedAWeekIsSentOnceTheNextWeekAgain(t *testing.T) {
	pool, s, push := learnedFixture(t)
	id, tok := learnedUser(t, pool, kathmandu, true, sundayKathmandu)
	ctx := context.Background()
	_, err := s.Tick(ctx, sundayKathmandu)
	require.NoError(t, err)
	_, err = s.Tick(ctx, sundayKathmandu.Add(10*time.Minute))
	require.NoError(t, err)
	require.Len(t, push.to(tok), 1)
	next := sundayKathmandu.Add(7 * day)
	_, err = pool.Exec(ctx, `INSERT INTO memories (id, tenant_id, content, kind, review_state, origin_kind, created_at, updated_at)
		VALUES ($1, $2, 'Window seat again', 'preference', 'auto', 'manual', $3, $3)`, uuid.New(), id, next.Add(-day))
	require.NoError(t, err)
	_, err = s.Tick(ctx, next)
	require.NoError(t, err)
	require.Len(t, push.to(tok), 2)
	require.Equal(t, "sent", *learnedOutcome(t, pool, id, "2026-10-19"))
}

func TestLearnedAnEmptyWeekSendsNothingAndIsNotRetried(t *testing.T) {
	pool, s, push := learnedFixture(t)
	id, tok := learnedUser(t, pool, kathmandu, false, sundayKathmandu)
	ctx := context.Background()
	_, err := s.Tick(ctx, sundayKathmandu)
	require.NoError(t, err)
	require.Equal(t, "empty", *learnedOutcome(t, pool, id, "2026-10-12"))
	// Something learned later in the hour waits for the app.
	_, err = pool.Exec(ctx, `INSERT INTO memories (id, tenant_id, content, kind, review_state, origin_kind, created_at, updated_at)
		VALUES ($1, $2, 'Late one', 'decision', 'auto', 'manual', $3, $3)`, uuid.New(), id, sundayKathmandu.Add(5*time.Minute))
	require.NoError(t, err)
	_, err = s.Tick(ctx, sundayKathmandu.Add(10*time.Minute))
	require.NoError(t, err)
	require.Empty(t, push.to(tok))
	require.Nil(t, budgetKind(t, pool, id, "2026-10-18"))
}

func TestLearnedASpentProactiveBudgetSkipsTheWeek(t *testing.T) {
	pool, s, push := learnedFixture(t)
	id, tok := learnedUser(t, pool, kathmandu, true, sundayKathmandu)
	ctx := context.Background()
	won, err := apns.ClaimProactivePush(ctx, pool, id, "brief", kathmandu, sundayKathmandu.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, won)
	_, err = s.Tick(ctx, sundayKathmandu)
	require.NoError(t, err)
	require.Empty(t, push.to(tok))
	require.Equal(t, "budget_spent", *learnedOutcome(t, pool, id, "2026-10-12"))
	require.Equal(t, "brief", *budgetKind(t, pool, id, "2026-10-18"))
}

func TestLearnedTheDigestOptOutSuppressesItAndLeavesTheBudgetAlone(t *testing.T) {
	pool, s, push := learnedFixture(t)
	id, tok := learnedUser(t, pool, kathmandu, true, sundayKathmandu)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO apns_category_prefs (tenant_id, digest_enabled) VALUES ($1, false)`, id)
	require.NoError(t, err)
	_, err = s.Tick(ctx, sundayKathmandu)
	require.NoError(t, err)
	require.Empty(t, push.to(tok))
	require.Equal(t, "opted_out", *learnedOutcome(t, pool, id, "2026-10-12"))
	require.Nil(t, budgetKind(t, pool, id, "2026-10-18"))
}

func TestLearnedWeekStartIsTheLocalMondayOfTheSunday(t *testing.T) {
	check := func(now time.Time, tz string, want string, ok bool) {
		t.Helper()
		got, gotOK := me.WeekStartIfSendHour(now, tz)
		require.Equal(t, ok, gotOK, tz)
		require.Equal(t, want, got, tz)
	}
	check(sundayKathmandu, kathmandu, "2026-10-12", true)
	check(mustISO("2026-10-19T01:30:00Z"), "America/Los_Angeles", "2026-10-12", true)
	check(mustISO("2026-10-19T01:30:00Z"), "UTC", "", false)
	check(mustISO("2026-10-18T18:05:00Z"), "Not/AZone", "2026-10-12", true)
}

func TestLearnedPushCopyCountsAndPluralises(t *testing.T) {
	require.Equal(t, "2 things about you and 3 decisions — keep, edit or forget them.",
		me.LearnedPushBody(0, []string{"preference", "preference", "decision", "decision", "decision"}))
	require.Equal(t, "1 thing about you — keep, edit or forget it.", me.LearnedPushBody(1, nil))
	require.Equal(t, "1 decision — keep, edit or forget it.", me.LearnedPushBody(0, []string{"decision"}))
	require.Empty(t, me.LearnedPushBody(0, nil))
}
