package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouterTelemetry(t *testing.T) {
	ctx, pool, newUser := llmDB(t)
	service := NewRouterTelemetryService(pool)
	const month = `date_trunc('month', CURRENT_DATE)::date`
	limit := func(n int64) *int64 { return &n }

	usage := func(t *testing.T, tenant uuid.UUID) (managed, byok, reserved int64) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `SELECT managed_usd_micros, byok_usd_micros, reserved_usd_micros FROM router_monthly_usage WHERE tenant_id = $1 AND month = `+month, tenant).Scan(&managed, &byok, &reserved))
		return managed, byok, reserved
	}
	// vaultFor makes the personal vault analytics_events.vault_id points at.
	vaultFor := func(t *testing.T, tenant uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $2, 'v', NOW(), NOW())`, id, tenant)
		require.NoError(t, err)
		return id
	}
	profileFor := func(t *testing.T, tenant uuid.UUID) uuid.UUID {
		t.Helper()
		p, err := NewRouterProfileRepository(pool, NewPreferenceRepository(pool), ManagedModel).Resolve(ctx, tenant, nil, "", "")
		require.NoError(t, err)
		return p.ID
	}
	metadata := func(t *testing.T, tenant uuid.UUID, mode CredentialMode, reservation int64) *CerberusMetadata {
		t.Helper()
		return &CerberusMetadata{
			ExecutionID: uuid.New(), TenantID: tenant, VaultID: vaultFor(t, tenant), ActorUserID: tenant, ProfileID: profileFor(t, tenant),
			Surface: SurfaceChat, TaskType: string(TaskCoding), Strategy: string(ActionSequential),
			Mode: mode, BudgetReservationUsdMicros: reservation,
		}
	}
	result := RouterExecutionResult{Status: "ok", Provider: OpenRouter, Model: "m", TokensIn: 10, TokensOut: 20, EstimatedCostUsdMicros: 300, LatencyMs: 40, UsageEstimated: true, FallbackCount: 2}

	t.Run("a zero prediction is allowed without touching the table", func(t *testing.T) {
		tenant := newUser()
		assert.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 0, RouterBudget{HardLimitUsdMicros: limit(1)}))
		var rows int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM router_monthly_usage WHERE tenant_id = $1`, tenant).Scan(&rows))
		assert.Zero(t, rows)
	})
	t.Run("without a hard limit nothing is reserved", func(t *testing.T) {
		tenant := newUser()
		assert.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 100, RouterBudget{}))
		_, _, reserved := usage(t, tenant)
		assert.Zero(t, reserved)
	})
	t.Run("a hard limit reserves up to the ceiling, then denies", func(t *testing.T) {
		tenant := newUser()
		policy := RouterBudget{HardLimitUsdMicros: limit(150)}
		assert.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 100, policy))
		assert.Equal(t, BudgetDenied, service.Reserve(ctx, tenant, 100, policy))
		_, _, reserved := usage(t, tenant)
		assert.Equal(t, int64(100), reserved)
		assert.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 50, policy))
	})
	t.Run("a soft limit reports on spend, not on reservations", func(t *testing.T) {
		tenant := newUser()
		assert.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 100, RouterBudget{SoftLimitUsdMicros: limit(10), HardLimitUsdMicros: limit(1000)}))
		_, err := pool.Exec(ctx, `UPDATE router_monthly_usage SET byok_usd_micros = 10 WHERE tenant_id = $1`, tenant)
		require.NoError(t, err)
		assert.Equal(t, BudgetSoftLimit, service.Reserve(ctx, tenant, 100, RouterBudget{SoftLimitUsdMicros: limit(10), HardLimitUsdMicros: limit(1000)}))
		assert.Equal(t, BudgetSoftLimit, service.Reserve(ctx, tenant, 100, RouterBudget{SoftLimitUsdMicros: limit(10)}))
		assert.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 100, RouterBudget{SoftLimitUsdMicros: limit(11)}))
	})
	t.Run("a zero hard limit means none", func(t *testing.T) {
		tenant := newUser()
		assert.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 100, RouterBudget{HardLimitUsdMicros: limit(0)}))
	})
	t.Run("a database failure denies", func(t *testing.T) {
		broken, cancel := contextCancelled(t)
		defer cancel()
		assert.Equal(t, BudgetDenied, service.Reserve(broken, newUser(), 100, RouterBudget{}))
	})
	t.Run("release floors at zero and ignores nothing to release", func(t *testing.T) {
		tenant := newUser()
		require.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 100, RouterBudget{HardLimitUsdMicros: limit(1000)}))
		service.Release(ctx, tenant, 0)
		_, _, reserved := usage(t, tenant)
		assert.Equal(t, int64(100), reserved)
		service.Release(ctx, tenant, 40)
		_, _, reserved = usage(t, tenant)
		assert.Equal(t, int64(60), reserved)
		service.Release(ctx, tenant, 1000)
		_, _, reserved = usage(t, tenant)
		assert.Zero(t, reserved)
	})
	t.Run("complete writes the execution, the event and the aggregate", func(t *testing.T) {
		tenant := newUser()
		require.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 500, RouterBudget{HardLimitUsdMicros: limit(1000)}))
		m := metadata(t, tenant, ModeManaged, 500)
		service.Complete(ctx, m, result)

		var (
			status, provider, model, surface, task, strategy string
			tokensIn, tokensOut, cost, latency               int64
			fallbacks                                        int
			estimated                                        bool
		)
		require.NoError(t, pool.QueryRow(ctx, `SELECT status, selected_provider, selected_model, surface, task_type, strategy, tokens_in, tokens_out, estimated_cost_usd_micros, latency_ms, fallback_count, usage_estimated
			FROM router_executions WHERE id = $1`, m.ExecutionID).Scan(&status, &provider, &model, &surface, &task, &strategy, &tokensIn, &tokensOut, &cost, &latency, &fallbacks, &estimated))
		assert.Equal(t, []string{"ok", "openRouter", "m", "chat", "coding", "sequential"}, []string{status, provider, model, surface, task, strategy})
		assert.Equal(t, []int64{10, 20, 300, 40}, []int64{tokensIn, tokensOut, cost, latency})
		assert.Equal(t, 2, fallbacks)
		assert.True(t, estimated)

		var dimensions, key, source string
		require.NoError(t, pool.QueryRow(ctx, `SELECT dimensions::text, idempotency_key, source FROM analytics_events WHERE actor_user_id = $1 AND event_name = 'ai_request_completed'`, tenant).Scan(&dimensions, &key, &source))
		assert.JSONEq(t, `{"provider":"openRouter","model":"m","status":"ok"}`, dimensions)
		assert.Equal(t, strings.ToUpper(m.ExecutionID.String()), key, "uuidString is uppercase, or both servers count the event")
		assert.Equal(t, "server", source)

		managed, byok, reserved := usage(t, tenant)
		assert.Equal(t, int64(300), managed)
		assert.Zero(t, byok)
		assert.Zero(t, reserved, "the reservation settles")
		var requests, successes, fallbackTotal, in, out, latencyTotal int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT requests, successful_requests, fallback_count, tokens_in, tokens_out, latency_ms_total FROM router_monthly_usage WHERE tenant_id = $1 AND month = `+month, tenant).
			Scan(&requests, &successes, &fallbackTotal, &in, &out, &latencyTotal))
		assert.Equal(t, []int64{1, 1, 2, 10, 20, 40}, []int64{requests, successes, fallbackTotal, in, out, latencyTotal})
	})
	t.Run("a byok failure goes to the byok column and counts no success", func(t *testing.T) {
		tenant := newUser()
		m := metadata(t, tenant, ModeBYOK, 0)
		failed := RouterExecutionResult{Status: "failed", EstimatedCostUsdMicros: 7}
		service.Complete(ctx, m, failed)
		managed, byok, _ := usage(t, tenant)
		assert.Zero(t, managed)
		assert.Equal(t, int64(7), byok)
		var dimensions string
		var provider, model *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT dimensions::text FROM analytics_events WHERE actor_user_id = $1`, tenant).Scan(&dimensions))
		assert.JSONEq(t, `{"provider":"unknown","model":"unknown","status":"failed"}`, dimensions)
		require.NoError(t, pool.QueryRow(ctx, `SELECT selected_provider, selected_model FROM router_executions WHERE id = $1`, m.ExecutionID).Scan(&provider, &model))
		assert.Nil(t, provider, "a missing provider is NULL, not unknown")
		assert.Nil(t, model)
		var successes int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT successful_requests FROM router_monthly_usage WHERE tenant_id = $1 AND month = `+month, tenant).Scan(&successes))
		assert.Zero(t, successes)
	})
	t.Run("completing the same execution twice updates it and adds no second event", func(t *testing.T) {
		tenant := newUser()
		m := metadata(t, tenant, ModeManaged, 0)
		service.Complete(ctx, m, result)
		again := result
		again.Status = "degraded"
		service.Complete(ctx, m, again)
		var events int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM analytics_events WHERE actor_user_id = $1`, tenant).Scan(&events))
		assert.Equal(t, 1, events)
		var status string
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM router_executions WHERE id = $1`, m.ExecutionID).Scan(&status))
		assert.Equal(t, "degraded", status)
		var requests int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT requests FROM router_monthly_usage WHERE tenant_id = $1 AND month = `+month, tenant).Scan(&requests))
		assert.Equal(t, int64(2), requests, "the aggregate has no idempotency key, as in Swift")
	})
	t.Run("QUIRK: BYO Hermes's random profile id fails the insert, so such a turn records nothing", func(t *testing.T) {
		tenant := newUser()
		m := metadata(t, tenant, ModeBYOK, 0)
		m.ProfileID = uuid.New() // what the deferred-to-Hermes decision carries
		service.Complete(ctx, m, result)
		var executions, events, months int
		require.NoError(t, pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM router_executions WHERE tenant_id = $1),
			(SELECT count(*) FROM analytics_events WHERE actor_user_id = $1),
			(SELECT count(*) FROM router_monthly_usage WHERE tenant_id = $1)`, tenant).Scan(&executions, &events, &months))
		assert.Zero(t, executions+events+months)
	})
	t.Run("QUIRK: a vault that does not exist loses the aggregate and releases the reservation", func(t *testing.T) {
		tenant := newUser()
		require.Equal(t, BudgetAllowed, service.Reserve(ctx, tenant, 500, RouterBudget{HardLimitUsdMicros: limit(1000)}))
		m := metadata(t, tenant, ModeManaged, 500)
		m.VaultID = uuid.New()
		service.Complete(ctx, m, result)
		managed, _, reserved := usage(t, tenant)
		assert.Zero(t, managed, "the failing event insert skips the aggregate")
		assert.Zero(t, reserved)
	})
}

func contextCancelled(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx, cancel
}
