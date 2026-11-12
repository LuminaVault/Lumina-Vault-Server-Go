package workflows

import (
	"context"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/telemetry"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Global spend caps: cerberus.studio.globalDailyUsdMicros and
// cerberus.studio.globalMonthlyUsdMicros defaults.
const (
	DefaultGlobalDailyUsdMicros   int64 = 10_000_000
	DefaultGlobalMonthlyUsdMicros int64 = 100_000_000
)

// spendDenied is WorkflowSpendError.denied(reason).
type spendDenied struct{ reason WorkflowPauseReason }

func (e spendDenied) Error() string { return "denied(" + string(e.reason) + ")" }

// spendReservation is WorkflowSpendReservation.
type spendReservation struct {
	tenant, run uuid.UUID
	amount      int64
	day, month  time.Time
}

// reserveManagedCall is WorkflowSpendService.reserveManagedCall: the run's
// remaining per-run allowance is reserved against the tenant's and the platform's
// day and month buckets in one transaction. Any denial names the bucket that
// refused; any other failure is reported as providerUnavailable.
func (m *Module) reserveManagedCall(ctx context.Context, tenant, run uuid.UUID, tier string) (spendReservation, error) {
	if !m.opts.ManagedInferenceAvailable {
		return spendReservation{}, spendDenied{ProviderUnavailable}
	}
	policy := policyFor(tier)
	if policy.perRunUsdMicros <= 0 {
		return spendReservation{}, spendDenied{RunSpendLimit}
	}
	day, month := periodStarts(m.now())
	res, err := m.reserveTx(ctx, tenant, run, policy, day, month)
	var denied spendDenied
	if err != nil && !errors.As(err, &denied) {
		m.log().Error("workflow spend reservation failed", "error", err.Error())
		return spendReservation{}, spendDenied{ProviderUnavailable}
	}
	return res, err
}

func (m *Module) reserveTx(ctx context.Context, tenant, run uuid.UUID, policy tierPolicy, day, month time.Time) (spendReservation, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return spendReservation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var spent int64
	err = tx.QueryRow(ctx, `SELECT managed_spend_usd_micros FROM workflow_runs
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, run, tenant).Scan(&spent)
	if errors.Is(err, pgx.ErrNoRows) {
		return spendReservation{}, spendDenied{RunSpendLimit}
	}
	if err != nil {
		return spendReservation{}, err
	}
	amount := policy.perRunUsdMicros - spent
	if amount <= 0 {
		return spendReservation{}, spendDenied{RunSpendLimit}
	}
	scope := "tenant:" + ids.Upper(tenant)
	buckets := []struct {
		scope, kind string
		start       time.Time
		limit       int64
		reason      WorkflowPauseReason
	}{
		{scope, "day", day, policy.dailyUsdMicros, DailySpendLimit},
		{scope, "month", month, policy.monthlyUsdMicros, MonthlySpendLimit},
		{"global", "day", day, m.globalDaily(), GlobalSpendLimit},
		{"global", "month", month, m.globalMonthly(), GlobalSpendLimit},
	}
	for _, b := range buckets {
		start := pgtype.Date{Time: b.start, Valid: true}
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_spend_buckets (scope_key, period_kind, period_start)
			VALUES ($1, $2, $3) ON CONFLICT (scope_key, period_kind, period_start) DO NOTHING`, b.scope, b.kind, start); err != nil {
			return spendReservation{}, err
		}
		tag, err := tx.Exec(ctx, `UPDATE workflow_spend_buckets
			SET reserved_usd_micros = reserved_usd_micros + $4, updated_at = NOW()
			WHERE scope_key = $1 AND period_kind = $2 AND period_start = $3
			  AND spent_usd_micros + reserved_usd_micros + $4 <= $5`, b.scope, b.kind, start, amount, b.limit)
		if err != nil {
			return spendReservation{}, err
		}
		if tag.RowsAffected() == 0 {
			return spendReservation{}, spendDenied{b.reason}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return spendReservation{}, err
	}
	return spendReservation{tenant: tenant, run: run, amount: amount, day: day, month: month}, nil
}

// reconcileSpend is WorkflowSpendService.reconcile: the reservation is released
// and the actual cost charged to every bucket and to the run. Failures are logged.
func (m *Module) reconcileSpend(ctx context.Context, r spendReservation, actual int64) {
	actual = max(0, actual)
	err := func() error {
		tx, err := m.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		scope := "tenant:" + ids.Upper(r.tenant)
		for _, b := range []struct {
			scope, kind string
			start       time.Time
		}{{scope, "day", r.day}, {scope, "month", r.month}, {"global", "day", r.day}, {"global", "month", r.month}} {
			if _, err := tx.Exec(ctx, `UPDATE workflow_spend_buckets
				SET reserved_usd_micros = GREATEST(0, reserved_usd_micros - $4),
				    spent_usd_micros = spent_usd_micros + $5, updated_at = NOW()
				WHERE scope_key = $1 AND period_kind = $2 AND period_start = $3`,
				b.scope, b.kind, pgtype.Date{Time: b.start, Valid: true}, r.amount, actual); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_runs
			SET managed_spend_usd_micros = managed_spend_usd_micros + $3, updated_at = NOW()
			WHERE id = $1 AND tenant_id = $2`, r.run, r.tenant, actual); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		m.log().Error("workflow spend reconciliation failed", "run_id", ids.Upper(r.run), "error", err.Error())
		return
	}
	telemetry.Global.Workflow.ManagedSpendUSDMicros.Record(ctx, actual)
}

func (m *Module) globalDaily() int64 {
	if m.opts.GlobalDailyUsdMicros != nil {
		return *m.opts.GlobalDailyUsdMicros
	}
	return DefaultGlobalDailyUsdMicros
}

func (m *Module) globalMonthly() int64 {
	if m.opts.GlobalMonthlyUsdMicros != nil {
		return *m.opts.GlobalMonthlyUsdMicros
	}
	return DefaultGlobalMonthlyUsdMicros
}
