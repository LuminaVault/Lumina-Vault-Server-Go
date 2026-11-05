package knowledge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"lumina/internal/modules/knowledge"
	"lumina/internal/ratelimit"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func fixture(prefix string, n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("%s-0000-4000-8000-%012X", prefix, n))
}

func node(n int) string { return ids.Upper(fixture("6B100000", n)) }
func edge(n int) string { return ids.Upper(fixture("6B200000", n)) }

var (
	kim       = fixture("6B000000", 0xA1)
	nia       = fixture("6B000000", 0xA2)
	rae       = fixture("6B000000", 0xA4)
	free      = fixture("6B000000", 0xA5)
	archived  = fixture("6B000000", 0xA6)
	viewer    = fixture("6B000000", 0xA7)
	editor    = fixture("6B000000", 0xA8)
	empty     = fixture("6B000000", 0xA9)
	teamVault = ids.Upper(fixture("6B0C0000", 0xC1))
	keys      = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

// One container per test binary would cross the rate-limit store over tests, so
// each test that writes or counts gets its own stack on the shared seed.
func setup(t *testing.T, enforcement bool) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "knowledge.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	gate := entitlement.Middleware(pool, entitlement.MemoryQuery, enforcement, nil)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{knowledge.New(pool, auth.Identify(keys, auth.PGUsers(pool)), gate, ratelimit.NewStore())},
	})
	return env{t: t, e: e, pool: pool}
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

func message(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out.Error.Message
}

type evidenceOut struct {
	ID                string  `json:"id"`
	MemoryID          string  `json:"memoryID"`
	SourceVaultFileID *string `json:"sourceVaultFileID"`
	Quote             string  `json:"quote"`
	StartOffset       *int    `json:"startOffset"`
	EndOffset         *int    `json:"endOffset"`
}

type nodeOut struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	Label      string        `json:"label"`
	Summary    *string       `json:"summary"`
	OccurredAt *string       `json:"occurredAt"`
	Confidence float64       `json:"confidence"`
	Evidence   []evidenceOut `json:"evidence"`
}

type edgeOut struct {
	ID              string        `json:"id"`
	From            string        `json:"from"`
	To              string        `json:"to"`
	Predicate       string        `json:"predicate"`
	State           string        `json:"state"`
	Confidence      float64       `json:"confidence"`
	Rationale       *string       `json:"rationale"`
	CounterEvidence *string       `json:"counterEvidence"`
	Evidence        []evidenceOut `json:"evidence"`
}

type graphOut struct {
	Nodes       []nodeOut `json:"nodes"`
	Edges       []edgeOut `json:"edges"`
	GeneratedAt string    `json:"generatedAt"`
}

func (v env) graph(user uuid.UUID, query string, headers ...string) graphOut {
	v.t.Helper()
	rec := v.do(user, http.MethodGet, "/v1/knowledge/graph"+query, "", headers...)
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var out graphOut
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(v.t, out.Nodes, "nodes is always an array")
	require.NotNil(v.t, out.Edges, "edges is always an array")
	return out
}

func nodeIDs(g graphOut) []string {
	out := make([]string, len(g.Nodes))
	for i, n := range g.Nodes {
		out[i] = n.ID
	}
	return out
}

func edgeIDs(g graphOut) []string {
	out := make([]string, len(g.Edges))
	for i, e := range g.Edges {
		out[i] = e.ID
	}
	return out
}

// Knowledge/KnowledgeVaultScopeIntegrationTests.swift: the graph reads the vault
// named by X-Vault-ID, and that vault's nodes stay out of the personal graph.
func TestKnowledgeGraphReadsSelectedSharedVaultPartition(t *testing.T) {
	v := setup(t, false)
	shared := v.graph(editor, "", "X-Vault-ID", teamVault)
	require.Contains(t, nodeIDs(shared), node(60))
	personal := v.graph(editor, "")
	require.NotContains(t, nodeIDs(personal), node(60))
}

func TestGraph(t *testing.T) {
	v := setup(t, false)

	t.Run("nodes by updated_at then id, edges by confidence then id", func(t *testing.T) {
		g := v.graph(kim, "")
		require.Equal(t, []string{node(1), node(2), node(3), node(4), node(5), node(6)}, nodeIDs(g))
		require.Equal(t, []string{edge(1), edge(2), edge(6), edge(3)}, edgeIDs(g), "dismissed and stale are off by default")
		require.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`, g.GeneratedAt)
	})
	t.Run("the DTO's fields, and absent keys rather than nulls", func(t *testing.T) {
		rec := v.do(kim, http.MethodGet, "/v1/knowledge/graph?limit=3", "")
		require.NotContains(t, rec.Body.String(), "null")
		g := v.graph(kim, "?limit=3")
		first := g.Nodes[0]
		require.Equal(t, "claim", first.Kind)
		require.Equal(t, "Atlas supports Beacon.", first.Label)
		require.Equal(t, "atlas summary", *first.Summary)
		require.Nil(t, first.OccurredAt)
		require.InDelta(t, 0.95, first.Confidence, 0)
		require.Len(t, first.Evidence, 2)
		require.Equal(t, "Beacon.", first.Evidence[0].Quote, "evidence is created_at ascending")
		require.NotNil(t, first.Evidence[0].SourceVaultFileID)
		require.Equal(t, 15, *first.Evidence[0].StartOffset)
		require.Nil(t, first.Evidence[0].EndOffset)
		require.Equal(t, "2026-02-01T10:00:00Z", *g.Nodes[2].OccurredAt, "fractions are dropped")
		require.Equal(t, []string{"tie one", "tie two"}, []string{g.Nodes[2].Evidence[0].Quote, g.Nodes[2].Evidence[1].Quote})
		require.Empty(t, g.Nodes[1].Evidence)
		require.NotNil(t, g.Nodes[1].Evidence)
		e := g.Edges[0]
		require.Equal(t, node(1), e.From)
		require.Equal(t, node(2), e.To)
		require.Equal(t, "supports", e.Predicate)
		require.Equal(t, "because", *e.Rationale)
		require.Len(t, e.Evidence, 1)
	})
	t.Run("an edge needs both ends among the returned nodes", func(t *testing.T) {
		g := v.graph(kim, "?limit=2")
		require.Equal(t, []string{edge(1)}, edgeIDs(g))
	})
	t.Run("limit clamps, and text falls back to the default", func(t *testing.T) {
		for query, want := range map[string]int{"?limit=1": 1, "?limit=0": 1, "?limit=-3": 1, "?limit=abc": 6, "?limit=99999": 6, "?limit=": 6} {
			require.Len(t, v.graph(kim, query).Nodes, want, query)
		}
	})
	t.Run("minimumConfidence clamps, and a NaN matches nothing", func(t *testing.T) {
		for query, want := range map[string]int{
			"?minimumConfidence=0.5": 4, "?minimumConfidence=2": 1, "?minimumConfidence=-1": 6, "?minimumConfidence=abc": 6,
			"?minimumConfidence=nan": 0, "?minimumConfidence=inf": 1, "?minimumConfidence=9e-1": 3, "?minimumConfidence=0x1p-1": 4,
		} {
			require.Len(t, v.graph(kim, query).Nodes, want, query)
		}
		require.Equal(t, []string{edge(1), edge(2), edge(6), edge(3)}, edgeIDs(v.graph(kim, "?minimumConfidence=0.1")))
		require.Equal(t, []string{edge(1), edge(2), edge(6)}, edgeIDs(v.graph(kim, "?minimumConfidence=0.6")), "the floor applies to edges too")
	})
	t.Run("kinds filter nodes and unknown values mean no filter", func(t *testing.T) {
		require.Equal(t, []string{node(1), node(5)}, nodeIDs(v.graph(kim, "?kinds=claim")))
		require.Equal(t, []string{node(1), node(3), node(5)}, nodeIDs(v.graph(kim, "?kinds=event,claim")))
		require.Len(t, v.graph(kim, "?kinds=bogus").Nodes, 6)
		require.Len(t, v.graph(kim, "?kinds=CLAIM").Nodes, 6, "raw values are exact")
		require.Equal(t, []string{node(2), node(4), node(6)}, nodeIDs(v.graph(kim, "?kinds=,,entity,")))
	})
	t.Run("predicates and states filter edges", func(t *testing.T) {
		require.Equal(t, []string{edge(1)}, edgeIDs(v.graph(kim, "?predicates=supports")))
		require.Equal(t, []string{edge(3)}, edgeIDs(v.graph(kim, "?predicates=related_to")))
		require.Len(t, v.graph(kim, "?predicates=relatedTo").Edges, 4, "the Swift case name is not the raw value")
		require.Equal(t, []string{edge(4)}, edgeIDs(v.graph(kim, "?states=dismissed")))
		require.Equal(t, []string{edge(3), edge(5)}, edgeIDs(v.graph(kim, "?states=stale,confirmed")))
		require.Len(t, v.graph(kim, "?states=BOGUS").Edges, 4)
	})
	t.Run("a fresh tenant has an empty graph", func(t *testing.T) {
		rec := v.do(empty, http.MethodGet, "/v1/knowledge/graph", "")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `"nodes":[]`)
		require.Contains(t, rec.Body.String(), `"edges":[]`)
	})
	t.Run("tenants don't see each other", func(t *testing.T) {
		require.NotContains(t, nodeIDs(v.graph(nia, "")), node(1))
	})
}

func TestVaultHeaderAndIdentityOrder(t *testing.T) {
	v := setup(t, false)
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/knowledge/graph", "").Code)
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/knowledge/graph", "", "Authorization", "Bearer not.a.jwt").Code)
	require.Equal(t, http.StatusBadRequest, v.do(uuid.Nil, http.MethodGet, "/v1/knowledge/graph", "", "X-Vault-ID", "nope").Code, "the header before identity")
	require.Equal(t, http.StatusBadRequest, v.do(uuid.Nil, http.MethodPost, "/v1/knowledge/connections/explain", "{").Code, "the body before identity")
	good := fmt.Sprintf(`{"fromNodeID":%q,"toNodeID":%q}`, node(11), node(12))
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, "/v1/knowledge/connections/explain", good).Code)
	r := v.do(uuid.Nil, http.MethodPost, "/v1/knowledge/edges/nope/confirm", "{")
	require.Equal(t, http.StatusBadRequest, r.Code)
	require.Equal(t, "invalid edge id", message(t, r), "the id before the body")
	require.Equal(t, http.StatusBadRequest, v.do(uuid.Nil, http.MethodPost, "/v1/knowledge/edges/"+edge(50)+"/confirm", "{").Code, "the body before identity")
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, "/v1/knowledge/edges/"+edge(50)+"/confirm", "{}").Code)
}

type explainOut struct {
	Explanation string `json:"explanation"`
	Paths       []struct {
		ID         string    `json:"id"`
		Nodes      []nodeOut `json:"nodes"`
		Edges      []edgeOut `json:"edges"`
		Confidence float64   `json:"confidence"`
	} `json:"paths"`
	Confidence float64  `json:"confidence"`
	Caveats    []string `json:"caveats"`
}

func (v env) explain(from, to int, maxDepth string) explainOut {
	v.t.Helper()
	extra := ""
	if maxDepth != "" {
		extra = `,"maxDepth":` + maxDepth
	}
	rec := v.do(nia, http.MethodPost, "/v1/knowledge/connections/explain", fmt.Sprintf(`{"fromNodeID":%q,"toNodeID":%q%s}`, node(from), node(to), extra))
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var out explainOut
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(v.t, out.Paths)
	require.NotNil(v.t, out.Caveats)
	return out
}

func TestExplain(t *testing.T) {
	v := setup(t, false)

	t.Run("the strongest path first, with a caveat for an inferred edge", func(t *testing.T) {
		out := v.explain(11, 13, "")
		require.Equal(t, "Node A connects to Node C because it supports Node B, then relates to Node C.", out.Explanation)
		require.Len(t, out.Paths, 2)
		require.InEpsilon(t, 0.72, out.Confidence, 1e-9)
		require.InDelta(t, 0.5, out.Paths[1].Confidence, 1e-9)
		require.Equal(t, []string{"This path includes a machine-inferred connection that has not been confirmed."}, out.Caveats)
		require.NotEqual(t, out.Paths[0].ID, out.Paths[1].ID, "path ids are new each time")
		require.Equal(t, strings.ToUpper(out.Paths[0].ID), out.Paths[0].ID)
	})
	t.Run("backwards reads the predicate from the other end", func(t *testing.T) {
		out := v.explain(13, 11, "")
		require.Equal(t, "Node C connects to Node A because it relates to Node B, then is supported by Node A.", out.Explanation)
	})
	t.Run("no caveat without an inferred edge", func(t *testing.T) {
		out := v.explain(14, 16, "")
		require.Empty(t, out.Caveats)
	})
	t.Run("dismissed and stale edges are not walked", func(t *testing.T) {
		out := v.explain(11, 16, "")
		require.Len(t, out.Paths, 1)
		require.Len(t, out.Paths[0].Edges, 4)
	})
	t.Run("depth is clamped, and the message names the clamped depth", func(t *testing.T) {
		out := v.explain(11, 16, "3")
		require.Empty(t, out.Paths)
		require.Equal(t, "No evidence-backed connection was found within 3 steps.", out.Explanation)
		require.Equal(t, []string{"The graph may still be processing recent memories."}, out.Caveats)
		require.Zero(t, out.Confidence)
		require.Contains(t, v.explain(11, 14, "0").Explanation, "within 1 steps.")
		require.Contains(t, v.explain(11, 14, "-9").Explanation, "within 1 steps.")
		require.Len(t, v.explain(11, 13, "1").Paths, 1)
		require.Len(t, v.explain(12, 16, "99").Paths, 1, "over the cap walks at most 4")
		require.Len(t, v.explain(11, 12, "2.0").Paths, 2, "a whole number decodes as Int: direct, and through Node C")
	})
	t.Run("every predicate, both ways", func(t *testing.T) {
		for i, want := range [][2]string{
			{"mentions", "is mentioned by"},
			{"is about", "is the subject of"},
			{"supports", "is supported by"},
			{"contradicts", "contradicts"},
			{"causes", "is caused by"},
			{"precedes", "follows"},
			{"relates to", "relates to"},
			{"is derived from", "is a source for"},
		} {
			forward := v.explain(20, 21+i, "")
			require.Contains(t, forward.Explanation, "because it "+want[0]+" ", i)
			backward := v.explain(21+i, 20, "")
			require.Contains(t, backward.Explanation, "because it "+want[1]+" Hub.", i)
		}
	})
	t.Run("the same node is a 400, whatever its case", func(t *testing.T) {
		r := v.do(nia, http.MethodPost, "/v1/knowledge/connections/explain", fmt.Sprintf(`{"fromNodeID":%q,"toNodeID":%q}`, node(11), strings.ToLower(node(11))))
		require.Equal(t, http.StatusBadRequest, r.Code)
		require.Equal(t, "choose two different nodes", message(t, r))
	})
	t.Run("an edge into another tenant's node can't be hydrated", func(t *testing.T) {
		r := v.do(nia, http.MethodPost, "/v1/knowledge/connections/explain", fmt.Sprintf(`{"fromNodeID":%q,"toNodeID":%q}`, node(17), node(40)))
		require.Equal(t, http.StatusInternalServerError, r.Code)
		require.Equal(t, "connection path could not be hydrated", message(t, r))
	})
	t.Run("another tenant's nodes have no path", func(t *testing.T) {
		require.Empty(t, v.explain(40, 41, "").Paths)
	})
	t.Run("malformed bodies say what Foundation says", func(t *testing.T) {
		for body, want := range map[string]string{
			"":                                  "The given data was not valid input: The given data was not valid JSON.",
			"{":                                 "The given data was not valid input: The given data was not valid JSON.",
			"{}":                                "Coding key `fromNodeID` not found.",
			`{"fromNodeID":1}`:                  "Type mismatch for `fromNodeID` key, expected `String` type.",
			`{"fromNodeID":"x","toNodeID":"y"}`: "The given data was not valid input: Attempted to decode UUID from invalid UUID string.",
		} {
			r := v.do(nia, http.MethodPost, "/v1/knowledge/connections/explain", body)
			require.Equal(t, http.StatusBadRequest, r.Code, body)
			require.Equal(t, want, message(t, r), body)
		}
	})
}

func (v env) edgeRow(id string) (state string, note *string, reviewed bool) {
	v.t.Helper()
	require.NoError(v.t, v.pool.QueryRow(context.Background(),
		`SELECT state, review_note, reviewed_at IS NOT NULL FROM knowledge_edges WHERE id = $1`, id).Scan(&state, &note, &reviewed))
	return state, note, reviewed
}

func TestReview(t *testing.T) {
	v := setup(t, false)
	review := func(user uuid.UUID, id, action, body string, headers ...string) *httptest.ResponseRecorder {
		t.Helper()
		return v.do(user, http.MethodPost, "/v1/knowledge/edges/"+id+"/"+action, body, headers...)
	}

	t.Run("confirm stores the note and returns the edge with its evidence", func(t *testing.T) {
		r := review(rae, edge(50), "confirm", `{"note":"looks right"}`)
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		var e edgeOut
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &e))
		require.Equal(t, "confirmed", e.State)
		require.Len(t, e.Evidence, 1)
		state, note, reviewed := v.edgeRow(edge(50))
		require.Equal(t, "confirmed", state)
		require.Equal(t, "looks right", *note)
		require.True(t, reviewed)
	})
	t.Run("only a suggested edge can be reviewed", func(t *testing.T) {
		for _, n := range []int{50, 53, 54, 55, 56} {
			r := review(rae, edge(n), "dismiss", `{}`)
			require.Equal(t, http.StatusNotFound, r.Code)
			require.Equal(t, "reviewable inference not found", message(t, r))
		}
	})
	t.Run("dismiss, with a null note", func(t *testing.T) {
		r := review(rae, edge(51), "dismiss", `{"note":null}`)
		require.Equal(t, http.StatusOK, r.Code)
		state, note, _ := v.edgeRow(edge(51))
		require.Equal(t, "dismissed", state)
		require.Nil(t, note)
	})
	t.Run("another tenant's edge is untouched", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, review(rae, edge(40), "confirm", `{}`).Code)
		state, _, reviewed := v.edgeRow(edge(40))
		require.Equal(t, "suggested", state)
		require.False(t, reviewed)
	})
	t.Run("a lowercase id works, a bare one doesn't", func(t *testing.T) {
		require.Equal(t, http.StatusOK, review(rae, strings.ToLower(edge(52)), "confirm", `{}`).Code)
		r := review(rae, strings.ReplaceAll(edge(57), "-", ""), "confirm", `{}`)
		require.Equal(t, http.StatusBadRequest, r.Code)
		require.Equal(t, "invalid edge id", message(t, r))
	})
	t.Run("a body is required", func(t *testing.T) {
		r := review(rae, edge(57), "confirm", "")
		require.Equal(t, http.StatusBadRequest, r.Code)
		r = review(rae, edge(57), "confirm", `{"note":5}`)
		require.Equal(t, "Type mismatch for `note` key, expected `String` type.", message(t, r))
		state, _, _ := v.edgeRow(edge(57))
		require.Equal(t, "suggested", state)
	})
	t.Run("a viewer can't, an editor can", func(t *testing.T) {
		require.Equal(t, http.StatusForbidden, review(viewer, edge(60), "confirm", `{}`, "X-Vault-ID", teamVault).Code)
		state, _, _ := v.edgeRow(edge(60))
		require.Equal(t, "suggested", state)
		require.Equal(t, http.StatusNotFound, review(editor, edge(60), "confirm", `{}`).Code, "not in the personal vault")
		require.Equal(t, http.StatusOK, review(editor, edge(60), "confirm", `{}`, "X-Vault-ID", teamVault).Code)
	})
}

func TestCaptureLimitScalesWithTier(t *testing.T) {
	v := setup(t, false)
	for i := range 20 {
		require.Equal(t, http.StatusOK, v.do(free, http.MethodGet, "/v1/knowledge/graph?limit=1", "").Code, i)
	}
	r := v.do(free, http.MethodGet, "/v1/knowledge/graph?limit=1", "")
	require.Equal(t, http.StatusTooManyRequests, r.Code)
	require.NotEmpty(t, r.Header().Get("Retry-After"))
	require.Equal(t, http.StatusOK, v.do(kim, http.MethodGet, "/v1/knowledge/graph?limit=1", "").Code, "buckets are per caller")
}

func TestPaywall(t *testing.T) {
	v := setup(t, true)
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/knowledge/graph", "", "X-Vault-ID", "nope").Code, "the gate asks for identity first")
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, "/v1/knowledge/connections/explain", "{").Code)
	require.Equal(t, http.StatusOK, v.do(free, http.MethodGet, "/v1/knowledge/graph", "").Code)
	for _, r := range []*httptest.ResponseRecorder{
		v.do(archived, http.MethodGet, "/v1/knowledge/graph", ""),
		v.do(archived, http.MethodPost, "/v1/knowledge/connections/explain", "{"),
		v.do(archived, http.MethodPost, "/v1/knowledge/edges/nope/confirm", "{"),
	} {
		require.Equal(t, http.StatusPaymentRequired, r.Code)
		require.JSONEq(t, `{"paywall":true,"paywallId":"default","requiredTier":"pro"}`, r.Body.String())
	}
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	v := setup(t, false)
	explain := fmt.Sprintf(`{"fromNodeID":%q,"toNodeID":%q}`, node(11), node(13))
	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
	}{
		{kim, http.MethodGet, "/v1/knowledge/graph", ""},
		{empty, http.MethodGet, "/v1/knowledge/graph", ""},
		{nia, http.MethodPost, "/v1/knowledge/connections/explain", explain},
		{nia, http.MethodPost, "/v1/knowledge/connections/explain", fmt.Sprintf(`{"fromNodeID":%q,"toNodeID":%q}`, node(11), node(18))},
		{rae, http.MethodPost, "/v1/knowledge/edges/" + edge(50) + "/confirm", `{"note":"n"}`},
		{rae, http.MethodPost, "/v1/knowledge/edges/" + edge(51) + "/dismiss", `{}`},
	} {
		r := v.do(c.user, c.method, c.path, c.body)
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			},
			Status: r.Code,
			Header: http.Header{"Content-Type": {r.Header().Get("Content-Type")}},
		}
		out.SetBodyBytes(r.Body.Bytes())
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.Body.String())
	}
}
