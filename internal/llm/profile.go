package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RouterObjective is RouterObjectiveWeightsDTO. Zero weights are legal.
type RouterObjective struct {
	Quality int `json:"quality"`
	Cost    int `json:"cost"`
	Latency int `json:"latency"`
}

// RouterBudget is RouterBudgetPolicyDTO. Nil limits mean none.
type RouterBudget struct {
	SoftLimitUsdMicros *int64 `json:"softLimitUsdMicros,omitempty"`
	HardLimitUsdMicros *int64 `json:"hardLimitUsdMicros,omitempty"`
}

// RouterAction is RouterActionDTO. Sequential actions use Routes as an ordered
// fallback chain. Nil optional fields stay out of the JSON, as Codable leaves them
// out; omitzero keeps an empty Participants distinct from a missing one.
type RouterAction struct {
	Kind                     RouterActionKind      `json:"kind"`
	Routes                   []WireRoute           `json:"routes"`
	SynthesisRoute           *WireRoute            `json:"synthesisRoute,omitempty"`
	MinimumSuccessfulResults *int                  `json:"minimumSuccessfulResults,omitempty"`
	RetryPolicy              RouterRetryPolicy     `json:"retryPolicy"`
	ParallelStrategy         *ParallelStrategy     `json:"parallelStrategy,omitempty"`
	Participants             []ParallelParticipant `json:"participants,omitzero"`
}

// RouterRule is RouterRuleDTO. Empty TaskTypes or Surfaces match everything.
type RouterRule struct {
	ID        uuid.UUID        `json:"id"`
	Name      string           `json:"name"`
	Enabled   bool             `json:"enabled"`
	Priority  int              `json:"priority"`
	TaskTypes []RouterTaskType `json:"taskTypes"`
	Surfaces  []RouterSurface  `json:"surfaces"`
	Action    RouterAction     `json:"action"`
}

// RouterProfileDocument is the router_profiles.document JSONB column.
type RouterProfileDocument struct {
	Objective        RouterObjective `json:"objective"`
	Budget           RouterBudget    `json:"budget"`
	AllowedProviders []ProviderID    `json:"allowedProviders"`
	BlockedProviders []ProviderID    `json:"blockedProviders"`
	DefaultAction    RouterAction    `json:"defaultAction"`
	Rules            []RouterRule    `json:"rules"`
	RoutingPolicy    RoutingPolicy   `json:"routingPolicy"`
}

// requireKeys is what synthesized Codable does for non-optional properties: a
// missing or null key fails the decode. A document Swift cannot read falls back
// to the table router, so Go has to refuse the same documents.
func requireKeys(data []byte, keys ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok || bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("missing required key %q", key)
		}
	}
	return nil
}

func (o *RouterObjective) UnmarshalJSON(data []byte) error {
	if err := requireKeys(data, "quality", "cost", "latency"); err != nil {
		return err
	}
	type plain RouterObjective
	return json.Unmarshal(data, (*plain)(o))
}

func (r *WireRoute) UnmarshalJSON(data []byte) error {
	if err := requireKeys(data, "provider", "model"); err != nil {
		return err
	}
	type plain WireRoute
	return json.Unmarshal(data, (*plain)(r))
}

func (p *ParallelParticipant) UnmarshalJSON(data []byte) error {
	if err := requireKeys(data, "id", "role", "route"); err != nil {
		return err
	}
	type plain ParallelParticipant
	return json.Unmarshal(data, (*plain)(p))
}

func (a *RouterAction) UnmarshalJSON(data []byte) error {
	if err := requireKeys(data, "kind", "routes", "retryPolicy"); err != nil {
		return err
	}
	type plain RouterAction
	return json.Unmarshal(data, (*plain)(a))
}

func (r *RouterRule) UnmarshalJSON(data []byte) error {
	if err := requireKeys(data, "id", "name", "enabled", "priority", "taskTypes", "surfaces", "action"); err != nil {
		return err
	}
	type plain RouterRule
	return json.Unmarshal(data, (*plain)(r))
}

// UnmarshalJSON is RouterProfileDocument.init(from:): objective, budget and
// defaultAction are required, the rest default.
func (d *RouterProfileDocument) UnmarshalJSON(data []byte) error {
	if err := requireKeys(data, "objective", "budget", "defaultAction"); err != nil {
		return err
	}
	type plain RouterProfileDocument
	if err := json.Unmarshal(data, (*plain)(d)); err != nil {
		return err
	}
	if d.AllowedProviders == nil {
		d.AllowedProviders = []ProviderID{}
	}
	if d.BlockedProviders == nil {
		d.BlockedProviders = []ProviderID{}
	}
	if d.Rules == nil {
		d.Rules = []RouterRule{}
	}
	if d.RoutingPolicy == "" {
		d.RoutingPolicy = PolicyAutoSmart
	}
	return nil
}

// RouterBindingScope is where a profile applies.
type RouterBindingScope string

const (
	BindingUser     RouterBindingScope = "user"
	BindingSpace    RouterBindingScope = "space"
	BindingJob      RouterBindingScope = "job"
	BindingWorkflow RouterBindingScope = "workflow"
)

// RouterProfile is a router_profiles row.
type RouterProfile struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Name      string
	Mode      string
	IsPreset  bool
	Document  RouterProfileDocument
	Revision  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BrainMode is the row's mode as RouterProfileRepository.toDTO reads it: anything
// but byok is managed.
func (p *RouterProfile) BrainMode() CredentialMode {
	if p.Mode == string(ModeBYOK) {
		return ModeBYOK
	}
	return ModeManaged
}

// RouterBinding is a router_bindings row.
type RouterBinding struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Scope     RouterBindingScope
	ScopeID   string
	ProfileID uuid.UUID
}

// RouterProfileWrite is RouterProfileWriteRequest. ExpectedRevision nil skips the
// conflict check.
type RouterProfileWrite struct {
	Name             string
	Mode             CredentialMode
	Objective        RouterObjective
	Budget           RouterBudget
	AllowedProviders []ProviderID
	BlockedProviders []ProviderID
	DefaultAction    RouterAction
	Rules            []RouterRule
	RoutingPolicy    RoutingPolicy
	ExpectedRevision *int
}

// RouterProfileRepositoryError values are RouterProfileRepositoryError.
var (
	ErrRouterRevisionConflict = errors.New("router profile revision conflict")
	ErrCannotDeleteDefault    = errors.New("cannot delete the default router profile")
	ErrRouterProfileNotFound  = errors.New("router profile not found")
)

// RouterProfileRepository is RouterProfileRepository. Reading a profile can write:
// a tenant's first routed call seeds its default, and any read normalizes a stale
// managed document. Both servers write these rows while both run, so a Go seed
// must read back exactly as a Swift seed does.
//
// Not ported: resolvingPlaceholders and RouterPlaceholderRoutes, which only the
// /v1/router save path uses and which need ModelDisclosurePolicy.
type RouterProfileRepository struct {
	pool         *pgxpool.Pool
	preferences  PreferenceSource
	managedModel string
	now          func() time.Time
}

// NewRouterProfileRepository wires the repository. managedModel is
// hermes.defaultManagedModel.
func NewRouterProfileRepository(pool *pgxpool.Pool, preferences PreferenceSource, managedModel string) *RouterProfileRepository {
	return &RouterProfileRepository{pool: pool, preferences: preferences, managedModel: managedModel, now: time.Now}
}

const profileColumns = `id, tenant_id, name, mode, is_preset, document, revision, created_at, updated_at`

func scanProfile(row pgx.Row) (*RouterProfile, error) {
	var p RouterProfile
	var document []byte
	if err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.Mode, &p.IsPreset, &document, &p.Revision, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(document, &p.Document); err != nil {
		return nil, fmt.Errorf("decode router profile %s: %w", p.ID, err)
	}
	return &p, nil
}

// List is list(tenantID:): every profile, oldest first, with the default's id.
func (r *RouterProfileRepository) List(ctx context.Context, tenant uuid.UUID) ([]*RouterProfile, uuid.UUID, error) {
	def, err := r.ensureDefault(ctx, tenant)
	if err != nil {
		return nil, uuid.Nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+profileColumns+` FROM router_profiles WHERE tenant_id = $1 ORDER BY created_at`, tenant)
	if err != nil {
		return nil, uuid.Nil, err
	}
	profiles, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*RouterProfile, error) { return scanProfile(row) })
	if err != nil {
		return nil, uuid.Nil, err
	}
	for _, p := range profiles {
		if err := r.reconcileManaged(ctx, p); err != nil {
			return nil, uuid.Nil, err
		}
	}
	return profiles, def.ID, nil
}

// Profile is profile(tenantID:id:). Nil with a nil error means no such row.
func (r *RouterProfileRepository) Profile(ctx context.Context, tenant, id uuid.UUID) (*RouterProfile, error) {
	p, err := scanProfile(r.pool.QueryRow(ctx, `SELECT `+profileColumns+` FROM router_profiles WHERE tenant_id = $1 AND id = $2 LIMIT 1`, tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := r.reconcileManaged(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *RouterProfileRepository) insertProfile(ctx context.Context, p *RouterProfile) error {
	document, err := encodeJSON(p.Document)
	if err != nil {
		return err
	}
	now := r.now()
	p.CreatedAt, p.UpdatedAt = now, now
	_, err = r.pool.Exec(ctx, `INSERT INTO router_profiles (id, tenant_id, name, mode, is_preset, document, revision, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, p.ID, p.TenantID, p.Name, p.Mode, p.IsPreset, document, p.Revision, now, now)
	return err
}

func (r *RouterProfileRepository) saveProfile(ctx context.Context, p *RouterProfile) error {
	document, err := encodeJSON(p.Document)
	if err != nil {
		return err
	}
	p.UpdatedAt = r.now()
	_, err = r.pool.Exec(ctx, `UPDATE router_profiles SET name = $1, mode = $2, document = $3, revision = $4, updated_at = $5 WHERE id = $6`,
		p.Name, p.Mode, document, p.Revision, p.UpdatedAt, p.ID)
	return err
}

// Create is create(tenantID:request:).
func (r *RouterProfileRepository) Create(ctx context.Context, tenant uuid.UUID, req RouterProfileWrite) (*RouterProfile, error) {
	p := &RouterProfile{ID: uuid.New(), TenantID: tenant, Name: req.Name, Mode: string(req.Mode), Document: r.normalizedDocument(req), Revision: 1}
	if err := r.insertProfile(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Update is update(tenantID:id:request:). Nil with a nil error means no such row.
func (r *RouterProfileRepository) Update(ctx context.Context, tenant, id uuid.UUID, req RouterProfileWrite) (*RouterProfile, error) {
	p, err := r.Profile(ctx, tenant, id)
	if err != nil || p == nil {
		return nil, err
	}
	if req.ExpectedRevision != nil && *req.ExpectedRevision != p.Revision {
		return nil, ErrRouterRevisionConflict
	}
	p.Name, p.Mode, p.Document = req.Name, string(req.Mode), r.normalizedDocument(req)
	p.Revision++
	if err := r.saveProfile(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Delete is delete(tenantID:id:). The default profile cannot go.
func (r *RouterProfileRepository) Delete(ctx context.Context, tenant, id uuid.UUID) (bool, error) {
	def, err := r.ensureDefault(ctx, tenant)
	if err != nil {
		return false, err
	}
	if def.ID == id {
		return false, ErrCannotDeleteDefault
	}
	p, err := r.Profile(ctx, tenant, id)
	if err != nil || p == nil {
		return false, err
	}
	_, err = r.pool.Exec(ctx, `DELETE FROM router_profiles WHERE id = $1`, id)
	return err == nil, err
}

const bindingColumns = `id, tenant_id, scope, scope_id, profile_id`

func scanBinding(row pgx.Row) (*RouterBinding, error) {
	var b RouterBinding
	if err := row.Scan(&b.ID, &b.TenantID, &b.Scope, &b.ScopeID, &b.ProfileID); err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *RouterProfileRepository) binding(ctx context.Context, tenant uuid.UUID, scope RouterBindingScope, scopeID string) (*RouterBinding, error) {
	b, err := scanBinding(r.pool.QueryRow(ctx, `SELECT `+bindingColumns+` FROM router_bindings WHERE tenant_id = $1 AND scope = $2 AND scope_id = $3 LIMIT 1`, tenant, scope, scopeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// Bindings is bindings(tenantID:).
func (r *RouterProfileRepository) Bindings(ctx context.Context, tenant uuid.UUID) ([]*RouterBinding, error) {
	if _, err := r.ensureDefault(ctx, tenant); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+bindingColumns+` FROM router_bindings WHERE tenant_id = $1`, tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*RouterBinding, error) { return scanBinding(row) })
}

// Bind is bind(tenantID:scope:scopeID:profileID:), an upsert on the scope.
func (r *RouterProfileRepository) Bind(ctx context.Context, tenant uuid.UUID, scope RouterBindingScope, scopeID string, profileID uuid.UUID) (*RouterBinding, error) {
	p, err := r.Profile(ctx, tenant, profileID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrRouterProfileNotFound
	}
	existing, err := r.binding(ctx, tenant, scope, scopeID)
	if err != nil {
		return nil, err
	}
	now := r.now()
	if existing != nil {
		existing.ProfileID = profileID
		_, err := r.pool.Exec(ctx, `UPDATE router_bindings SET profile_id = $1, updated_at = $2 WHERE id = $3`, profileID, now, existing.ID)
		return existing, err
	}
	b := &RouterBinding{ID: uuid.New(), TenantID: tenant, Scope: scope, ScopeID: scopeID, ProfileID: profileID}
	_, err = r.pool.Exec(ctx, `INSERT INTO router_bindings (id, tenant_id, scope, scope_id, profile_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, b.ID, tenant, scope, scopeID, profileID, now, now)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Unbind is unbind(tenantID:scope:scopeID:). The user binding cannot go.
func (r *RouterProfileRepository) Unbind(ctx context.Context, tenant uuid.UUID, scope RouterBindingScope, scopeID string) (bool, error) {
	if scope == BindingUser {
		return false, ErrCannotDeleteDefault
	}
	b, err := r.binding(ctx, tenant, scope, scopeID)
	if err != nil || b == nil {
		return false, err
	}
	_, err = r.pool.Exec(ctx, `DELETE FROM router_bindings WHERE id = $1`, b.ID)
	return err == nil, err
}

// Resolve is resolve(tenantID:spaceID:jobID:workflowID:): the most specific
// binding wins, and a binding whose profile is gone is skipped. Seeds the default
// when nothing is bound.
func (r *RouterProfileRepository) Resolve(ctx context.Context, tenant uuid.UUID, spaceID *uuid.UUID, jobID, workflowID string) (*RouterProfile, error) {
	space := ""
	if spaceID != nil {
		space = swiftUUID(*spaceID)
	}
	for _, c := range []struct {
		scope RouterBindingScope
		key   string
	}{
		{BindingWorkflow, workflowID},
		{BindingJob, jobID},
		{BindingSpace, space},
		{BindingUser, swiftUUID(tenant)},
	} {
		if c.key == "" {
			continue
		}
		b, err := r.binding(ctx, tenant, c.scope, c.key)
		if err != nil {
			return nil, err
		}
		if b == nil {
			continue
		}
		p, err := r.Profile(ctx, tenant, b.ProfileID)
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
	}
	return r.ensureDefault(ctx, tenant)
}

// SynchronizeDefault is synchronizeDefault: it keeps the user-scope profile aligned
// with the brain-mode preference. LLMPreferencesController calls it in wave 5.
func (r *RouterProfileRepository) SynchronizeDefault(ctx context.Context, tenant uuid.UUID, mode CredentialMode, primary WireRoute, fallbacks []WireRoute, allowed, blocked []ProviderID) error {
	p, err := r.ensureDefault(ctx, tenant)
	if err != nil {
		return err
	}
	p.Mode = string(mode)
	if mode == ModeBYOK {
		routes := append([]WireRoute{{Provider: primary.Provider, Model: primary.Model}}, fallbacks...)
		p.Document.AllowedProviders = allowed
		p.Document.BlockedProviders = blocked
		p.Document.DefaultAction = RouterAction{Kind: ActionSequential, Routes: routes, RetryPolicy: p.Document.DefaultAction.RetryPolicy}
	} else {
		p.Document = r.managedDocument(p.Document)
	}
	p.Revision++
	return r.saveProfile(ctx, p)
}

// ensureDefault returns the user-scope profile, seeding it from the legacy
// preference on a tenant's first routed call. A create race is resolved by
// reloading the winner.
func (r *RouterProfileRepository) ensureDefault(ctx context.Context, tenant uuid.UUID) (*RouterProfile, error) {
	load := func() (*RouterProfile, error) {
		b, err := r.binding(ctx, tenant, BindingUser, swiftUUID(tenant))
		if err != nil || b == nil {
			return nil, err
		}
		return r.Profile(ctx, tenant, b.ProfileID)
	}
	if p, err := load(); err != nil || p != nil {
		return p, err
	}

	// A failed read seeds as managed (Swift's try?).
	legacy, err := r.preferences.Get(ctx, tenant)
	if err != nil {
		legacy = nil
	}
	mode := ModeManaged
	if legacy != nil && legacy.Mode == ModeBYOK {
		mode = ModeBYOK
	}
	seed := []WireRoute{{Provider: ManagedProvider, Model: r.managedModel}}
	allowed, blocked := []ProviderID{ManagedProvider}, []ProviderID{}
	if mode == ModeBYOK {
		primary := WireRoute{Provider: ManagedProvider, Model: r.managedModel}
		if id, ok := legacy.PrimaryProvider.ToShared(); ok {
			primary.Provider = id
		}
		if legacy.PrimaryModel != "" {
			primary.Model = legacy.PrimaryModel
		}
		seed = []WireRoute{primary}
		for _, step := range legacy.FallbackChain {
			if id, ok := step.Provider.ToShared(); ok {
				seed = append(seed, WireRoute{Provider: id, Model: step.Model})
			}
		}
		// Multi-tier seed, so Auto has cheap and strong options from day one.
		for _, route := range []WireRoute{
			{Provider: ProviderID(Gemini), Model: "gemini-2.5-flash"},
			{Provider: ProviderID(Anthropic), Model: "claude-3-5-haiku-20241022"},
			{Provider: ProviderID(OpenAI), Model: "gpt-4o-mini"},
			{Provider: ManagedProvider, Model: ManagedModel},
			{Provider: ProviderID(Anthropic), Model: "claude-sonnet-4-6"},
			{Provider: ProviderID(OpenAI), Model: "gpt-4o"},
			{Provider: ProviderID(Anthropic), Model: "claude-opus-4-1"},
		} {
			if !slices.ContainsFunc(seed, func(s WireRoute) bool { return s.ID() == route.ID() }) {
				seed = append(seed, route)
			}
		}
		allowed, blocked = sharedProviders(legacy.Allowed), sharedProviders(legacy.Blocked)
	}

	p := &RouterProfile{
		ID: uuid.New(), TenantID: tenant, Name: "Default", Mode: string(mode), IsPreset: true, Revision: 1,
		Document: RouterProfileDocument{
			Objective:        RouterObjective{Quality: 50, Cost: 25, Latency: 25},
			AllowedProviders: allowed,
			BlockedProviders: blocked,
			DefaultAction:    RouterAction{Kind: ActionSequential, Routes: seed, RetryPolicy: RetryFast},
			Rules:            []RouterRule{},
			RoutingPolicy:    PolicyAutoSmart,
		},
	}
	err = r.insertProfile(ctx, p)
	if err == nil {
		_, err = r.Bind(ctx, tenant, BindingUser, swiftUUID(tenant), p.ID)
	}
	if err != nil {
		slog.WarnContext(ctx, "cerberus default profile create raced; reloading", "tenant_id", swiftUUID(tenant))
		winner, loadErr := load()
		if loadErr != nil {
			return nil, loadErr
		}
		if winner != nil {
			return winner, nil
		}
		return nil, err
	}
	return p, nil
}

func sharedProviders(kinds []ProviderKind) []ProviderID {
	out := []ProviderID{}
	for _, k := range kinds {
		if id, ok := k.ToShared(); ok {
			out = append(out, id)
		}
	}
	return out
}

func (r *RouterProfileRepository) normalizedDocument(req RouterProfileWrite) RouterProfileDocument {
	requested := RouterProfileDocument{
		Objective:        req.Objective,
		Budget:           req.Budget,
		AllowedProviders: req.AllowedProviders,
		BlockedProviders: req.BlockedProviders,
		DefaultAction:    req.DefaultAction,
		Rules:            req.Rules,
		RoutingPolicy:    req.RoutingPolicy,
	}
	if req.Mode == ModeManaged {
		return r.managedDocument(requested)
	}
	return requested
}

// reconcileManaged is reconcileManagedProfile: a managed row must only ever name
// the managed route, so a stale one is rewritten on read and its revision bumped.
// Comparing encodings is Swift's Equatable: nil and empty optionals stay distinct.
func (r *RouterProfileRepository) reconcileManaged(ctx context.Context, p *RouterProfile) error {
	if p.Mode != string(ModeManaged) {
		return nil
	}
	normalized := r.managedDocument(p.Document)
	current, err := encodeJSON(p.Document)
	if err != nil {
		return err
	}
	next, err := encodeJSON(normalized)
	if err != nil {
		return err
	}
	if bytes.Equal(current, next) {
		return nil
	}
	p.Document = normalized
	p.Revision++
	return r.saveProfile(ctx, p)
}

func (r *RouterProfileRepository) managedDocument(d RouterProfileDocument) RouterProfileDocument {
	action := RouterAction{
		Kind:        ActionSequential,
		Routes:      []WireRoute{{Provider: ManagedProvider, Model: r.managedModel}},
		RetryPolicy: d.DefaultAction.RetryPolicy,
	}
	rules := make([]RouterRule, 0, len(d.Rules))
	for _, rule := range d.Rules {
		rule.Action = action
		rules = append(rules, rule)
	}
	return RouterProfileDocument{
		Objective:        d.Objective,
		Budget:           d.Budget,
		AllowedProviders: []ProviderID{ManagedProvider},
		BlockedProviders: []ProviderID{},
		DefaultAction:    action,
		Rules:            rules,
		RoutingPolicy:    d.RoutingPolicy,
	}
}

// ManagedModel is hermes.defaultManagedModel, the model a managed profile routes
// to. LLMPreferencesController stores it as a managed preference's primary model.
func (r *RouterProfileRepository) ManagedModel() string { return r.managedModel }

// MarshalJSON writes the id as Swift's JSONEncoder writes a UUID, uppercase. The
// document is stored as JSONB and returned by /v1/router, so a lowercase id would
// differ from a row Swift wrote.
func (r RouterRule) MarshalJSON() ([]byte, error) {
	type plain RouterRule
	return json.Marshal(struct {
		ID string `json:"id"`
		plain
	}{swiftUUID(r.ID), plain(r)})
}

// MarshalJSON writes the id uppercase, as RouterRule's does.
func (p ParallelParticipant) MarshalJSON() ([]byte, error) {
	type plain ParallelParticipant
	return json.Marshal(struct {
		ID string `json:"id"`
		plain
	}{swiftUUID(p.ID), plain(p)})
}
