package meproviders

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/entitlement"
	"lumina/internal/llm"
	"lumina/internal/modules/meproviders/db"
	"lumina/internal/wire"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// managedProvider is ManagedLLMDefaults.provider.
const managedProvider llm.ProviderID = "openRouter"

// managedWire is LLMPreferencesController.managedWire: the generic brain label,
// never the concrete managed model.
func managedWire() preferencesResponse {
	return preferencesResponse{
		Mode: "managed", PrimaryProvider: managedProvider, PrimaryModel: llm.GenericBrainName,
		FallbackChain: []routeDTO{}, AllowedProviders: []llm.ProviderID{}, BlockedProviders: []llm.ProviderID{},
		RoutingPolicy: "autoSmart",
	}
}

// toWire is LLMPreferencesController.toWire. False when the stored primary
// provider has no wire id.
func toWire(p *llm.Preference) (preferencesResponse, bool) {
	if p.Mode != llm.ModeBYOK {
		return managedWire(), true
	}
	primary, ok := p.PrimaryProvider.ToShared()
	if !ok {
		return preferencesResponse{}, false
	}
	out := preferencesResponse{
		Mode: "byok", PrimaryProvider: primary, PrimaryModel: p.PrimaryModel,
		FallbackChain: []routeDTO{}, AllowedProviders: []llm.ProviderID{}, BlockedProviders: []llm.ProviderID{},
		RoutingPolicy: "autoSmart",
	}
	for _, s := range p.FallbackChain {
		if id, ok := s.Provider.ToShared(); ok {
			out.FallbackChain = append(out.FallbackChain, routeDTO{id, s.Model})
		}
	}
	for _, k := range p.Allowed {
		if id, ok := k.ToShared(); ok {
			out.AllowedProviders = append(out.AllowedProviders, id)
		}
	}
	for _, k := range p.Blocked {
		if id, ok := k.ToShared(); ok {
			out.BlockedProviders = append(out.BlockedProviders, id)
		}
	}
	return out, true
}

func toWireOrManaged(p *llm.Preference) preferencesResponse {
	if w, ok := toWire(p); ok {
		return w
	}
	return managedWire()
}

// effectiveTier is the caller's tier after the override, from the users row.
// A missing row is the identity being unavailable.
func (m *Module) effectiveTier(ctx context.Context, user uuid.UUID) (string, error) {
	row, err := m.q.UserTier(ctx, user)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", wire.ErrUnauthenticated
	}
	if err != nil {
		return "", err
	}
	return entitlement.EffectiveTier(row.Tier, row.TierOverride), nil
}

// hasUsableCredential is App+build's byoHasUsableCredential: any user-credential
// target with a spendable key or base URL.
func (m *Module) hasUsableCredential(ctx context.Context, tenant uuid.UUID) bool {
	store := m.opts.Stack.Credentials
	if store == nil {
		return false
	}
	for _, id := range providerIDs {
		kind, _ := llm.FromShared(id)
		resolved, err := store.Credential(ctx, kind, tenant)
		if err != nil || resolved == nil {
			continue
		}
		if kind.IsSpendable(resolved.APIKey, resolved.BaseURL) {
			return true
		}
	}
	return false
}

// effectiveWire is LLMPreferencesController.effectiveWire: what the router will
// actually do, so a forced-free-lane user is not told BYOK took effect.
func (m *Module) effectiveWire(ctx context.Context, p *llm.Preference, user uuid.UUID, tier string) preferencesResponse {
	if p == nil {
		return managedWire()
	}
	stack := m.opts.Stack
	if stack.FreeLane == nil {
		return toWireOrManaged(p)
	}
	honoured := llm.HonoursUserChoice(llm.FreeLaneInput{
		EffectiveTier:                tier,
		RequestedMode:                p.Mode,
		HasUsableUserCredential:      m.hasUsableCredential(ctx, user),
		PlatformPaidManagedAvailable: stack.Registry != nil && stack.Registry.IsEnabled(llm.OpenRouter),
		FreeLaneEnabled:              true,
	})
	if honoured {
		return toWireOrManaged(p)
	}
	return managedWire()
}

func (m *Module) getPreferences(c *echo.Context) error {
	user := auth.UserID(c)
	ctx := c.Request().Context()
	tier, err := m.effectiveTier(ctx, user)
	if err != nil {
		return err
	}
	p, err := m.opts.Stack.Preferences.Get(ctx, user)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, m.effectiveWire(ctx, p, user, tier))
}

// isMaskedModel is LLMPreferencesController.isMaskedModel: the brain label, or
// the hidden route id on the managed provider.
func isMaskedModel(model string, provider llm.ProviderID) bool {
	return model == llm.GenericBrainName || (provider == managedProvider && model == llm.GenericModelID)
}

// realPrimaryModel is LLMPreferencesController.realPrimaryModel: a masked label
// keeps the stored BYOK model on the same provider, or the save is refused.
func (m *Module) realPrimaryModel(ctx context.Context, body *preferencesPutRequest, tenant uuid.UUID) (string, error) {
	provider := llm.ProviderID(body.PrimaryProvider)
	if !isMaskedModel(body.PrimaryModel, provider) {
		return body.PrimaryModel, nil
	}
	if stored, err := m.opts.Stack.Preferences.Get(ctx, tenant); err == nil && stored != nil && stored.Mode == llm.ModeBYOK {
		if id, ok := stored.PrimaryProvider.ToShared(); ok && id == provider &&
			stored.PrimaryModel != "" && !isMaskedModel(stored.PrimaryModel, provider) {
			return stored.PrimaryModel, nil
		}
	}
	return "", wire.NewError(http.StatusBadRequest, "primary_model_placeholder")
}

type storedStep struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type storedChain struct {
	Steps []storedStep `json:"steps"`
}

type storedList struct {
	Providers []string `json:"providers"`
}

func (m *Module) putPreferences(c *echo.Context) error {
	tenant := auth.UserID(c)
	ctx := c.Request().Context()
	body, err := wire.Bind[preferencesPutRequest](c)
	if err != nil {
		return err
	}
	mode := llm.ModeManaged
	if body.Mode != nil && *body.Mode == "byok" {
		mode = llm.ModeBYOK
	}

	// Managed policy belongs to the backend: a stale client's provider and model
	// never pin the platform. BYOK stays fully user-configurable.
	pref := &llm.Preference{Mode: mode, PrimaryProvider: llm.OpenRouter, PrimaryModel: m.opts.Stack.Profiles.ManagedModel()}
	var chain []modelRoute
	var allowed, blocked []providerID
	if mode == llm.ModeBYOK {
		if body.PrimaryModel == "" {
			return wire.NewError(http.StatusBadRequest, "primary_model_required")
		}
		for _, s := range body.FallbackChain {
			if s.Model == "" {
				return wire.NewError(http.StatusBadRequest, "fallback_model_required")
			}
		}
		for _, s := range body.FallbackChain {
			if isMaskedModel(s.Model, llm.ProviderID(s.Provider)) {
				return wire.NewError(http.StatusBadRequest, "fallback_model_placeholder")
			}
		}
		model, err := m.realPrimaryModel(ctx, body, tenant)
		if err != nil {
			return err
		}
		pref.PrimaryProvider, pref.PrimaryModel = llm.ProviderKind(body.PrimaryProvider), model
		chain, allowed, blocked = body.FallbackChain, body.AllowedProviders, body.BlockedProviders
	}

	stored := storedChain{Steps: []storedStep{}}
	for _, s := range chain {
		stored.Steps = append(stored.Steps, storedStep{string(s.Provider), s.Model})
		pref.FallbackChain = append(pref.FallbackChain, llm.PreferenceStep{Provider: llm.ProviderKind(s.Provider), Model: s.Model})
	}
	allowedList, blockedList := storedList{Providers: []string{}}, storedList{Providers: []string{}}
	for _, p := range allowed {
		allowedList.Providers = append(allowedList.Providers, string(p))
		pref.Allowed = append(pref.Allowed, llm.ProviderKind(p))
	}
	for _, p := range blocked {
		blockedList.Providers = append(blockedList.Providers, string(p))
		pref.Blocked = append(pref.Blocked, llm.ProviderKind(p))
	}

	if err := m.savePreference(ctx, tenant, pref, stored, allowedList, blockedList); err != nil {
		m.log.ErrorContext(ctx, "llm preference upsert failed", "error", err)
		return wire.NewError(http.StatusInternalServerError, "preference_save_failed")
	}
	if _, ok := toWire(pref); !ok {
		return wire.NewError(http.StatusInternalServerError, "preference_unmappable")
	}
	// The write is stored verbatim; the response reports what the router will do.
	tier, err := m.effectiveTier(ctx, tenant)
	if err != nil {
		return wire.JSON(c, http.StatusOK, toWireOrManaged(pref))
	}
	return wire.JSON(c, http.StatusOK, m.effectiveWire(ctx, pref, tenant, tier))
}

// savePreference is UserLLMPreferenceRepository.upsert followed by
// RouterProfileRepository.synchronizeDefault.
func (m *Module) savePreference(ctx context.Context, tenant uuid.UUID, p *llm.Preference, chain storedChain, allowed, blocked storedList) error {
	chainJSON, _ := json.Marshal(chain)
	allowedJSON, _ := json.Marshal(allowed)
	blockedJSON, _ := json.Marshal(blocked)
	now := m.now()
	existing, err := m.q.GetPreferenceID(ctx, tenant)
	switch {
	case err == nil:
		err = m.q.UpdatePreference(ctx, db.UpdatePreferenceParams{
			ID: existing, Mode: string(p.Mode), PrimaryProvider: string(p.PrimaryProvider), PrimaryModel: p.PrimaryModel,
			FallbackChain: chainJSON, AllowedProviders: allowedJSON, BlockedProviders: blockedJSON, UpdatedAt: &now,
		})
	case errors.Is(err, pgx.ErrNoRows):
		err = m.q.InsertPreference(ctx, db.InsertPreferenceParams{
			ID: uuid.New(), TenantID: tenant, Mode: string(p.Mode), PrimaryProvider: string(p.PrimaryProvider), PrimaryModel: p.PrimaryModel,
			FallbackChain: chainJSON, AllowedProviders: allowedJSON, BlockedProviders: blockedJSON, CreatedAt: &now,
		})
	}
	if err != nil {
		return err
	}
	m.opts.Stack.Preferences.Invalidate(tenant)

	primary, _ := p.PrimaryProvider.ToShared()
	fallbacks := make([]llm.WireRoute, 0, len(chain.Steps))
	for _, s := range chain.Steps {
		fallbacks = append(fallbacks, llm.WireRoute{Provider: llm.ProviderID(s.Provider), Model: s.Model})
	}
	toIDs := func(l storedList) []llm.ProviderID {
		out := make([]llm.ProviderID, len(l.Providers))
		for i, s := range l.Providers {
			out[i] = llm.ProviderID(s)
		}
		return out
	}
	return m.opts.Stack.Profiles.SynchronizeDefault(ctx, tenant, p.Mode,
		llm.WireRoute{Provider: primary, Model: p.PrimaryModel}, fallbacks, toIDs(allowed), toIDs(blocked))
}
