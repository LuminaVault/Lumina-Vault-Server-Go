package llm

import (
	"context"
	"log/slog"
	"lumina/internal/entitlement"
	"lumina/internal/swiftstr"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// CerberusRouter is CerberusModelRouter: profile-aware routing. The table remains
// its safety net, so a profile lookup or validation failure never leaves a call
// without a route.
type CerberusRouter struct {
	Profiles         *RouterProfileRepository
	Fallback         Router
	Budget           *RouterTelemetryService
	EnsemblesEnabled bool
	// Credentials is nil without a BYOK store. Every BYOK tenant then fails closed.
	Credentials CredentialSource
	Registry    *Registry
	// FreeLane nil is the freelane.enabled kill switch.
	FreeLane *FreeLaneRuntime
}

var _ Router = (*CerberusRouter)(nil)

// Pick reads CerberusScope, CerberusPrompt, CurrentResolution and the parallel
// values from ctx. It never fails: the table's decision is the fallback.
func (r *CerberusRouter) Pick(ctx context.Context, model string, capability Capability, user *User) Decision {
	table := r.Fallback.Pick(ctx, model, capability, user)
	if user == nil {
		return table
	}
	values := RoutingValues(ctx)
	scope := CerberusScope{Surface: SurfaceSystem}
	if values.CerberusScope != nil {
		scope = *values.CerberusScope
	}
	turn := cerberusTurn{
		tenant: user.ID,
		vault:  user.ID,
		scope:  scope,
		prompt: values.CerberusPrompt,
		table:  table,
		task:   ClassifyTask(values.CerberusPrompt, scope.Surface),
		level:  ClassifyComplexity(values.CerberusPrompt, scope.Surface),
	}
	if values.AnalyticsVaultID != nil {
		turn.vault = *values.AnalyticsVaultID
	}
	// BYO Hermes owns model choice, so Auto defers.
	if res := values.CurrentResolution; res != nil && res.IsUserOverride {
		return turn.deferredToHermes(model)
	}
	decision, err := r.pickFromProfile(ctx, &turn, user, values)
	if err != nil {
		slog.ErrorContext(ctx, "cerberus decision failed; using table router", "error", err)
		return table
	}
	return decision
}

// cerberusTurn is what every decision on one call shares.
type cerberusTurn struct {
	tenant, vault uuid.UUID
	scope         CerberusScope
	prompt        string
	table         Decision
	task          RouterTaskType
	level         RouterComplexity
}

func (t *cerberusTurn) metadata() *CerberusMetadata {
	return &CerberusMetadata{
		ExecutionID:              uuid.New(),
		TenantID:                 t.tenant,
		VaultID:                  t.vault,
		ActorUserID:              t.tenant,
		TaskType:                 string(t.task),
		Surface:                  t.scope.Surface,
		SpaceID:                  t.scope.SpaceID,
		ConversationID:           t.scope.ConversationID,
		Strategy:                 string(ActionSequential),
		MinimumSuccessfulResults: 1,
		RetryPolicy:              RetryFast,
		Complexity:               string(t.level),
	}
}

// deferredToHermes routes to the user's own gateway, not whatever the table ranks
// first (the table appends the gateway last). There are no fallbacks: the transport
// retries down the candidates on any recoverable error, so a blip on the user's
// tailnet would send their prompt to a platform provider on our key.
func (t *cerberusTurn) deferredToHermes(model string) Decision {
	primary := Route{HermesGateway, model}
	if model == "" {
		primary.ModelID = t.table.Primary.ModelID
	}
	for _, c := range t.table.Candidates() {
		if c.Provider == HermesGateway {
			primary = c
			break
		}
	}
	selected := WireRoute{Provider: ManagedProvider, Model: primary.ModelID}
	m := t.metadata()
	m.ProfileID = uuid.New()
	m.ProfileName = "BYO Hermes"
	m.Routes = []WireRoute{selected}
	m.Mode = ModeBYOK
	m.RoutingPolicy = string(PolicyLocked)
	m.Reason = RoutingReason(PolicyLocked, t.level, t.task, selected, true)
	m.DeferredToHermes = true
	return Decision{Primary: primary, Cerberus: m, CredentialMode: ModeBYOK}
}

func (r *CerberusRouter) pickFromProfile(ctx context.Context, t *cerberusTurn, user *User, values Values) (Decision, error) {
	profile, err := r.Profiles.Resolve(ctx, t.tenant, t.scope.SpaceID, t.scope.JobID, t.scope.WorkflowID)
	if err != nil {
		return Decision{}, err
	}
	doc := profile.Document
	mode := profile.BrainMode()
	rule := matchRule(doc.Rules, t.task, t.scope.Surface)
	action := doc.DefaultAction
	if rule != nil {
		action = rule.Action
	}
	tier := entitlement.EffectiveTier(user.Tier, user.TierOverride)
	requested, parallel := requestedParallel(values)
	action = r.applyParallel(action, doc, t.table, requested, parallel, tier)

	credentialed := r.credentialedProviders(ctx, t.tenant)
	deployment := r.deploymentEnabledProviders()

	// The lane comes before the Auto downgrade and the BYOK fail-closed: for a
	// non-entitled user a working reply beats a degraded pool or a 403.
	verdict := EvaluateFreeLane(FreeLaneInput{
		EffectiveTier:                tier,
		RequestedMode:                mode,
		HasUsableUserCredential:      len(credentialed) > 0,
		PlatformPaidManagedAvailable: r.registryEnabled(OpenRouter),
		FreeLaneEnabled:              r.FreeLane != nil,
	})
	if verdict != nil {
		return r.freeLaneDecision(ctx, t, profile, rule, *verdict, FreeLaneRecoveryActions(tier, mode, verdict.Trigger)), nil
	}

	// Auto is OpenRouter-only. BYOK Auto without the tenant's own OpenRouter key
	// degrades to Balanced for this turn rather than failing the chat.
	policy := doc.RoutingPolicy
	if policy == PolicyAutoSmart && mode == ModeBYOK && !credentialed[ManagedProvider] {
		policy = PolicyBalanced
		slog.InfoContext(ctx, "cerberus autoSmart downgraded to balanced: byok tenant has no OpenRouter credential")
	}
	if mode == ModeBYOK && len(credentialed) == 0 {
		return t.byokKeysRequired(profile, rule, policy, "Add an API key (OpenRouter recommended) or switch to Managed"), nil
	}

	minTier := TierFast
	switch policy {
	case PolicyBalanced, PolicyAutoSmart:
		minTier = MinimumTier(t.level)
	case PolicyMaxQuality:
		if t.level != ComplexityLow {
			minTier = TierMax
		}
	}
	base := filterRoutes(action.Routes, doc.BlockedProviders, doc.AllowedProviders)
	pool := base
	if policy != PolicyLocked {
		pool = BuildModelPool(PoolInput{
			Mode: mode, Policy: policy, ProfileRoutes: base,
			AllowedProviders: doc.AllowedProviders, BlockedProviders: doc.BlockedProviders,
			CredentialedProviders: credentialed, DeploymentEnabledProviders: deployment, MinTier: minTier,
		})
	}
	// An empty pool for a tenant holding keys is a stale allow-list, seeded while
	// managed, not a missing key. Retry once ignoring it before failing closed.
	if len(pool) == 0 && mode == ModeBYOK && len(credentialed) > 0 && policy != PolicyLocked {
		relaxed := BuildModelPool(PoolInput{
			Mode: mode, Policy: policy, ProfileRoutes: filterRoutes(action.Routes, doc.BlockedProviders, nil),
			BlockedProviders:      doc.BlockedProviders,
			CredentialedProviders: credentialed, DeploymentEnabledProviders: deployment, MinTier: minTier,
		})
		if len(relaxed) > 0 {
			slog.WarnContext(ctx, "cerberus.route.allowlist_bypassed: profile allow-list excludes every credentialed provider; routing anyway to avoid a false byok_keys_required",
				"tenant", t.tenant, "allowed", doc.AllowedProviders)
			pool = relaxed
		}
	}
	if len(pool) == 0 {
		if mode == ModeBYOK {
			return t.byokKeysRequired(profile, rule, policy, "No BYOK providers are available for this router profile"), nil
		}
		return t.table, nil
	}

	promptTokens := max(1, swiftstr.CharacterCount(t.prompt)/4)
	effectiveParallel := requested
	if effectiveParallel == "" && action.ParallelStrategy != nil {
		effectiveParallel = *action.ParallelStrategy
	}
	workerRounds := 1
	if effectiveParallel == ParallelDebate {
		workerRounds = 2
	}
	predicted := predictedCost(pool, action.SynthesisRoute, promptTokens, workerRounds)
	budget := r.Budget.Reserve(ctx, t.tenant, predicted, doc.Budget)
	weights := doc.Objective
	if preset := policy.presetObjective(); preset != nil {
		weights = *preset
	}
	ordered := scoreRoutes(pool, t.task, weights, promptTokens, budget == BudgetSoftLimit)

	// Managed Auto rides the shared gateway with the picked OpenRouter model id:
	// the gateway holds the system key, the registry does not.
	viaGateway := mode == ModeManaged && policy == PolicyAutoSmart
	mapped := make([]Route, 0, len(ordered))
	for _, route := range ordered {
		if m, ok := ToModelRoute(route, viaGateway); ok {
			mapped = append(mapped, m)
		}
	}
	if len(mapped) == 0 {
		r.Budget.Release(ctx, t.tenant, predicted)
		if mode == ModeBYOK {
			return t.byokKeysRequired(profile, rule, policy, "Choose a supported BYOK provider for this router profile"), nil
		}
		return t.table, nil
	}
	primary, selected := mapped[0], ordered[0]

	m := t.metadata()
	t.attachProfile(m, profile, rule)
	m.Strategy = string(action.Kind)
	if action.Kind == ActionEnsemble {
		m.ParallelStrategy = effectiveParallel
		if m.ParallelStrategy == "" {
			m.ParallelStrategy = ParallelConsensus
		}
	}
	m.Participants = action.Participants
	if parallel != nil && parallel.Participants != nil {
		m.Participants = parallel.Participants
	}
	m.Routes = ordered
	m.SynthesisRoute = action.SynthesisRoute
	m.MinimumSuccessfulResults = 2
	if action.MinimumSuccessfulResults != nil {
		m.MinimumSuccessfulResults = *action.MinimumSuccessfulResults
	}
	m.RetryPolicy = action.RetryPolicy
	m.PredictedCostUsdMicros = predicted
	if budget != BudgetDenied {
		m.BudgetReservationUsdMicros = predicted
	}
	m.BudgetDenied = budget == BudgetDenied
	m.Mode = mode
	m.RoutingPolicy = string(policy)
	m.Reason = RoutingReason(policy, t.level, t.task, selected, false)

	// Managed tenants never learn which model served them, so the server log is
	// the only place a decision can be inspected.
	slog.InfoContext(ctx, "cerberus routing decision",
		"event", "cerberus.route.selected",
		"provider", selected.Provider,
		"model", selected.Model,
		"dispatch_provider", primary.Provider,
		"task", t.task,
		"complexity", t.level,
		"policy", policy,
		"mode", mode,
		"tier", RouteTier(selected),
		"pool_size", len(pool),
		"surface", t.scope.Surface,
	)

	fallbacks := slices.Clone(mapped[1:])
	if mode == ModeManaged {
		// Managed chat always ends on the shared gateway: the server may hold no
		// direct provider keys.
		for _, c := range t.table.Candidates() {
			if c.Provider == HermesGateway && c != primary && !slices.Contains(fallbacks, c) {
				fallbacks = append(fallbacks, c)
			}
		}
	}
	return Decision{Primary: primary, Fallbacks: fallbacks, Cerberus: m, CredentialMode: mode}, nil
}

func (t *cerberusTurn) attachProfile(m *CerberusMetadata, profile *RouterProfile, rule *RouterRule) {
	m.ProfileID = profile.ID
	m.ProfileName = profile.Name
	if rule != nil {
		id := rule.ID
		m.RuleID = &id
	}
}

// byokKeysRequired fails closed. Primary is unused: the transport checks the
// metadata before dispatch.
func (t *cerberusTurn) byokKeysRequired(profile *RouterProfile, rule *RouterRule, policy RoutingPolicy, reason string) Decision {
	m := t.metadata()
	t.attachProfile(m, profile, rule)
	m.Mode = ModeBYOK
	m.RoutingPolicy = string(policy)
	m.Reason = reason
	m.BYOKKeysRequired = true
	return Decision{Primary: t.table.Primary, Cerberus: m, CredentialMode: ModeBYOK}
}

// freeLaneDecision builds the forced lane. Mode is managed because the lane is
// platform-funded, so adapters use the env keys and clients never see the slug.
// Costs are zero with no reservation. Routes go direct, never onto the gateway, and
// the fallbacks are the other free legs only: the terminal gateway cascade is the
// unbounded-cost path this lane exists to close.
func (r *CerberusRouter) freeLaneDecision(ctx context.Context, t *cerberusTurn, profile *RouterProfile, rule *RouterRule, verdict FreeLaneVerdict, actions []string) Decision {
	var legs []FreeLaneLeg
	for _, leg := range freeLaneLegs {
		provider := Nvidia
		if leg == LegOpenRouterFree {
			provider = OpenRouter
		}
		if r.registryEnabled(provider) {
			legs = append(legs, leg)
		}
	}
	decide := func(primary Route, fallbacks []Route, routes []WireRoute, reason string, set func(*CerberusMetadata)) Decision {
		m := t.metadata()
		t.attachProfile(m, profile, rule)
		m.Routes = routes
		m.Mode = ModeManaged
		m.RoutingPolicy = string(PolicyLocked)
		m.Reason = reason
		m.IsFreeLane = true
		m.FreeLaneActions = actions
		if set != nil {
			set(m)
		}
		return Decision{Primary: primary, Fallbacks: fallbacks, Cerberus: m, CredentialMode: ModeManaged}
	}
	unavailable := func(reason string) Decision {
		return decide(t.table.Primary, nil, nil, reason, func(m *CerberusMetadata) { m.FreeLaneUnavailable = true })
	}
	// No leg can serve anyone. That is a configuration fact, known before any
	// claim, so nothing is charged and it is not reported as exhaustion.
	if len(legs) == 0 {
		slog.WarnContext(ctx, "cerberus.route.free_lane", "trigger", verdict.Trigger, "leg", "none", "outcome", "unavailable", "tenant", t.tenant)
		return unavailable("Free lane unavailable")
	}
	outcome := r.FreeLane.Gate.Claim(ctx, t.tenant, legs)
	if outcome.Granted == "" {
		slog.InfoContext(ctx, "cerberus.route.free_lane", "trigger", verdict.Trigger, "leg", "none", "outcome", "exhausted", "tenant", t.tenant)
		return decide(t.table.Primary, nil, nil, "Free lane exhausted", func(m *CerberusMetadata) {
			m.FreeLaneExhausted = true
			m.FreeLaneRetryAfterSeconds = int(outcome.RetryAfterSeconds)
		})
	}
	if verdict.Trigger == TriggerPlatformUnavailable {
		// Loud on purpose: this degrades paying users. Alert on it.
		slog.WarnContext(ctx, "cerberus.route.free_lane", "trigger", verdict.Trigger, "leg", outcome.Granted, "tenant", t.tenant)
	} else {
		slog.InfoContext(ctx, "cerberus.route.free_lane", "trigger", verdict.Trigger, "leg", outcome.Granted, "tenant", t.tenant)
	}
	var routes []WireRoute
	var mapped []Route
	for _, lane := range r.FreeLane.Routes(outcome.Granted) {
		route := WireRoute{Provider: lane.Provider, Model: lane.Model}
		routes = append(routes, route)
		if m, ok := ToModelRoute(route, false); ok {
			mapped = append(mapped, m)
		}
	}
	if len(mapped) == 0 {
		return unavailable("Free lane has no routable leg")
	}
	return decide(mapped[0], mapped[1:], routes, "Free lane "+string(verdict.Trigger), nil)
}

// matchRule is the first enabled matching rule by ascending priority. Stable, so
// equal priorities keep document order.
func matchRule(rules []RouterRule, task RouterTaskType, surface RouterSurface) *RouterRule {
	var best *RouterRule
	for i := range rules {
		rule := &rules[i]
		if !rule.Enabled ||
			(len(rule.TaskTypes) > 0 && !slices.Contains(rule.TaskTypes, task)) ||
			(len(rule.Surfaces) > 0 && !slices.Contains(rule.Surfaces, surface)) {
			continue
		}
		if best == nil || rule.Priority < best.Priority {
			best = rule
		}
	}
	return best
}

// requestedParallel is parallelRequest?.strategy ?? parallelStrategy.
func requestedParallel(values Values) (ParallelStrategy, *ParallelRequest) {
	if values.ParallelRequest != nil {
		return values.ParallelRequest.Strategy, values.ParallelRequest
	}
	return values.ParallelStrategy, nil
}

// applyParallel turns a requested strategy into an ensemble for an Ultimate tenant
// with the gate on, and takes ensembles away from everyone else.
func (r *CerberusRouter) applyParallel(action RouterAction, doc RouterProfileDocument, table Decision, requested ParallelStrategy, parallel *ParallelRequest, tier string) RouterAction {
	if requested != "" && r.EnsemblesEnabled && tier == "ultimate" {
		routes := slices.Clone(action.Routes)
		if parallel != nil && parallel.Participants != nil {
			routes = make([]WireRoute, 0, len(parallel.Participants))
			for _, p := range parallel.Participants {
				routes = append(routes, p.Route)
			}
		}
		for _, c := range table.Candidates() {
			id, ok := c.Provider.ToShared()
			if !ok {
				continue
			}
			route := WireRoute{Provider: id, Model: c.ModelID}
			if !slices.ContainsFunc(routes, func(existing WireRoute) bool { return existing.ID() == route.ID() }) {
				routes = append(routes, route)
			}
		}
		routes = routes[:min(3, len(routes))]
		if len(routes) >= 2 {
			synthesis := action.SynthesisRoute
			if parallel != nil && parallel.SynthesisRoute != nil {
				synthesis = parallel.SynthesisRoute
			}
			if synthesis == nil {
				synthesis = &routes[0]
			}
			minimum := 2
			if requested == ParallelBestOfN {
				minimum = 1
			}
			action = RouterAction{Kind: ActionEnsemble, Routes: routes, SynthesisRoute: synthesis, MinimumSuccessfulResults: &minimum, RetryPolicy: action.RetryPolicy}
		}
	}
	if action.Kind == ActionEnsemble && (!r.EnsemblesEnabled || tier != "ultimate") {
		if doc.DefaultAction.Kind == ActionSequential {
			return doc.DefaultAction
		}
		return RouterAction{Kind: ActionSequential, Routes: action.Routes, RetryPolicy: action.RetryPolicy}
	}
	return action
}

func filterRoutes(routes []WireRoute, blocked, allowed []ProviderID) []WireRoute {
	out := make([]WireRoute, 0, len(routes))
	for _, route := range routes {
		if slices.Contains(blocked, route.Provider) {
			continue
		}
		if len(allowed) > 0 && !slices.Contains(allowed, route.Provider) {
			continue
		}
		out = append(out, route)
	}
	return out
}

func (r *CerberusRouter) registryEnabled(kind ProviderKind) bool {
	return r.Registry != nil && r.Registry.IsEnabled(kind)
}

// credentialedProviders is credentialedProviderIDs: the providers this tenant can
// spend. An empty set fails a BYOK tenant closed, so an unreadable key is logged
// loudly rather than treated as absent in silence. Each lookup advances a key
// pool's rotation cursor, as in Swift.
func (r *CerberusRouter) credentialedProviders(ctx context.Context, tenant uuid.UUID) map[ProviderID]bool {
	result := map[ProviderID]bool{}
	if r.Credentials == nil {
		slog.ErrorContext(ctx, "credential store unavailable (no SecretBox?); every BYOK tenant will fail closed")
		return result
	}
	var failures []string
	for kind := range userCredentialTargets {
		id, ok := kind.ToShared()
		if !ok {
			continue
		}
		cred, err := r.Credentials.Credential(ctx, kind, tenant)
		if err != nil {
			failures = append(failures, string(kind)+": "+err.Error())
			continue
		}
		if cred != nil && kind.IsSpendable(cred.APIKey, cred.BaseURL) {
			result[id] = true
		}
	}
	if len(failures) > 0 {
		slices.Sort(failures)
		slog.ErrorContext(ctx, "credential reads failed; keys may exist but be unreadable", "tenant", swiftUUID(tenant), "failures", strings.Join(failures, "; "))
	}
	return result
}

func (r *CerberusRouter) deploymentEnabledProviders() map[ProviderID]bool {
	result := map[ProviderID]bool{}
	if r.Registry == nil {
		return result
	}
	for _, kind := range allProviderKinds {
		if id, ok := kind.ToShared(); ok && r.Registry.IsEnabled(kind) {
			result[id] = true
		}
	}
	return result
}

// IsSpendable is ProviderKind.isSpendable: a base URL alone proves nothing, except
// for ollama and custom, which address a server the user runs and commonly has no
// auth at all.
func (k ProviderKind) IsSpendable(apiKey *string, baseURL *url.URL) bool {
	if apiKey != nil && *apiKey != "" {
		return true
	}
	if k != Ollama && k != Custom {
		return false
	}
	return baseURL != nil && baseURL.Scheme != "" && baseURL.Host != ""
}

// scoreRoutes orders routes best first, stably. A soft budget limit makes cost
// dominate.
func scoreRoutes(routes []WireRoute, task RouterTaskType, weights RouterObjective, promptTokens int, costFirst bool) []WireRoute {
	if costFirst {
		weights = RouterObjective{Quality: 10, Cost: 80, Latency: 10}
	}
	type scored struct {
		route WireRoute
		score float64
	}
	ranked := make([]scored, 0, len(routes))
	for _, route := range routes {
		ranked = append(ranked, scored{route, routeScore(route, task, weights, promptTokens)})
	}
	slices.SortStableFunc(ranked, func(a, b scored) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return 0
	})
	out := make([]WireRoute, 0, len(ranked))
	for _, s := range ranked {
		out = append(out, s.route)
	}
	return out
}

func routeScore(route WireRoute, task RouterTaskType, weights RouterObjective, promptTokens int) float64 {
	quality, latency := 50.0, 2000.0
	if e, ok := CatalogEntry(route.Provider, route.Model); ok {
		if q, ok := e.TaskQuality[task]; ok {
			quality = float64(q)
		}
		latency = float64(e.DefaultLatencyMs)
	}
	latencyScore := 1 / (1 + latency/1000)
	cost := float64(predictedCost([]WireRoute{route}, nil, promptTokens, 1))
	costScore := 1 / (1 + cost/1_000_000)
	return quality/100*float64(weights.Quality) + costScore*float64(weights.Cost) + latencyScore*float64(weights.Latency)
}

func routeRates(route WireRoute) (input, output int64) {
	if e, ok := CatalogEntry(route.Provider, route.Model); ok {
		input, output = e.InputPerMillionUsdMicros, e.OutputPerMillionUsdMicros
	}
	if route.InputPerMillionUsdMicros != nil {
		input = *route.InputPerMillionUsdMicros
	}
	if route.OutputPerMillionUsdMicros != nil {
		output = *route.OutputPerMillionUsdMicros
	}
	return input, output
}

// predictedCost assumes 1024 output tokens per worker. Integer division, as Swift.
func predictedCost(routes []WireRoute, synthesis *WireRoute, promptTokens, workerRounds int) int64 {
	const outputTokens = 1024
	var workers int64
	for _, route := range routes {
		input, output := routeRates(route)
		workers += int64(promptTokens)*input/1_000_000 + outputTokens*output/1_000_000
	}
	var synthesisCost int64
	if synthesis != nil {
		input, output := routeRates(*synthesis)
		synthesisCost = int64(promptTokens*max(2, len(routes)))*input/1_000_000 + outputTokens*output/1_000_000
	}
	return workers*int64(max(1, workerRounds)) + synthesisCost
}

// ToModelRoute is CerberusModelRouter.toModelRoute.
func ToModelRoute(route WireRoute, viaGateway bool) (Route, bool) {
	if viaGateway && route.Provider == ManagedProvider {
		return Route{HermesGateway, route.Model}, true
	}
	kind, ok := FromShared(route.Provider)
	if !ok {
		return Route{}, false
	}
	return Route{kind, route.Model}, true
}
