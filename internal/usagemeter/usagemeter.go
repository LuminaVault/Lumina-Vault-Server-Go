// Package usagemeter is Swift's UsageMeterService
// (Sources/App/Billing/UsageMeterService.swift): the usage_meter rows for tokens,
// characters and vision embeds, and the two daily budget checks that read them.
package usagemeter

import (
	"context"
	"encoding/json"
	"log/slog"
	"lumina/internal/ids"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const insertTimeout = 10 * time.Second

const upsertUsage = `INSERT INTO usage_meter (tenant_id, day, model, mtok_in, mtok_out)
VALUES ($1, CURRENT_DATE, $2, $3, $4)
ON CONFLICT (tenant_id, day, model) DO UPDATE
    SET mtok_in  = usage_meter.mtok_in  + EXCLUDED.mtok_in,
        mtok_out = usage_meter.mtok_out + EXCLUDED.mtok_out`

const upsertCharacters = `INSERT INTO usage_meter (tenant_id, day, model, chars_out)
VALUES ($1, CURRENT_DATE, $2, $3)
ON CONFLICT (tenant_id, day, model) DO UPDATE
    SET chars_out = usage_meter.chars_out + EXCLUDED.chars_out`

// SUM over bigint is numeric in Postgres, hence the cast.
const (
	sumTenantToday = `SELECT COALESCE(SUM(mtok_in + mtok_out), 0)::BIGINT
FROM usage_meter
WHERE tenant_id = $1
  AND day = CURRENT_DATE`
	sumSkillToday = sumTenantToday + `
  AND model LIKE $2`
)

// Budgets is usage.* config. The caps are in million tokens.
type Budgets struct {
	// FreeMtokDaily is usage.freeMtokDaily, the trial tier's hard cap. The soft
	// cap, where the model is degraded, is 80% of it.
	FreeMtokDaily float64
	// PerSkillMtokDaily is usage.perSkillMtokDaily. 0 means no cap.
	PerSkillMtokDaily float64
	// DegradeModel is usage.degradeModel.
	DegradeModel string
}

// DefaultBudgets is what App+build falls back to when usage.* isn't set.
func DefaultBudgets() Budgets {
	return Budgets{FreeMtokDaily: 1.0, PerSkillMtokDaily: 0.2, DegradeModel: "hermes-3-small"}
}

// Meter writes and reads usage_meter rows. A nil *Meter does nothing and allows
// everything, as Swift's optional usageMeter does at its call sites.
type Meter struct {
	pool          *pgxpool.Pool
	freeCapTokens int64
	freeSoftCap   int64
	perSkillCap   int64
	degradeModel  string
	now           func() time.Time
}

// New builds a meter with DefaultBudgets.
func New(pool *pgxpool.Pool) *Meter {
	return NewWithBudgets(pool, DefaultBudgets())
}

// NewWithBudgets builds a meter with configured caps.
func NewWithBudgets(pool *pgxpool.Pool, b Budgets) *Meter {
	return &Meter{
		pool:          pool,
		freeCapTokens: int64(b.FreeMtokDaily * 1_000_000),
		freeSoftCap:   int64(b.FreeMtokDaily * 1_000_000 * 0.8),
		perSkillCap:   int64(b.PerSkillMtokDaily * 1_000_000),
		degradeModel:  b.DegradeModel,
		now:           time.Now,
	}
}

// Record adds to today's row for (tenant, model). It does nothing when both
// counts are zero or negative, and logs failures rather than returning them:
// metering must never break the hot path.
func (m *Meter) Record(ctx context.Context, tenant uuid.UUID, model string, tokensIn, tokensOut int64) {
	if m == nil || (tokensIn <= 0 && tokensOut <= 0) {
		return
	}
	if _, err := m.pool.Exec(ctx, upsertUsage, tenant, model, tokensIn, tokensOut); err != nil {
		slog.ErrorContext(ctx, "usage_meter record failed", "tenant_id", ids.Upper(tenant), "error", err)
	}
}

// RecordCharacters adds to today's chars_out for (tenant, model), TTS's metering.
// It does nothing for a count of zero or less, and logs failures rather than
// returning them.
func (m *Meter) RecordCharacters(ctx context.Context, tenant uuid.UUID, model string, charactersOut int64) {
	if m == nil || charactersOut <= 0 {
		return
	}
	if _, err := m.pool.Exec(ctx, upsertCharacters, tenant, model, charactersOut); err != nil {
		slog.ErrorContext(ctx, "usage_meter chars record failed", "tenant_id", ids.Upper(tenant), "error", err)
	}
}

// VisionEmbedModel is the usage_meter.model value for an image embed under the
// active provider kind.
func VisionEmbedModel(kind string) string {
	return "vision.embed:" + kind
}

// RecordVisionEmbed is VisionEmbedService's fire-and-forget usage row: one
// "call" as a constant token equivalent (tokensIn 1), on its own goroutine, so
// the embed response never waits on it. done, if set, runs when the write ends.
func (m *Meter) RecordVisionEmbed(tenant uuid.UUID, kind string, done func()) {
	if m == nil {
		if done != nil {
			done()
		}
		return
	}
	go func() {
		if done != nil {
			defer done()
		}
		ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
		defer cancel()
		m.Record(ctx, tenant, VisionEmbedModel(kind), 1, 0)
	}()
}

// BudgetAction is what a budget check tells the caller to do.
type BudgetAction int

const (
	// BudgetAllow is the happy path.
	BudgetAllow BudgetAction = iota
	// BudgetDegrade means swap BudgetDecision.Model into the request.
	BudgetDegrade
	// BudgetDeny means answer CapExceededError with BudgetDecision.RetryAfter.
	BudgetDeny
)

// BudgetDecision is Swift's BudgetDecision.
type BudgetDecision struct {
	Action     BudgetAction
	Model      string
	RetryAfter time.Duration
}

// CheckBudget is checkBudget, which only LLMController calls. tier is users.tier;
// text that is no tier counts as trial, LLMController's `?? .trial`. Pro, ultimate,
// free and lapsed are always allowed (free and lapsed are bounded by the free lane
// instead), archived is always denied, and trial compares today's total across all
// models with the cap. A failed query allows.
func (m *Meter) CheckBudget(ctx context.Context, tenant uuid.UUID, tier string) BudgetDecision {
	if m == nil {
		return BudgetDecision{Action: BudgetAllow}
	}
	switch tier {
	case "pro", "ultimate", "free", "lapsed":
		return BudgetDecision{Action: BudgetAllow}
	case "archived":
		return BudgetDecision{Action: BudgetDeny, RetryAfter: m.untilUTCMidnight()}
	}
	var used int64
	if err := m.pool.QueryRow(ctx, sumTenantToday, tenant).Scan(&used); err != nil {
		slog.ErrorContext(ctx, "usage_meter budget check failed", "error", err)
		return BudgetDecision{Action: BudgetAllow}
	}
	if used >= m.freeCapTokens {
		slog.InfoContext(ctx, "usage cap exceeded", "tenant_id", ids.Upper(tenant), "used", used, "cap", m.freeCapTokens)
		return BudgetDecision{Action: BudgetDeny, RetryAfter: m.untilUTCMidnight()}
	}
	if used >= m.freeSoftCap {
		slog.InfoContext(ctx, "usage soft cap hit, degrading", "tenant_id", ids.Upper(tenant), "used", used, "soft_cap", m.freeSoftCap)
		return BudgetDecision{Action: BudgetDegrade, Model: m.degradeModel}
	}
	return BudgetDecision{Action: BudgetAllow}
}

// CheckSkillBudget is checkSkillBudget, which only SkillRunner calls: today's rows
// whose model starts "skill:<name>/", the tag SkillRunner records under, against the
// per-skill cap. The name goes into LIKE unescaped, so % and _ in it are wildcards,
// as in Swift. A cap of 0 or a failed query allows.
func (m *Meter) CheckSkillBudget(ctx context.Context, tenant uuid.UUID, skillName string) BudgetDecision {
	if m == nil || m.perSkillCap <= 0 {
		return BudgetDecision{Action: BudgetAllow}
	}
	var used int64
	if err := m.pool.QueryRow(ctx, sumSkillToday, tenant, "skill:"+skillName+"/%").Scan(&used); err != nil {
		slog.ErrorContext(ctx, "usage_meter skill budget check failed", "error", err)
		return BudgetDecision{Action: BudgetAllow}
	}
	if used >= m.perSkillCap {
		slog.InfoContext(ctx, "per-skill budget exceeded", "tenant_id", ids.Upper(tenant), "skill", skillName, "used", used, "cap", m.perSkillCap)
		return BudgetDecision{Action: BudgetDeny, RetryAfter: m.untilUTCMidnight()}
	}
	return BudgetDecision{Action: BudgetAllow}
}

// untilUTCMidnight is at least a second, to satisfy Retry-After.
func (m *Meter) untilUTCMidnight() time.Duration {
	now := m.now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return max(time.Second, midnight.Sub(now))
}

// CapExceededError is UsageCapExceededError: the daily allowance is spent. It
// satisfies llm.UserFacingError. Handlers answer HTTPStatus and Body, with a
// Retry-After header of RetryAfterSeconds.
type CapExceededError struct {
	RetryAfter time.Duration
}

const capExceededMessage = "You've reached today's usage limit. Upgrade for more, or add your own API key in Settings to keep going now."

func (e *CapExceededError) Error() string             { return "usage_cap_exceeded" }
func (e *CapExceededError) ReasonCode() string        { return "usage_cap_exceeded" }
func (e *CapExceededError) UserMessage() string       { return capExceededMessage }
func (e *CapExceededError) RecoveryActions() []string { return []string{"upgrade", "add_key"} }
func (e *CapExceededError) HTTPStatus() int           { return http.StatusTooManyRequests }

// RetryAfterSeconds rounds up, to at least 1.
func (e *CapExceededError) RetryAfterSeconds() int {
	return max(1, int(math.Ceil(e.RetryAfter.Seconds())))
}

// Body is the envelope with sorted keys, as JSONSerialization writes it.
func (e *CapExceededError) Body() []byte {
	type inner struct {
		Code              string   `json:"code"`
		CTA               []string `json:"cta"`
		Message           string   `json:"message"`
		RetryAfterSeconds int      `json:"retryAfterSeconds"`
	}
	b, err := json.Marshal(struct {
		Error inner `json:"error"`
	}{inner{e.ReasonCode(), e.RecoveryActions(), e.UserMessage(), e.RetryAfterSeconds()}})
	if err != nil {
		return nil // every value is a string, an int or []string
	}
	return b
}
