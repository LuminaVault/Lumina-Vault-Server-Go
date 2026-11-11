package brain_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/brain"
	"lumina/internal/ratelimit"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	alice     = uuid.MustParse("a11ce000-0000-4000-8000-000000000001")
	bob       = uuid.MustParse("b0b00000-0000-4000-8000-000000000002")
	teamVault = uuid.MustParse("7ea40000-0000-4000-8000-0000000000a1")
	keys      = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t      *testing.T
	e      *echo.Echo
	pool   *pgxpool.Pool
	limits *ratelimit.Store
}

// Each test gets its own stack, so the rate-limit store never crosses tests.
func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"../../../parity/seed/base.sql", "testdata/seed.sql"} {
		seed, err := os.ReadFile(f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	limits := ratelimit.NewStore()
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{brain.New(pool, auth.Identify(keys, auth.PGUsers(pool)), limits)},
	})
	return env{t: t, e: e, pool: pool, limits: limits}
}

func (v env) do(user uuid.UUID, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

// validate checks a response against api/openapi.yaml.
func validate(t *testing.T, method, path string, rec *httptest.ResponseRecorder) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, nil)
	route, params, err := router.FindRoute(req)
	require.NoError(t, err, path)
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status: rec.Code,
		Header: http.Header{"Content-Type": {rec.Header().Get("Content-Type")}},
	}
	out.SetBodyBytes(rec.Body.Bytes())
	require.NoError(t, openapi3filter.ValidateResponse(context.Background(), out), "%s %s: %s", method, path, rec.Body.String())
}

func TestGraphRequiresAuth(t *testing.T) {
	v := setup(t)
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/brain/graph", "").Code)
}

// emptyVault removes everything the seed put in a user's personal vault, so the
// empty-graph cases still describe a user with no notes and no memories.
func emptyVault(t *testing.T, v env, user uuid.UUID) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM vault_links WHERE tenant_id = $1`,
		`DELETE FROM memories WHERE tenant_id = $1`,
		`DELETE FROM vault_files WHERE tenant_id = $1`,
	} {
		_, err := v.pool.Exec(context.Background(), q, user)
		require.NoError(t, err)
	}
}

func TestGraphEmptyMatchesSpec(t *testing.T) {
	v := setup(t)
	emptyVault(t, v, bob)
	rec := v.do(bob, http.MethodGet, "/v1/brain/graph", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	validate(t, http.MethodGet, "/v1/brain/graph", rec)
	var out struct {
		Nodes []json.RawMessage `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(t, out.Nodes)
	require.Empty(t, out.Nodes)
	require.NotNil(t, out.Edges)
	require.Empty(t, out.Edges)
}

func TestGraphRateLimited(t *testing.T) {
	v := setup(t)
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	v.limits.SetClock(func() time.Time { return now })
	var tier string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT tier FROM users WHERE id = $1`, alice).Scan(&tier))
	max := ratelimit.BrainGraphByUser.EffectiveMax(tier)
	require.Positive(t, max)
	for i := 1; i <= max; i++ {
		require.Equal(t, http.StatusOK, v.do(alice, http.MethodGet, "/v1/brain/graph", "").Code, "request %d of %d", i, max)
	}
	require.Equal(t, http.StatusTooManyRequests, v.do(alice, http.MethodGet, "/v1/brain/graph", "").Code)
}

type vaultOut struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Personal bool   `json:"personal"`
}

func vaultsOf(t *testing.T, rec *httptest.ResponseRecorder) []vaultOut {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Vaults []vaultOut `json:"vaults"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	for i := range out.Vaults {
		out.Vaults[i].ID = strings.ToLower(out.Vaults[i].ID)
	}
	return out.Vaults
}

func TestVaultsDefaultToAllReadable(t *testing.T) {
	v := setup(t)
	got := vaultsOf(t, v.do(alice, http.MethodGet, "/v1/brain/graph", ""))
	require.Equal(t, []vaultOut{
		{ID: alice.String(), Name: "alice's Vault", Personal: true},
		{ID: teamVault.String(), Name: "Team Vault", Personal: false},
	}, got)
}

func TestVaultsParamNarrows(t *testing.T) {
	v := setup(t)
	got := vaultsOf(t, v.do(alice, http.MethodGet, "/v1/brain/graph?vaults="+teamVault.String()+","+teamVault.String(), ""))
	require.Equal(t, []vaultOut{{ID: teamVault.String(), Name: "Team Vault"}}, got)
	// An empty value is the same as an absent one.
	require.Len(t, vaultsOf(t, v.do(alice, http.MethodGet, "/v1/brain/graph?vaults=", "")), 2)
}

func TestVaultsParamRejectsUnreadable(t *testing.T) {
	v := setup(t)
	for _, id := range []uuid.UUID{bob, uuid.MustParse("7ea40000-0000-4000-8000-0000000000a2"), uuid.New()} {
		rec := v.do(alice, http.MethodGet, "/v1/brain/graph?vaults="+alice.String()+","+id.String(), "")
		require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "nodes")
	}
	require.Equal(t, http.StatusForbidden, v.do(alice, http.MethodGet, "/v1/brain/graph?vaults="+bob.String(), "").Code)
}

func TestVaultsParamMalformed(t *testing.T) {
	v := setup(t)
	for _, q := range []string{"nope", alice.String() + ",nope", alice.String() + ",,"} {
		rec := v.do(alice, http.MethodGet, "/v1/brain/graph?vaults="+q, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, q)
		require.Contains(t, rec.Body.String(), "invalid vaults")
	}
}

func TestNewUserHasEmptyGraph(t *testing.T) {
	v := setup(t)
	emptyVault(t, v, bob)
	rec := v.do(bob, http.MethodGet, "/v1/brain/graph", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	validate(t, http.MethodGet, "/v1/brain/graph", rec)
	require.Equal(t, []vaultOut{{ID: bob.String(), Name: "bob's Vault", Personal: true}}, vaultsOf(t, rec))
	require.Contains(t, rec.Body.String(), `"nodes":[]`)
	require.Contains(t, rec.Body.String(), `"edges":[]`)
}

const (
	n1 = "00000000-0000-4000-8000-0000000001a1"
	n2 = "00000000-0000-4000-8000-0000000001a2"
	n3 = "00000000-0000-4000-8000-0000000001a3"
	m1 = "00000000-0000-4000-8000-0000000002a1"
	m2 = "00000000-0000-4000-8000-0000000002a2"
	bn = "00000000-0000-4000-8000-0000000001b1"
	bm = "00000000-0000-4000-8000-0000000002b1"
)

type graphOut struct {
	Truncated bool `json:"truncated"`
	Nodes     []struct {
		ID      string                     `json:"id"`
		Kind    string                     `json:"kind"`
		Label   string                     `json:"label"`
		VaultID string                     `json:"vaultID"`
		Weight  int                        `json:"weight"`
		Pos     *struct{ X, Y, Z float64 } `json:"pos"`
	} `json:"nodes"`
	Edges []struct {
		From string `json:"from"`
		To   string `json:"to"`
		Kind string `json:"kind"`
	} `json:"edges"`
}

func getGraph(t *testing.T, v env, query string) graphOut {
	t.Helper()
	rec := v.do(alice, http.MethodGet, "/v1/brain/graph"+query, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	validate(t, http.MethodGet, "/v1/brain/graph"+query, rec)
	var out graphOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	for i := range out.Nodes {
		out.Nodes[i].VaultID = strings.ToLower(out.Nodes[i].VaultID)
	}
	return out
}

func (g graphOut) itemNodes() int {
	n := 0
	for _, x := range g.Nodes {
		if x.Kind == "note" || x.Kind == "memory" {
			n++
		}
	}
	return n
}

func TestItemsSpanVaults(t *testing.T) {
	v := setup(t)
	g := getGraph(t, v, "")
	got := map[string][2]string{}
	for _, n := range g.Nodes {
		got[n.ID] = [2]string{n.Kind, n.VaultID}
	}
	require.Equal(t, [2]string{"note", alice.String()}, got[n1])
	require.Equal(t, [2]string{"note", alice.String()}, got[n2])
	require.Equal(t, [2]string{"note", teamVault.String()}, got[n3])
	require.Equal(t, [2]string{"memory", alice.String()}, got[m1])
	require.Equal(t, [2]string{"memory", teamVault.String()}, got[m2])
	require.NotContains(t, got, bn)
	require.NotContains(t, got, bm)
	for _, n := range g.Nodes {
		require.NotEqual(t, bob.String(), n.VaultID)
		switch n.ID {
		case n1:
			require.Equal(t, "First Note", n.Label)
		case m1:
			require.Equal(t, "Alice memory", n.Label)
			require.NotNil(t, n.Pos)
		case m2:
			require.Nil(t, n.Pos)
		}
	}
	require.False(t, g.Truncated)
}

func TestLinkAndSourceEdges(t *testing.T) {
	v := setup(t)
	g := getGraph(t, v, "")
	type e struct{ From, To, Kind string }
	var got []e
	for _, x := range g.Edges {
		got = append(got, e{x.From, x.To, x.Kind})
	}
	require.Equal(t, []e{{n1, n2, "link"}, {m1, n1, "source"}}, got)
	for _, n := range g.Nodes {
		if n.ID == n1 {
			require.Equal(t, 2, n.Weight)
		}
	}
}

func TestLimitTruncates(t *testing.T) {
	v := setup(t)
	g := getGraph(t, v, "?limit=1")
	require.Equal(t, 1, g.itemNodes())
	require.True(t, g.Truncated)
	require.Empty(t, g.Edges, "an edge to a node outside the window must be dropped")

	all := getGraph(t, v, "?limit=2000")
	require.False(t, all.Truncated)
	require.Equal(t, 8, all.itemNodes())

	require.Equal(t, 1, getGraph(t, v, "?limit=0").itemNodes())
	require.Equal(t, 8, getGraph(t, v, "?limit=99999").itemNodes())
	require.Equal(t, 8, getGraph(t, v, "?limit=abc").itemNodes())
	require.Equal(t, 1, getGraph(t, v, "?limit=-5").itemNodes())
}

func TestLinksParamFilters(t *testing.T) {
	v := setup(t)
	g := getGraph(t, v, "?links=source")
	require.Len(t, g.Edges, 1)
	require.Equal(t, "source", g.Edges[0].Kind)
	g = getGraph(t, v, "?links=link")
	require.Len(t, g.Edges, 1)
	require.Equal(t, "link", g.Edges[0].Kind)
}

// exec runs setup SQL for one test. The shared seed stays free of hub attributes so that the
// edge and count assertions above describe only links and sources.
func exec(t *testing.T, v env, sql string, args ...any) {
	t.Helper()
	_, err := v.pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err, sql)
}

type hubGraph struct {
	Nodes []struct {
		ID      string  `json:"id"`
		Kind    string  `json:"kind"`
		Label   string  `json:"label"`
		VaultID *string `json:"vaultID"`
		Weight  int     `json:"weight"`
		Refs    []struct {
			VaultID string `json:"vaultID"`
			ID      string `json:"id"`
		} `json:"refs"`
	} `json:"nodes"`
	Edges []struct {
		From string `json:"from"`
		To   string `json:"to"`
		Kind string `json:"kind"`
	} `json:"edges"`
}

func getHubs(t *testing.T, v env, query string) hubGraph {
	t.Helper()
	rec := v.do(alice, http.MethodGet, "/v1/brain/graph"+query, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	validate(t, http.MethodGet, "/v1/brain/graph"+query, rec)
	var out hubGraph
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (g hubGraph) node(id string) (kind, label string, vault *string, weight int, ok bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n.Kind, n.Label, n.VaultID, n.Weight, true
		}
	}
	return "", "", nil, 0, false
}

func (g hubGraph) edgesTo(to string) []string {
	var out []string
	for _, e := range g.Edges {
		if e.To == to {
			out = append(out, e.Kind+":"+e.From)
		}
	}
	return out
}

func TestSharedTagHubAcrossVaults(t *testing.T) {
	v := setup(t)
	exec(t, v, `UPDATE memories SET tags = ARRAY[' Go ', 'solo'] WHERE id = $1`, m1)
	exec(t, v, `UPDATE memories SET tags = ARRAY['go'] WHERE id = $1`, m2)
	g := getHubs(t, v, "")
	kind, label, vault, weight, ok := g.node("tag:go")
	require.True(t, ok)
	require.Equal(t, "tag", kind)
	require.Equal(t, "go", label)
	require.Nil(t, vault)
	require.Equal(t, 2, weight)
	require.Equal(t, []string{"tag:" + m1, "tag:" + m2}, g.edgesTo("tag:go"))
	_, _, _, _, ok = g.node("tag:solo")
	require.False(t, ok)
	require.Empty(t, g.edgesTo("tag:solo"))
}

func TestSpaceHub(t *testing.T) {
	v := setup(t)
	space := "5ace0000-0000-4000-8000-000000000001"
	exec(t, v, `INSERT INTO spaces (id, tenant_id, name, slug) VALUES ($1, $2, 'Research', 'research')`, space, alice)
	exec(t, v, `UPDATE vault_files SET space_id = $1 WHERE id = $2`, space, n1)
	exec(t, v, `UPDATE memories SET space_id = $1 WHERE id = $2`, space, m1)
	g := getHubs(t, v, "")
	kind, label, vault, weight, ok := g.node("space:" + space)
	require.True(t, ok)
	require.Equal(t, "space", kind)
	require.Equal(t, "Research", label)
	require.NotNil(t, vault)
	require.Equal(t, alice.String(), strings.ToLower(*vault))
	require.Equal(t, 2, weight)
	require.Equal(t, []string{"space:" + n1, "space:" + m1}, g.edgesTo("space:"+space))
}

func TestSpaceInOtherVaultIsNotRead(t *testing.T) {
	v := setup(t)
	// Bob's space is referenced by two of alice's items; it is outside her scope.
	space := "5ace0000-0000-4000-8000-000000000002"
	exec(t, v, `INSERT INTO spaces (id, tenant_id, name, slug) VALUES ($1, $2, 'Bob Private', 'bp')`, space, bob)
	exec(t, v, `UPDATE vault_files SET space_id = $1 WHERE id = $2`, space, n1)
	exec(t, v, `UPDATE memories SET space_id = $1 WHERE id = $2`, space, m1)
	rec := v.do(alice, http.MethodGet, "/v1/brain/graph", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "Bob Private")
	require.NotContains(t, rec.Body.String(), "space:")
}

func TestUserAuthorHub(t *testing.T) {
	v := setup(t)
	exec(t, v, `UPDATE vault_files SET created_by_user_id = $1 WHERE id = $2`, alice, n1)
	exec(t, v, `UPDATE memories SET created_by_user_id = $1 WHERE id = $2`, alice, m1)
	g := getHubs(t, v, "")
	id := "author:user:" + alice.String()
	kind, label, vault, weight, ok := g.node(id)
	require.True(t, ok)
	require.Equal(t, "author", kind)
	require.Equal(t, "alice", label)
	require.Nil(t, vault)
	require.Equal(t, 2, weight)
	require.Equal(t, []string{"author:" + n1, "author:" + m1}, g.edgesTo(id))
}

func TestModelAuthorHub(t *testing.T) {
	v := setup(t)
	// memory_contributions.tenant_id references users, so both memories live in alice's vault.
	m3 := "00000000-0000-4000-8000-0000000002a3"
	exec(t, v, `INSERT INTO memories (id, tenant_id, content, created_at, updated_at) VALUES ($1, $2, 'third', now(), now())`, m3, alice)
	for _, mem := range []string{m1, m3} {
		// Two rows for one memory must still count once.
		for j := 0; j < 2; j++ {
			exec(t, v, `INSERT INTO memory_contributions (id, tenant_id, memory_id, operation, actor_kind, source_kind, provider, model)
				VALUES (gen_random_uuid(), $1, $2, 'create', 'model', 'extraction', 'anthropic', 'claude-haiku-5-5')`, alice, mem)
		}
	}
	// A human contribution and a model row without a model add nothing.
	exec(t, v, `INSERT INTO memory_contributions (id, tenant_id, memory_id, operation, actor_kind, source_kind, provider, model)
		VALUES (gen_random_uuid(), $1, $2, 'create', 'user', 'manual', NULL, 'ignored'), (gen_random_uuid(), $1, $2, 'create', 'model', 'extraction', 'x', NULL)`, alice, m1)
	g := getHubs(t, v, "")
	id := "author:model:anthropic/claude-haiku-5-5"
	kind, label, _, weight, ok := g.node(id)
	require.True(t, ok)
	require.Equal(t, "author", kind)
	require.Equal(t, "claude-haiku-5-5", label)
	require.Equal(t, 2, weight)
	require.Equal(t, []string{"author:" + m1, "author:" + m3}, g.edgesTo(id))
	for _, n := range g.Nodes {
		require.NotContains(t, n.ID, "ignored")
	}
}

func TestModelAuthorHubInTeamVault(t *testing.T) {
	v := setup(t)
	m3 := "00000000-0000-4000-8000-0000000002a4"
	exec(t, v, `INSERT INTO memories (id, tenant_id, content, created_at, updated_at) VALUES ($1, $2, 'team third', now(), now())`, m3, teamVault)
	// tenant_id is the contributing user, not the vault. It is bob, who is not one of alice's
	// vault ids: with alice's own id the old tenant filter would match her personal vault id.
	for _, mem := range []string{m2, m3} {
		exec(t, v, `INSERT INTO memory_contributions (id, tenant_id, memory_id, operation, actor_kind, source_kind, provider, model)
			VALUES (gen_random_uuid(), $1, $2, 'create', 'model', 'extraction', 'anthropic', 'claude-haiku-5-5')`, bob, mem)
	}
	g := getHubs(t, v, "")
	id := "author:model:anthropic/claude-haiku-5-5"
	_, _, _, weight, ok := g.node(id)
	require.True(t, ok)
	require.Equal(t, 2, weight)
}

func TestModelContributionForOutOfScopeMemoryIsIgnored(t *testing.T) {
	v := setup(t)
	bm2 := "00000000-0000-4000-8000-0000000002b2"
	exec(t, v, `INSERT INTO memories (id, tenant_id, content, created_at, updated_at) VALUES ($1, $2, 'bob two', now(), now())`, bm2, bob)
	for _, mem := range []string{bm, bm2} {
		exec(t, v, `INSERT INTO memory_contributions (id, tenant_id, memory_id, operation, actor_kind, source_kind, provider, model)
			VALUES (gen_random_uuid(), $1, $2, 'create', 'model', 'extraction', 'p', 'm')`, bob, mem)
	}
	require.NotContains(t, v.do(alice, http.MethodGet, "/v1/brain/graph", "").Body.String(), "author:model")
}

func TestUnknownAuthorIsSkipped(t *testing.T) {
	v := setup(t)
	ghost := "dead0000-0000-4000-8000-000000000001"
	exec(t, v, `ALTER TABLE memories DROP CONSTRAINT memories_created_by_user_id_fkey`)
	exec(t, v, `UPDATE memories SET created_by_user_id = $1 WHERE id = $2`, ghost, m1)
	exec(t, v, `UPDATE memories SET created_by_user_id = $1 WHERE id = $2`, ghost, m2)
	g := getHubs(t, v, "")
	for _, e := range g.Edges {
		require.NotEqual(t, "author", e.Kind)
	}
	for _, n := range g.Nodes {
		require.NotEqual(t, "author", n.Kind)
	}
}

func TestEntityHubCarriesRefs(t *testing.T) {
	v := setup(t)
	acmeA, acmeB, weak := "e0000000-0000-4000-8000-0000000000a1", "e0000000-0000-4000-8000-0000000000a2", "e0000000-0000-4000-8000-0000000000a3"
	exec(t, v, `INSERT INTO knowledge_nodes (id, tenant_id, kind, canonical_key, label, confidence) VALUES
		($1, $4, 'entity', 'acme', 'Acme', 0.7),
		($2, $5, 'entity', 'acme', 'ACME Corp', 0.9),
		($3, $4, 'entity', 'weak', 'Weak', 0.4)`, acmeA, acmeB, weak, alice, teamVault)
	exec(t, v, `INSERT INTO knowledge_nodes (id, tenant_id, kind, canonical_key, label, confidence) VALUES
		('e0000000-0000-4000-8000-0000000000a4', $1, 'entity', 'weak', 'Weak', 0.4)`, teamVault)
	exec(t, v, `INSERT INTO knowledge_evidence (id, tenant_id, node_id, memory_id, quote) VALUES
		(gen_random_uuid(), $1, $3, $5, 'q'),
		(gen_random_uuid(), $2, $4, $6, 'q'),
		(gen_random_uuid(), $1, $7, $5, 'q'),
		(gen_random_uuid(), $2, 'e0000000-0000-4000-8000-0000000000a4', $6, 'q')`, alice, teamVault, acmeA, acmeB, m1, m2, weak)
	g := getHubs(t, v, "")
	kind, label, vault, weight, ok := g.node("entity:acme")
	require.True(t, ok)
	require.Equal(t, "entity", kind)
	require.Equal(t, "ACME Corp", label, "label comes from the highest-confidence row")
	require.Nil(t, vault)
	require.Equal(t, 2, weight)
	require.Equal(t, []string{"entity:" + m1, "entity:" + m2}, g.edgesTo("entity:acme"))
	for _, n := range g.Nodes {
		if n.ID == "entity:acme" {
			require.Len(t, n.Refs, 2)
			got := map[string]string{}
			for _, r := range n.Refs {
				got[strings.ToLower(r.VaultID)] = strings.ToLower(r.ID)
			}
			require.Equal(t, map[string]string{alice.String(): acmeA, teamVault.String(): acmeB}, got)
		}
	}
	_, _, _, _, ok = g.node("entity:weak")
	require.False(t, ok)
}

func TestEntityEvidenceFromOtherVaultIsIgnored(t *testing.T) {
	v := setup(t)
	// Bob's entity node, evidenced against alice's memories under bob's tenant: out of scope.
	exec(t, v, `INSERT INTO knowledge_nodes (id, tenant_id, kind, canonical_key, label, confidence)
		VALUES ('e0000000-0000-4000-8000-0000000000b1', $1, 'entity', 'secret', 'Secret', 0.9)`, bob)
	for _, mem := range []string{m1, m2} {
		exec(t, v, `INSERT INTO knowledge_evidence (id, tenant_id, node_id, memory_id, quote)
			VALUES (gen_random_uuid(), $1, 'e0000000-0000-4000-8000-0000000000b1', $2, 'q')`, bob, mem)
	}
	require.NotContains(t, v.do(alice, http.MethodGet, "/v1/brain/graph", "").Body.String(), "Secret")
}

func TestLinksParamDropsHubKinds(t *testing.T) {
	v := setup(t)
	space := "5ace0000-0000-4000-8000-000000000001"
	exec(t, v, `INSERT INTO spaces (id, tenant_id, name, slug) VALUES ($1, $2, 'Research', 'research')`, space, alice)
	exec(t, v, `UPDATE vault_files SET space_id = $1, created_by_user_id = $2 WHERE id IN ($3, $4)`, space, alice, n1, n2)
	exec(t, v, `UPDATE memories SET tags = ARRAY['go'], space_id = $1, created_by_user_id = $2 WHERE id IN ($3, $4)`, space, alice, m1, m2)
	exec(t, v, `INSERT INTO knowledge_nodes (id, tenant_id, kind, canonical_key, label, confidence)
		VALUES ('e0000000-0000-4000-8000-0000000000c1', $1, 'entity', 'acme', 'Acme', 0.9)`, alice)
	exec(t, v, `INSERT INTO knowledge_evidence (id, tenant_id, node_id, memory_id, quote)
		VALUES (gen_random_uuid(), $1, 'e0000000-0000-4000-8000-0000000000c1', $2, 'q')`, alice, m1)
	all := getHubs(t, v, "")
	seen := map[string]bool{}
	for _, n := range all.Nodes {
		seen[n.Kind] = true
	}
	require.True(t, seen["tag"] && seen["space"] && seen["author"])

	g := getHubs(t, v, "?links=tag")
	for _, n := range g.Nodes {
		require.Contains(t, []string{"note", "memory", "tag"}, n.Kind)
	}
	require.NotEmpty(t, g.edgesTo("tag:go"))
	for _, e := range g.Edges {
		require.Equal(t, "tag", e.Kind)
	}
}
