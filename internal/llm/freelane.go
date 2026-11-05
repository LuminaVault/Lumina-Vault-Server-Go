package llm

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FreeLaneCatalog: the zero-cost lane, for users neither paying nor bringing a
// key, and for when platform managed inference is gone.
//
// The two OpenRouter slugs deliberately share one leg. FreeLaneGate meters one
// counter per leg and OpenRouter's :free allowance is account-wide across slugs,
// so a second leg would spend twice the allowance. The NIM leg is a real, priced
// model on finite signup credits: its daily ceiling is a spend cap.
const (
	FreeLaneOpenRouterModel          = "nvidia/nemotron-3-ultra-550b-a55b:free"
	FreeLaneOpenRouterSecondaryModel = "nvidia/nemotron-3-super-120b-a12b:free"
	FreeLaneNvidiaModel              = "nvidia/nemotron-3-super-120b-a12b"
	// HermesMinimumContextWindow: Hermes refuses a turn below this, so every
	// free slug has to clear it.
	HermesMinimumContextWindow = 64000
)

// FreeLaneLeg is FreeLaneCatalog.Leg. The values appear in bucket scope keys.
type FreeLaneLeg string

const (
	LegOpenRouterFree FreeLaneLeg = "openRouterFree"
	LegNvidiaDirect   FreeLaneLeg = "nvidiaDirect"
)

// freeLaneLegs is Leg.allCases.
var freeLaneLegs = []FreeLaneLeg{LegOpenRouterFree, LegNvidiaDirect}

// FreeLaneRoute is FreeLaneCatalog.Route.
type FreeLaneRoute struct {
	Leg      FreeLaneLeg
	Provider ProviderID
	Model    string
}

// FreeLaneRoutes is FreeLaneCatalog.routes: the failover order. A non-empty
// preferring promotes every route on that leg, in order, ahead of the others.
func FreeLaneRoutes(openRouterModel, openRouterSecondaryModel, nvidiaModel string, preferring FreeLaneLeg) []FreeLaneRoute {
	all := []FreeLaneRoute{
		{LegOpenRouterFree, ProviderID(OpenRouter), openRouterModel},
		{LegOpenRouterFree, ProviderID(OpenRouter), openRouterSecondaryModel},
		{LegNvidiaDirect, ProviderID(Nvidia), nvidiaModel},
	}
	if preferring == "" {
		return all
	}
	promoted := make([]FreeLaneRoute, 0, len(all))
	for _, r := range all {
		if r.Leg == preferring {
			promoted = append(promoted, r)
		}
	}
	if len(promoted) == 0 {
		return all
	}
	for _, r := range all {
		if r.Leg != preferring {
			promoted = append(promoted, r)
		}
	}
	return promoted
}

// FreeLaneTrigger says why a request was pushed onto the lane.
type FreeLaneTrigger string

const (
	// TriggerNotEntitled is the ordinary case.
	TriggerNotEntitled FreeLaneTrigger = "notEntitled"
	// TriggerPlatformUnavailable degrades even paying users, and should alert.
	TriggerPlatformUnavailable FreeLaneTrigger = "platformUnavailable"
)

// FreeLaneVerdict is a lane decision. The stored preference is ignored for this
// request, never overwritten.
type FreeLaneVerdict struct {
	Trigger FreeLaneTrigger
	Forced  bool
}

// FreeLaneInput is FreeLanePolicy.Input. EffectiveTier is entitlement.EffectiveTier.
type FreeLaneInput struct {
	EffectiveTier                string
	RequestedMode                CredentialMode
	HasUsableUserCredential      bool
	PlatformPaidManagedAvailable bool
	FreeLaneEnabled              bool
}

// EvaluateFreeLane is FreeLanePolicy.evaluate, the one statement of who pays:
// paying or BYOK with a real key means the user's choice, everyone else the lane.
// Nil honours the user's choice. Pure.
func EvaluateFreeLane(in FreeLaneInput) *FreeLaneVerdict {
	if !in.FreeLaneEnabled {
		return nil
	}
	// BYOK with a real key pays its own provider, on every tier.
	if in.RequestedMode == ModeBYOK && in.HasUsableUserCredential {
		return nil
	}
	// Archived is refused with a 402 before routing; a route would only mask it.
	if in.EffectiveTier == "archived" {
		return nil
	}
	// BYOK with no key: a working free answer beats a 403 dead end.
	if in.RequestedMode == ModeBYOK && !in.HasUsableUserCredential {
		return &FreeLaneVerdict{TriggerNotEntitled, true}
	}
	switch in.EffectiveTier {
	case "pro", "ultimate", "trial":
		if in.PlatformPaidManagedAvailable {
			return nil
		}
		return &FreeLaneVerdict{TriggerPlatformUnavailable, true}
	}
	return &FreeLaneVerdict{TriggerNotEntitled, true}
}

// HonoursUserChoice is FreeLanePolicy.honoursUserChoice, for surfaces reporting
// the effective route rather than the stored preference.
func HonoursUserChoice(in FreeLaneInput) bool { return EvaluateFreeLane(in) == nil }

// FreeLaneRecoveryActions is FreeLanePolicy.recoveryActions: the cta tokens that
// are real for this caller when the lane cannot serve them.
func FreeLaneRecoveryActions(effectiveTier string, requestedMode CredentialMode, trigger FreeLaneTrigger) []string {
	unpaid := effectiveTier == "free" || effectiveTier == "lapsed"
	managedWouldWork := !unpaid && requestedMode == ModeBYOK && trigger != TriggerPlatformUnavailable
	var actions []string
	if unpaid {
		actions = append(actions, "upgrade")
	}
	actions = append(actions, "add_key")
	if managedWouldWork {
		actions = append(actions, "switch_to_managed")
	}
	return actions
}

// FreeLaneLimits is FreeLaneGate.Limits.
type FreeLaneLimits struct {
	PerUserDaily int64
	PerLegDaily  map[FreeLaneLeg]int64
}

// FreeLaneOutcome is FreeLaneGate.Outcome: a granted leg, or exhaustion with the
// seconds until the buckets roll.
type FreeLaneOutcome struct {
	Granted           FreeLaneLeg // empty means exhausted
	RetryAfterSeconds float64
}

// FreeLaneGate holds the per-user daily grace and the per-leg platform ceilings.
// Both exist because OpenRouter's free limits are account-wide.
//
// Storage is workflow_spend_buckets, in a freelane: scope-key namespace. The unit
// in spent_usd_micros is requests, not money. reserved_usd_micros stays 0: a free
// request is spent when dispatched, and a failed one still used the quota.
type FreeLaneGate struct {
	pool   *pgxpool.Pool
	limits FreeLaneLimits
	now    func() time.Time
}

// NewFreeLaneGate builds a gate on the shared buckets.
func NewFreeLaneGate(pool *pgxpool.Pool, limits FreeLaneLimits) *FreeLaneGate {
	return &FreeLaneGate{pool: pool, limits: limits, now: time.Now}
}

// Claim charges the tenant's grace, then the first leg with room. A grace
// charged before every leg turned out full stays charged. It fails open on a
// database error: a metering outage must not take chat down for every non-paying
// user, and what it grants costs nothing but provider rate limits.
func (g *FreeLaneGate) Claim(ctx context.Context, tenant uuid.UUID, legs []FreeLaneLeg) FreeLaneOutcome {
	exhausted := func() FreeLaneOutcome {
		return FreeLaneOutcome{RetryAfterSeconds: secondsUntilUTCMidnight(g.now())}
	}
	if len(legs) == 0 {
		return exhausted()
	}
	first := legs[0]
	charged, err := g.charge(ctx, freeLaneTenantScopeKey(tenant), g.limits.PerUserDaily)
	if err != nil {
		slog.ErrorContext(ctx, "free lane gate failed; failing open", "leg", first, "error", err)
		return FreeLaneOutcome{Granted: first}
	}
	if !charged {
		return exhausted()
	}
	for _, leg := range legs {
		charged, err := g.charge(ctx, freeLaneLegScopeKey(leg), g.limits.PerLegDaily[leg])
		if err != nil {
			slog.ErrorContext(ctx, "free lane gate failed; failing open", "leg", first, "error", err)
			return FreeLaneOutcome{Granted: first}
		}
		if charged {
			return FreeLaneOutcome{Granted: leg}
		}
		slog.WarnContext(ctx, "free lane leg exhausted its daily platform ceiling", "leg", leg)
	}
	return exhausted()
}

// RemainingToday is FreeLaneGate.remainingToday: the tenant's grace left.
func (g *FreeLaneGate) RemainingToday(ctx context.Context, tenant uuid.UUID) int64 {
	var spent int64
	err := g.pool.QueryRow(ctx, `SELECT spent_usd_micros FROM workflow_spend_buckets
		WHERE scope_key = $1 AND period_kind = $2 AND period_start = $3`,
		freeLaneTenantScopeKey(tenant), "day", utcDay(g.now())).Scan(&spent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "free lane remaining lookup failed", "error", err)
		return g.limits.PerUserDaily
	}
	return max(0, g.limits.PerUserDaily-spent)
}

// charge is the conditional increment: UPDATE ... WHERE spent + 1 <= limit is
// race-free, so N concurrent claims against a limit of M grant exactly M. A limit
// of zero means the leg is unavailable, not unlimited.
func (g *FreeLaneGate) charge(ctx context.Context, scopeKey string, limit int64) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	day := utcDay(g.now())
	if _, err := g.pool.Exec(ctx, `INSERT INTO workflow_spend_buckets (scope_key, period_kind, period_start)
		VALUES ($1, $2, $3) ON CONFLICT (scope_key, period_kind, period_start) DO NOTHING`, scopeKey, "day", day); err != nil {
		return false, err
	}
	var key string
	err := g.pool.QueryRow(ctx, `UPDATE workflow_spend_buckets
		SET spent_usd_micros = spent_usd_micros + 1, updated_at = NOW()
		WHERE scope_key = $1 AND period_kind = $2 AND period_start = $3 AND spent_usd_micros + 1 <= $4
		RETURNING scope_key`, scopeKey, "day", day, limit).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// swiftUUID is UUID.uuidString: uppercase. Scope keys and binding ids are text
// shared with Swift while both servers run, so the case has to match or a tenant
// gets a second counter and a second default profile.
func swiftUUID(id uuid.UUID) string { return strings.ToUpper(id.String()) }

func freeLaneTenantScopeKey(tenant uuid.UUID) string { return "freelane:tenant:" + swiftUUID(tenant) }

func freeLaneLegScopeKey(leg FreeLaneLeg) string { return "freelane:leg:" + string(leg) }

// utcDay is the bucket's period_start: it rolls at UTC midnight, which is what
// the retry hint promises.
func utcDay(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// secondsUntilUTCMidnight is CostLedgerService.secondsUntilUTCMidnight, floored
// at 60 as there.
func secondsUntilUTCMidnight(now time.Time) float64 {
	return max(60, utcDay(now).AddDate(0, 0, 1).Sub(now).Seconds())
}

// FreeLaneRuntime is everything the router needs to serve the lane. Nil on the
// router is the kill switch and restores pre-lane routing exactly.
type FreeLaneRuntime struct {
	Gate                     *FreeLaneGate
	OpenRouterModel          string
	OpenRouterSecondaryModel string
	NvidiaModel              string
}

// Routes is FreeLaneRuntime.routes(preferring:).
func (r *FreeLaneRuntime) Routes(preferring FreeLaneLeg) []FreeLaneRoute {
	return FreeLaneRoutes(r.OpenRouterModel, r.OpenRouterSecondaryModel, r.NvidiaModel, preferring)
}

// FreeLaneStartupWarning is what to say at boot when the lane is on but no leg
// has a platform key: a deploy mistake every free-lane request answers with a 503.
func FreeLaneStartupWarning(enabled, openRouterEnabled, nvidiaEnabled bool) string {
	if !enabled || openRouterEnabled || nvidiaEnabled {
		return ""
	}
	return "freelane.enabled is true but neither the OpenRouter nor the NVIDIA platform key loaded; " +
		"every free-lane request will answer 503 free_lane_unavailable"
}

// envBool is swift-configuration's reader.bool(forKey:default:) over the
// environment: an unparseable or missing value is the default.
func envBool(getenv func(string) string, key string, fallback bool) bool {
	switch strings.ToLower(getenv(envKey(key))) {
	case "true", "yes", "1":
		return true
	case "false", "no", "0":
		return false
	}
	return fallback
}

// envInt is reader.int(forKey:default:).
func envInt(getenv func(string) string, key string, fallback int64) int64 {
	if v, err := strconv.ParseInt(getenv(envKey(key)), 10, 64); err == nil {
		return v
	}
	return fallback
}

// envString is reader.string(forKey:default:). A variable set to the empty string
// reads as unset here, where Swift returns the empty string.
func envString(getenv func(string) string, key, fallback string) string {
	if v := getenv(envKey(key)); v != "" {
		return v
	}
	return fallback
}

// LoadFreeLaneRuntime reads the freelane.* keys App+build.swift reads. Nil when
// freelane.enabled is off.
func LoadFreeLaneRuntime(getenv func(string) string, pool *pgxpool.Pool) *FreeLaneRuntime {
	if !envBool(getenv, "freelane.enabled", true) {
		return nil
	}
	return &FreeLaneRuntime{
		Gate: NewFreeLaneGate(pool, FreeLaneLimits{
			PerUserDaily: envInt(getenv, "freelane.perUserDailyRequests", 20),
			PerLegDaily: map[FreeLaneLeg]int64{
				LegOpenRouterFree: envInt(getenv, "freelane.openRouterDailyRequests", 45),
				LegNvidiaDirect:   envInt(getenv, "freelane.nvidiaDailyRequests", 900),
			},
		}),
		OpenRouterModel:          envString(getenv, "freelane.openRouterModel", FreeLaneOpenRouterModel),
		OpenRouterSecondaryModel: envString(getenv, "freelane.openRouterSecondaryModel", FreeLaneOpenRouterSecondaryModel),
		NvidiaModel:              envString(getenv, "freelane.nvidiaModel", FreeLaneNvidiaModel),
	}
}
