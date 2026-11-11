package llmrouter

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/llmrouter/db"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// autoRequiresOpenRouter is LLMRoutingErrorCode.autoRequiresOpenRouter.
const autoRequiresOpenRouter = "AUTO_REQUIRES_OPENROUTER"

// managedProvider is ManagedLLMDefaults.provider.
const managedProvider llm.ProviderID = "openRouter"

// MARK: - Disclosure

// placeholder is the route a hidden profile shows: (openRouter, "auto").
func placeholder() llm.WireRoute {
	return llm.WireRoute{Provider: managedProvider, Model: llm.GenericModelID}
}

func isPlaceholder(r llm.WireRoute) bool {
	return r.Provider == managedProvider && r.Model == llm.GenericModelID
}

// scrubAction is ModelDisclosurePolicy.scrub(_: RouterActionDTO): one placeholder
// route, a placeholder synthesis route when there was one, and no participants.
func scrubAction(a llm.RouterAction) llm.RouterAction {
	out := llm.RouterAction{
		Kind: a.Kind, Routes: []llm.WireRoute{placeholder()},
		MinimumSuccessfulResults: a.MinimumSuccessfulResults, RetryPolicy: a.RetryPolicy, ParallelStrategy: a.ParallelStrategy,
	}
	if a.SynthesisRoute != nil {
		p := placeholder()
		out.SynthesisRoute = &p
	}
	return out
}

// profileOut is RouterProfileRepository.toDTO scrubbed by
// ModelDisclosure.forBrainMode(profile.mode): only BYOK sees its routes.
func profileOut(p *llm.RouterProfile) profileDTO {
	d := p.Document
	dto := profileDTO{
		ID: wire.UUID{UUID: p.ID}, Name: p.Name, Mode: string(p.BrainMode()), IsPreset: p.IsPreset,
		Objective: d.Objective, Budget: d.Budget, AllowedProviders: d.AllowedProviders, BlockedProviders: d.BlockedProviders,
		DefaultAction: d.DefaultAction, Rules: d.Rules, RoutingPolicy: d.RoutingPolicy, Revision: p.Revision,
		CreatedAt: wtime(p.CreatedAt), UpdatedAt: wtime(p.UpdatedAt),
	}
	if dto.AllowedProviders == nil {
		dto.AllowedProviders = []llm.ProviderID{}
	}
	if dto.BlockedProviders == nil {
		dto.BlockedProviders = []llm.ProviderID{}
	}
	if dto.Rules == nil {
		dto.Rules = []llm.RouterRule{}
	}
	if p.BrainMode() == llm.ModeBYOK {
		return dto
	}
	dto.AllowedProviders = []llm.ProviderID{managedProvider}
	dto.BlockedProviders = []llm.ProviderID{}
	dto.DefaultAction = scrubAction(d.DefaultAction)
	rules := make([]llm.RouterRule, len(dto.Rules))
	for i, r := range dto.Rules {
		r.Action = scrubAction(r.Action)
		rules[i] = r
	}
	dto.Rules = rules
	return dto
}

// MARK: - Placeholder routes

var errPlaceholderRoute = wire.NewError(http.StatusBadRequest, "router_placeholder_route")

// savedByokChain is RouterProfileRepository.savedByokChain: the saved BYOK primary
// and fallbacks, minus any that are the placeholder or the brain label.
func savedByokChain(p *llm.Preference) []llm.WireRoute {
	if p == nil || p.Mode != llm.ModeBYOK {
		return nil
	}
	primary, ok := p.PrimaryProvider.ToShared()
	if !ok {
		return nil
	}
	all := []llm.WireRoute{{Provider: primary, Model: p.PrimaryModel}}
	for _, s := range p.FallbackChain {
		if id, ok := s.Provider.ToShared(); ok {
			all = append(all, llm.WireRoute{Provider: id, Model: s.Model})
		}
	}
	var out []llm.WireRoute
	for _, r := range all {
		if r.Model != "" && r.Model != llm.GenericBrainName && !isPlaceholder(r) {
			out = append(out, r)
		}
	}
	return out
}

func carriesPlaceholder(a llm.RouterAction) bool {
	for _, r := range a.Routes {
		if isPlaceholder(r) {
			return true
		}
	}
	return a.SynthesisRoute != nil && isPlaceholder(*a.SynthesisRoute)
}

// substitutePlaceholders is RouterPlaceholderRoutes.substitute: in a BYOK save,
// the managed placeholder becomes the saved chain (deduplicated by
// provider:model, first wins), and a placeholder synthesis route its primary.
// With no chain to splice in, the save is refused.
func substitutePlaceholders(req llm.RouterProfileWrite, chain []llm.WireRoute) (llm.RouterProfileWrite, error) {
	if req.Mode != llm.ModeBYOK {
		return req, nil
	}
	carries := carriesPlaceholder(req.DefaultAction)
	for _, r := range req.Rules {
		carries = carries || carriesPlaceholder(r.Action)
	}
	if !carries {
		return req, nil
	}
	if len(chain) == 0 {
		return req, errPlaceholderRoute
	}
	resolve := func(a llm.RouterAction) llm.RouterAction {
		if !carriesPlaceholder(a) {
			return a
		}
		seen := map[string]bool{}
		routes := []llm.WireRoute{}
		for _, r := range a.Routes {
			expanded := []llm.WireRoute{r}
			if isPlaceholder(r) {
				expanded = chain
			}
			for _, e := range expanded {
				key := string(e.Provider) + ":" + e.Model
				if !seen[key] {
					seen[key] = true
					routes = append(routes, e)
				}
			}
		}
		a.Routes = routes
		if a.SynthesisRoute != nil && isPlaceholder(*a.SynthesisRoute) {
			primary := chain[0]
			a.SynthesisRoute = &primary
		}
		return a
	}
	req.DefaultAction = resolve(req.DefaultAction)
	rules := make([]llm.RouterRule, len(req.Rules))
	for i, r := range req.Rules {
		r.Action = resolve(r.Action)
		rules[i] = r
	}
	req.Rules = rules
	return req, nil
}

func (m *Module) resolvingPlaceholders(ctx context.Context, tenant uuid.UUID, req llm.RouterProfileWrite) (llm.RouterProfileWrite, error) {
	pref, err := m.preference(ctx, tenant)
	if err != nil {
		return req, err
	}
	return substitutePlaceholders(req, savedByokChain(pref))
}

// MARK: - Validation

// validate is RouterController.validate.
func validate(req llm.RouterProfileWrite, id *identity, ensemblesEnabled bool) error {
	if swiftstr.TrimWhitespacesAndNewlines(req.Name) == "" {
		return wire.NewError(http.StatusBadRequest, "router_profile_name_required")
	}
	o := req.Objective
	if o.Quality < 0 || o.Cost < 0 || o.Latency < 0 || o.Quality+o.Cost+o.Latency != 100 {
		return wire.NewError(http.StatusBadRequest, "router_objective_weights_must_total_100")
	}
	if s, h := req.Budget.SoftLimitUsdMicros, req.Budget.HardLimitUsdMicros; s != nil && h != nil && *s > *h {
		return wire.NewError(http.StatusBadRequest, "router_soft_budget_exceeds_hard_budget")
	}
	actions := []llm.RouterAction{req.DefaultAction}
	for _, r := range req.Rules {
		actions = append(actions, r.Action)
	}
	for _, a := range actions {
		if len(a.Routes) == 0 {
			return wire.NewError(http.StatusBadRequest, "router_action_requires_route")
		}
		if a.Kind == "ensemble" {
			if !ensemblesEnabled || !id.ultimate() {
				return wire.NewError(http.StatusForbidden, "router_ensemble_requires_ultimate")
			}
			if len(a.Routes) < 2 || len(a.Routes) > 4 || a.SynthesisRoute == nil {
				return wire.NewError(http.StatusBadRequest, "router_ensemble_invalid")
			}
		}
		for _, r := range a.Routes {
			if swiftstr.TrimWhitespacesAndNewlines(r.Model) == "" {
				return wire.NewError(http.StatusBadRequest, "router_model_required")
			}
		}
	}
	return nil
}

// validateAutoPolicy is RouterController.validateAutoPolicy: a BYOK Auto profile
// needs an OpenRouter key or base URL. Skipped without a credential store.
func (m *Module) validateAutoPolicy(ctx context.Context, req llm.RouterProfileWrite, tenant uuid.UUID) error {
	if req.RoutingPolicy != llm.PolicyAutoSmart || req.Mode != llm.ModeBYOK || m.opts.Stack == nil || m.opts.Stack.Credentials == nil {
		return nil
	}
	cred, err := m.opts.Stack.Credentials.Credential(ctx, llm.OpenRouter, tenant)
	if err != nil || cred == nil || (cred.APIKey == nil && cred.BaseURL == nil) {
		return wire.NewError(http.StatusBadRequest, autoRequiresOpenRouter)
	}
	return nil
}

func (m *Module) ensemblesEnabled() bool { return m.opts.Stack != nil && m.opts.Stack.ParallelEnabled }

var errCustomRequiresPro = wire.NewError(http.StatusForbidden, "router_custom_profile_requires_pro")

// prepare decodes, substitutes and validates a profile write, in Swift's order.
func (m *Module) prepare(c *echo.Context, id *identity) (llm.RouterProfileWrite, error) {
	body, err := wire.Bind[writeReq](c)
	if err != nil {
		return llm.RouterProfileWrite{}, err
	}
	ctx := c.Request().Context()
	// Substitution runs before validation: splicing in a saved chain can change
	// the route count an ensemble is validated against.
	req, err := m.resolvingPlaceholders(ctx, id.id, body.write())
	if err != nil {
		return req, err
	}
	if err := validate(req, id, m.ensemblesEnabled()); err != nil {
		return req, err
	}
	return req, m.validateAutoPolicy(ctx, req, id.id)
}

// MARK: - Profiles

func (m *Module) list(c *echo.Context) error {
	profiles, def, err := m.opts.Stack.Profiles.List(c.Request().Context(), auth.UserID(c))
	if err != nil {
		return err
	}
	out := profilesResponse{Profiles: make([]profileDTO, len(profiles)), DefaultProfileID: wire.UUID{UUID: def}}
	for i, p := range profiles {
		out.Profiles[i] = profileOut(p)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) create(c *echo.Context) error {
	id, err := m.requireIdentity(c)
	if err != nil {
		return err
	}
	if !id.proOrUltimate() {
		return errCustomRequiresPro
	}
	req, err := m.prepare(c, id)
	if err != nil {
		return err
	}
	p, err := m.opts.Stack.Profiles.Create(c.Request().Context(), id.id, req)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, profileOut(p))
}

// update has no tier check, as in Swift.
func (m *Module) update(c *echo.Context) error {
	id, err := m.requireIdentity(c)
	if err != nil {
		return err
	}
	profileID, err := pathID(c, "id")
	if err != nil {
		return err
	}
	req, err := m.prepare(c, id)
	if err != nil {
		return err
	}
	p, err := m.opts.Stack.Profiles.Update(c.Request().Context(), id.id, profileID, req)
	switch {
	case errors.Is(err, llm.ErrRouterRevisionConflict):
		return wire.NewError(http.StatusConflict, "router_profile_revision_conflict")
	case err != nil:
		return err
	case p == nil:
		return wire.NewError(http.StatusNotFound, "router_profile_not_found")
	}
	return wire.JSON(c, http.StatusOK, profileOut(p))
}

func (m *Module) delete(c *echo.Context) error {
	id, err := m.requireIdentity(c)
	if err != nil {
		return err
	}
	if !id.proOrUltimate() {
		return errCustomRequiresPro
	}
	profileID, err := pathID(c, "id")
	if err != nil {
		return err
	}
	ok, err := m.opts.Stack.Profiles.Delete(c.Request().Context(), id.id, profileID)
	switch {
	case errors.Is(err, llm.ErrCannotDeleteDefault):
		return wire.NewError(http.StatusConflict, "router_default_profile_cannot_be_deleted")
	case err != nil:
		return err
	case !ok:
		return wire.NewError(http.StatusNotFound, "router_profile_not_found")
	}
	return c.NoContent(http.StatusNoContent)
}

// MARK: - Catalog

func (m *Module) catalog(c *echo.Context) error {
	id, err := m.requireIdentity(c)
	if err != nil {
		return err
	}
	entries := llm.RouterCatalogEntries()
	models := make([]catalogEntryDTO, len(entries))
	for i, e := range entries {
		quality := make(map[string]int, len(e.TaskQuality))
		for k, v := range e.TaskQuality {
			quality[string(k)] = v
		}
		caps := e.Capabilities
		if caps == nil {
			caps = []string{}
		}
		models[i] = catalogEntryDTO{
			Provider: e.Provider, Model: e.Model, DisplayName: e.DisplayName, TaskQuality: quality,
			InputPerMillionUsdMicros: e.InputPerMillionUsdMicros, OutputPerMillionUsdMicros: e.OutputPerMillionUsdMicros,
			DefaultLatencyMs: e.DefaultLatencyMs, Capabilities: caps, Tier: string(e.Tier),
		}
	}
	return wire.JSON(c, http.StatusOK, catalogResponse{
		Models: models, TaskTypes: taskTypes, Surfaces: surfaces,
		CustomProfilesAllowed: id.proOrUltimate(), EnsemblesAllowed: m.ensemblesEnabled() && id.ultimate(),
	})
}

// MARK: - Bindings

func bindingOut(b *llm.RouterBinding) bindingDTO {
	return bindingDTO{ID: wire.UUID{UUID: b.ID}, Scope: string(b.Scope), ScopeID: b.ScopeID, ProfileID: wire.UUID{UUID: b.ProfileID}}
}

func (m *Module) bindings(c *echo.Context) error {
	rows, err := m.opts.Stack.Profiles.Bindings(c.Request().Context(), auth.UserID(c))
	if err != nil {
		return err
	}
	out := bindingsResponse{Bindings: make([]bindingDTO, len(rows))}
	for i, b := range rows {
		out.Bindings[i] = bindingOut(b)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// bindingScope is RouterController.bindingScope.
func bindingScope(c *echo.Context) (llm.RouterBindingScope, error) {
	switch s := llm.RouterBindingScope(c.Param("scope")); s {
	case llm.BindingUser, llm.BindingSpace, llm.BindingJob, llm.BindingWorkflow:
		return s, nil
	}
	return "", wire.NewError(http.StatusBadRequest, "invalid_router_scope")
}

// scopeID is RouterController.scopeID.
func scopeID(c *echo.Context) (string, error) {
	if id := c.Param("scopeID"); id != "" {
		return id, nil
	}
	return "", wire.NewError(http.StatusBadRequest, "router_scope_id_required")
}

// validateScopeOwnership is RouterController.validateScopeOwnership.
func (m *Module) validateScopeOwnership(ctx context.Context, tenant uuid.UUID, scope llm.RouterBindingScope, id string) error {
	switch scope {
	case llm.BindingSpace:
		parsed, err := ids.Parse(id)
		if err == nil {
			ok, err := m.q.SpaceExists(ctx, db.SpaceExistsParams{TenantID: tenant, ID: parsed})
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
		return wire.NewError(http.StatusNotFound, "space_not_found")
	case llm.BindingJob:
		ok, err := m.q.JobExists(ctx, db.JobExistsParams{TenantID: tenant, Name: id})
		if err != nil {
			return err
		}
		if !ok {
			return wire.NewError(http.StatusNotFound, "job_not_found")
		}
	case llm.BindingWorkflow:
		parsed, err := ids.Parse(id)
		if err == nil {
			ok, err := m.q.WorkflowExists(ctx, db.WorkflowExistsParams{TenantID: tenant, ID: parsed})
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
		return wire.NewError(http.StatusNotFound, "workflow_not_found")
	case llm.BindingUser:
		if id != ids.Upper(tenant) {
			return wire.NewError(http.StatusForbidden, "router_user_scope_mismatch")
		}
	}
	return nil
}

func (m *Module) putBinding(c *echo.Context) error {
	id, err := m.requireIdentity(c)
	if err != nil {
		return err
	}
	scope, err := bindingScope(c)
	if err != nil {
		return err
	}
	if scope != llm.BindingUser && !id.proOrUltimate() {
		return wire.NewError(http.StatusForbidden, "router_scoped_profile_requires_pro")
	}
	requested, err := scopeID(c)
	if err != nil {
		return err
	}
	sid := requested
	if scope == llm.BindingUser {
		sid = ids.Upper(id.id)
	}
	ctx := c.Request().Context()
	if err := m.validateScopeOwnership(ctx, id.id, scope, sid); err != nil {
		return err
	}
	body, err := wire.Bind[bindingPutReq](c)
	if err != nil {
		return err
	}
	b, err := m.opts.Stack.Profiles.Bind(ctx, id.id, scope, sid, body.ProfileID.UUID)
	if errors.Is(err, llm.ErrRouterProfileNotFound) {
		return wire.NewError(http.StatusNotFound, "router_profile_not_found")
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, bindingOut(b))
}

// deleteBinding: unbinding the user scope throws cannotDeleteDefault, which
// RouterController does not catch, so it is a bare 500.
func (m *Module) deleteBinding(c *echo.Context) error {
	scope, err := bindingScope(c)
	if err != nil {
		return err
	}
	sid, err := scopeID(c)
	if err != nil {
		return err
	}
	if _, err := m.opts.Stack.Profiles.Unbind(c.Request().Context(), auth.UserID(c), scope, sid); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// MARK: - Dashboard

func (m *Module) dashboard(c *echo.Context) error {
	tenant := auth.UserID(c)
	ctx := c.Request().Context()
	row, err := m.q.Dashboard(ctx, tenant)
	if err != nil {
		return err
	}
	profiles, def, err := m.opts.Stack.Profiles.List(ctx, tenant)
	if err != nil {
		return err
	}
	now := m.now().UTC()
	out := dashboardResponse{
		PeriodStart: wire.Time{Time: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)}, PeriodEnd: wire.Time{Time: now},
		Requests: row.Requests, SuccessfulRequests: row.Successful, FallbackCount: row.FallbackCount,
		TokensIn: row.TokensIn, TokensOut: row.TokensOut, EstimatedCostUsdMicros: row.Cost, AverageLatencyMs: row.Latency,
	}
	for _, p := range profiles {
		if p.ID == def {
			out.MonthlySoftLimitUsdMicros = p.Document.Budget.SoftLimitUsdMicros
			out.MonthlyHardLimitUsdMicros = p.Document.Budget.HardLimitUsdMicros
			break
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}
