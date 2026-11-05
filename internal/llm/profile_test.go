package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouterProfileDocumentDecoding(t *testing.T) {
	const minimal = `{"objective":{"quality":1,"cost":2,"latency":3},"budget":{},"defaultAction":{"kind":"sequential","routes":[],"retryPolicy":"fast"}}`
	t.Run("the optional keys default", func(t *testing.T) {
		var doc RouterProfileDocument
		require.NoError(t, json.Unmarshal([]byte(minimal), &doc))
		assert.Equal(t, PolicyAutoSmart, doc.RoutingPolicy)
		assert.NotNil(t, doc.AllowedProviders)
		assert.NotNil(t, doc.BlockedProviders)
		assert.NotNil(t, doc.Rules)
	})
	t.Run("a document Swift's Codable refuses is refused", func(t *testing.T) {
		for name, raw := range map[string]string{
			"no objective":      `{"budget":{},"defaultAction":{"kind":"sequential","routes":[],"retryPolicy":"fast"}}`,
			"partial objective": `{"objective":{"quality":1},"budget":{},"defaultAction":{"kind":"sequential","routes":[],"retryPolicy":"fast"}}`,
			"null budget":       `{"objective":{"quality":1,"cost":2,"latency":3},"budget":null,"defaultAction":{"kind":"sequential","routes":[],"retryPolicy":"fast"}}`,
			"no retry policy":   `{"objective":{"quality":1,"cost":2,"latency":3},"budget":{},"defaultAction":{"kind":"sequential","routes":[]}}`,
			"unknown enum":      `{"objective":{"quality":1,"cost":2,"latency":3},"budget":{},"defaultAction":{"kind":"nope","routes":[],"retryPolicy":"fast"}}`,
			"unknown provider":  `{"objective":{"quality":1,"cost":2,"latency":3},"budget":{},"allowedProviders":["groq"],"defaultAction":{"kind":"sequential","routes":[],"retryPolicy":"fast"}}`,
			"route sans model":  `{"objective":{"quality":1,"cost":2,"latency":3},"budget":{},"defaultAction":{"kind":"sequential","routes":[{"provider":"openai"}],"retryPolicy":"fast"}}`,
			"rule sans action":  `{"objective":{"quality":1,"cost":2,"latency":3},"budget":{},"rules":[{"id":"6F9619FF-8B86-D011-B42D-00C04FC964FF","name":"r","enabled":true,"priority":1,"taskTypes":[],"surfaces":[]}],"defaultAction":{"kind":"sequential","routes":[],"retryPolicy":"fast"}}`,
		} {
			var doc RouterProfileDocument
			assert.Error(t, json.Unmarshal([]byte(raw), &doc), name)
		}
	})
	t.Run("nil optionals stay out of the encoding, and an empty participants list stays in", func(t *testing.T) {
		action := RouterAction{Kind: ActionSequential, Routes: []WireRoute{}, RetryPolicy: RetryFast}
		data, err := encodeJSON(action)
		require.NoError(t, err)
		assert.JSONEq(t, `{"kind":"sequential","routes":[],"retryPolicy":"fast"}`, string(data))
		action.Participants = []ParallelParticipant{}
		data, err = encodeJSON(action)
		require.NoError(t, err)
		assert.JSONEq(t, `{"kind":"sequential","routes":[],"retryPolicy":"fast","participants":[]}`, string(data))
	})
}

func TestRouterProfileRepository(t *testing.T) {
	ctx, pool, newUser := llmDB(t)
	preferences := NewPreferenceRepository(pool)
	const managed = "deepseek/deepseek-v4-flash"
	repo := NewRouterProfileRepository(pool, preferences, managed)

	stored := func(t *testing.T, id uuid.UUID) (doc string, revision int) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `SELECT document::text, revision FROM router_profiles WHERE id = $1`, id).Scan(&doc, &revision))
		return doc, revision
	}
	writeFrom := func(p *RouterProfile, mode CredentialMode) RouterProfileWrite {
		return RouterProfileWrite{
			Name: p.Name, Mode: mode, Objective: p.Document.Objective, Budget: p.Document.Budget,
			AllowedProviders: []ProviderID{}, BlockedProviders: []ProviderID{},
			DefaultAction: RouterAction{Kind: ActionSequential, RetryPolicy: RetryFast, Routes: []WireRoute{wireRoute(Anthropic, "claude-sonnet-4-6")}},
			Rules:         []RouterRule{}, RoutingPolicy: PolicyBalanced,
		}
	}

	t.Run("a first routed call seeds a managed default, on the JSON Swift writes", func(t *testing.T) {
		tenant := newUser()
		p, err := repo.Resolve(ctx, tenant, nil, "", "")
		require.NoError(t, err)
		assert.Equal(t, "Default", p.Name)
		assert.Equal(t, "managed", p.Mode)
		assert.True(t, p.IsPreset)
		assert.Equal(t, 1, p.Revision)
		doc, revision := stored(t, p.ID)
		assert.Equal(t, 1, revision)
		assert.JSONEq(t, `{
			"objective":{"quality":50,"cost":25,"latency":25},
			"budget":{},
			"allowedProviders":["openRouter"],
			"blockedProviders":[],
			"defaultAction":{"kind":"sequential","routes":[{"provider":"openRouter","model":"deepseek/deepseek-v4-flash"}],"retryPolicy":"fast"},
			"rules":[],
			"routingPolicy":"autoSmart"
		}`, doc)

		// The binding scope id is Swift's uppercase uuidString: a lowercase row
		// would never be found by the Swift server, and it would seed a second one.
		var scope, scopeID string
		require.NoError(t, pool.QueryRow(ctx, `SELECT scope, scope_id FROM router_bindings WHERE tenant_id = $1`, tenant).Scan(&scope, &scopeID))
		assert.Equal(t, "user", scope)
		assert.Equal(t, strings.ToUpper(tenant.String()), scopeID)

		again, err := repo.Resolve(ctx, tenant, nil, "", "")
		require.NoError(t, err)
		assert.Equal(t, p.ID, again.ID, "seeded once")
	})
	t.Run("a Swift-written default is found through its uppercase binding", func(t *testing.T) {
		tenant := newUser()
		first, err := repo.Resolve(ctx, tenant, nil, "", "")
		require.NoError(t, err)
		var rows int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM router_bindings WHERE tenant_id = $1 AND scope_id = $2`, tenant, strings.ToUpper(tenant.String())).Scan(&rows))
		assert.Equal(t, 1, rows)
		assert.Equal(t, first.ID, mustResolve(t, repo, tenant).ID)
	})
	t.Run("a byok preference seeds its own primary, chain and the multi-tier auto routes", func(t *testing.T) {
		tenant := newUser()
		_, err := pool.Exec(ctx, `INSERT INTO user_llm_preferences (id, tenant_id, primary_provider, primary_model, fallback_chain, mode, allowed_providers, blocked_providers)
			VALUES (gen_random_uuid(), $1, 'anthropic', 'claude-opus-4-7', $2::jsonb, 'byok', $3::jsonb, $4::jsonb)`,
			tenant, `{"steps":[{"provider":"openai","model":"gpt-4o"},{"provider":"groq","model":"dropped"}]}`, `{"providers":["anthropic","groq"]}`, `{"providers":["xai"]}`)
		require.NoError(t, err)
		p, err := repo.Resolve(ctx, tenant, nil, "", "")
		require.NoError(t, err)
		assert.Equal(t, "byok", p.Mode)
		assert.Equal(t, []WireRoute{
			wireRoute(Anthropic, "claude-opus-4-7"),
			wireRoute(OpenAI, "gpt-4o"),
			wireRoute(Gemini, "gemini-2.5-flash"),
			wireRoute(Anthropic, "claude-3-5-haiku-20241022"),
			wireRoute(OpenAI, "gpt-4o-mini"),
			{Provider: ManagedProvider, Model: ManagedModel},
			wireRoute(Anthropic, "claude-sonnet-4-6"),
			wireRoute(Anthropic, "claude-opus-4-1"),
		}, p.Document.DefaultAction.Routes)
		assert.Equal(t, []ProviderID{ProviderID(Anthropic)}, p.Document.AllowedProviders, "groq has no wire id")
		assert.Equal(t, []ProviderID{ProviderID(XAI)}, p.Document.BlockedProviders)
	})
	t.Run("the most specific binding wins, keyed on Swift's uuidString", func(t *testing.T) {
		tenant := newUser()
		def := mustResolve(t, repo, tenant)
		other := func(name string) *RouterProfile {
			w := writeFrom(def, ModeBYOK)
			w.Name = name
			p, err := repo.Create(ctx, tenant, w)
			require.NoError(t, err)
			return p
		}
		space, job, workflow := other("space"), other("job"), other("workflow")
		spaceID := uuid.New()
		_, err := repo.Bind(ctx, tenant, BindingSpace, swiftUUID(spaceID), space.ID)
		require.NoError(t, err)
		_, err = repo.Bind(ctx, tenant, BindingJob, "job-1", job.ID)
		require.NoError(t, err)
		_, err = repo.Bind(ctx, tenant, BindingWorkflow, "wf-1", workflow.ID)
		require.NoError(t, err)

		resolve := func(space *uuid.UUID, jobID, workflowID string) string {
			p, err := repo.Resolve(ctx, tenant, space, jobID, workflowID)
			require.NoError(t, err)
			return p.Name
		}
		assert.Equal(t, "workflow", resolve(&spaceID, "job-1", "wf-1"))
		assert.Equal(t, "job", resolve(&spaceID, "job-1", "other"))
		assert.Equal(t, "space", resolve(&spaceID, "other", ""))
		unbound := uuid.New()
		assert.Equal(t, "Default", resolve(&unbound, "", ""))
		assert.Equal(t, "Default", resolve(nil, "", ""))

		var keys int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM router_bindings WHERE tenant_id = $1 AND scope = 'space' AND scope_id = $2`, tenant, strings.ToUpper(spaceID.String())).Scan(&keys))
		assert.Equal(t, 1, keys)
	})
	t.Run("a stale managed document is rewritten on read and its revision bumped", func(t *testing.T) {
		tenant := newUser()
		p := mustResolve(t, repo, tenant)
		soft := int64(9)
		ruleID := uuid.New()
		stale := p.Document
		stale.Budget = RouterBudget{SoftLimitUsdMicros: &soft}
		stale.AllowedProviders = []ProviderID{ProviderID(Anthropic)}
		stale.RoutingPolicy = PolicyMaxQuality
		stale.DefaultAction = RouterAction{Kind: ActionEnsemble, RetryPolicy: RetryResilient, Routes: []WireRoute{wireRoute(Anthropic, "claude-opus-4-1")}}
		stale.Rules = []RouterRule{{ID: ruleID, Name: "r", Enabled: true, Priority: 1, TaskTypes: []RouterTaskType{TaskCoding}, Surfaces: []RouterSurface{}, Action: stale.DefaultAction}}
		raw, err := encodeJSON(stale)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE router_profiles SET document = $1 WHERE id = $2`, raw, p.ID)
		require.NoError(t, err)

		got := mustResolve(t, repo, tenant)
		assert.Equal(t, 2, got.Revision)
		managedRoutes := []WireRoute{{Provider: ManagedProvider, Model: managed}}
		assert.Equal(t, managedRoutes, got.Document.DefaultAction.Routes)
		assert.Equal(t, RetryResilient, got.Document.DefaultAction.RetryPolicy, "the retry policy is kept")
		assert.Equal(t, ActionSequential, got.Document.DefaultAction.Kind)
		assert.Equal(t, []ProviderID{ManagedProvider}, got.Document.AllowedProviders)
		assert.Equal(t, PolicyMaxQuality, got.Document.RoutingPolicy, "the policy is kept")
		assert.Equal(t, &soft, got.Document.Budget.SoftLimitUsdMicros, "the budget is kept")
		require.Len(t, got.Document.Rules, 1)
		assert.Equal(t, managedRoutes, got.Document.Rules[0].Action.Routes)
		assert.Equal(t, ruleID, got.Document.Rules[0].ID)

		_, revision := stored(t, p.ID)
		assert.Equal(t, 2, revision)
		mustResolve(t, repo, tenant)
		_, revision = stored(t, p.ID)
		assert.Equal(t, 2, revision, "a clean document is not rewritten")
	})
	t.Run("a mode Swift cannot read is not reconciled, and reads as managed", func(t *testing.T) {
		tenant := newUser()
		p := mustResolve(t, repo, tenant)
		_, err := pool.Exec(ctx, `UPDATE router_profiles SET mode = 'legacy', document = jsonb_set(document, '{rules}', '[]') WHERE id = $1`, p.ID)
		require.NoError(t, err)
		got := mustResolve(t, repo, tenant)
		assert.Equal(t, ModeManaged, got.BrainMode())
		assert.Equal(t, p.Revision, got.Revision)
	})
	t.Run("update checks the revision", func(t *testing.T) {
		tenant := newUser()
		p := mustResolve(t, repo, tenant)
		write := writeFrom(p, ModeBYOK)
		stale := p.Revision + 5
		write.ExpectedRevision = &stale
		_, err := repo.Update(ctx, tenant, p.ID, write)
		require.ErrorIs(t, err, ErrRouterRevisionConflict)

		write.ExpectedRevision = &p.Revision
		updated, err := repo.Update(ctx, tenant, p.ID, write)
		require.NoError(t, err)
		assert.Equal(t, p.Revision+1, updated.Revision)
		assert.Equal(t, "byok", updated.Mode)
		missing, err := repo.Update(ctx, tenant, uuid.New(), write)
		require.NoError(t, err)
		assert.Nil(t, missing)
	})
	t.Run("a managed write is normalized, a byok write is not", func(t *testing.T) {
		tenant := newUser()
		p := mustResolve(t, repo, tenant)
		write := writeFrom(p, ModeManaged)
		write.Name = "normalized"
		created, err := repo.Create(ctx, tenant, write)
		require.NoError(t, err)
		assert.False(t, created.IsPreset)
		assert.Equal(t, 1, created.Revision)
		assert.Equal(t, []WireRoute{{Provider: ManagedProvider, Model: managed}}, created.Document.DefaultAction.Routes)

		write.Name, write.Mode = "kept", ModeBYOK
		kept, err := repo.Create(ctx, tenant, write)
		require.NoError(t, err)
		assert.Equal(t, write.DefaultAction.Routes, kept.Document.DefaultAction.Routes)
	})
	t.Run("list, delete and unbind", func(t *testing.T) {
		tenant := newUser()
		def := mustResolve(t, repo, tenant)
		write := writeFrom(def, ModeBYOK)
		write.Name = "extra"
		extra, err := repo.Create(ctx, tenant, write)
		require.NoError(t, err)

		profiles, defaultID, err := repo.List(ctx, tenant)
		require.NoError(t, err)
		assert.Equal(t, def.ID, defaultID)
		require.Len(t, profiles, 2)
		assert.Equal(t, def.ID, profiles[0].ID, "oldest first")

		_, err = repo.Delete(ctx, tenant, def.ID)
		require.ErrorIs(t, err, ErrCannotDeleteDefault)
		deleted, err := repo.Delete(ctx, tenant, uuid.New())
		require.NoError(t, err)
		assert.False(t, deleted)

		binding, err := repo.Bind(ctx, tenant, BindingJob, "j", extra.ID)
		require.NoError(t, err)
		rebound, err := repo.Bind(ctx, tenant, BindingJob, "j", def.ID)
		require.NoError(t, err)
		assert.Equal(t, binding.ID, rebound.ID, "bind is an upsert on the scope")
		assert.Equal(t, def.ID, rebound.ProfileID)
		_, err = repo.Bind(ctx, tenant, BindingJob, "j", uuid.New())
		require.ErrorIs(t, err, ErrRouterProfileNotFound)

		bindings, err := repo.Bindings(ctx, tenant)
		require.NoError(t, err)
		assert.Len(t, bindings, 2)

		_, err = repo.Unbind(ctx, tenant, BindingUser, swiftUUID(tenant))
		require.ErrorIs(t, err, ErrCannotDeleteDefault)
		gone, err := repo.Unbind(ctx, tenant, BindingJob, "j")
		require.NoError(t, err)
		assert.True(t, gone)
		gone, err = repo.Unbind(ctx, tenant, BindingJob, "j")
		require.NoError(t, err)
		assert.False(t, gone)

		deleted, err = repo.Delete(ctx, tenant, extra.ID)
		require.NoError(t, err)
		assert.True(t, deleted)
	})
	t.Run("another tenant's profile cannot be read, updated or bound", func(t *testing.T) {
		owner, intruder := newUser(), newUser()
		p := mustResolve(t, repo, owner)
		got, err := repo.Profile(ctx, intruder, p.ID)
		require.NoError(t, err)
		assert.Nil(t, got)
		_, err = repo.Bind(ctx, intruder, BindingJob, "j", p.ID)
		require.ErrorIs(t, err, ErrRouterProfileNotFound)
	})
	t.Run("synchronizeDefault aligns the default with the brain mode", func(t *testing.T) {
		tenant := newUser()
		p := mustResolve(t, repo, tenant)
		require.NoError(t, repo.SynchronizeDefault(ctx, tenant, ModeBYOK, wireRoute(Anthropic, "m"), []WireRoute{wireRoute(OpenAI, "n")}, []ProviderID{ProviderID(Anthropic)}, []ProviderID{}))
		got := mustResolve(t, repo, tenant)
		assert.Equal(t, "byok", got.Mode)
		assert.Equal(t, p.Revision+1, got.Revision)
		assert.Equal(t, []WireRoute{wireRoute(Anthropic, "m"), wireRoute(OpenAI, "n")}, got.Document.DefaultAction.Routes)
		assert.Equal(t, []ProviderID{ProviderID(Anthropic)}, got.Document.AllowedProviders)

		require.NoError(t, repo.SynchronizeDefault(ctx, tenant, ModeManaged, WireRoute{}, nil, nil, nil))
		back := mustResolve(t, repo, tenant)
		assert.Equal(t, "managed", back.Mode)
		assert.Equal(t, []WireRoute{{Provider: ManagedProvider, Model: managed}}, back.Document.DefaultAction.Routes)
	})
}

func mustResolve(t *testing.T, repo *RouterProfileRepository, tenant uuid.UUID) *RouterProfile {
	t.Helper()
	p, err := repo.Resolve(t.Context(), tenant, nil, "", "")
	require.NoError(t, err)
	return p
}
