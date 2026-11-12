package workflows

import (
	"context"
	"lumina/internal/ids"
	"lumina/internal/modules/workflows/db"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// tierPolicy is WorkflowTierPolicy.
type tierPolicy struct {
	activeRunLimit         int
	minimumScheduleMinutes int
	perRunUsdMicros        int64
	dailyUsdMicros         int64
	monthlyUsdMicros       int64
}

func policyFor(tier string) tierPolicy {
	switch tier {
	case tierPro:
		return tierPolicy{activeRunLimit: 1, minimumScheduleMinutes: 60, perRunUsdMicros: 200_000, dailyUsdMicros: 500_000, monthlyUsdMicros: 2_000_000}
	case tierUltimate:
		return tierPolicy{activeRunLimit: 3, minimumScheduleMinutes: 5, perRunUsdMicros: 1_000_000, dailyUsdMicros: 2_000_000, monthlyUsdMicros: 8_000_000}
	default:
		return tierPolicy{}
	}
}

// ensureCanEnqueue is WorkflowSpendService.ensureCanEnqueue. The reservation half
// (reserveManagedCall, reconcile) is in reserve.go.
func (m *Module) ensureCanEnqueue(ctx context.Context, tenant uuid.UUID, tier string) (tierPolicy, error) {
	policy := policyFor(tier)
	if policy.activeRunLimit <= 0 {
		return policy, errActiveRunLimit
	}
	active, err := m.q.CountActiveRuns(ctx, tenant)
	if err != nil {
		return policy, err
	}
	if int(active) >= policy.activeRunLimit {
		return policy, errActiveRunLimit
	}
	return policy, nil
}

// limitsFor is WorkflowSpendService.limits: a failed read counts as zero, as try? does.
func (m *Module) limitsFor(ctx context.Context, tenant uuid.UUID, tier string) WorkflowLimitsDTO {
	policy := policyFor(tier)
	active, err := m.q.CountActiveRuns(ctx, tenant)
	if err != nil {
		active = 0
	}
	daily, monthly := m.tenantSpend(ctx, tenant)
	return WorkflowLimitsDTO{
		Tier:                      WorkflowLimitsDTOTier(tier),
		CanAuthor:                 policy.activeRunLimit > 0,
		ActiveRuns:                int(active),
		ActiveRunLimit:            policy.activeRunLimit,
		MinimumScheduleMinutes:    policy.minimumScheduleMinutes,
		PerRunLimitUsdMicros:      policy.perRunUsdMicros,
		DailyLimitUsdMicros:       policy.dailyUsdMicros,
		DailySpentUsdMicros:       daily,
		MonthlyLimitUsdMicros:     policy.monthlyUsdMicros,
		MonthlySpentUsdMicros:     monthly,
		ManagedInferenceAvailable: m.opts.ManagedInferenceAvailable,
		FreeFallbackActive:        !m.opts.ManagedInferenceAvailable,
	}
}

func (m *Module) tenantSpend(ctx context.Context, tenant uuid.UUID) (daily, monthly int64) {
	day, month := periodStarts(m.now())
	rows, err := m.q.TenantSpend(ctx, db.TenantSpendParams{
		ScopeKey: "tenant:" + ids.Upper(tenant),
		Day:      pgtype.Date{Time: day, Valid: true},
		Month:    pgtype.Date{Time: month, Valid: true},
	})
	if err != nil {
		return 0, 0
	}
	for _, row := range rows {
		switch row.PeriodKind {
		case "day":
			daily = row.SpentUsdMicros
		case "month":
			monthly = row.SpentUsdMicros
		}
	}
	return daily, monthly
}

// periodStarts is WorkflowSpendService.periodStarts: UTC.
func periodStarts(now time.Time) (day, month time.Time) {
	y, mo, d := now.UTC().Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC), time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC)
}
