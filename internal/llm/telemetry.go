package llm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RouterBudgetState is the outcome of a monthly budget reservation.
type RouterBudgetState int

const (
	BudgetAllowed RouterBudgetState = iota
	BudgetSoftLimit
	BudgetDenied
)

// RouterTelemetryService is RouterTelemetryService: prompt-free router telemetry
// and the atomic monthly budget reservation. Its writes never fail a request.
type RouterTelemetryService struct{ pool *pgxpool.Pool }

var _ RouterTelemetry = (*RouterTelemetryService)(nil)

// NewRouterTelemetryService wires the service.
func NewRouterTelemetryService(pool *pgxpool.Pool) *RouterTelemetryService {
	return &RouterTelemetryService{pool: pool}
}

// Reserve is reserve(tenantID:predictedUsdMicros:policy:). The hard-limit update is
// conditional, so concurrent requests cannot both reserve the same balance. A
// database failure denies.
func (s *RouterTelemetryService) Reserve(ctx context.Context, tenant uuid.UUID, predictedUsdMicros int64, policy RouterBudget) RouterBudgetState {
	if predictedUsdMicros <= 0 {
		return BudgetAllowed
	}
	state, err := s.reserve(ctx, tenant, predictedUsdMicros, policy)
	if err != nil {
		slog.ErrorContext(ctx, "cerberus budget reservation failed", "error", err)
		return BudgetDenied
	}
	return state
}

func (s *RouterTelemetryService) reserve(ctx context.Context, tenant uuid.UUID, predicted int64, policy RouterBudget) (RouterBudgetState, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO router_monthly_usage (tenant_id, month)
		VALUES ($1, date_trunc('month', CURRENT_DATE)::date)
		ON CONFLICT (tenant_id, month) DO NOTHING`, tenant); err != nil {
		return BudgetDenied, err
	}
	softState := func(spent int64) RouterBudgetState {
		if policy.SoftLimitUsdMicros != nil && spent >= *policy.SoftLimitUsdMicros {
			return BudgetSoftLimit
		}
		return BudgetAllowed
	}
	if hard := policy.HardLimitUsdMicros; hard != nil && *hard > 0 {
		var spent int64
		err := s.pool.QueryRow(ctx, `UPDATE router_monthly_usage
			SET reserved_usd_micros = reserved_usd_micros + $1::bigint, updated_at = NOW()
			WHERE tenant_id = $2
			  AND month = date_trunc('month', CURRENT_DATE)::date
			  AND managed_usd_micros + byok_usd_micros + reserved_usd_micros + $1::bigint <= $3::bigint
			RETURNING managed_usd_micros + byok_usd_micros AS spent`, predicted, tenant, *hard).Scan(&spent)
		if errors.Is(err, pgx.ErrNoRows) {
			return BudgetDenied, nil
		}
		if err != nil {
			return BudgetDenied, err
		}
		return softState(spent), nil
	}
	var spent int64
	err := s.pool.QueryRow(ctx, `SELECT managed_usd_micros + byok_usd_micros AS spent
		FROM router_monthly_usage
		WHERE tenant_id = $1 AND month = date_trunc('month', CURRENT_DATE)::date`, tenant).Scan(&spent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BudgetDenied, err
	}
	return softState(spent), nil
}

// Release is release(tenantID:reservedUsdMicros:).
func (s *RouterTelemetryService) Release(ctx context.Context, tenant uuid.UUID, reservedUsdMicros int64) {
	if reservedUsdMicros <= 0 {
		return
	}
	_, err := s.pool.Exec(ctx, `UPDATE router_monthly_usage
		SET reserved_usd_micros = GREATEST(0, reserved_usd_micros - $1::bigint), updated_at = NOW()
		WHERE tenant_id = $2 AND month = date_trunc('month', CURRENT_DATE)::date`, reservedUsdMicros, tenant)
	if err != nil {
		slog.ErrorContext(ctx, "cerberus budget release failed", "error", err)
	}
}

// settleManaged and settleBYOK differ only in the spend column.
const (
	settleManaged = `INSERT INTO router_monthly_usage
		(tenant_id, month, managed_usd_micros, requests, successful_requests, fallback_count, tokens_in, tokens_out, latency_ms_total)
		VALUES ($1, date_trunc('month', CURRENT_DATE)::date, $2, 1, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, month) DO UPDATE SET
			managed_usd_micros = router_monthly_usage.managed_usd_micros + EXCLUDED.managed_usd_micros,
			reserved_usd_micros = GREATEST(0, router_monthly_usage.reserved_usd_micros - $8::bigint),
			requests = router_monthly_usage.requests + 1,
			successful_requests = router_monthly_usage.successful_requests + EXCLUDED.successful_requests,
			fallback_count = router_monthly_usage.fallback_count + EXCLUDED.fallback_count,
			tokens_in = router_monthly_usage.tokens_in + EXCLUDED.tokens_in,
			tokens_out = router_monthly_usage.tokens_out + EXCLUDED.tokens_out,
			latency_ms_total = router_monthly_usage.latency_ms_total + EXCLUDED.latency_ms_total,
			updated_at = NOW()`
	settleBYOK = `INSERT INTO router_monthly_usage
		(tenant_id, month, byok_usd_micros, requests, successful_requests, fallback_count, tokens_in, tokens_out, latency_ms_total)
		VALUES ($1, date_trunc('month', CURRENT_DATE)::date, $2, 1, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, month) DO UPDATE SET
			byok_usd_micros = router_monthly_usage.byok_usd_micros + EXCLUDED.byok_usd_micros,
			reserved_usd_micros = GREATEST(0, router_monthly_usage.reserved_usd_micros - $8::bigint),
			requests = router_monthly_usage.requests + 1,
			successful_requests = router_monthly_usage.successful_requests + EXCLUDED.successful_requests,
			fallback_count = router_monthly_usage.fallback_count + EXCLUDED.fallback_count,
			tokens_in = router_monthly_usage.tokens_in + EXCLUDED.tokens_in,
			tokens_out = router_monthly_usage.tokens_out + EXCLUDED.tokens_out,
			latency_ms_total = router_monthly_usage.latency_ms_total + EXCLUDED.latency_ms_total,
			updated_at = NOW()`
)

// Complete is complete(metadata:result:): one execution row, one analytics event,
// and the monthly aggregate, which also settles the reservation. A failure anywhere
// releases the reservation instead.
func (s *RouterTelemetryService) Complete(ctx context.Context, m *CerberusMetadata, result RouterExecutionResult) {
	if err := s.complete(ctx, m, result); err != nil {
		slog.ErrorContext(ctx, "cerberus execution telemetry failed", "error", err)
		s.Release(ctx, m.TenantID, m.BudgetReservationUsdMicros)
	}
}

func (s *RouterTelemetryService) complete(ctx context.Context, m *CerberusMetadata, result RouterExecutionResult) error {
	provider, dimProvider := (*string)(nil), "unknown"
	if result.Provider != "" {
		name := string(result.Provider)
		provider, dimProvider = &name, name
	}
	model, dimModel := (*string)(nil), "unknown"
	if result.Model != "" {
		model, dimModel = &result.Model, result.Model
	}
	dimensions, err := json.Marshal(map[string]string{"provider": dimProvider, "model": dimModel, "status": result.Status})
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO router_executions
		(id, tenant_id, vault_id, actor_user_id, profile_id, rule_id, surface, task_type, strategy, status,
		 selected_provider, selected_model, tokens_in, tokens_out,
		 estimated_cost_usd_micros, latency_ms, usage_estimated, fallback_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			selected_provider = EXCLUDED.selected_provider,
			selected_model = EXCLUDED.selected_model,
			tokens_in = EXCLUDED.tokens_in,
			tokens_out = EXCLUDED.tokens_out,
			estimated_cost_usd_micros = EXCLUDED.estimated_cost_usd_micros,
			latency_ms = EXCLUDED.latency_ms,
			usage_estimated = EXCLUDED.usage_estimated,
			fallback_count = EXCLUDED.fallback_count`,
		m.ExecutionID, m.TenantID, m.VaultID, m.ActorUserID, m.ProfileID, m.RuleID, m.Surface, m.TaskType, m.Strategy, result.Status,
		provider, model, int64(result.TokensIn), int64(result.TokensOut),
		result.EstimatedCostUsdMicros, int64(result.LatencyMs), result.UsageEstimated, result.FallbackCount)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO analytics_events
		(vault_id, actor_user_id, event_name, source, dimensions, idempotency_key)
		VALUES ($1, $2, 'ai_request_completed', 'server', $3::jsonb, $4)
		ON CONFLICT (actor_user_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
		m.VaultID, m.ActorUserID, string(dimensions), swiftUUID(m.ExecutionID))
	if err != nil {
		return err
	}
	settle := settleBYOK
	if m.Mode == ModeManaged {
		settle = settleManaged
	}
	successful := int64(0)
	if result.Status == "ok" {
		successful = 1
	}
	_, err = s.pool.Exec(ctx, settle, m.TenantID, result.EstimatedCostUsdMicros, successful, int64(result.FallbackCount),
		int64(result.TokensIn), int64(result.TokensOut), int64(result.LatencyMs), m.BudgetReservationUsdMicros)
	return err
}
