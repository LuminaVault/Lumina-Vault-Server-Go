package brain

import (
	"lumina/internal/ids"
	"lumina/internal/modules/brain/db"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	idA = uuid.MustParse("00000000-0000-4000-8000-00000000000a")
	idB = uuid.MustParse("00000000-0000-4000-8000-00000000000b")
	idC = uuid.MustParse("00000000-0000-4000-8000-00000000000c")
	vlt = uuid.MustParse("00000000-0000-4000-8000-0000000000f1")
)

func note(id uuid.UUID) item { return item{ID: id, VaultID: vlt, Kind: "note", Label: id.String()} }
func mem(id uuid.UUID) item  { return item{ID: id, VaultID: vlt, Kind: "memory", Label: id.String()} }

func kinds(es []BrainGraphEdge) []string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.Kind)+":"+e.From+">"+e.To)
	}
	return out
}

func TestMemoryLabel(t *testing.T) {
	require.Equal(t, "Untitled", memoryLabel(""))
	require.Equal(t, "Untitled", memoryLabel("\n \n"))
	require.Equal(t, "# Title", memoryLabel("\n# Title\nbody"))
	require.Len(t, memoryLabel(strings.Repeat("x", 200)), 80)
	got := memoryLabel(strings.Repeat("日", 200))
	require.True(t, utf8.ValidString(got))
	require.Equal(t, 80, utf8.RuneCountInString(got))
}

func TestNoteLabel(t *testing.T) {
	require.Equal(t, "My Note", noteLabel("folder/My Note.md"))
	require.Equal(t, "README", noteLabel("README"))
	require.Equal(t, "日本語", noteLabel("a/日本語.md"))
}

func TestAssembleDropsDanglingAndSelfEdges(t *testing.T) {
	_, edges := assemble([]item{note(idA), note(idB)}, nil, []relation{
		{From: nodeID(idA), To: nodeID(idB), Kind: "link"},
		{From: nodeID(idA), To: nodeID(idC), Kind: "link"},
		{From: nodeID(idA), To: nodeID(idA), Kind: "link"},
	})
	require.Equal(t, []string{"link:" + nodeID(idA) + ">" + nodeID(idB)}, kinds(edges))
}

func TestAssembleDedupesUndirectedPairs(t *testing.T) {
	_, edges := assemble([]item{note(idA), note(idB)}, nil, []relation{
		{From: nodeID(idA), To: nodeID(idB), Kind: "link"},
		{From: nodeID(idB), To: nodeID(idA), Kind: "link"},
		{From: nodeID(idA), To: nodeID(idB), Kind: "link"},
	})
	require.Len(t, edges, 1)
}

func TestAssembleDropsSmallHubs(t *testing.T) {
	nodes, edges := assemble([]item{note(idA), note(idB)},
		[]hub{
			{ID: "tag:solo", Kind: "tag", Label: "solo"},
			{ID: "tag:pair", Kind: "tag", Label: "pair"},
		},
		[]relation{
			{From: nodeID(idA), To: "tag:solo", Kind: "tag"},
			{From: nodeID(idA), To: "tag:pair", Kind: "tag"},
			{From: nodeID(idB), To: "tag:pair", Kind: "tag"},
		})
	for _, n := range nodes {
		require.NotEqual(t, "tag:solo", n.Id)
	}
	require.Len(t, nodes, 3)
	require.Equal(t, []string{"tag:" + nodeID(idA) + ">tag:pair", "tag:" + nodeID(idB) + ">tag:pair"}, kinds(edges))
}

func TestAssembleSortsAndWeights(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := mem(idC)
	m.CreatedAt = &ts
	m.Pos = &[3]float64{1, 2, 3}
	sp := idB
	n := note(idB)
	n.SpaceID = &sp
	nodes, edges := assemble([]item{m, n, note(idA)}, nil, []relation{
		{From: nodeID(idC), To: nodeID(idB), Kind: "source"},
		{From: nodeID(idB), To: nodeID(idA), Kind: "link"},
	})
	require.Len(t, nodes, 3)
	// (kind, id): memory < note
	require.Equal(t, nodeID(idC), nodes[0].Id)
	require.Equal(t, nodeID(idA), nodes[1].Id)
	require.Equal(t, nodeID(idB), nodes[2].Id)
	require.Equal(t, []int{1, 1, 2}, []int{nodes[0].Weight, nodes[1].Weight, nodes[2].Weight})
	require.NotNil(t, nodes[0].Pos)
	require.NotNil(t, nodes[0].CreatedAt)
	require.NotNil(t, nodes[2].SpaceID)
	require.Equal(t, []string{"link:" + nodeID(idB) + ">" + nodeID(idA), "source:" + nodeID(idC) + ">" + nodeID(idB)}, kinds(edges))
	// An item with no edges has weight 0.
	nodes, _ = assemble([]item{note(idA)}, nil, nil)
	require.Equal(t, 0, nodes[0].Weight)
}

func TestParseLinks(t *testing.T) {
	defaults := map[string]bool{"link": true, "source": true, "tag": true, "author": true, "entity": true, "space": true}
	require.Equal(t, defaults, parseLinks(""))
	require.Equal(t, defaults, parseLinks("bogus"))
	require.Equal(t, map[string]bool{"tag": true, "similar": true}, parseLinks("tag,similar,bogus"))
}

func TestNormaliseTag(t *testing.T) {
	require.Equal(t, "go", normaliseTag(" Go "))
	require.Equal(t, "", normaliseTag("  "))
	require.Equal(t, "ünï", normaliseTag("ÜNÏ"))
}

func TestTagHubsNormaliseAndSkipEmpty(t *testing.T) {
	a, b := mem(idA), mem(idB)
	a.Tags = []string{" Go ", "go", "  "}
	b.Tags = []string{"GO"}
	hubs, rels := tagHubs([]item{a, b, note(idC)})
	require.Len(t, hubs, 1)
	require.Equal(t, hub{ID: "tag:go", Kind: "tag", Label: "go"}, hubs[0])
	require.Len(t, rels, 3, "duplicates collapse later in assemble")
	nodes, edges := assemble([]item{a, b}, hubs, rels)
	require.Len(t, nodes, 3)
	require.Len(t, edges, 2)
}

func TestModelAuthorHubIDWithNullProvider(t *testing.T) {
	prov := "anthropic"
	hubs, rels := modelAuthorHubs([]db.ModelAuthorsRow{
		{MemoryID: idA, Provider: nil, Model: "m1"},
		{MemoryID: idB, Provider: &prov, Model: "m1"},
	})
	ids := []string{}
	for _, h := range hubs {
		ids = append(ids, h.ID)
	}
	require.ElementsMatch(t, []string{"author:model:/m1", "author:model:anthropic/m1"}, ids)
	require.Len(t, rels, 2)
}

func TestEntityHubsPickBestLabelAndSortRefs(t *testing.T) {
	v1 := uuid.MustParse("00000000-0000-4000-8000-0000000000f1")
	v2 := uuid.MustParse("00000000-0000-4000-8000-0000000000f2")
	n1 := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	n2 := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	n3 := uuid.MustParse("00000000-0000-4000-8000-000000000003")
	hubs, rels := entityHubs([]db.EntitiesRow{
		{ID: n2, TenantID: v2, CanonicalKey: "acme", Label: "Beta", Confidence: 0.8, MemoryID: idA},
		{ID: n1, TenantID: v1, CanonicalKey: "acme", Label: "Alpha low", Confidence: 0.6, MemoryID: idB},
		{ID: n3, TenantID: v1, CanonicalKey: "acme", Label: "Alpha high", Confidence: 0.8, MemoryID: idB},
		{ID: n3, TenantID: v1, CanonicalKey: "acme", Label: "Alpha high", Confidence: 0.8, MemoryID: idC},
	})
	require.Len(t, hubs, 1)
	require.Equal(t, "entity:acme", hubs[0].ID)
	require.Equal(t, "Beta", hubs[0].Label, "0.8 ties across vaults; the smallest id (…02) wins")
	require.Equal(t, []BrainGraphRef{
		{VaultID: wire.UUID{UUID: v1}, Id: wire.UUID{UUID: n3}},
		{VaultID: wire.UUID{UUID: v2}, Id: wire.UUID{UUID: n2}},
	}, hubs[0].Refs)
	require.Len(t, rels, 4)
}

var idD = uuid.MustParse("00000000-0000-4000-8000-00000000000d")

// chain is A-B-C-D, with the middle edge written backwards to show edges are undirected.
func chain() ([]BrainGraphNode, []BrainGraphEdge) {
	nodes, edges := assemble([]item{note(idA), note(idB), note(idC), note(idD)}, nil, []relation{
		{From: nodeID(idA), To: nodeID(idB), Kind: "link"},
		{From: nodeID(idC), To: nodeID(idB), Kind: "link"},
		{From: nodeID(idC), To: nodeID(idD), Kind: "link"},
	})
	return nodes, edges
}

func idsOf(nodes []BrainGraphNode) (out []string) {
	for _, n := range nodes {
		out = append(out, n.Id)
	}
	return out
}

func TestNeighbourhood(t *testing.T) {
	nodes, edges := chain()
	a, b, c, d := nodeID(idA), nodeID(idB), nodeID(idC), nodeID(idD)

	got, es, ok := neighbourhood(nodes, edges, a, 1)
	require.True(t, ok)
	require.Equal(t, []string{a, b}, idsOf(got))
	require.Len(t, es, 1)
	require.Equal(t, []int{1, 1}, []int{got[0].Weight, got[1].Weight})

	got, es, _ = neighbourhood(nodes, edges, a, 2)
	require.Equal(t, []string{a, b, c}, idsOf(got))
	require.Len(t, es, 2)
	require.Equal(t, []int{1, 2, 1}, []int{got[0].Weight, got[1].Weight, got[2].Weight})

	got, _, _ = neighbourhood(nodes, edges, c, 1)
	require.Equal(t, []string{b, c, d}, idsOf(got), "walks edges against their direction")

	got, es, ok = neighbourhood(nodes, edges, "nope", 1)
	require.False(t, ok)
	require.Empty(t, got)
	require.Empty(t, es)
}

func TestNeighbourhoodKeepsSingleMemberHubAndNonNilEdges(t *testing.T) {
	nodes, edges := assemble([]item{note(idA), note(idB)}, []hub{{ID: "tag:x", Kind: "tag", Label: "x"}}, []relation{
		{From: nodeID(idA), To: "tag:x", Kind: "tag"},
		{From: nodeID(idB), To: "tag:x", Kind: "tag"},
	})
	got, es, ok := neighbourhood(nodes, edges, nodeID(idA), 1)
	require.True(t, ok)
	require.Contains(t, idsOf(got), "tag:x")
	require.Len(t, es, 1)
	// Depth 1 from an isolated-after-focus node still yields an empty, non-nil edge slice.
	_, es, _ = neighbourhood([]BrainGraphNode{{Id: "z", Kind: "note"}}, nil, "z", 1)
	require.NotNil(t, es)
}

func TestETagIgnoresGeneratedAt(t *testing.T) {
	nodes, edges := chain()
	resp := BrainGraphResponse{GeneratedAt: wire.Time{Time: time.Unix(100, 0)}, Nodes: nodes, Edges: edges, Vaults: []BrainGraphVault{}}
	later := resp
	later.GeneratedAt = wire.Time{Time: time.Unix(999, 0)}
	require.Equal(t, etag(resp), etag(later))
	require.Regexp(t, `^"[0-9a-f]{64}"$`, etag(resp))

	more := resp
	more.Nodes = append(append([]BrainGraphNode{}, nodes...), BrainGraphNode{Id: "extra", Kind: "note"})
	require.NotEqual(t, etag(resp), etag(more))
}

func TestFocusIgnoresDefaultLimit(t *testing.T) {
	parse := func(query string) params {
		req := httptest.NewRequest(http.MethodGet, "/v1/brain/graph"+query, nil)
		c := echo.New().NewContext(req, httptest.NewRecorder())
		p, err := parseParams(c)
		require.NoError(t, err)
		return p
	}
	require.EqualValues(t, 400, parse("").limit)
	require.EqualValues(t, 2000, parse("?focus=x").limit)
	require.EqualValues(t, 50, parse("?focus=x&limit=50").limit)
	require.EqualValues(t, 2000, parse("?focus=x&limit=99999").limit)
	require.Equal(t, "x", parse("?focus=x").focus)
	require.Empty(t, parse("").focus)
	require.Equal(t, 1, parse("?focus=x").depth)
	require.Equal(t, 3, parse("?focus=x&depth=9").depth)
	require.Equal(t, 1, parse("?focus=x&depth=0").depth)
	require.Equal(t, 1, parse("?focus=x&depth=abc").depth)

	for _, q := range []string{"?focus=", "?focus=" + strings.Repeat("x", 301)} {
		req := httptest.NewRequest(http.MethodGet, "/v1/brain/graph"+q, nil)
		_, err := parseParams(echo.New().NewContext(req, httptest.NewRecorder()))
		require.Equal(t, errInvalidFocus, err)
	}
}

func TestNormaliseFocus(t *testing.T) {
	up := "00000000-0000-4000-8000-0000000002A1"
	low := strings.ToLower(up)
	for in, want := range map[string]string{
		low:                     up,
		up:                      up,
		"space:" + low:          "space:" + up,
		"author:user:" + low:    "author:user:" + up,
		"author:user:" + up:     "author:user:" + up,
		"tag:Go":                "tag:Go",
		"entity:" + low:         "entity:" + low,
		"author:model:p/" + low: "author:model:p/" + low,
		"SPACE:" + low:          "SPACE:" + low,
		"space:not-a-uuid":      "space:not-a-uuid",
		"{" + low + "}":         "{" + low + "}",
	} {
		require.Equal(t, want, normaliseFocus(in), in)
	}
}

func TestNodeIDsAreUppercase(t *testing.T) {
	sp := idB
	n := note(idA)
	n.SpaceID = &sp
	n.CreatedBy = &idC
	n.Tags = []string{"Go"}
	m := mem(idD)
	m.SpaceID = &sp
	m.CreatedBy = &idC
	m.Tags = []string{"go"}
	items := []item{n, m}
	var hubs []hub
	var rels []relation
	for _, f := range []func() ([]hub, []relation){
		func() ([]hub, []relation) { return tagHubs(items) },
		func() ([]hub, []relation) {
			return spaceHubs(items, []db.SpacesRow{{ID: idB, TenantID: vlt, Name: "S"}})
		},
		func() ([]hub, []relation) { return userAuthorHubs(items, []db.AuthorsRow{{ID: idC, Username: "c"}}) },
	} {
		h, r := f()
		hubs = append(hubs, h...)
		rels = append(rels, r...)
	}
	nodes, edges := assemble(items, hubs, rels)
	require.ElementsMatch(t, []string{
		"00000000-0000-4000-8000-00000000000A",
		"00000000-0000-4000-8000-00000000000D",
		"tag:go",
		"space:00000000-0000-4000-8000-00000000000B",
		"author:user:00000000-0000-4000-8000-00000000000C",
	}, idsOf(nodes))
	require.Len(t, edges, 6)
	for _, nd := range nodes {
		if nd.SpaceID != nil {
			require.Equal(t, "space:"+ids.Upper(nd.SpaceID.UUID), "space:00000000-0000-4000-8000-00000000000B")
		}
	}
}

func TestFocusLengthCountsRunes(t *testing.T) {
	parse := func(focus string) error {
		req := httptest.NewRequest(http.MethodGet, "/v1/brain/graph?focus="+url.QueryEscape(focus), nil)
		_, err := parseParams(echo.New().NewContext(req, httptest.NewRecorder()))
		return err
	}
	require.NoError(t, parse(strings.Repeat("日", 300)), "300 runes is 900 bytes and still allowed")
	require.Equal(t, errInvalidFocus, parse(strings.Repeat("日", 301)))
}

func TestCapSimilarKeepsTheNewest400(t *testing.T) {
	require.Equal(t, 400, maxSimilarMemories)
	in := make([]uuid.UUID, 0, 450)
	for range 450 {
		in = append(in, uuid.New())
	}
	require.Equal(t, in[:400], capSimilar(in), "rows arrive newest first; the first 400 are kept in order")
	require.Equal(t, in[:400], capSimilar(in[:400]))
	require.Equal(t, in[:7], capSimilar(in[:7]))
	require.Empty(t, capSimilar(nil))
}
