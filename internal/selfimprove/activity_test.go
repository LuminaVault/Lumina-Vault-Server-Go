package selfimprove

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// No Swift test calls noteActivity or triggerComplexSession; these pin
// SelfImprovementService.swift:293-311 and its enqueue, settings and requireManaged.
func TestActivity(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	newTenant := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
			VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", id.String())
		require.NoError(t, err)
		return id
	}
	runs := func(t *testing.T, tenant uuid.UUID) int {
		t.Helper()
		return countRows(ctx, t, pool, `SELECT count(*) FROM self_improvement_runs WHERE tenant_id = $1`, tenant)
	}
	settingsRows := func(t *testing.T, tenant uuid.UUID) int {
		t.Helper()
		return countRows(ctx, t, pool, `SELECT count(*) FROM self_improvement_settings WHERE tenant_id = $1`, tenant)
	}
	set := func(t *testing.T, tenant uuid.UUID, assignment string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE self_improvement_settings SET `+assignment+` WHERE tenant_id = $1`, append([]any{tenant}, args...)...)
		require.NoError(t, err)
	}
	active := NewActivity(pool, true, nil)

	t.Run("noteActivity creates the row with the defaults and stamps it", func(t *testing.T) {
		tenant := newTenant(t)
		at := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
		before := time.Now()
		active.NoteActivity(ctx, tenant, at)

		var enabled, curator, consolidate, prune, soul, complexSessions bool
		var interval, idle, keep, window, cooldown int
		var mode string
		var last, next, created, updated time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT enabled, curator_enabled, interval_hours, minimum_idle_hours, consolidate, prune_builtins,
				backup_keep, soul_review_enabled, review_complex_sessions, soul_review_window_days, soul_review_cooldown_hours,
				model_mode, last_activity_at, next_review_at, created_at, updated_at
			FROM self_improvement_settings WHERE tenant_id = $1`, tenant).Scan(
			&enabled, &curator, &interval, &idle, &consolidate, &prune, &keep, &soul, &complexSessions, &window, &cooldown, &mode, &last, &next, &created, &updated))
		assert.True(t, enabled && curator && consolidate && soul && complexSessions)
		assert.False(t, prune)
		assert.Equal(t, []int{168, 2, 5, 14, 24}, []int{interval, idle, keep, window, cooldown})
		assert.Equal(t, "economy", mode)
		assert.True(t, last.Equal(at))
		assert.WithinDuration(t, before.Add(168*time.Hour), next, time.Minute)
		assert.False(t, updated.Before(created))

		// The second call reuses the row.
		later := at.Add(time.Hour)
		active.NoteActivity(ctx, tenant, later)
		assert.Equal(t, 1, settingsRows(t, tenant))
		require.NoError(t, pool.QueryRow(ctx, `SELECT last_activity_at FROM self_improvement_settings WHERE tenant_id = $1`, tenant).Scan(&last))
		assert.True(t, last.Equal(later))
	})
	t.Run("noteActivity swallows failures", func(t *testing.T) {
		require.NotPanics(t, func() { active.NoteActivity(ctx, uuid.New(), time.Now()) })
	})
	t.Run("a simple session does nothing, not even the settings row", func(t *testing.T) {
		tenant := newTenant(t)
		active.TriggerComplexSession(ctx, tenant, 4, false)
		assert.Zero(t, runs(t, tenant))
		assert.Zero(t, settingsRows(t, tenant))
	})
	t.Run("five tool calls queue one soul review", func(t *testing.T) {
		tenant := newTenant(t)
		active.TriggerComplexSession(ctx, tenant, 5, false)
		var kind, status, trigger string
		var dryRun bool
		var applied, skipped int
		require.NoError(t, pool.QueryRow(ctx, `SELECT kind, status, trigger, dry_run, actions_applied, actions_skipped
			FROM self_improvement_runs WHERE tenant_id = $1`, tenant).Scan(&kind, &status, &trigger, &dryRun, &applied, &skipped))
		assert.Equal(t, []string{"soul", "queued", "complex_session"}, []string{kind, status, trigger})
		assert.False(t, dryRun)
		assert.Zero(t, applied+skipped)

		// A queued run covers the next complex session.
		active.TriggerComplexSession(ctx, tenant, 9, false)
		assert.Equal(t, 1, runs(t, tenant))
	})
	t.Run("a created workflow counts whatever the tool calls", func(t *testing.T) {
		tenant := newTenant(t)
		active.TriggerComplexSession(ctx, tenant, 0, true)
		assert.Equal(t, 1, runs(t, tenant))
	})
	t.Run("switches", func(t *testing.T) {
		for _, column := range []string{"enabled", "soul_review_enabled", "review_complex_sessions"} {
			tenant := newTenant(t)
			active.NoteActivity(ctx, tenant, time.Now())
			set(t, tenant, column+` = FALSE`)
			active.TriggerComplexSession(ctx, tenant, 5, false)
			assert.Zero(t, runs(t, tenant), column)
		}
	})
	t.Run("the operator's switch off still creates the settings row", func(t *testing.T) {
		tenant := newTenant(t)
		NewActivity(pool, false, nil).TriggerComplexSession(ctx, tenant, 5, false)
		assert.Zero(t, runs(t, tenant))
		assert.Equal(t, 1, settingsRows(t, tenant))
	})
	t.Run("the cooldown", func(t *testing.T) {
		tenant := newTenant(t)
		active.NoteActivity(ctx, tenant, time.Now())
		set(t, tenant, `last_soul_review_at = now() - interval '23 hours'`)
		active.TriggerComplexSession(ctx, tenant, 5, false)
		assert.Zero(t, runs(t, tenant), "inside the default 24 hours")

		set(t, tenant, `soul_review_cooldown_hours = 2`)
		active.TriggerComplexSession(ctx, tenant, 5, false)
		assert.Equal(t, 1, runs(t, tenant), "the tenant's own cooldown has passed")
	})
	t.Run("a bring-your-own Hermes is read-only", func(t *testing.T) {
		tenant := newTenant(t)
		byo := NewActivity(pool, true, func(_ context.Context, id uuid.UUID) bool { return id == tenant })
		byo.TriggerComplexSession(ctx, tenant, 5, false)
		assert.Zero(t, runs(t, tenant))
		assert.Equal(t, 1, settingsRows(t, tenant))
	})
	t.Run("a live dry run is a conflict, and a finished run is no obstacle", func(t *testing.T) {
		tenant := newTenant(t)
		insert := func(status string, dryRun bool) {
			_, err := pool.Exec(ctx, `INSERT INTO self_improvement_runs (id, tenant_id, kind, status, trigger, dry_run, created_at)
				VALUES ($1, $2, 'soul', $3, 'manual', $4, now())`, uuid.New(), tenant, status, dryRun)
			require.NoError(t, err)
		}
		insert("running", true)
		active.TriggerComplexSession(ctx, tenant, 5, false)
		assert.Equal(t, 1, runs(t, tenant))

		_, err := pool.Exec(ctx, `UPDATE self_improvement_runs SET status = 'succeeded' WHERE tenant_id = $1`, tenant)
		require.NoError(t, err)
		active.TriggerComplexSession(ctx, tenant, 5, false)
		assert.Equal(t, 2, runs(t, tenant))
	})
	t.Run("a curator run doesn't block a soul review", func(t *testing.T) {
		tenant := newTenant(t)
		_, err := pool.Exec(ctx, `INSERT INTO self_improvement_runs (id, tenant_id, kind, status, trigger, dry_run, created_at)
			VALUES ($1, $2, 'curator', 'queued', 'manual', TRUE, now())`, uuid.New(), tenant)
		require.NoError(t, err)
		active.TriggerComplexSession(ctx, tenant, 5, false)
		assert.Equal(t, 2, runs(t, tenant))
	})
	t.Run("nil is safe, Swift's selfImprovement?", func(t *testing.T) {
		var none *Activity
		require.NotPanics(t, func() {
			none.NoteActivity(ctx, uuid.New(), time.Now())
			none.TriggerComplexSession(ctx, uuid.New(), 9, true)
		})
	})
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, query, args...).Scan(&n))
	return n
}
