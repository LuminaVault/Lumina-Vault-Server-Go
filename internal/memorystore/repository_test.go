package memorystore

import (
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MemoryCitationTrailTests.

func TestTrailJoinsPathHeadingsAndLineRange(t *testing.T) {
	path := "projects/hermes.md"
	c := Citation{ChunkID: "c", DocumentID: "d", Path: &path, HeadingPath: []string{"Routing", "Fallbacks"}, StartLine: 40, EndLine: 58}
	assert.Equal(t, "projects/hermes.md › Routing › Fallbacks (L40-58)", c.DisplayTrail())
}

func TestASingleLineChunkReadsAsOneLine(t *testing.T) {
	path := "a.md"
	c := Citation{ChunkID: "c", DocumentID: "d", Path: &path, StartLine: 7, EndLine: 7}
	assert.Equal(t, "a.md (L7)", c.DisplayTrail())
}

func TestAChunkWithNoSourceFileStillCitesItsLines(t *testing.T) {
	c := Citation{ChunkID: "c", DocumentID: "d", StartLine: 1, EndLine: 3}
	assert.Equal(t, "L1-3", c.DisplayTrail())
}

func TestParseSourceKindReadsUnknownAsLegacy(t *testing.T) {
	assert.Equal(t, SourceKnowledgeCompile, ParseSourceKind("knowledge_compile"))
	assert.Equal(t, SourceLegacy, ParseSourceKind("from-the-future"))
}

func TestParseVector(t *testing.T) {
	assert.Equal(t, []float32{0.1, -2, 3e-05}, ParseVector("[0.1,-2,3e-05]"))
	assert.Equal(t, []float32{1, 2}, ParseVector(" [ 1 , x, 2 ] "), "compactMap drops what does not parse")
	assert.Empty(t, ParseVector("[]"))
}

func row(kind string, distance float32) searchRow {
	return searchRow{id: uuid.New(), originKind: kind, distance: distance}
}

func kindsOf(rows []searchRow) string {
	out := ""
	for _, r := range rows {
		if r.originKind == SourceChat || r.originKind == SourceQuery {
			out += "o"
		} else {
			out += "f"
		}
	}
	return out
}

// The 40% model-output quota: int(limit*0.4), at least one.
func TestBalancedHoldsTheOutputQuotaWhenFactsSuffice(t *testing.T) {
	rows := []searchRow{
		row(SourceChat, 0.1), row(SourceQuery, 0.2), row(SourceChat, 0.3), row(SourceChat, 0.35),
		row(SourceManual, 0.4), row(SourceVault, 0.5), row(SourceLegacy, 0.6), row(SourceLink, 0.7),
	}
	selected := balanced(rows, 5)
	assert.Equal(t, "oofff", kindsOf(selected), "2 outputs, 3 facts, sorted by distance")
	assert.InDelta(t, 0.1, selected[0].distance, 1e-6)
}

func TestBalancedBackfillsUnusedSlotsFromOutputs(t *testing.T) {
	rows := []searchRow{row(SourceChat, 0.1), row(SourceChat, 0.2), row(SourceChat, 0.3), row(SourceManual, 0.4)}
	assert.Equal(t, "ooof", kindsOf(balanced(rows, 5)))
}

func TestBalancedLimitOneTakesTheNearestWhateverItIs(t *testing.T) {
	// facts quota is 0, output quota 1: an output wins even behind a nearer fact.
	assert.Equal(t, "o", kindsOf(balanced([]searchRow{row(SourceManual, 0.1), row(SourceChat, 0.2)}, 1)))
	assert.Equal(t, "f", kindsOf(balanced([]searchRow{row(SourceManual, 0.1)}, 1)))
}

// Every lexical hit has distance 1.0: facts stay ahead of outputs, in rank order.
func TestBalancedKeepsOrderOnEqualDistance(t *testing.T) {
	rows := []searchRow{row(SourceChat, 1), row(SourceManual, 1), row(SourceQuery, 1), row(SourceVault, 1)}
	selected := balanced(rows, 4)
	assert.Equal(t, []uuid.UUID{rows[1].id, rows[3].id, rows[0].id, rows[2].id}, []uuid.UUID{selected[0].id, selected[1].id, selected[2].id, selected[3].id})
}

func TestBalancedEmpty(t *testing.T) {
	assert.Empty(t, balanced(nil, 0))
	assert.Empty(t, balanced(nil, 5))
}

func TestBalancedQuotaByLimit(t *testing.T) {
	var rows []searchRow
	for i := range 30 {
		rows = append(rows, row(SourceChat, float32(i)))
		rows = append(rows, row(SourceManual, float32(i)+0.5))
	}
	for limit, outputs := range map[int]int{1: 1, 2: 1, 4: 1, 5: 2, 10: 4, 20: 8} {
		selected := balanced(rows, limit)
		require.Len(t, selected, limit, strconv.Itoa(limit))
		count := 0
		for _, r := range selected {
			if r.originKind == SourceChat {
				count++
			}
		}
		assert.Equal(t, outputs, count, "limit %d", limit)
	}
}
