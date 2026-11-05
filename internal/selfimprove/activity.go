// Package selfimprove is the part of Swift's SelfImprovementService
// (Sources/App/SelfImprovement/SelfImprovementService.swift) chat and workflows call
// on every turn: noteActivity and triggerComplexSession. The controller, policy,
// review runs and scheduler come with 5H; the scheduler stays in Swift and picks up
// the queued rows this writes.
package selfimprove

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/hermesresolve"
	"lumina/internal/ids"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	complexSessionToolCalls = 5
	firstReviewDelay        = 168 * time.Hour

	kindSoul              = "soul"
	statusQueued          = "queued"
	statusRunning         = "running"
	triggerComplexSession = "complex_session"
)

// Activity records tenant activity and queues the soul review a complex session
// earns. A nil *Activity does nothing, Swift's optional `selfImprovement?`.
type Activity struct {
	pool            *pgxpool.Pool
	globallyEnabled bool
	isUserOverride  func(ctx context.Context, tenant uuid.UUID) bool
	now             func() time.Time
}

// NewActivity builds it. globallyEnabled is selfImprovement.enabled. isUserOverride
// says the tenant runs its own Hermes, where self-improvement is read-only; nil means
// never, and ResolverOverride is what production passes.
func NewActivity(pool *pgxpool.Pool, globallyEnabled bool, isUserOverride func(context.Context, uuid.UUID) bool) *Activity {
	return &Activity{pool: pool, globallyEnabled: globallyEnabled, isUserOverride: isUserOverride, now: time.Now}
}

// ResolverOverride is HermesRemoteCapabilitiesService.isUserOverride: a failed
// resolution counts as no override.
func ResolverOverride(r *hermesresolve.Resolver) func(context.Context, uuid.UUID) bool {
	return func(ctx context.Context, tenant uuid.UUID) bool {
		resolution, err := r.Resolve(ctx, tenant)
		return err == nil && resolution.IsUserOverride
	}
}

// settingsRow holds the self_improvement_settings columns these two paths read.
type settingsRow struct {
	id                    uuid.UUID
	enabled               bool
	soulReviewEnabled     bool
	reviewComplexSessions bool
	cooldownHours         int
	lastSoulReviewAt      *time.Time
}

// NoteActivity stamps last_activity_at. Failures are swallowed, as try? does in
// Swift. Like Swift it creates the settings row on first use, even when the feature
// is switched off.
func (a *Activity) NoteActivity(ctx context.Context, tenant uuid.UUID, at time.Time) {
	if a == nil {
		return
	}
	row, err := a.settings(ctx, tenant)
	if err != nil {
		return
	}
	_, err = a.pool.Exec(ctx, `UPDATE self_improvement_settings SET last_activity_at = $1, updated_at = $2 WHERE id = $3`, at, a.now(), row.id)
	if err != nil {
		slog.DebugContext(ctx, "self_improvement.note_activity_failed", "tenant", ids.Upper(tenant), "error", err)
	}
}

// TriggerComplexSession queues a soul review after five or more tool calls or a
// created workflow, unless the tenant turned it off or is inside its cooldown. It
// never fails its caller; a problem is logged.
func (a *Activity) TriggerComplexSession(ctx context.Context, tenant uuid.UUID, toolCallCount int, createdWorkflow bool) {
	if a == nil || (toolCallCount < complexSessionToolCalls && !createdWorkflow) {
		return
	}
	if err := a.triggerComplexSession(ctx, tenant); err != nil {
		slog.WarnContext(ctx, "self_improvement.complex_trigger_failed", "tenant", ids.Upper(tenant), "error", err)
	}
}

func (a *Activity) triggerComplexSession(ctx context.Context, tenant uuid.UUID) error {
	row, err := a.settings(ctx, tenant)
	if err != nil {
		return err
	}
	if !a.globallyEnabled || !row.enabled || !row.soulReviewEnabled || !row.reviewComplexSessions {
		return nil
	}
	cooldown := time.Duration(row.cooldownHours) * time.Hour
	if row.lastSoulReviewAt != nil && a.now().Sub(*row.lastSoulReviewAt) < cooldown {
		return nil
	}
	return a.enqueueSoulReview(ctx, tenant)
}

var (
	errReadOnlyOnBYOHermes = errors.New("self_improvement_read_only_on_byo_hermes")
	errRunAlreadyActive    = errors.New("self_improvement_run_already_active")
)

// enqueueSoulReview is enqueue(kind: .soul, trigger: .complexSession, dryRun: false).
// A live non-dry run already covers it; a live dry run is a conflict.
func (a *Activity) enqueueSoulReview(ctx context.Context, tenant uuid.UUID) error {
	if a.isUserOverride != nil && a.isUserOverride(ctx, tenant) {
		return errReadOnlyOnBYOHermes
	}
	var activeDryRun bool
	err := a.pool.QueryRow(ctx, `SELECT dry_run FROM self_improvement_runs
		WHERE tenant_id = $1 AND kind = $2 AND (status = $3 OR status = $4)
		ORDER BY created_at DESC LIMIT 1`, tenant, kindSoul, statusQueued, statusRunning).Scan(&activeDryRun)
	switch {
	case err == nil && activeDryRun:
		return errRunAlreadyActive
	case err == nil:
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	_, err = a.pool.Exec(ctx, `INSERT INTO self_improvement_runs
		(id, tenant_id, kind, status, trigger, dry_run, actions_applied, actions_skipped, created_at)
		VALUES ($1, $2, $3, $4, $5, FALSE, 0, 0, $6)`,
		uuid.New(), tenant, kindSoul, statusQueued, triggerComplexSession, a.now())
	return err
}

// settings finds the tenant's row or creates it with ImprovementSettingsDTO's
// defaults. Swift has no upsert here: losing the race on tenant_id is an error.
func (a *Activity) settings(ctx context.Context, tenant uuid.UUID) (settingsRow, error) {
	var row settingsRow
	err := a.pool.QueryRow(ctx, `SELECT id, enabled, soul_review_enabled, review_complex_sessions,
			soul_review_cooldown_hours, last_soul_review_at
		FROM self_improvement_settings WHERE tenant_id = $1 LIMIT 1`, tenant).
		Scan(&row.id, &row.enabled, &row.soulReviewEnabled, &row.reviewComplexSessions, &row.cooldownHours, &row.lastSoulReviewAt)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return row, err
	}
	now := a.now()
	row = settingsRow{id: uuid.New(), enabled: true, soulReviewEnabled: true, reviewComplexSessions: true, cooldownHours: 24}
	_, err = a.pool.Exec(ctx, `INSERT INTO self_improvement_settings
		(id, tenant_id, enabled, curator_enabled, interval_hours, minimum_idle_hours, consolidate, prune_builtins,
		 backup_keep, soul_review_enabled, review_complex_sessions, soul_review_window_days, soul_review_cooldown_hours,
		 model_mode, next_review_at, created_at, updated_at)
		VALUES ($1, $2, TRUE, TRUE, 168, 2, TRUE, FALSE, 5, TRUE, TRUE, 14, 24, 'economy', $3, $4, $4)`,
		row.id, tenant, now.Add(firstReviewDelay), now)
	return row, err
}
