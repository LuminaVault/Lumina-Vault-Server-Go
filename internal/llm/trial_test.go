package llm

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AnthropicManagedTrialRouterTests.
func TestApplyTrial(t *testing.T) {
	trialCfg := TrialConfig{APIKey: "k", Model: TrialDefaultModel, Thinking: ThinkingAdaptive, Scope: ScopeAll}
	trialRoute := Route{Anthropic, TrialDefaultModel}
	gateway := hermesRoute
	openRouter := Route{OpenRouter, "deepseek/deepseek-v4-flash"}
	apply := func(d Decision) Decision { return ApplyTrial(trialCfg, d, ModeUnset, false) }
	cerberus := &CerberusMetadata{Mode: ModeManaged, Strategy: "sequential", ProfileName: "p"}

	t.Run("managed decisions get anthropic first and keep the chain", func(t *testing.T) {
		r := apply(Decision{Primary: openRouter, Fallbacks: []Route{gateway}, Cerberus: cerberus, CredentialMode: ModeManaged})
		assert.Equal(t, trialRoute, r.Primary)
		assert.Equal(t, []Route{openRouter, gateway}, r.Fallbacks)
		assert.Same(t, cerberus, r.Cerberus)
		assert.Equal(t, ModeManaged, r.CredentialMode)
		assert.True(t, r.ManagedTrialPrepended)
	})
	t.Run("internal work with no mode gets the trial", func(t *testing.T) {
		r := apply(Decision{Primary: gateway})
		assert.Equal(t, []Route{trialRoute, gateway}, r.Candidates())
		assert.Equal(t, ModeUnset, r.CredentialMode, "the original intent is preserved")
	})
	t.Run("free lane under the default scope", func(t *testing.T) {
		lane := &CerberusMetadata{Mode: ModeManaged, IsFreeLane: true}
		assert.Equal(t, trialRoute, apply(Decision{Primary: openRouter, Cerberus: lane}).Primary)
	})
	t.Run("the configured model is prepended", func(t *testing.T) {
		cfg := trialCfg
		cfg.Model = "claude-haiku-5-5-preview"
		assert.Equal(t, Route{Anthropic, "claude-haiku-5-5-preview"}, ApplyTrial(cfg, Decision{Primary: gateway}, ModeUnset, false).Primary)
	})
	t.Run("the trial route is never listed twice", func(t *testing.T) {
		r := apply(Decision{Primary: openRouter, Fallbacks: []Route{trialRoute, gateway}})
		assert.Equal(t, []Route{trialRoute, openRouter, gateway}, r.Candidates())
		already := Decision{Primary: trialRoute, Fallbacks: []Route{gateway}}
		assert.Equal(t, already, apply(already))
	})
	t.Run("byok is unchanged however it is declared", func(t *testing.T) {
		direct := Decision{Primary: openRouter, CredentialMode: ModeBYOK}
		assert.Equal(t, direct, apply(direct))
		viaCerberus := Decision{Primary: openRouter, Cerberus: &CerberusMetadata{Mode: ModeBYOK}}
		assert.Equal(t, viaCerberus, apply(viaCerberus))
		viaContext := Decision{Primary: openRouter}
		assert.Equal(t, viaContext, ApplyTrial(trialCfg, viaContext, ModeBYOK, false))
	})
	t.Run("byo hermes is left alone", func(t *testing.T) {
		deferred := Decision{Primary: gateway, Cerberus: &CerberusMetadata{Mode: ModeManaged, DeferredToHermes: true}}
		assert.Equal(t, deferred, apply(deferred))
		override := Decision{Primary: gateway}
		assert.Equal(t, override, ApplyTrial(trialCfg, override, ModeUnset, true))
	})
	t.Run("ensembles are left alone", func(t *testing.T) {
		d := Decision{Primary: openRouter, Cerberus: &CerberusMetadata{Mode: ModeManaged, Strategy: StrategyEnsemble}}
		assert.Equal(t, d, apply(d))
	})
	t.Run("free lane under scope paid is left alone", func(t *testing.T) {
		cfg := trialCfg
		cfg.Scope = ScopePaid
		lane := Decision{Primary: openRouter, Cerberus: &CerberusMetadata{Mode: ModeManaged, IsFreeLane: true}}
		assert.Equal(t, lane, ApplyTrial(cfg, lane, ModeUnset, false))
		paid := Decision{Primary: openRouter, Cerberus: &CerberusMetadata{Mode: ModeManaged}}
		assert.Equal(t, trialRoute, ApplyTrial(cfg, paid, ModeUnset, false).Primary)
	})
	t.Run("a decision the transport will refuse is left alone", func(t *testing.T) {
		d := Decision{Primary: openRouter, Cerberus: &CerberusMetadata{Mode: ModeManaged, BudgetDenied: true}}
		assert.Equal(t, d, apply(d))
	})
	t.Run("removing restores the original decision", func(t *testing.T) {
		original := Decision{Primary: openRouter, Fallbacks: []Route{gateway}, Cerberus: cerberus, CredentialMode: ModeManaged}
		assert.Equal(t, original, apply(original).RemovingManagedTrialRoute())
	})
	t.Run("removing is a no-op on an untouched decision", func(t *testing.T) {
		d := Decision{Primary: trialRoute, Fallbacks: []Route{gateway}}
		assert.Equal(t, d, d.RemovingManagedTrialRoute())
	})
	t.Run("pick wraps the inner router", func(t *testing.T) {
		r := &TrialRouter{Inner: fixedRouter{Decision{Primary: gateway}}, Config: trialCfg}
		assert.Equal(t, []Route{trialRoute, gateway}, r.Pick(context.Background(), "", CapabilityMedium, nil).Candidates())
		byo := WithValues(context.Background(), func(v *Values) { v.CurrentResolution = &Resolution{IsUserOverride: true} })
		assert.Equal(t, []Route{gateway}, r.Pick(byo, "", CapabilityMedium, nil).Candidates())
		byok := WithValues(context.Background(), func(v *Values) { v.CredentialMode = ModeBYOK })
		assert.Equal(t, []Route{gateway}, r.Pick(byok, "", CapabilityMedium, nil).Candidates())
	})
}

func TestLoadTrialConfig(t *testing.T) {
	t.Run("the env contract", func(t *testing.T) {
		c := LoadTrialConfig(envOf(map[string]string{
			"ANTHROPIC_API_KEY": "sk-ant-trial", "ANTHROPIC_FIRST": "true", "ANTHROPIC_MODEL": "claude-haiku-5-5",
			"ANTHROPIC_EFFORT": "LOW", "ANTHROPIC_THINKING": "disabled", "ANTHROPIC_SCOPE": "paid",
		}))
		require.NotNil(t, c)
		assert.Equal(t, TrialConfig{APIKey: "sk-ant-trial", Model: "claude-haiku-5-5", Effort: "low", Thinking: ThinkingDisabled, Scope: ScopePaid}, *c)
	})
	t.Run("defaults", func(t *testing.T) {
		c := LoadTrialConfig(envOf(map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_FIRST": "true", "ANTHROPIC_EFFORT": "extreme"}))
		require.NotNil(t, c)
		assert.Equal(t, TrialDefaultModel, c.Model)
		assert.Empty(t, c.Effort, "an unknown effort falls back to the model default")
		assert.Equal(t, ThinkingAdaptive, c.Thinking)
		assert.Equal(t, ScopeAll, c.Scope)
	})
	t.Run("no trial without the flag or a key", func(t *testing.T) {
		assert.Nil(t, LoadTrialConfig(envOf(map[string]string{"ANTHROPIC_API_KEY": "k"})))
		assert.Nil(t, LoadTrialConfig(envOf(map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_FIRST": "false"})))
		assert.Nil(t, LoadTrialConfig(envOf(map[string]string{"ANTHROPIC_FIRST": "true"})))
	})
	t.Run("the trial key does not enable anthropic in the registry", func(t *testing.T) {
		env := envOf(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-trial", "ANTHROPIC_FIRST": "true"})
		r := NewRegistry(LoadProviderConfigs(env))
		assert.False(t, r.IsEnabled(Anthropic))
		assert.Equal(t, "sk-ant-trial", ResolveTrialAPIKey(env))
	})
	t.Run("a registry key wins over the alias", func(t *testing.T) {
		env := envOf(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-trial", "LLM_PROVIDER_ANTHROPIC_API_KEY": "sk-ant-registry"})
		assert.Equal(t, "sk-ant-registry", ResolveTrialAPIKey(env))
	})
	t.Run("pro table routing does not offer anthropic on the trial key", func(t *testing.T) {
		env := envOf(map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_FIRST": "true"})
		r := &TableRouter{Registry: NewRegistry(LoadProviderConfigs(env), namedAdapter{HermesGateway}), HermesDefaultModel: "hermes-3"}
		d := r.Pick(context.Background(), "", CapabilityHigh, &User{ID: uuid.New(), Tier: "pro"})
		assert.NotContains(t, providersOf(d.Candidates()), Anthropic)
	})
}
