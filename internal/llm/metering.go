package llm

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UsageMeter is the write half of UsageMeterService: daily token counts per
// tenant and model. Its budget checks stay with billing.
type UsageMeter interface {
	Record(ctx context.Context, tenant uuid.UUID, model string, tokensIn, tokensOut int)
}

// CostLedger is the write half of CostLedgerService: daily managed spend per
// tenant and provider, the only USD-denominated meter. Never called for BYOK.
type CostLedger interface {
	Record(ctx context.Context, tenant uuid.UUID, provider ProviderID, usdMicros int64)
}

// TraceTurn is AgentTurnTraceRecorder.Turn. ToolNames nil means the response
// could not be read; empty means the model called nothing. ToolCallCount is for
// callers that can count invocations but not name them.
type TraceTurn struct {
	TenantID               uuid.UUID
	ConversationMessageID  *uuid.UUID
	Provider               ProviderID
	Model                  string
	ToolNames              []string
	ToolCallCount          *int
	FailoverCount          int
	TokensIn               int
	TokensOut              int
	EstimatedCostUsdMicros int64
	LatencyMs              int
	CredentialMode         CredentialMode
}

// TraceRecorder writes one agent_turn_traces row per routed call.
type TraceRecorder interface {
	Record(ctx context.Context, turn TraceTurn)
}

// FailoverSource is provider_failover_events.source.
type FailoverSource string

const (
	FailoverHosted FailoverSource = "hosted"
	FailoverBYO    FailoverSource = "byo"
)

// FailoverNotice is ProviderFailoverNotice: one transition, built when a
// candidate failed recoverably and a later one answered. Published to
// Values.FailoverSink and the FailoverLogger.
type FailoverNotice struct {
	OriginalProvider ProviderKind
	OriginalModel    string
	FallbackProvider ProviderKind
	FallbackModel    string
	ReasonCode       string
	UserMessage      string
	StatusCode       *int
	BodyPreview      *string
	Source           FailoverSource
}

// FailoverLogger persists notices to provider_failover_events.
type FailoverLogger interface {
	Record(ctx context.Context, notice FailoverNotice, tenant *uuid.UUID)
}

// RouterExecutionResult is what Cerberus telemetry records for a finished call.
type RouterExecutionResult struct {
	Provider               ProviderKind
	Model                  string
	Status                 string
	TokensIn               int
	TokensOut              int
	EstimatedCostUsdMicros int64
	LatencyMs              int
	UsageEstimated         bool
	FallbackCount          int
}

// RouterTelemetry is the RouterTelemetryService hook: settling or releasing a
// Cerberus budget reservation. Only decisions with Cerberus metadata reach it.
type RouterTelemetry interface {
	Complete(ctx context.Context, metadata *CerberusMetadata, result RouterExecutionResult)
	Release(ctx context.Context, tenant uuid.UUID, reservedUsdMicros int64)
}

// EnsembleResult is the part of ParallelExecutor's result the transport reads.
type EnsembleResult struct {
	Metadata               Metadata
	Route                  Route
	Status                 string // "completed" or otherwise
	TokensIn               int
	TokensOut              int
	EstimatedCostUsdMicros int64
	LatencyMs              int
}

// EnsembleExecutor is the ParallelExecutor hook. Nil on Transport means every
// ensemble attempt counts as failed.
type EnsembleExecutor interface {
	Execute(ctx context.Context, payload []byte, sessionKey, sessionID string, metadata *CerberusMetadata, prompt string) (*EnsembleResult, error)
}

// PGMeters implements UsageMeter, CostLedger, TraceRecorder and FailoverLogger on
// Postgres. Every write is fire-and-forget: failures are logged and swallowed,
// because a metering blip must never break a request.
type PGMeters struct{ Pool *pgxpool.Pool }

// UsageMeter returns the usage_meter writer.
func (m PGMeters) UsageMeter() UsageMeter { return pgUsageMeter(m) }

// CostLedger returns the cost_ledger writer.
func (m PGMeters) CostLedger() CostLedger { return pgCostLedger(m) }

// TraceRecorder returns the agent_turn_traces writer.
func (m PGMeters) TraceRecorder() TraceRecorder { return pgTraceRecorder(m) }

// FailoverLogger returns the provider_failover_events writer.
func (m PGMeters) FailoverLogger() FailoverLogger { return pgFailoverLogger(m) }

type (
	pgUsageMeter     PGMeters
	pgCostLedger     PGMeters
	pgTraceRecorder  PGMeters
	pgFailoverLogger PGMeters
)

func (m pgUsageMeter) Record(ctx context.Context, tenant uuid.UUID, model string, tokensIn, tokensOut int) {
	if tokensIn <= 0 && tokensOut <= 0 {
		return
	}
	_, err := m.Pool.Exec(ctx, `INSERT INTO usage_meter (tenant_id, day, model, mtok_in, mtok_out)
		VALUES ($1, CURRENT_DATE, $2, $3, $4)
		ON CONFLICT (tenant_id, day, model) DO UPDATE
			SET mtok_in  = usage_meter.mtok_in  + EXCLUDED.mtok_in,
			    mtok_out = usage_meter.mtok_out + EXCLUDED.mtok_out`,
		tenant, model, int64(tokensIn), int64(tokensOut))
	if err != nil {
		slog.ErrorContext(ctx, "usage_meter record failed", "tenant_id", tenant, "error", err)
	}
}

func (m pgCostLedger) Record(ctx context.Context, tenant uuid.UUID, provider ProviderID, usdMicros int64) {
	_, err := m.Pool.Exec(ctx, `INSERT INTO cost_ledger (tenant_id, day, provider, usd_micros, calls)
		VALUES ($1, CURRENT_DATE, $2, $3, $4)
		ON CONFLICT (tenant_id, day, provider) DO UPDATE
			SET usd_micros = cost_ledger.usd_micros + EXCLUDED.usd_micros,
			    calls      = cost_ledger.calls      + EXCLUDED.calls`,
		tenant, string(provider), usdMicros, int64(1))
	if err != nil {
		slog.ErrorContext(ctx, "cost_ledger record failed", "tenant_id", tenant, "provider", provider, "error", err)
	}
}

func (m pgTraceRecorder) Record(ctx context.Context, turn TraceTurn) {
	var toolCalls []byte
	if record := ToolRecord(turn.ToolNames, turn.ToolCallCount); record != nil {
		encoded, err := json.Marshal(record)
		if err != nil {
			slog.DebugContext(ctx, "agent turn trace not recorded", "error", err)
			return
		}
		toolCalls = encoded
	}
	var mode *string
	if turn.CredentialMode != ModeUnset {
		mode = (*string)(&turn.CredentialMode)
	}
	_, err := m.Pool.Exec(ctx, `INSERT INTO agent_turn_traces
		(id, tenant_id, conversation_message_id, provider, model, tool_calls, failover_count,
		 tokens_in, tokens_out, estimated_cost_usd_micros, latency_ms, credential_mode, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		uuid.New(), turn.TenantID, turn.ConversationMessageID, string(turn.Provider), turn.Model, toolCalls,
		max(0, turn.FailoverCount), int64(max(0, turn.TokensIn)), int64(max(0, turn.TokensOut)),
		max(0, turn.EstimatedCostUsdMicros), int64(max(0, turn.LatencyMs)), mode, time.Now())
	if err != nil {
		slog.DebugContext(ctx, "agent turn trace not recorded", "error", err)
	}
}

func (m pgFailoverLogger) Record(ctx context.Context, notice FailoverNotice, tenant *uuid.UUID) {
	_, err := m.Pool.Exec(ctx, `INSERT INTO provider_failover_events
		(id, tenant_id, provider, model, status_code, error_code, fallback_provider, fallback_model, source, happened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		uuid.New(), tenant, string(notice.OriginalProvider), notice.OriginalModel, notice.StatusCode, notice.ReasonCode,
		string(notice.FallbackProvider), notice.FallbackModel, string(notice.Source), time.Now())
	if err != nil {
		slog.ErrorContext(ctx, "provider_failover_events insert failed", "error", err)
	}
}
