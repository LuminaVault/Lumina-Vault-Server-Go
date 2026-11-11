package brain

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
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
		{From: idA.String(), To: idB.String(), Kind: "link"},
		{From: idA.String(), To: idC.String(), Kind: "link"},
		{From: idA.String(), To: idA.String(), Kind: "link"},
	})
	require.Equal(t, []string{"link:" + idA.String() + ">" + idB.String()}, kinds(edges))
}

func TestAssembleDedupesUndirectedPairs(t *testing.T) {
	_, edges := assemble([]item{note(idA), note(idB)}, nil, []relation{
		{From: idA.String(), To: idB.String(), Kind: "link"},
		{From: idB.String(), To: idA.String(), Kind: "link"},
		{From: idA.String(), To: idB.String(), Kind: "link"},
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
			{From: idA.String(), To: "tag:solo", Kind: "tag"},
			{From: idA.String(), To: "tag:pair", Kind: "tag"},
			{From: idB.String(), To: "tag:pair", Kind: "tag"},
		})
	for _, n := range nodes {
		require.NotEqual(t, "tag:solo", n.Id)
	}
	require.Len(t, nodes, 3)
	require.Equal(t, []string{"tag:" + idA.String() + ">tag:pair", "tag:" + idB.String() + ">tag:pair"}, kinds(edges))
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
		{From: idC.String(), To: idB.String(), Kind: "source"},
		{From: idB.String(), To: idA.String(), Kind: "link"},
	})
	require.Len(t, nodes, 3)
	// (kind, id): memory < note
	require.Equal(t, idC.String(), nodes[0].Id)
	require.Equal(t, idA.String(), nodes[1].Id)
	require.Equal(t, idB.String(), nodes[2].Id)
	require.Equal(t, []int{1, 1, 2}, []int{nodes[0].Weight, nodes[1].Weight, nodes[2].Weight})
	require.NotNil(t, nodes[0].Pos)
	require.NotNil(t, nodes[0].CreatedAt)
	require.NotNil(t, nodes[2].SpaceID)
	require.Equal(t, []string{"link:" + idB.String() + ">" + idA.String(), "source:" + idC.String() + ">" + idB.String()}, kinds(edges))
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
