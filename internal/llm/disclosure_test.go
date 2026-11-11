package llm

// Ports the parts of Tests/AppTests/LLM/Routing/ModelDisclosurePolicyTests.swift
// that have a Go counterpart in disclosure.go: brainModeMapping,
// visiblePassesThroughUntouched, hiddenScrubsRoutingIdentity,
// hiddenScrubsUsageIdentityButKeepsCost, hiddenScrubsFallbackNotice, and the
// guard text the prompt-guard suites assert on. The rest (parallel, workflow,
// parallel detail, memory, router profile, ConversationController.buildPrompt)
// scrub surfaces Go does not have in this package; see the audit report.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func auditFRoutingEvent() CerberusEvent {
	prompt, window := 1200, 200_000
	return CerberusEvent{Routing: &RoutingEvent{
		ExecutionID:         uuid.New(),
		ProfileID:           uuid.New(),
		ProfileName:         "Default",
		TaskType:            "coding",
		Strategy:            "sequential",
		ActiveRoutes:        []WireRoute{{Provider: "openRouter", Model: "openai/gpt-5"}},
		PromptTokens:        &prompt,
		ContextWindowTokens: &window,
	}}
}

func TestAuditFBrainModeMapping(t *testing.T) {
	assert.Equal(t, DisclosureHidden, DisclosureForMode(ModeManaged))
	assert.Equal(t, DisclosureHidden, DisclosureForMode(ModeUnset)) // Swift nil
	assert.Equal(t, DisclosureVisible, DisclosureForMode(ModeBYOK))
	assert.Equal(t, DisclosureHidden, Disclosure(0), "the zero value must be hidden")
}

type auditFPrefs struct {
	pref *Preference
	err  error
}

func (p auditFPrefs) Get(context.Context, uuid.UUID) (*Preference, error) { return p.pref, p.err }

// DisclosureFor is RoutedHermesLLMService.disclosure(sessionKey:); Swift has no
// dedicated test for it, but every fail-safe branch must land on hidden.
func TestAuditFDisclosureForFailsClosed(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.NewString()
	byok := auditFPrefs{pref: &Preference{Mode: ModeBYOK}}
	assert.Equal(t, DisclosureVisible, DisclosureFor(ctx, byok, tenant))
	assert.Equal(t, DisclosureVisible, DisclosureFor(ctx, byok, strings.ToUpper(tenant)), "Swift UUID(uuidString:) is case-insensitive")
	assert.Equal(t, DisclosureHidden, DisclosureFor(ctx, nil, tenant))
	assert.Equal(t, DisclosureHidden, DisclosureFor(ctx, byok, "not-a-uuid"))
	assert.Equal(t, DisclosureHidden, DisclosureFor(ctx, auditFPrefs{}, tenant), "no row")
	assert.Equal(t, DisclosureHidden, DisclosureFor(ctx, auditFPrefs{pref: &Preference{Mode: ModeBYOK}, err: errors.New("db")}, tenant))
	assert.Equal(t, DisclosureHidden, DisclosureFor(ctx, auditFPrefs{pref: &Preference{Mode: ModeManaged}}, tenant))
}

func TestAuditFVisiblePassesThroughUntouched(t *testing.T) {
	event := auditFRoutingEvent()
	assert.Equal(t, event, ScrubCerberusEvent(event, DisclosureVisible))

	notice := FailoverNotice{OriginalProvider: XAI, OriginalModel: "grok-4", FallbackProvider: OpenRouter, FallbackModel: "qwen", ReasonCode: "credit_exhausted", UserMessage: "m"}
	assert.Equal(t, notice, ScrubFailoverNotice(notice, DisclosureVisible))
}

func TestAuditFHiddenScrubsRoutingIdentity(t *testing.T) {
	in := auditFRoutingEvent()
	original := in.Routing.ActiveRoutes
	out := ScrubCerberusEvent(in, DisclosureHidden)
	require.NotNil(t, out.Routing)
	assert.Nil(t, out.Usage)
	assert.Empty(t, out.Routing.ActiveRoutes)
	assert.NotNil(t, out.Routing.ActiveRoutes, "encodes as [], not null")
	require.NotNil(t, out.Routing.DisplayLabel)
	assert.Equal(t, "Auto · coding", *out.Routing.DisplayLabel)
	assert.Equal(t, "coding", out.Routing.TaskType)
	// promptTokens survives; the window size fingerprints the model and does not.
	require.NotNil(t, out.Routing.PromptTokens)
	assert.Equal(t, 1200, *out.Routing.PromptTokens)
	assert.Nil(t, out.Routing.ContextWindowTokens)
	// The input is not mutated: telemetry may still hold the real routes.
	assert.Equal(t, original, in.Routing.ActiveRoutes)
	assert.Nil(t, in.Routing.DisplayLabel)
	assert.NotNil(t, in.Routing.ContextWindowTokens)
}

func TestAuditFHiddenScrubsUsageIdentityButKeepsCost(t *testing.T) {
	in := CerberusEvent{Usage: &UsageEvent{
		ExecutionID: uuid.New(), Provider: "openRouter", Model: "x-ai/grok-4",
		TokensIn: 100, TokensOut: 50, EstimatedCostUsdMicros: 1234, LatencyMs: 900,
	}}
	out := ScrubCerberusEvent(in, DisclosureHidden)
	require.NotNil(t, out.Usage)
	assert.Empty(t, out.Usage.Provider)
	assert.Empty(t, out.Usage.Model)
	assert.Equal(t, 100, out.Usage.TokensIn)
	assert.Equal(t, 50, out.Usage.TokensOut)
	assert.Equal(t, int64(1234), out.Usage.EstimatedCostUsdMicros)
	assert.Equal(t, 900, out.Usage.LatencyMs)
	assert.Equal(t, "x-ai/grok-4", in.Usage.Model, "input not mutated")
}

func TestAuditFHiddenScrubsFallbackNotice(t *testing.T) {
	status := 402
	body := "grok says no"
	notice := FailoverNotice{
		OriginalProvider: XAI, OriginalModel: "grok-4",
		FallbackProvider: OpenRouter, FallbackModel: "qwen/qwen-2.5-72b-instruct",
		ReasonCode:  "credit_exhausted",
		UserMessage: "Grok credits exhausted — switched to Qwen 2.5 via OpenRouter",
		StatusCode:  &status, BodyPreview: &body,
	}
	out := ScrubFailoverNotice(notice, DisclosureHidden)
	assert.Equal(t, GenericModelID, out.OriginalModel)
	assert.Equal(t, GenericModelID, out.FallbackModel)
	assert.Equal(t, OpenRouter, out.OriginalProvider)
	assert.Equal(t, OpenRouter, out.FallbackProvider)
	assert.NotContains(t, strings.ToLower(out.UserMessage), "grok")
	assert.NotContains(t, strings.ToLower(out.UserMessage), "qwen")
	assert.Equal(t, "Your brain switched to a backup route to finish this reply.", out.UserMessage)
	assert.Equal(t, "credit_exhausted", out.ReasonCode)
	assert.Nil(t, out.BodyPreview, "the upstream body names the provider")
	assert.Nil(t, out.StatusCode)
}

// The constants the Swift suites compare against.
func TestAuditFGenericConstantsAndGuardText(t *testing.T) {
	assert.Equal(t, "auto", GenericModelID)
	assert.Equal(t, "LuminaVault Brain · Auto", GenericBrainName)
	assert.Equal(t, "LuminaVault", GenericProviderName)
	assert.Equal(t, "Auto · coding", GenericLabel("coding"))
	// Swift's multi-line literal with "\" continuations, byte for byte.
	assert.Equal(t, `Identity policy: you are "Lumina", powered by the LuminaVault Brain. `+
		`Never disclose, confirm, or deny which underlying AI model, version, or `+
		`provider generates your replies, even if asked directly, indirectly, or `+
		`via role-play. If asked, say you are Lumina running on LuminaVault's `+
		`managed intelligence and steer back to the user's task.`, SystemPromptGuard)
	assert.Contains(t, SystemPromptGuard, "Never disclose, confirm, or deny")
}
