package llm

import (
	"context"
	"lumina/internal/swiftstr"
	"slices"
	"strings"
)

// TrialThinking is ANTHROPIC_THINKING.
type TrialThinking string

const (
	ThinkingAdaptive TrialThinking = "adaptive"
	ThinkingDisabled TrialThinking = "disabled"
)

// TrialScope is ANTHROPIC_SCOPE.
type TrialScope string

const (
	ScopeAll  TrialScope = "all"
	ScopePaid TrialScope = "paid" // skip the free lane
)

// TrialDefaultModel is ANTHROPIC_MODEL's default.
const TrialDefaultModel = "claude-haiku-5-5"

var trialEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// TrialConfig is AnthropicManagedTrialConfig: Anthropic as the first rung of
// platform-paid routing with the existing chain behind it. Env contract:
// platform/infra/docs/llm-haiku-trial.md.
//
// The key reaches only AnthropicAdapter, and TrialRouter is the only thing that
// routes to it. An empty Effort means the model default.
type TrialConfig struct {
	APIKey   string
	Model    string
	Effort   string
	Thinking TrialThinking
	Scope    TrialScope
}

// Route is the route the trial puts in front of every eligible decision.
func (c TrialConfig) Route() Route { return Route{Anthropic, c.Model} }

// parseEnvBool is swift-configuration's environment bool: true/false, yes/no and
// 1/0 in any case. Anything else is the default.
func parseEnvBool(s string) bool {
	switch strings.ToLower(s) {
	case "true", "yes", "1":
		return true
	}
	return false
}

// LoadTrialConfig returns nil unless ANTHROPIC_FIRST is on and a key resolves,
// so with the flag off or no key nothing about routing or the adapter changes.
func LoadTrialConfig(getenv func(string) string) *TrialConfig {
	if !parseEnvBool(getenv("ANTHROPIC_FIRST")) {
		return nil
	}
	apiKey := ResolveTrialAPIKey(getenv)
	if apiKey == "" {
		return nil
	}
	trim := swiftstr.TrimWhitespacesAndNewlines
	cfg := &TrialConfig{APIKey: apiKey, Model: trim(getenv("ANTHROPIC_MODEL")), Thinking: ThinkingAdaptive, Scope: ScopeAll}
	if cfg.Model == "" {
		cfg.Model = TrialDefaultModel
	}
	if effort := strings.ToLower(trim(getenv("ANTHROPIC_EFFORT"))); slices.Contains(trialEfforts, effort) {
		cfg.Effort = effort
	}
	if strings.ToLower(trim(getenv("ANTHROPIC_THINKING"))) == string(ThinkingDisabled) {
		cfg.Thinking = ThinkingDisabled
	}
	if strings.ToLower(trim(getenv("ANTHROPIC_SCOPE"))) == string(ScopePaid) {
		cfg.Scope = ScopePaid
	}
	return cfg
}

// ResolveTrialAPIKey is canonical provider key, legacy spelling, then
// ANTHROPIC_API_KEY. A deployment that enables Anthropic in the registry keeps
// spending that key.
func ResolveTrialAPIKey(getenv func(string) string) string {
	return firstTrimmed(
		getenv(envKey(apiKeyConfigKey("anthropic"))),
		getenv(envKey(legacyAPIKeyConfigKey("anthropic"))),
		getenv(envKey("anthropic.apiKey")),
	)
}

// TrialRouter is AnthropicManagedTrialRouter: it puts the trial route in front
// of the decision another router made. Any failure on the Anthropic leg is
// recoverable, so hitting the spend cap silently hands traffic back to the old
// chain. Only platform-paid traffic is touched.
type TrialRouter struct {
	Inner  Router
	Config TrialConfig
}

func (r *TrialRouter) Pick(ctx context.Context, model string, capability Capability, user *User) Decision {
	decision := r.Inner.Pick(ctx, model, capability, user)
	values := RoutingValues(ctx)
	hermesOverride := values.CurrentResolution != nil && values.CurrentResolution.IsUserOverride
	return ApplyTrial(r.Config, decision, values.CredentialMode, hermesOverride)
}

// effectiveMode is the precedence the transport uses when it binds the mode.
func effectiveMode(d Decision, contextMode CredentialMode) CredentialMode {
	if d.CredentialMode != ModeUnset {
		return d.CredentialMode
	}
	if d.Cerberus != nil && d.Cerberus.Mode != ModeUnset {
		return d.Cerberus.Mode
	}
	return contextMode
}

// ApplyTrial is the pure decision rewrite. Unchanged: BYOK, BYO Hermes,
// ensembles (ParallelExecutor dispatches the Cerberus routes, not candidates),
// decisions the transport will refuse anyway, and the free lane under scope paid.
func ApplyTrial(cfg TrialConfig, d Decision, contextMode CredentialMode, isUserHermesOverride bool) Decision {
	if effectiveMode(d, contextMode) == ModeBYOK || isUserHermesOverride {
		return d
	}
	if c := d.Cerberus; c != nil {
		if c.DeferredToHermes || c.Strategy == StrategyEnsemble || c.PreflightError() != nil {
			return d
		}
		if c.IsFreeLane && cfg.Scope == ScopePaid {
			return d
		}
	}
	trial := cfg.Route()
	if d.Primary == trial {
		return d
	}
	fallbacks := slices.DeleteFunc(d.Candidates(), func(r Route) bool { return r == trial })
	return Decision{Primary: trial, Fallbacks: fallbacks, Cerberus: d.Cerberus, CredentialMode: d.CredentialMode, ManagedTrialPrepended: true}
}

// RemovingManagedTrialRoute returns the decision as it was before the trial
// route went in front of it, for callers that cannot dispatch to Anthropic
// themselves (the managed gateway stream).
func (d Decision) RemovingManagedTrialRoute() Decision {
	if !d.ManagedTrialPrepended || len(d.Fallbacks) == 0 {
		return d
	}
	return Decision{Primary: d.Fallbacks[0], Fallbacks: d.Fallbacks[1:], Cerberus: d.Cerberus, CredentialMode: d.CredentialMode}
}
