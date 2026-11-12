package llm

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingRouter counts picks: a pick is what claims a free-lane grant.
type countingRouter struct {
	decision Decision
	picks    atomic.Int32
}

func (r *countingRouter) Pick(context.Context, string, Capability, *User) Decision {
	r.picks.Add(1)
	return r.decision
}

// countingEnsemble records whether the ensemble path was taken.
type countingEnsemble struct{ calls atomic.Int32 }

func (e *countingEnsemble) Execute(context.Context, []byte, string, string, *CerberusMetadata, string) (*EnsembleResult, error) {
	e.calls.Add(1)
	return &EnsembleResult{Route: Route{OpenAI, "gpt-5"}, Metadata: Metadata{Data: []byte("ENSEMBLE")}, Status: "completed"}, nil
}

const decidedPayload = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// Ports QuickAssistantRoutingTests (the removal half).
func TestGatewayOnlyDecisionHasNoToolRoute(t *testing.T) {
	d := Decision{Primary: Route{HermesGateway, "hermes-3"}}
	_, ok := d.Removing(HermesGateway)
	assert.False(t, ok)
}

func TestOllamaOnlyDecisionHasNoToolRoute(t *testing.T) {
	d := Decision{Primary: Route{Ollama, "llama"}, Fallbacks: []Route{{Ollama, "mistral"}}, CredentialMode: ModeBYOK}
	_, ok := d.Removing(Ollama)
	assert.False(t, ok)
}

func TestGeminiOnlyDecisionHasNoToolRoute(t *testing.T) {
	d := Decision{Primary: Route{Gemini, "gemini-2.5-flash"}, Fallbacks: []Route{{Gemini, "gemini-2.5-pro"}}, CredentialMode: ModeBYOK}
	_, ok := d.Removing(Gemini)
	assert.False(t, ok)
}

func TestTrialRouteSurvivesAndGatewayFallbackGoes(t *testing.T) {
	d := Decision{
		Primary:               Route{Anthropic, "claude-haiku-5-5"},
		Fallbacks:             []Route{{HermesGateway, "hermes-3"}},
		ManagedTrialPrepended: true,
	}
	stripped, ok := d.Removing(HermesGateway)
	require.True(t, ok)
	assert.Equal(t, Route{Anthropic, "claude-haiku-5-5"}, stripped.Primary)
	assert.Empty(t, stripped.Fallbacks)
	assert.True(t, stripped.ManagedTrialPrepended)
}

func TestRemovingPrimaryPromotesNextInOrder(t *testing.T) {
	d := Decision{
		Primary:               Route{HermesGateway, "hermes-3"},
		Fallbacks:             []Route{{OpenRouter, "a"}, {HermesGateway, "hermes-3-small"}, {Groq, "b"}},
		ManagedTrialPrepended: true,
	}
	stripped, ok := d.Removing(HermesGateway)
	require.True(t, ok)
	assert.Equal(t, Route{OpenRouter, "a"}, stripped.Primary)
	assert.Equal(t, []Route{{Groq, "b"}}, stripped.Fallbacks)
	assert.False(t, stripped.ManagedTrialPrepended, "the trial route was not the one kept in front")

	ollama := Decision{
		Primary:        Route{Ollama, "llama"},
		Fallbacks:      []Route{{Anthropic, "a"}, {Ollama, "mistral"}, {OpenAI, "b"}},
		CredentialMode: ModeBYOK,
	}
	stripped, ok = ollama.Removing(Ollama)
	require.True(t, ok)
	assert.Equal(t, Route{Anthropic, "a"}, stripped.Primary)
	assert.Equal(t, []Route{{OpenAI, "b"}}, stripped.Fallbacks)
}

func TestRemovingKeepsCredentialModeAndCerberus(t *testing.T) {
	c := &CerberusMetadata{ExecutionID: uuid.New(), BudgetReservationUsdMicros: 42}
	d := Decision{Primary: Route{Anthropic, "m"}, Fallbacks: []Route{{HermesGateway, "hermes-3"}}, Cerberus: c, CredentialMode: ModeBYOK}
	stripped, ok := d.Removing(HermesGateway)
	require.True(t, ok)
	assert.Same(t, c, stripped.Cerberus)
	assert.Equal(t, ModeBYOK, stripped.CredentialMode)
}

func TestEachStepRemovesOnlyItsOwnProvider(t *testing.T) {
	d := Decision{Primary: Route{HermesGateway, "hermes-3"}, Fallbacks: []Route{{Ollama, "llama"}}}
	withoutOllama, ok := d.Removing(Ollama)
	require.True(t, ok)
	assert.Equal(t, HermesGateway, withoutOllama.Primary.Provider)
	withoutGateway, ok := d.Removing(HermesGateway)
	require.True(t, ok)
	assert.Equal(t, Ollama, withoutGateway.Primary.Provider)
	_, ok = withoutGateway.Removing(Ollama)
	assert.False(t, ok)
}

// The managed pro table puts Gemini behind Sonnet; the loop must not fail over to it.
func TestAManagedGeminiFallbackIsDroppedAndTheRestKept(t *testing.T) {
	d := Decision{Primary: Route{Anthropic, "claude-sonnet-4-6"}, Fallbacks: []Route{{Gemini, "gemini-2.5-pro"}}, CredentialMode: ModeManaged}
	stripped, ok := d.Removing(Gemini)
	require.True(t, ok)
	assert.Equal(t, Route{Anthropic, "claude-sonnet-4-6"}, stripped.Primary)
	assert.Empty(t, stripped.Fallbacks)
	assert.Equal(t, ModeManaged, stripped.CredentialMode)
}

func TestWithoutBudgetReservationZeroesOnlyTheReservation(t *testing.T) {
	rule := uuid.New()
	c := &CerberusMetadata{
		ExecutionID: uuid.New(), TenantID: uuid.New(), ProfileID: uuid.New(), ProfileName: "QA", RuleID: &rule,
		Strategy: string(ActionSequential), Routes: []WireRoute{{Provider: "anthropic", Model: "m"}},
		PredictedCostUsdMicros: 900, BudgetReservationUsdMicros: 900, Mode: ModeManaged, IsFreeLane: true,
	}
	original := Decision{
		Primary: Route{Anthropic, "m"}, Fallbacks: []Route{{OpenAI, "x"}}, Cerberus: c,
		CredentialMode: ModeManaged, ManagedTrialPrepended: true,
	}
	stripped := original.WithoutBudgetReservation()

	require.NotNil(t, stripped.Cerberus)
	assert.NotSame(t, c, stripped.Cerberus, "a new metadata, so the original keeps its reservation")
	assert.Equal(t, int64(0), stripped.Cerberus.BudgetReservationUsdMicros)
	assert.Equal(t, int64(900), c.BudgetReservationUsdMicros)
	want := *c
	want.BudgetReservationUsdMicros = 0
	assert.Equal(t, want, *stripped.Cerberus)
	assert.Equal(t, original.Primary, stripped.Primary)
	assert.Equal(t, original.Fallbacks, stripped.Fallbacks)
	assert.Equal(t, original.CredentialMode, stripped.CredentialMode)
	assert.Equal(t, original.ManagedTrialPrepended, stripped.ManagedTrialPrepended)

	assert.Nil(t, Decision{Primary: Route{Anthropic, "m"}}.WithoutBudgetReservation().Cerberus)
}

func TestDecidedCompletionDoesNotPickAgain(t *testing.T) {
	adapter := &scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply("1"), reply("2"), reply("3")}}
	router := &countingRouter{decision: Decision{Primary: Route{Anthropic, "m"}}}
	transport := &Transport{Registry: NewRegistry(nil, adapter), Router: router}

	picked := transport.PickDecision(context.Background(), []byte(decidedPayload))
	var answers []string
	for range 3 {
		metadata, err := transport.ChatCompletionsDecided(context.Background(), []byte(decidedPayload), "alice", "", picked)
		require.NoError(t, err)
		answers = append(answers, string(metadata.Data))
	}
	assert.Equal(t, []string{"1", "2", "3"}, answers)
	assert.Equal(t, int32(1), router.picks.Load())

	router.picks.Store(0)
	adapter.outcomes = []outcome{reply("4")}
	_, err := transport.ChatCompletionsDecided(context.Background(), []byte(decidedPayload), "alice", "", picked)
	require.NoError(t, err)
	assert.Zero(t, router.picks.Load(), "ChatCompletionsDecided never picks")
}

func TestDecidedCompletionWalksFallbacks(t *testing.T) {
	first := &scriptedAdapter{kind: XAI, outcomes: []outcome{failing(Transient(XAI, 503, strPtr("unavailable")))}}
	second := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{reply("SECOND")}}
	decision := Decision{Primary: Route{XAI, "grok-4"}, Fallbacks: []Route{{OpenRouter, "qwen"}}}
	router := &countingRouter{decision: decision}
	transport := &Transport{Registry: NewRegistry(nil, first, second), Router: router}

	metadata, err := transport.ChatCompletionsDecided(context.Background(), []byte(decidedPayload), "alice", "", decision)
	require.NoError(t, err)
	assert.Equal(t, "SECOND", string(metadata.Data))
	assert.Equal(t, []string{"qwen"}, second.models(t))
	assert.Zero(t, router.picks.Load())
}

func TestDecidedPathNeverRunsTheEnsemble(t *testing.T) {
	participant := &scriptedAdapter{kind: OpenAI, outcomes: []outcome{reply("participant")}}
	candidate := &scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply("candidate")}}
	ensemble := &countingEnsemble{}
	decision := Decision{
		Primary: Route{Anthropic, "claude-sonnet-4.6"},
		Cerberus: &CerberusMetadata{
			ExecutionID: uuid.New(), Strategy: StrategyEnsemble, ParallelStrategy: "consensus", Mode: ModeManaged,
			Routes:                   []WireRoute{{Provider: "openai", Model: "gpt-5"}, {Provider: "anthropic", Model: "claude-sonnet-4-6"}},
			SynthesisRoute:           &WireRoute{Provider: "anthropic", Model: "claude-sonnet-4-6"},
			MinimumSuccessfulResults: 2,
		},
		CredentialMode: ModeManaged,
	}
	transport := &Transport{Registry: NewRegistry(nil, participant, candidate), Router: fixedRouter{decision}, Ensemble: ensemble}
	ctx := WithValues(context.Background(), func(v *Values) { v.ParallelStrategy = "consensus" })

	metadata, err := transport.ChatCompletionsDecided(ctx, []byte(decidedPayload), "k", "", decision)
	require.NoError(t, err)
	assert.Equal(t, "candidate", string(metadata.Data))
	assert.Zero(t, ensemble.calls.Load(), "the ensemble is never run")
	assert.Zero(t, participant.callCount(), "ensemble participants are never dispatched")
	assert.Equal(t, 1, candidate.callCount(), "the one candidate answered")
}

func TestDecidedCompletionHonoursForcedRoute(t *testing.T) {
	anthropic := &scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply("anthropic")}}
	openAI := &scriptedAdapter{kind: OpenAI, outcomes: []outcome{reply("openai")}}
	decision := Decision{Primary: Route{Anthropic, "claude-haiku-5-5"}, CredentialMode: ModeBYOK}
	transport := &Transport{Registry: NewRegistry(nil, anthropic, openAI), Router: fixedRouter{decision}}
	ctx := WithValues(context.Background(), func(v *Values) { v.ForcedRoute = &WireRoute{Provider: "openai", Model: "x"} })

	metadata, err := transport.ChatCompletionsDecided(ctx, []byte(decidedPayload), "alice", "", decision)
	require.NoError(t, err)
	assert.Equal(t, "openai", string(metadata.Data))
	assert.Equal(t, []string{"x"}, openAI.models(t))
	assert.Zero(t, anthropic.callCount())
	assert.Equal(t, []CredentialMode{ModeBYOK}, openAI.modes, "the forced route keeps the decision's mode")
}

// Ports QuickAssistantReservationTests: several rounds ride one decision, and
// the reservation sits in the tenant's single monthly row beside every other
// in-flight request's, so it must be subtracted once per question. Real
// database, real RouterTelemetryService: the assertion is on the row itself.
func TestReservationIsSubtractedOnce(t *testing.T) {
	ctx, pool, newUser := llmDB(t)
	const (
		ours   int64 = 1000
		others int64 = 5000
		month        = `date_trunc('month', CURRENT_DATE)::date`
	)
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("q", 4000) + `"}]}`)
	answer := strings.Repeat("x", 4000)

	type row struct{ reserved, managed, requests int64 }
	read := func(t *testing.T, tenant uuid.UUID) row {
		t.Helper()
		var r row
		require.NoError(t, pool.QueryRow(ctx, `SELECT reserved_usd_micros, managed_usd_micros, requests FROM router_monthly_usage WHERE tenant_id = $1 AND month = `+month, tenant).
			Scan(&r.reserved, &r.managed, &r.requests))
		return r
	}
	seed := func(t *testing.T) (uuid.UUID, Decision) {
		t.Helper()
		tenant := newUser()
		// complete writes vault-scoped analytics, and router_executions.profile_id is a foreign key.
		_, err := pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'qa', NOW(), NOW())`, tenant)
		require.NoError(t, err)
		profile := uuid.New()
		_, err = pool.Exec(ctx, `INSERT INTO router_profiles (id, tenant_id, name, mode, document) VALUES ($1, $2, 'qa', 'managed', '{}'::jsonb)`, profile, tenant)
		require.NoError(t, err)
		// Our reservation plus another in-flight request's.
		_, err = pool.Exec(ctx, `INSERT INTO router_monthly_usage (tenant_id, month, reserved_usd_micros) VALUES ($1, `+month+`, $2)`, tenant, ours+others)
		require.NoError(t, err)
		return tenant, Decision{
			// A catalogued model, so a round has a non-zero cost to record.
			Primary: Route{Anthropic, "claude-3-5-haiku-20241022"},
			Cerberus: &CerberusMetadata{
				ExecutionID: uuid.New(), TenantID: tenant, VaultID: tenant, ActorUserID: tenant, ProfileID: profile, ProfileName: "QA",
				TaskType: string(TaskGeneral), Surface: SurfaceChat, Strategy: string(ActionSequential), MinimumSuccessfulResults: 1,
				RetryPolicy: RetryFast, PredictedCostUsdMicros: ours, BudgetReservationUsdMicros: ours, Mode: ModeManaged,
			},
			CredentialMode: ModeManaged,
		}
	}
	transportWith := func(adapter Adapter, decision Decision) *Transport {
		return &Transport{Registry: NewRegistry(nil, adapter), Router: fixedRouter{decision}, RouterTelemetry: NewRouterTelemetryService(pool)}
	}

	t.Run("TestReservationIsSubtractedOnceAcrossSuccessfulRounds", func(t *testing.T) {
		tenant, original := seed(t)
		adapter := &scriptedAdapter{kind: Anthropic, outcomes: []outcome{reply(answer), reply(answer), reply(answer)}}
		transport := transportWith(adapter, original)
		stripped := original.WithoutBudgetReservation()

		var managed []int64
		for _, decision := range []Decision{original, stripped, stripped} {
			_, err := transport.ChatCompletionsDecided(ctx, payload, "k", "", decision)
			require.NoError(t, err)
			managed = append(managed, read(t, tenant).managed)
		}
		end := read(t, tenant)
		assert.Equal(t, others, end.reserved, "ours subtracted exactly once; the other request's hold untouched")
		assert.Equal(t, int64(3), end.requests)
		assert.Positive(t, managed[0], "round 1 records cost")
		assert.Greater(t, managed[1], managed[0], "round 2 still records its own cost")
		assert.Greater(t, managed[2], managed[1], "round 3 still records its own cost")
	})
	t.Run("TestReservationIsSubtractedOnceWhenRoundThreeFails", func(t *testing.T) {
		tenant, original := seed(t)
		adapter := &scriptedAdapter{kind: Anthropic, outcomes: []outcome{
			reply(answer), reply(answer), failing(Permanent(Anthropic, 400, strPtr("bad"))),
		}}
		transport := transportWith(adapter, original)
		stripped := original.WithoutBudgetReservation()

		for _, decision := range []Decision{original, stripped} {
			_, err := transport.ChatCompletionsDecided(ctx, payload, "k", "", decision)
			require.NoError(t, err)
		}
		_, err := transport.ChatCompletionsDecided(ctx, payload, "k", "", stripped)
		var up *UpstreamErrorResponse
		require.ErrorAs(t, err, &up)

		end := read(t, tenant)
		assert.Equal(t, others, end.reserved)
		assert.Equal(t, int64(2), end.requests)
	})
}
