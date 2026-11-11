package llm

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cerberusFixture struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	newUser func() uuid.UUID
}

// makeUser inserts the row profiles hang off. Swift's tests do the same.
func (f cerberusFixture) makeUser(tier, override string) *User {
	return &User{ID: f.newUser(), Tier: tier, TierOverride: override}
}

type cerberusOptions struct {
	table       Decision
	registry    *Registry
	freeLane    *FreeLaneRuntime
	ensembles   bool
	credentials CredentialSource
}

func (f cerberusFixture) router(o cerberusOptions) *CerberusRouter {
	if o.registry == nil {
		o.registry = enabledRegistry(OpenRouter, Nvidia)
	}
	preferences := NewPreferenceRepository(f.pool)
	return &CerberusRouter{
		Profiles:         NewRouterProfileRepository(f.pool, preferences, ManagedModel),
		Fallback:         fixedRouter{o.table},
		Budget:           NewRouterTelemetryService(f.pool),
		EnsemblesEnabled: o.ensembles,
		Credentials:      o.credentials,
		Registry:         o.registry,
		FreeLane:         o.freeLane,
	}
}

func (f cerberusFixture) runtime(perUser int64) *FreeLaneRuntime {
	return &FreeLaneRuntime{
		Gate:                     NewFreeLaneGate(f.pool, FreeLaneLimits{PerUserDaily: perUser, PerLegDaily: map[FreeLaneLeg]int64{LegOpenRouterFree: 1000, LegNvidiaDirect: 1000}}),
		OpenRouterModel:          FreeLaneOpenRouterModel,
		OpenRouterSecondaryModel: FreeLaneOpenRouterSecondaryModel,
		NvidiaModel:              FreeLaneNvidiaModel,
	}
}

func (f cerberusFixture) resetLegBuckets(t *testing.T) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, `DELETE FROM workflow_spend_buckets WHERE scope_key LIKE 'freelane:leg:%'`)
	require.NoError(t, err)
}

func (f cerberusFixture) byokPreference(t *testing.T, tenant uuid.UUID) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, `INSERT INTO user_llm_preferences (id, tenant_id, primary_provider, primary_model, fallback_chain, mode)
		VALUES (gen_random_uuid(), $1, 'anthropic', 'claude-opus-4-7', '{"steps":[]}'::jsonb, 'byok')`, tenant)
	require.NoError(t, err)
}

// credentialFor reports one spendable key, for one provider.
type credentialFor map[ProviderKind]string

func (c credentialFor) Credential(_ context.Context, provider ProviderKind, _ uuid.UUID) (*ResolvedCredential, error) {
	if key, ok := c[provider]; ok {
		return &ResolvedCredential{APIKey: &key}, nil
	}
	return nil, nil
}

var (
	// gatewayTable is FreeLaneRoutingTests' stand-in: all billable gateway.
	gatewayTable = Decision{Primary: Route{HermesGateway, "hermes-3"}, Fallbacks: []Route{{HermesGateway, "hermes-3-small"}}}
	// platformFirstTable is BYOHermesRoutingTests': the gateway ranked last.
	platformFirstTable = Decision{Primary: Route{Anthropic, "claude-sonnet-4-6"}, Fallbacks: []Route{{OpenAI, "gpt-5"}, {HermesGateway, "hermes-3"}}}
)

func hasGateway(d Decision) bool {
	return slices.ContainsFunc(d.Candidates(), func(r Route) bool { return r.Provider == HermesGateway })
}

func modelsOfRoutes(routes []Route) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.ModelID)
	}
	return out
}

// One database for every case: each owns its tenant, and the shared leg buckets
// are cleared where they matter.
func TestCerberusRouter(t *testing.T) {
	ctx, pool, newUser := llmDB(t)
	f := cerberusFixture{ctx, pool, newUser}

	// BYOHermesRoutingTests.
	t.Run("BYO Hermes", func(t *testing.T) {
		own := WithValues(ctx, func(v *Values) {
			auth := "Bearer gateway-key"
			v.CurrentResolution = &Resolution{BaseURL: mustParseURL("http://100.105.117.67:8642"), AuthHeader: &auth, IsUserOverride: true}
		})
		router := f.router(cerberusOptions{table: platformFirstTable, registry: enabledRegistry(Anthropic)})
		t.Run("a BYO tenant is routed to their own gateway, not a platform provider", func(t *testing.T) {
			d := router.Pick(own, "", CapabilityHigh, f.makeUser("pro", ""))
			assert.Equal(t, Route{HermesGateway, "hermes-3"}, d.Primary)
		})
		t.Run("a BYO tenant has no platform fallback to leak to", func(t *testing.T) {
			d := router.Pick(own, "", CapabilityHigh, f.makeUser("pro", ""))
			assert.Empty(t, d.Fallbacks)
			for _, c := range d.Candidates() {
				assert.Equal(t, HermesGateway, c.Provider)
			}
		})
		t.Run("the decision still reports itself as deferred to hermes", func(t *testing.T) {
			d := router.Pick(own, "", CapabilityHigh, f.makeUser("pro", ""))
			require.NotNil(t, d.Cerberus)
			assert.True(t, d.Cerberus.DeferredToHermes)
			assert.Equal(t, "BYO Hermes", d.Cerberus.ProfileName)
			assert.Zero(t, d.Cerberus.PredictedCostUsdMicros)
			assert.Equal(t, "hermes-3", d.Cerberus.Routes[0].Model)
			assert.Equal(t, ModeBYOK, d.CredentialMode)
			assert.Equal(t, string(PolicyLocked), d.Cerberus.RoutingPolicy)
		})
		t.Run("a managed tenant keeps the platform table", func(t *testing.T) {
			managed := WithValues(ctx, func(v *Values) {
				v.CurrentResolution = &Resolution{BaseURL: mustParseURL("http://managed.internal:8642")}
			})
			router := f.router(cerberusOptions{table: platformFirstTable, registry: enabledRegistry(Anthropic)})
			// Swift's case never saves this user, so seeding the default profile hits
			// the users foreign key and the error path returns the table. It pins
			// that path, not managed routing, which the "decision" cases cover.
			d := router.Pick(managed, "", CapabilityHigh, &User{ID: uuid.New(), Tier: "pro"})
			assert.Equal(t, Anthropic, d.Primary.Provider)
			assert.Nil(t, d.Cerberus)
		})
		t.Run("the gateway model falls back to the caller's hint, then the table", func(t *testing.T) {
			noGateway := Decision{Primary: Route{Anthropic, "claude-sonnet-4-6"}}
			router := f.router(cerberusOptions{table: noGateway, registry: enabledRegistry(Anthropic)})
			assert.Equal(t, Route{HermesGateway, "hint"}, router.Pick(own, "hint", CapabilityHigh, f.makeUser("pro", "")).Primary)
			assert.Equal(t, Route{HermesGateway, "claude-sonnet-4-6"}, router.Pick(own, "", CapabilityHigh, f.makeUser("pro", "")).Primary)
		})
	})

	// FreeLaneRoutingTests.
	t.Run("free lane", func(t *testing.T) {
		t.Run("each streamed message claims exactly one free-lane grant", func(t *testing.T) {
			f.resetLegBuckets(t)
			user := f.makeUser("lapsed", "none")
			router := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(1)})
			lane := &scriptedAdapter{kind: OpenRouter, outcomes: []outcome{reply(`{"choices":[{"message":{"role":"assistant","content":"free reply"}}]}`)}}
			reserve := &scriptedAdapter{kind: Nvidia}
			transport := &Transport{Registry: NewRegistry(nil, lane, reserve), Router: router}
			turnCtx := WithValues(ctx, func(v *Values) { v.CurrentUser = user })
			payload := []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)

			chunks, err := collect(transport.ChatStream(turnCtx, payload, user.ID.String(), "c1"))
			require.NoError(t, err)
			require.Len(t, chunks, 1)
			assert.Equal(t, "free reply", chunks[0].Delta)
			assert.Equal(t, 1, lane.callCount())

			_, err = collect(transport.ChatStream(turnCtx, payload, user.ID.String(), "c1"))
			var exhausted *FreeLaneExhaustedError
			require.ErrorAs(t, err, &exhausted)
			assert.Equal(t, 1, lane.callCount(), "the refused turn must not have been dispatched")
		})
		t.Run("a lapsed user routes to the free lane and never to the billable gateway", func(t *testing.T) {
			f.resetLegBuckets(t)
			d := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(20)}).Pick(ctx, "", CapabilityMedium, f.makeUser("lapsed", "none"))
			assert.Equal(t, Route{OpenRouter, FreeLaneOpenRouterModel}, d.Primary)
			assert.False(t, hasGateway(d))
			assert.Equal(t, []string{FreeLaneOpenRouterSecondaryModel, FreeLaneNvidiaModel}, modelsOfRoutes(d.Fallbacks))
			assert.Equal(t, []ProviderKind{OpenRouter, Nvidia}, providersOf(d.Fallbacks))
		})
		t.Run("an entitled byok user with no keys lands on the free lane, not a 403", func(t *testing.T) {
			f.resetLegBuckets(t)
			user := f.makeUser("pro", "none")
			f.byokPreference(t, user.ID)
			d := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(20)}).Pick(ctx, "", CapabilityMedium, user)
			assert.Equal(t, Route{OpenRouter, FreeLaneOpenRouterModel}, d.Primary)
			assert.Equal(t, ModeManaged, d.CredentialMode)
			assert.False(t, d.Cerberus.FreeLaneExhausted)
			assert.False(t, hasGateway(d))
		})
		t.Run("the free lane presents as managed and reserves nothing", func(t *testing.T) {
			f.resetLegBuckets(t)
			d := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(20)}).Pick(ctx, "", CapabilityMedium, f.makeUser("lapsed", "none"))
			assert.Equal(t, ModeManaged, d.CredentialMode)
			require.NotNil(t, d.Cerberus)
			assert.Equal(t, ModeManaged, d.Cerberus.Mode)
			assert.Equal(t, DisclosureHidden, DisclosureForMode(d.Cerberus.Mode))
			assert.Zero(t, d.Cerberus.PredictedCostUsdMicros)
			assert.Zero(t, d.Cerberus.BudgetReservationUsdMicros)
			assert.False(t, d.Cerberus.BudgetDenied)
			assert.False(t, d.Cerberus.FreeLaneExhausted)
			assert.True(t, d.Cerberus.IsFreeLane)
			assert.Equal(t, "Free lane notEntitled", d.Cerberus.Reason)
			assert.Equal(t, string(PolicyLocked), d.Cerberus.RoutingPolicy)
		})
		for _, tier := range []string{"pro", "ultimate", "trial"} {
			t.Run("entitled tiers keep their own routing: "+tier, func(t *testing.T) {
				d := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(20)}).Pick(ctx, "", CapabilityMedium, f.makeUser(tier, "none"))
				assert.NotEqual(t, FreeLaneOpenRouterModel, d.Primary.ModelID)
				assert.False(t, d.Cerberus.FreeLaneExhausted)
				assert.False(t, d.Cerberus.IsFreeLane)
			})
		}
		t.Run("a tier_override rescues a lapsed user from the lane", func(t *testing.T) {
			d := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(20)}).Pick(ctx, "", CapabilityMedium, f.makeUser("lapsed", "pro"))
			assert.NotEqual(t, FreeLaneOpenRouterModel, d.Primary.ModelID)
		})
		t.Run("a platform outage degrades a paying user onto the lane", func(t *testing.T) {
			f.resetLegBuckets(t)
			d := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(20), registry: enabledRegistry(Nvidia)}).Pick(ctx, "", CapabilityMedium, f.makeUser("pro", "none"))
			assert.Equal(t, Route{Nvidia, FreeLaneNvidiaModel}, d.Primary)
			assert.False(t, hasGateway(d))
			assert.Equal(t, "Free lane platformUnavailable", d.Cerberus.Reason)
		})
		t.Run("an exhausted lane surfaces a retry hint instead of a route", func(t *testing.T) {
			f.resetLegBuckets(t)
			user := f.makeUser("lapsed", "none")
			router := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(1)})
			router.Pick(ctx, "", CapabilityMedium, user)
			d := router.Pick(ctx, "", CapabilityMedium, user)
			require.NotNil(t, d.Cerberus)
			assert.True(t, d.Cerberus.FreeLaneExhausted)
			assert.Positive(t, d.Cerberus.FreeLaneRetryAfterSeconds)
			assert.Zero(t, d.Cerberus.BudgetReservationUsdMicros)
			assert.Equal(t, "Free lane exhausted", d.Cerberus.Reason)
		})
		t.Run("no funded leg is unavailability rather than a gateway fallback", func(t *testing.T) {
			user := f.makeUser("lapsed", "none")
			runtime := f.runtime(5)
			d := f.router(cerberusOptions{table: gatewayTable, freeLane: runtime, registry: enabledRegistry()}).Pick(ctx, "", CapabilityMedium, user)
			assert.True(t, d.Cerberus.FreeLaneUnavailable)
			assert.False(t, d.Cerberus.FreeLaneExhausted)
			assert.Empty(t, d.Fallbacks)
			assert.Equal(t, int64(5), runtime.Gate.RemainingToday(ctx, user.ID), "no grant may be charged")
			assert.Equal(t, []string{"upgrade", "add_key"}, d.Cerberus.FreeLaneActions)
		})
		t.Run("an unavailable lane offers a paying byok user managed, not an upgrade", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			f.byokPreference(t, user.ID)
			d := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(20), registry: enabledRegistry()}).Pick(ctx, "", CapabilityMedium, user)
			assert.True(t, d.Cerberus.FreeLaneUnavailable)
			assert.Equal(t, []string{"add_key", "switch_to_managed"}, d.Cerberus.FreeLaneActions)
		})
		t.Run("an exhausted lane offers a paying byok user managed, not an upgrade", func(t *testing.T) {
			f.resetLegBuckets(t)
			user := f.makeUser("pro", "none")
			f.byokPreference(t, user.ID)
			router := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(1)})
			router.Pick(ctx, "", CapabilityMedium, user)
			d := router.Pick(ctx, "", CapabilityMedium, user)
			var exhausted *FreeLaneExhaustedError
			require.ErrorAs(t, d.Cerberus.PreflightError(), &exhausted)
			assert.Equal(t, []string{"add_key", "switch_to_managed"}, exhausted.Actions)
		})
		t.Run("the kill switch restores pre-lane routing", func(t *testing.T) {
			d := f.router(cerberusOptions{table: gatewayTable}).Pick(ctx, "", CapabilityMedium, f.makeUser("lapsed", "none"))
			assert.NotEqual(t, FreeLaneOpenRouterModel, d.Primary.ModelID)
			assert.False(t, d.Cerberus.FreeLaneExhausted)
		})
	})

	// BYOKConsistencyTests, at the layer below the routes they call. The HTTP half
	// (/v1/router/{id}, /v1/me/preferences/llm, /v1/llm/chat) belongs with those
	// routes' owners in wave 5.
	t.Run("BYOK consistency", func(t *testing.T) {
		t.Run("trial user can set default router profile to byok", func(t *testing.T) {
			user := f.makeUser("trial", "none")
			repo := NewRouterProfileRepository(pool, NewPreferenceRepository(pool), ManagedModel)
			active, err := repo.Resolve(ctx, user.ID, nil, "", "")
			require.NoError(t, err)
			revision := active.Revision
			updated, err := repo.Update(ctx, user.ID, active.ID, RouterProfileWrite{
				Name: active.Name, Mode: ModeBYOK, Objective: active.Document.Objective, Budget: active.Document.Budget,
				AllowedProviders: active.Document.AllowedProviders, BlockedProviders: active.Document.BlockedProviders,
				DefaultAction: RouterAction{Kind: ActionSequential, RetryPolicy: RetryFast, Routes: []WireRoute{{Provider: ManagedProvider, Model: ManagedModel}}},
				Rules:         active.Document.Rules, RoutingPolicy: PolicyBalanced, ExpectedRevision: &revision,
			})
			require.NoError(t, err)
			assert.Equal(t, ModeBYOK, updated.BrainMode())
		})
		t.Run("byok chat without provider keys reaches the free lane, not a 403", func(t *testing.T) {
			user := f.makeUser("trial", "none")
			f.byokPreference(t, user.ID)
			d := f.router(cerberusOptions{table: gatewayTable, freeLane: f.runtime(20), registry: enabledRegistry()}).Pick(ctx, "", CapabilityMedium, user)
			var unavailable *FreeLaneUnavailableError
			require.ErrorAs(t, d.Cerberus.PreflightError(), &unavailable)
			assert.False(t, isByokRequired(d.Cerberus.PreflightError()))
			assert.Contains(t, unavailable.Actions, "add_key")
			assert.Contains(t, unavailable.Actions, "switch_to_managed")
			assert.NotContains(t, unavailable.Actions, "upgrade")
		})
	})

	// CerberusModelRouter's branches no Swift suite pins directly.
	t.Run("decision", func(t *testing.T) {
		repo := NewRouterProfileRepository(pool, NewPreferenceRepository(pool), ManagedModel)
		byokProfile := func(t *testing.T, user *User, write RouterProfileWrite) *RouterProfile {
			t.Helper()
			profile, err := repo.Resolve(ctx, user.ID, nil, "", "")
			require.NoError(t, err)
			write.Name, write.Mode = profile.Name, ModeBYOK
			if write.Objective == (RouterObjective{}) {
				write.Objective = profile.Document.Objective
			}
			if write.AllowedProviders == nil {
				write.AllowedProviders = []ProviderID{}
			}
			write.BlockedProviders = []ProviderID{}
			if write.Rules == nil {
				write.Rules = []RouterRule{}
			}
			if write.DefaultAction.Kind == "" {
				write.DefaultAction = RouterAction{Kind: ActionSequential, RetryPolicy: RetryResilient, Routes: []WireRoute{wireRoute(Anthropic, "claude-sonnet-4-6"), wireRoute(OpenAI, "gpt-4o")}}
			}
			updated, err := repo.Update(ctx, user.ID, profile.ID, write)
			require.NoError(t, err)
			return updated
		}
		anthropicKey := credentialFor{Anthropic: "k"}

		t.Run("no user, no decision: the table answers", func(t *testing.T) {
			d := f.router(cerberusOptions{table: platformFirstTable}).Pick(ctx, "", CapabilityMedium, nil)
			assert.Nil(t, d.Cerberus)
			assert.Equal(t, platformFirstTable.Primary, d.Primary)
		})
		t.Run("managed autoSmart rides the gateway and ends on the table's gateway routes", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			d := f.router(cerberusOptions{table: gatewayTable}).Pick(ctx, "", CapabilityMedium, user)
			require.NotNil(t, d.Cerberus)
			assert.Equal(t, HermesGateway, d.Primary.Provider)
			assert.Equal(t, ModeManaged, d.CredentialMode)
			assert.Equal(t, string(PolicyAutoSmart), d.Cerberus.RoutingPolicy)
			assert.Equal(t, ProviderID(OpenRouter), d.Cerberus.Routes[0].Provider, "metadata keeps the OpenRouter id")
			assert.Equal(t, d.Primary.ModelID, d.Cerberus.Routes[0].Model)
			assert.Contains(t, d.Fallbacks, Route{HermesGateway, "hermes-3"})
			assert.Contains(t, d.Fallbacks, Route{HermesGateway, "hermes-3-small"})
			assert.Equal(t, user.ID, d.Cerberus.VaultID, "the vault defaults to the tenant")
			assert.Equal(t, SurfaceSystem, d.Cerberus.Surface, "the scope defaults to system")
		})
		t.Run("byok with zero keys and the lane off fails closed", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			f.byokPreference(t, user.ID)
			d := f.router(cerberusOptions{table: gatewayTable}).Pick(ctx, "", CapabilityMedium, user)
			assert.True(t, d.Cerberus.BYOKKeysRequired)
			assert.Equal(t, ModeBYOK, d.CredentialMode)
			assert.Equal(t, "Add an API key (OpenRouter recommended) or switch to Managed", d.Cerberus.Reason)
			assert.Empty(t, d.Fallbacks)
			assert.True(t, isByokRequired(d.Cerberus.PreflightError()))
		})
		t.Run("byok autoSmart without an OpenRouter key degrades to balanced", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			byokProfile(t, user, RouterProfileWrite{RoutingPolicy: PolicyAutoSmart})
			d := f.router(cerberusOptions{table: gatewayTable, credentials: anthropicKey}).Pick(ctx, "", CapabilityMedium, user)
			assert.Equal(t, string(PolicyBalanced), d.Cerberus.RoutingPolicy)
			assert.Equal(t, Anthropic, d.Primary.Provider)
			assert.Equal(t, RetryResilient, d.Cerberus.RetryPolicy)
			assert.False(t, hasGateway(d), "byok never falls back to the managed gateway")
		})
		t.Run("byok autoSmart with an OpenRouter key dispatches direct", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			byokProfile(t, user, RouterProfileWrite{RoutingPolicy: PolicyAutoSmart, DefaultAction: RouterAction{Kind: ActionSequential, RetryPolicy: RetryFast, Routes: []WireRoute{{Provider: ManagedProvider, Model: ManagedModel}}}})
			d := f.router(cerberusOptions{table: gatewayTable, credentials: credentialFor{OpenRouter: "k"}}).Pick(ctx, "", CapabilityMedium, user)
			assert.Equal(t, OpenRouter, d.Primary.Provider)
			assert.Equal(t, ModeBYOK, d.CredentialMode)
		})
		t.Run("a stale allow-list is bypassed rather than answering byok_keys_required", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			byokProfile(t, user, RouterProfileWrite{RoutingPolicy: PolicyBalanced, AllowedProviders: []ProviderID{ManagedProvider}})
			d := f.router(cerberusOptions{table: gatewayTable, credentials: anthropicKey}).Pick(ctx, "", CapabilityMedium, user)
			assert.False(t, d.Cerberus.BYOKKeysRequired)
			assert.Equal(t, Anthropic, d.Primary.Provider)
		})
		t.Run("a locked profile uses its own routes, in score order, with no pool expansion", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			byokProfile(t, user, RouterProfileWrite{RoutingPolicy: PolicyLocked, DefaultAction: RouterAction{Kind: ActionSequential, RetryPolicy: RetryFast, Routes: []WireRoute{wireRoute(Anthropic, "claude-sonnet-4-6")}}})
			d := f.router(cerberusOptions{table: gatewayTable, credentials: anthropicKey}).Pick(ctx, "", CapabilityMedium, user)
			assert.Equal(t, Route{Anthropic, "claude-sonnet-4-6"}, d.Primary)
			assert.Empty(t, d.Fallbacks)
			assert.Equal(t, string(PolicyLocked), d.Cerberus.RoutingPolicy)
		})
		t.Run("a locked profile with an excluded route fails closed", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			byokProfile(t, user, RouterProfileWrite{RoutingPolicy: PolicyLocked, AllowedProviders: []ProviderID{ProviderID(OpenAI)}, DefaultAction: RouterAction{Kind: ActionSequential, RetryPolicy: RetryFast, Routes: []WireRoute{wireRoute(Anthropic, "claude-sonnet-4-6")}}})
			d := f.router(cerberusOptions{table: gatewayTable, credentials: anthropicKey}).Pick(ctx, "", CapabilityMedium, user)
			assert.True(t, d.Cerberus.BYOKKeysRequired)
			assert.Equal(t, "No BYOK providers are available for this router profile", d.Cerberus.Reason)
		})
		t.Run("the lowest priority matching rule wins, and its id is recorded", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			lockedTo := func(model string) RouterAction {
				return RouterAction{Kind: ActionSequential, RetryPolicy: RetryFast, Routes: []WireRoute{wireRoute(Anthropic, model)}}
			}
			winner, loser, disabled, otherTask := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			rule := func(id uuid.UUID, priority int, enabled bool, tasks []RouterTaskType, model string) RouterRule {
				return RouterRule{ID: id, Name: "r", Enabled: enabled, Priority: priority, TaskTypes: tasks, Surfaces: []RouterSurface{}, Action: lockedTo(model)}
			}
			byokProfile(t, user, RouterProfileWrite{RoutingPolicy: PolicyLocked, Rules: []RouterRule{
				rule(loser, 5, true, []RouterTaskType{}, "claude-sonnet-4-6"),
				rule(disabled, 0, false, []RouterTaskType{}, "disabled"),
				rule(otherTask, 1, true, []RouterTaskType{TaskCreative}, "wrong-task"),
				rule(winner, 2, true, []RouterTaskType{TaskCoding}, "claude-opus-4-1"),
			}})
			promptCtx := WithValues(ctx, func(v *Values) { v.CerberusPrompt = "Debug this Swift function" })
			d := f.router(cerberusOptions{table: gatewayTable, credentials: anthropicKey}).Pick(promptCtx, "", CapabilityMedium, user)
			assert.Equal(t, Route{Anthropic, "claude-opus-4-1"}, d.Primary)
			require.NotNil(t, d.Cerberus.RuleID)
			assert.Equal(t, winner, *d.Cerberus.RuleID)
			assert.Equal(t, string(TaskCoding), d.Cerberus.TaskType)
		})
		t.Run("a soft limit makes cost dominate and a hard limit denies the reservation", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			routes := []WireRoute{wireRoute(Anthropic, "claude-opus-4-1"), wireRoute(Anthropic, "claude-3-5-haiku-20241022")}
			zero := int64(0)
			one := int64(1)
			profile := byokProfile(t, user, RouterProfileWrite{
				RoutingPolicy: PolicyLocked, Objective: RouterObjective{Quality: 100},
				Budget:        RouterBudget{SoftLimitUsdMicros: &zero},
				DefaultAction: RouterAction{Kind: ActionSequential, RetryPolicy: RetryFast, Routes: routes},
			})
			router := f.router(cerberusOptions{table: gatewayTable, credentials: anthropicKey})
			d := router.Pick(ctx, "", CapabilityMedium, user)
			assert.Equal(t, "claude-3-5-haiku-20241022", d.Primary.ModelID, "soft limit: 10/80/10 weights")
			assert.False(t, d.Cerberus.BudgetDenied)
			assert.Positive(t, d.Cerberus.BudgetReservationUsdMicros)
			assert.Equal(t, d.Cerberus.PredictedCostUsdMicros, d.Cerberus.BudgetReservationUsdMicros)

			_, err := repo.Update(ctx, user.ID, profile.ID, RouterProfileWrite{
				Name: profile.Name, Mode: ModeBYOK, Objective: RouterObjective{Quality: 100},
				Budget: RouterBudget{HardLimitUsdMicros: &one}, RoutingPolicy: PolicyLocked,
				AllowedProviders: []ProviderID{}, BlockedProviders: []ProviderID{}, Rules: []RouterRule{},
				DefaultAction: RouterAction{Kind: ActionSequential, RetryPolicy: RetryFast, Routes: routes},
			})
			require.NoError(t, err)
			d = router.Pick(ctx, "", CapabilityMedium, user)
			assert.True(t, d.Cerberus.BudgetDenied)
			assert.Zero(t, d.Cerberus.BudgetReservationUsdMicros)
			assert.Positive(t, d.Cerberus.PredictedCostUsdMicros)
			var capped *UsageCapExceededError
			assert.ErrorAs(t, d.Cerberus.PreflightError(), &capped)
		})
		t.Run("a requested strategy becomes an ensemble only for ultimate with the gate on", func(t *testing.T) {
			user := f.makeUser("ultimate", "none")
			byokProfile(t, user, RouterProfileWrite{RoutingPolicy: PolicyLocked})
			parallel := WithValues(ctx, func(v *Values) { v.ParallelStrategy = ParallelBestOfN })
			on := f.router(cerberusOptions{table: gatewayTable, credentials: credentialFor{Anthropic: "k", OpenAI: "k"}, ensembles: true})

			d := on.Pick(parallel, "", CapabilityMedium, user)
			assert.Equal(t, StrategyEnsemble, d.Cerberus.Strategy)
			assert.Equal(t, ParallelBestOfN, d.Cerberus.ParallelStrategy)
			assert.Equal(t, 1, d.Cerberus.MinimumSuccessfulResults)
			require.NotNil(t, d.Cerberus.SynthesisRoute)

			off := f.router(cerberusOptions{table: gatewayTable, credentials: credentialFor{Anthropic: "k", OpenAI: "k"}})
			d = off.Pick(parallel, "", CapabilityMedium, user)
			assert.Equal(t, string(ActionSequential), d.Cerberus.Strategy)
			assert.Empty(t, d.Cerberus.ParallelStrategy)

			pro := f.makeUser("pro", "none")
			byokProfile(t, pro, RouterProfileWrite{RoutingPolicy: PolicyLocked})
			d = on.Pick(parallel, "", CapabilityMedium, pro)
			assert.Equal(t, string(ActionSequential), d.Cerberus.Strategy)
		})
		t.Run("an ensemble action is downgraded when the gate is off", func(t *testing.T) {
			user := f.makeUser("ultimate", "none")
			routes := []WireRoute{wireRoute(Anthropic, "claude-sonnet-4-6"), wireRoute(OpenAI, "gpt-4o")}
			byokProfile(t, user, RouterProfileWrite{RoutingPolicy: PolicyLocked, DefaultAction: RouterAction{Kind: ActionEnsemble, RetryPolicy: RetryResilient, Routes: routes}})
			d := f.router(cerberusOptions{table: gatewayTable, credentials: credentialFor{Anthropic: "k", OpenAI: "k"}}).Pick(ctx, "", CapabilityMedium, user)
			assert.Equal(t, string(ActionSequential), d.Cerberus.Strategy)
			assert.Equal(t, RetryResilient, d.Cerberus.RetryPolicy)
			assert.Len(t, d.Candidates(), 2)
		})
		t.Run("an unreadable profile document falls back to the table", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			profile, err := repo.Resolve(ctx, user.ID, nil, "", "")
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `UPDATE router_profiles SET document = '{"objective":{}}'::jsonb WHERE id = $1`, profile.ID)
			require.NoError(t, err)
			d := f.router(cerberusOptions{table: platformFirstTable}).Pick(ctx, "", CapabilityMedium, user)
			assert.Nil(t, d.Cerberus)
			assert.Equal(t, platformFirstTable.Primary, d.Primary)
		})
		t.Run("an unreadable credential is not an absent one, but still fails closed", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			f.byokPreference(t, user.ID)
			d := f.router(cerberusOptions{table: gatewayTable, credentials: stubCredentials{err: context.DeadlineExceeded}}).Pick(ctx, "", CapabilityMedium, user)
			assert.True(t, d.Cerberus.BYOKKeysRequired)
		})
		t.Run("scope values reach the metadata", func(t *testing.T) {
			user := f.makeUser("pro", "none")
			space, conversation, vault := uuid.New(), uuid.New(), uuid.New()
			scoped := WithValues(ctx, func(v *Values) {
				v.CerberusScope = &CerberusScope{Surface: SurfaceChat, SpaceID: &space, ConversationID: &conversation}
				v.AnalyticsVaultID = &vault
			})
			d := f.router(cerberusOptions{table: gatewayTable}).Pick(scoped, "", CapabilityMedium, user)
			assert.Equal(t, SurfaceChat, d.Cerberus.Surface)
			assert.Equal(t, &space, d.Cerberus.SpaceID)
			assert.Equal(t, &conversation, d.Cerberus.ConversationID)
			assert.Equal(t, vault, d.Cerberus.VaultID)
			assert.Equal(t, user.ID, d.Cerberus.ActorUserID)
		})
	})
}

func TestScoring(t *testing.T) {
	opus, haiku := wireRoute(Anthropic, "claude-opus-4-1"), wireRoute(Anthropic, "claude-3-5-haiku-20241022")
	t.Run("quality weights prefer the stronger model, cost weights the cheaper", func(t *testing.T) {
		routes := []WireRoute{haiku, opus}
		assert.Equal(t, opus, scoreRoutes(routes, TaskCoding, RouterObjective{Quality: 100}, 10, false)[0])
		assert.Equal(t, haiku, scoreRoutes(routes, TaskCoding, RouterObjective{Cost: 100}, 10, false)[0])
		assert.Equal(t, haiku, scoreRoutes(routes, TaskCoding, RouterObjective{Quality: 100}, 10, true)[0], "cost first overrides the profile")
	})
	t.Run("ties keep their input order", func(t *testing.T) {
		a, b := wireRoute(Anthropic, "unknown-a"), wireRoute(Anthropic, "unknown-b")
		assert.Equal(t, []WireRoute{a, b}, scoreRoutes([]WireRoute{a, b}, TaskGeneral, RouterObjective{Quality: 1}, 1, false))
		assert.Equal(t, []WireRoute{b, a}, scoreRoutes([]WireRoute{b, a}, TaskGeneral, RouterObjective{Quality: 1}, 1, false))
	})
	t.Run("predicted cost uses integer division, synthesis and debate rounds", func(t *testing.T) {
		input, output := int64(1_500_000), int64(3_000_000)
		route := WireRoute{Provider: "custom", Model: "m", InputPerMillionUsdMicros: &input, OutputPerMillionUsdMicros: &output}
		// 3 * 1.5 = 4 (4.5 truncated), 1024 * 3 = 3072.
		assert.Equal(t, int64(3076), predictedCost([]WireRoute{route}, nil, 3, 1))
		assert.Equal(t, int64(6152), predictedCost([]WireRoute{route}, nil, 3, 2))
		// Synthesis reads promptTokens * max(2, routes) inputs: 6 * 1.5 = 9.
		assert.Equal(t, int64(3076+9+3072), predictedCost([]WireRoute{route}, &route, 3, 1))
		assert.Zero(t, predictedCost([]WireRoute{wireRoute(Custom, "not-catalogued")}, nil, 100, 1))
	})
	t.Run("a route's own rate beats the catalog's", func(t *testing.T) {
		entry, ok := CatalogEntry(ManagedProvider, ManagedModel)
		require.True(t, ok)
		zero := int64(0)
		overridden := WireRoute{Provider: ManagedProvider, Model: ManagedModel, InputPerMillionUsdMicros: &zero, OutputPerMillionUsdMicros: &zero}
		assert.Zero(t, predictedCost([]WireRoute{overridden}, nil, 1000, 1))
		assert.Equal(t, 1024*entry.OutputPerMillionUsdMicros/1_000_000, predictedCost([]WireRoute{{Provider: ManagedProvider, Model: ManagedModel}}, nil, 1, 1))
	})
}

func TestIsSpendable(t *testing.T) {
	key, empty := "k", ""
	host := mustParseURL("http://10.0.0.1:11434")
	assert.True(t, OpenAI.IsSpendable(&key, nil))
	assert.False(t, OpenAI.IsSpendable(&empty, host), "a base URL alone proves nothing")
	assert.False(t, OpenAI.IsSpendable(nil, host))
	assert.True(t, Ollama.IsSpendable(nil, host))
	assert.True(t, Custom.IsSpendable(&empty, host))
	assert.False(t, Ollama.IsSpendable(nil, nil))
	assert.False(t, Custom.IsSpendable(nil, mustParseURL("localhost")), "no scheme and no host")
}

// The chain and the modes are what App+build.swift builds from the same keys.
func TestStackExecutionModes(t *testing.T) {
	hermes := mustParseURL("http://hermes.test")
	build := func(env map[string]string) *Stack {
		return NewStack(StackConfig{Getenv: envOf(env), HermesURL: hermes, HermesDefaultModel: "hermes-3"})
	}
	unwrap := func(t *testing.T, r Router) *TrialRouter {
		t.Helper()
		trial, ok := r.(*TrialRouter)
		require.True(t, ok)
		return trial
	}
	t.Run("active is the default: Cerberus around the preference router", func(t *testing.T) {
		stack := build(nil)
		cerberus, ok := stack.Router.(*CerberusRouter)
		require.True(t, ok)
		assert.IsType(t, &UserPreferenceRouter{}, cerberus.Fallback)
		assert.False(t, cerberus.EnsemblesEnabled)
		assert.NotNil(t, cerberus.FreeLane)
		assert.NotNil(t, stack.Transport.RouterTelemetry)
		assert.Same(t, stack.Telemetry, cerberus.Budget)
		assert.Nil(t, stack.Transport.Ensemble, "ParallelExecutor is a hook")
	})
	t.Run("the mode is case-insensitive", func(t *testing.T) {
		assert.IsType(t, &CerberusRouter{}, build(map[string]string{"CERBERUS_EXECUTION_MODE": "ACTIVE"}).Router)
	})
	t.Run("any other mode keeps the legacy chain, with telemetry still wired", func(t *testing.T) {
		stack := build(map[string]string{"CERBERUS_EXECUTION_MODE": "shadow"})
		assert.IsType(t, &UserPreferenceRouter{}, stack.Router)
		assert.NotNil(t, stack.Transport.RouterTelemetry)
	})
	t.Run("the trial wraps whichever base was chosen", func(t *testing.T) {
		trial := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-trial", "ANTHROPIC_FIRST": "true"}
		assert.IsType(t, &CerberusRouter{}, unwrap(t, build(trial).Router).Inner)
		trial["CERBERUS_EXECUTION_MODE"] = "off"
		assert.IsType(t, &UserPreferenceRouter{}, unwrap(t, build(trial).Router).Inner)
	})
	t.Run("parallelEnabled defaults to ensemblesEnabled, and overrides it", func(t *testing.T) {
		enabled := func(env map[string]string) bool {
			stack := build(env)
			cerberus, ok := stack.Router.(*CerberusRouter)
			require.True(t, ok)
			assert.Equal(t, stack.ParallelEnabled, cerberus.EnsemblesEnabled)
			return cerberus.EnsemblesEnabled
		}
		assert.False(t, enabled(nil))
		assert.True(t, enabled(map[string]string{"CERBERUS_ENSEMBLES_ENABLED": "true"}))
		assert.True(t, enabled(map[string]string{"CERBERUS_PARALLEL_ENABLED": "yes"}))
		assert.False(t, enabled(map[string]string{"CERBERUS_ENSEMBLES_ENABLED": "true", "CERBERUS_PARALLEL_ENABLED": "false"}))
		assert.False(t, enabled(map[string]string{"CERBERUS_PARALLEL_ENABLED": "garbage"}))
		assert.True(t, enabled(map[string]string{"CERBERUS_ENSEMBLES_ENABLED": "1", "CERBERUS_PARALLEL_ENABLED": "garbage"}))
	})
	t.Run("FREELANE_ENABLED=false leaves the runtime nil", func(t *testing.T) {
		stack := build(map[string]string{"FREELANE_ENABLED": "false"})
		assert.Nil(t, stack.FreeLane)
		cerberus, ok := stack.Router.(*CerberusRouter)
		require.True(t, ok)
		assert.Nil(t, cerberus.FreeLane)
	})
	t.Run("freelane keys configure the runtime", func(t *testing.T) {
		stack := build(map[string]string{
			"FREELANE_PER_USER_DAILY_REQUESTS": "3", "FREELANE_OPEN_ROUTER_DAILY_REQUESTS": "4", "FREELANE_NVIDIA_DAILY_REQUESTS": "5",
			"FREELANE_OPEN_ROUTER_MODEL": "a:free", "FREELANE_OPEN_ROUTER_SECONDARY_MODEL": "b:free", "FREELANE_NVIDIA_MODEL": "c",
		})
		require.NotNil(t, stack.FreeLane)
		assert.Equal(t, "a:free", stack.FreeLane.OpenRouterModel)
		assert.Equal(t, "b:free", stack.FreeLane.OpenRouterSecondaryModel)
		assert.Equal(t, "c", stack.FreeLane.NvidiaModel)
		assert.Equal(t, int64(3), stack.FreeLane.Gate.limits.PerUserDaily)
		assert.Equal(t, int64(4), stack.FreeLane.Gate.limits.PerLegDaily[LegOpenRouterFree])
		assert.Equal(t, int64(5), stack.FreeLane.Gate.limits.PerLegDaily[LegNvidiaDirect])
	})
	t.Run("hermes.defaultManagedModel seeds the profile repository", func(t *testing.T) {
		assert.Equal(t, defaultManagedModel, build(nil).Profiles.managedModel)
		assert.Equal(t, "x/y", build(map[string]string{"HERMES_DEFAULT_MANAGED_MODEL": "x/y"}).Profiles.managedModel)
	})
	t.Run("GEMINI_API_KEY reaches the Gemini adapter, and OLLAMA's base URL its adapter", func(t *testing.T) {
		stack := build(map[string]string{"GEMINI_API_KEY": "g-key", "LLM_PROVIDER_OLLAMA_BASE_URL": "http://10.0.0.9:11434"})
		adapter, ok := stack.Registry.Adapter(Gemini)
		require.True(t, ok)
		gemini, ok := adapter.(*GeminiContentsAdapter)
		require.True(t, ok)
		assert.Equal(t, "g-key", gemini.APIKey)
		adapter, ok = stack.Registry.Adapter(Ollama)
		require.True(t, ok)
		ollama, ok := adapter.(*OllamaAdapter)
		require.True(t, ok)
		assert.Equal(t, "http://10.0.0.9:11434", ollama.DefaultBaseURL.String())
		adapter, ok = build(nil).Registry.Adapter(Ollama)
		require.True(t, ok)
		assert.Equal(t, "http://localhost:11434", adapter.(*OllamaAdapter).DefaultBaseURL.String())
		assert.False(t, strings.Contains(adapter.(*OllamaAdapter).DefaultBaseURL.String(), "?"))
	})
}
