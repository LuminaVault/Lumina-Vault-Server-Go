package knowledge

// Ports Tests/AppTests/Knowledge/KnowledgeReasoningPathTests.swift

import (
	"lumina/internal/wire"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reasonNode(label string) KnowledgeNodeDTO {
	return KnowledgeNodeDTO{Id: wire.UUID{UUID: uuid.New()}, Kind: "claim", Label: label, Confidence: 1, Evidence: []KnowledgeEvidenceDTO{}}
}

func reasonEdge(from, to KnowledgeNodeDTO, state KnowledgeEdgeStateDTO, confidence float64) KnowledgeEdgeDTO {
	return KnowledgeEdgeDTO{
		Id: wire.UUID{UUID: uuid.New()}, From: from.Id, To: to.Id, Predicate: "related_to",
		State: state, Confidence: wire.Float64(confidence), Evidence: []KnowledgeEvidenceDTO{},
	}
}

func TestReasoningPathsAreCycleSafeRankedAndDepthBounded(t *testing.T) {
	a, b, c, d := reasonNode("A"), reasonNode("B"), reasonNode("C"), reasonNode("D")
	paths := ReasoningPaths(
		[]KnowledgeNodeDTO{a, b, c, d},
		[]KnowledgeEdgeDTO{
			reasonEdge(a, b, Asserted, 0.9), reasonEdge(b, c, Asserted, 0.8),
			reasonEdge(c, a, Asserted, 0.2), reasonEdge(c, d, Asserted, 0.95),
		},
		map[uuid.UUID]bool{a.Id.UUID: true, c.Id.UUID: true, d.Id.UUID: true}, 3, 5,
	)
	require.NotEmpty(t, paths)
	first := paths[0]
	ends := []uuid.UUID{c.Id.UUID, d.Id.UUID}
	assert.Contains(t, ends, first.Nodes[0].Id.UUID)
	assert.Contains(t, ends, first.Nodes[len(first.Nodes)-1].Id.UUID)
	assert.Len(t, first.Edges, 1)
	for _, path := range paths {
		assert.LessOrEqual(t, len(path.Edges), 3)
		seen := map[uuid.UUID]bool{}
		for _, n := range path.Nodes {
			assert.False(t, seen[n.Id.UUID], "a path never revisits a node")
			seen[n.Id.UUID] = true
		}
	}
}

func TestDismissedAndStaleEdgesNeverGroundReasoning(t *testing.T) {
	a, b := reasonNode("A"), reasonNode("B")
	seeds := map[uuid.UUID]bool{a.Id.UUID: true, b.Id.UUID: true}
	for _, state := range []KnowledgeEdgeStateDTO{Dismissed, Stale} {
		paths := ReasoningPaths([]KnowledgeNodeDTO{a, b}, []KnowledgeEdgeDTO{reasonEdge(a, b, state, 1)}, seeds, 4, 5)
		assert.Empty(t, paths, state)
	}
}

func TestNaturalLanguageQueriesBecomeSafePrefixORExpressions(t *testing.T) {
	assert.Equal(t, "atlas:* | connected:* | vendor:* | risk:*", SearchExpression("How is Atlas connected to vendor-risk?"))
	assert.Equal(t, "atlas:*", SearchExpression("Atlas atlas"))
	assert.Equal(t, "ai:*", SearchExpression("AI"))
	assert.Empty(t, SearchExpression("?!"))
}
