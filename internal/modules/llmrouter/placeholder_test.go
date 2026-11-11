package llmrouter

import (
	"lumina/internal/llm"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RouterPlaceholderRoutesTests: the substitution rule on its own, without HTTP
// or a database.

var (
	opus = llm.WireRoute{Provider: "anthropic", Model: "claude-opus-4-7"}
	mini = llm.WireRoute{Provider: "openai", Model: "gpt-4o-mini"}
)

func write(mode llm.CredentialMode, action llm.RouterAction, rules ...llm.RouterRule) llm.RouterProfileWrite {
	return llm.RouterProfileWrite{
		Name: "Default", Mode: mode,
		Objective:     llm.RouterObjective{Quality: 50, Cost: 25, Latency: 25},
		DefaultAction: action, Rules: rules, RoutingPolicy: llm.PolicyBalanced,
	}
}

func seq(routes ...llm.WireRoute) llm.RouterAction {
	return llm.RouterAction{Kind: llm.ActionSequential, Routes: routes, RetryPolicy: llm.RetryFast}
}

func TestPlaceholderIsRecognisedAndARealOpenRouterModelIsNot(t *testing.T) {
	assert.True(t, isPlaceholder(placeholder()))
	assert.False(t, isPlaceholder(llm.WireRoute{Provider: "openRouter", Model: llm.ManagedModel}))
	assert.False(t, isPlaceholder(llm.WireRoute{Provider: "anthropic", Model: "auto"}))
}

func TestPlaceholderBecomesTheSavedChainInOrder(t *testing.T) {
	got, err := substitutePlaceholders(write(llm.ModeBYOK, seq(placeholder())), []llm.WireRoute{opus, mini})
	require.NoError(t, err)
	assert.Equal(t, []llm.WireRoute{opus, mini}, got.DefaultAction.Routes)
}

func TestPlaceholderRealRoutesStayAndDuplicatesCollapse(t *testing.T) {
	got, err := substitutePlaceholders(write(llm.ModeBYOK, seq(mini, placeholder())), []llm.WireRoute{opus, mini})
	require.NoError(t, err)
	assert.Equal(t, []llm.WireRoute{mini, opus}, got.DefaultAction.Routes)
}

func TestPlaceholderRuleActionsAndSynthesisRouteAreResolved(t *testing.T) {
	synth := placeholder()
	rule := llm.RouterRule{
		ID: uuid.New(), Name: "reasoning", Enabled: true, Priority: 1, TaskTypes: []llm.RouterTaskType{"reasoning"},
		Action: llm.RouterAction{Kind: llm.ActionEnsemble, Routes: []llm.WireRoute{placeholder(), mini}, SynthesisRoute: &synth},
	}
	got, err := substitutePlaceholders(write(llm.ModeBYOK, seq(mini), rule), []llm.WireRoute{opus})
	require.NoError(t, err)
	require.Len(t, got.Rules, 1)
	assert.Equal(t, []llm.WireRoute{opus, mini}, got.Rules[0].Action.Routes)
	// The synthesis step is one model, so it takes the chain's primary.
	require.NotNil(t, got.Rules[0].Action.SynthesisRoute)
	assert.Equal(t, opus, *got.Rules[0].Action.SynthesisRoute)
	assert.Equal(t, rule.ID, got.Rules[0].ID)
}

func TestPlaceholderWithNothingToSubstituteIsRefused(t *testing.T) {
	_, err := substitutePlaceholders(write(llm.ModeBYOK, seq(placeholder())), nil)
	assert.ErrorIs(t, err, errPlaceholderRoute)
}

func TestPlaceholderARequestWithoutItIsReturnedUntouched(t *testing.T) {
	req := write(llm.ModeBYOK, seq(mini))
	got, err := substitutePlaceholders(req, nil)
	require.NoError(t, err)
	assert.Equal(t, req.DefaultAction, got.DefaultAction)
}

func TestPlaceholderAManagedSaveIsLeftAlone(t *testing.T) {
	req := write(llm.ModeManaged, seq(placeholder()))
	got, err := substitutePlaceholders(req, nil)
	require.NoError(t, err)
	assert.Equal(t, req.DefaultAction, got.DefaultAction)
}

// savedByokChain drops the placeholder and the brain label, and needs BYOK.
func TestSavedByokChainDropsPlaceholders(t *testing.T) {
	pref := &llm.Preference{
		Mode: llm.ModeBYOK, PrimaryProvider: "anthropic", PrimaryModel: "claude-opus-4-7",
		FallbackChain: []llm.PreferenceStep{
			{Provider: "openRouter", Model: llm.GenericModelID},
			{Provider: "openRouter", Model: llm.GenericBrainName},
			{Provider: "hermesGateway", Model: "x"},
			{Provider: "openai", Model: "gpt-4o-mini"},
		},
	}
	assert.Equal(t, []llm.WireRoute{opus, mini}, savedByokChain(pref))
	pref.Mode = llm.ModeManaged
	assert.Empty(t, savedByokChain(pref))
	assert.Empty(t, savedByokChain(nil))
}

// Without FixDashboard the route is Swift's bare 500.
func TestDashboardIsSwiftsBare500ByDefault(t *testing.T) {
	m := New(nil, Options{})
	assert.ErrorIs(t, m.dashboard(nil), errDashboardDecode)
}
