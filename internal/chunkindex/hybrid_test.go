package chunkindex

import (
	"lumina/internal/memorystore"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HybridMemorySearchTests. A memory found by both arms must appear once, as the chunk
// hit, because only that hit carries a line range a user can check. The prompt tests in
// the same Swift file (QueryPromptCitationTests) go with QueryController in group 4F;
// the citation trail ones are in memorystore.

func chunkHit(id uuid.UUID, path string, startLine int) memorystore.SearchResult {
	return memorystore.SearchResult{
		ID: id, TenantID: uuid.New(), Content: "chunk text", Distance: 0.2,
		Citation: &memorystore.Citation{
			ChunkID: ChunkPrefix + strings.Repeat("a", 32), DocumentID: DocumentPrefix + strings.Repeat("b", 32),
			Path: &path, HeadingPath: []string{"Routing"}, StartLine: startLine, EndLine: startLine + 4,
		},
	}
}

func documentHit(id uuid.UUID) memorystore.SearchResult {
	return memorystore.SearchResult{ID: id, TenantID: uuid.New(), Content: "whole document text", Distance: 0.3}
}

func TestAMemoryFoundByBothArmsAppearsOnceAsTheChunkHit(t *testing.T) {
	shared := uuid.New()
	merged := MergeHits([]memorystore.SearchResult{chunkHit(shared, "projects/hermes.md", 1)}, []memorystore.SearchResult{documentHit(shared)}, 10)
	require.Len(t, merged, 1)
	assert.Equal(t, "projects/hermes.md", *merged[0].Citation.Path)
}

func TestDocumentHitsFillRemainingSlots(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	merged := MergeHits([]memorystore.SearchResult{chunkHit(a, "a.md", 1)}, []memorystore.SearchResult{documentHit(b)}, 10)
	require.Len(t, merged, 2)
	assert.Equal(t, a, merged[0].ID, "chunk hits keep their fused order and come first")
	assert.Nil(t, merged[1].Citation)
}

func TestLimitIsRespectedAcrossBothArms(t *testing.T) {
	var chunks, documents []memorystore.SearchResult
	for range 5 {
		chunks = append(chunks, chunkHit(uuid.New(), "c.md", 1))
		documents = append(documents, documentHit(uuid.New()))
	}
	merged := MergeHits(chunks, documents, 3)
	require.Len(t, merged, 3)
	for _, hit := range merged {
		assert.NotNil(t, hit.Citation, "chunk hits fill the budget first")
	}
}

func TestAnEmptyChunkArmDegradesToDocumentOnlyBehavior(t *testing.T) {
	documents := []memorystore.SearchResult{documentHit(uuid.New()), documentHit(uuid.New()), documentHit(uuid.New())}
	merged := MergeHits(nil, documents, 5)
	require.Len(t, merged, 3)
	for _, hit := range merged {
		assert.Nil(t, hit.Citation)
	}
}

func TestDuplicatesWithinOneArmAreCollapsed(t *testing.T) {
	id := uuid.New()
	merged := MergeHits([]memorystore.SearchResult{chunkHit(id, "a.md", 1), chunkHit(id, "a.md", 40)}, nil, 10)
	assert.Len(t, merged, 1, "one memory is one source, however many of its chunks matched")
}
