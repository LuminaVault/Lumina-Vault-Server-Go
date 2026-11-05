package onboardlatch

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type state struct {
	signup, capture, query, memory, kb           bool
	captureAt, queryAt, memoryAt, kbAt, signupAt *time.Time
}

func read(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) state {
	t.Helper()
	var s state
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT signup_completed, signup_completed_at,
		first_capture_completed, first_capture_completed_at, first_query_completed, first_query_completed_at,
		first_memory_compile_completed, first_memory_compile_completed_at, first_kb_compile_completed, first_kb_compile_completed_at
		FROM onboarding_state WHERE tenant_id = $1`, tenant).Scan(&s.signup, &s.signupAt, &s.capture, &s.captureAt, &s.query, &s.queryAt,
		&s.memory, &s.memoryAt, &s.kb, &s.kbAt))
	return s
}

func TestLatches(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	l := New(pool)
	user := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
			VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@test", id.String())
		require.NoError(t, err)
		return id
	}

	// A capture as the first thing a fresh account does creates the row.
	t.Run("creates the row with signup latched", func(t *testing.T) {
		id := user(t)
		l.Latch(ctx, FirstCapture, id)
		s := read(t, pool, id)
		require.True(t, s.signup)
		require.NotNil(t, s.signupAt)
		require.True(t, s.capture)
		require.NotNil(t, s.captureAt)
		require.False(t, s.query)
		require.False(t, s.memory)
	})

	t.Run("first means first", func(t *testing.T) {
		for _, latch := range []Latch{FirstCapture, FirstQuery, FirstMemoryCompile} {
			id := user(t)
			l.Latch(ctx, latch, id)
			first := read(t, pool, id)
			_, err := pool.Exec(ctx, `UPDATE onboarding_state SET updated_at = 'epoch' WHERE tenant_id = $1`, id)
			require.NoError(t, err)
			l.Latch(ctx, latch, id)
			again := read(t, pool, id)
			require.Equal(t, first, again, string(latch))
			var updated time.Time
			require.NoError(t, pool.QueryRow(ctx, `SELECT updated_at FROM onboarding_state WHERE tenant_id = $1`, id).Scan(&updated))
			require.Equal(t, int64(0), updated.Unix(), "%s: a repeat latch writes nothing", latch)
		}
	})

	// A concurrent PATCH /v1/onboarding (a dismissal) survives: only latch columns are written.
	t.Run("does not clobber other columns", func(t *testing.T) {
		id := user(t)
		l.Latch(ctx, FirstCapture, id)
		_, err := pool.Exec(ctx, `UPDATE onboarding_state SET soul_configured_completed = TRUE WHERE tenant_id = $1`, id)
		require.NoError(t, err)
		l.Latch(ctx, FirstQuery, id)
		var soul bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT soul_configured_completed FROM onboarding_state WHERE tenant_id = $1`, id).Scan(&soul))
		require.True(t, soul)
		s := read(t, pool, id)
		require.True(t, s.capture)
		require.True(t, s.query)
	})

	t.Run("memory compile dual-writes the legacy pair", func(t *testing.T) {
		id := user(t)
		l.Latch(ctx, FirstMemoryCompile, id)
		s := read(t, pool, id)
		require.True(t, s.memory)
		require.True(t, s.kb)
		require.NotNil(t, s.memoryAt)
		require.NotNil(t, s.kbAt)
	})

	// The pre-M130 bug left legacy true, memory false; the date the user
	// really compiled on is kept.
	t.Run("memory compile self-heals a stranded row", func(t *testing.T) {
		id := user(t)
		compiled := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		_, err := pool.Exec(ctx, `INSERT INTO onboarding_state (id, tenant_id, first_kb_compile_completed, first_kb_compile_completed_at, created_at, updated_at)
			VALUES ($1, $2, TRUE, $3, now(), now())`, uuid.New(), id, compiled)
		require.NoError(t, err)
		l.Latch(ctx, FirstMemoryCompile, id)
		s := read(t, pool, id)
		require.True(t, s.memory)
		require.True(t, s.kb)
		require.True(t, s.memoryAt.Equal(compiled))
		require.True(t, s.kbAt.Equal(compiled))
	})

	// A hung-up client doesn't lose the flag for work that did happen.
	t.Run("a cancelled request still latches", func(t *testing.T) {
		id := user(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		l.Latch(cancelled, FirstCapture, id)
		require.True(t, read(t, pool, id).capture)
	})

	t.Run("failures are swallowed", func(t *testing.T) {
		require.NotPanics(t, func() {
			l.Latch(ctx, FirstCapture, uuid.New()) // FK violation: no such user
			l.Latch(ctx, Latch("nope"), uuid.New())
			var none *Latches
			none.Latch(ctx, FirstCapture, uuid.New())
		})
	})
}
