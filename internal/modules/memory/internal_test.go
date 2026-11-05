package memory

import (
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// LocalMemorySyncCursorTests.

func TestOpaqueCursorRoundTripsEveryStableOrderingField(t *testing.T) {
	cursor := syncCursor{
		timestamp: 1_726_000_000.125 - 978307200,
		id:        uuid.MustParse("aabbccdd-0000-4000-8000-000000000001"),
		kind:      kindDeletion,
	}
	encoded, err := cursor.encode()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(encoded, "v1."))
	decoded, err := decodeCursor(encoded)
	require.NoError(t, err)
	assert.Equal(t, cursor, *decoded)
}

func TestLegacyTimestampCursorRemainsAccepted(t *testing.T) {
	decoded, err := decodeCursor("2026-07-13T12:34:56Z")
	require.NoError(t, err)
	require.NotNil(t, decoded)
	want := time.Date(2026, 7, 13, 12, 34, 56, 0, time.UTC)
	assert.InDelta(t, float64(want.Unix()-978307200), decoded.timestamp, 0)
	assert.Equal(t, kindMemory, decoded.kind)
	assert.Equal(t, uuid.Nil, decoded.id)
}

func TestMalformedOpaqueCursorIsRejected(t *testing.T) {
	for _, raw := range []string{"v1.not-base64", "garbage", "v1.", "v1.e30", "2026-07-13T12:34:56.5Z", "v1.e30\n"} {
		_, err := decodeCursor(raw)
		assert.ErrorIs(t, err, errBadCursor, raw)
	}
	decoded, err := decodeCursor("")
	require.NoError(t, err)
	assert.Nil(t, decoded)
}

func TestCursorTimeRoundTripTruncatesLikePostgresNIO(t *testing.T) {
	at := time.Date(2026, 1, 10, 10, 0, 0, 123456000, time.UTC)
	back := bindTime(referenceSeconds(at))
	assert.LessOrEqual(t, at.Sub(back), time.Microsecond)
	assert.False(t, back.After(at))
}

// MemorySavedIDTests. Go has no optional id to unwrap, so savedID is the field.

func TestDTOBuildsFromAHydratedMemory(t *testing.T) {
	id := uuid.New()
	dto := toDTO(&memorystore.Memory{ID: id, Content: "hello", Tags: []string{"tag1", "tag2"}}, nil)
	assert.Equal(t, id, dto.ID.UUID)
	assert.Equal(t, "hello", dto.Content)
	assert.Equal(t, []string{"tag1", "tag2"}, dto.Tags)
}

func TestDTONormalizesNilTagsToEmptyArray(t *testing.T) {
	dto := toDTO(&memorystore.Memory{ID: uuid.New(), Content: "x"}, nil)
	assert.NotNil(t, dto.Tags)
	assert.Empty(t, dto.Tags)
}

func TestDTOPreservesCreatedAt(t *testing.T) {
	now := time.Now()
	dto := toDTO(&memorystore.Memory{ID: uuid.New(), Content: "x", CreatedAt: &now}, nil)
	require.NotNil(t, dto.CreatedAt)
	assert.True(t, dto.CreatedAt.Equal(now))
}

func TestDTOKeepsOnlyKnownKinds(t *testing.T) {
	bad, good := "hunch", "fact"
	assert.Nil(t, toDTO(&memorystore.Memory{Kind: &bad}, nil).Kind)
	assert.Equal(t, &good, toDTO(&memorystore.Memory{Kind: &good}, nil).Kind)
}

// SuggestionsControllerTests.

func TestSuggestionDefaultsAreNonEmptyAndNonBlank(t *testing.T) {
	assert.NotEmpty(t, suggestions)
	for _, s := range suggestions {
		assert.NotEmpty(t, swiftstr.TrimWhitespacesAndNewlines(s))
	}
}

// ModelDisclosurePolicy.

func TestScrubKeepsOnlyAgentAttribution(t *testing.T) {
	hidden := MemoryContributionDTO{Model: &ModelProvenanceDTO{Provider: "openrouter", Model: "m"}}
	scrubContribution(&hidden)
	assert.Nil(t, hidden.Model)
	for _, provider := range []string{"mcp", "mcp:codex"} {
		kept := MemoryContributionDTO{Model: &ModelProvenanceDTO{Provider: provider, Model: "m"}}
		scrubContribution(&kept)
		assert.NotNil(t, kept.Model, provider)
	}
	lookalike := MemoryContributionDTO{Model: &ModelProvenanceDTO{Provider: "mcpx", Model: "m"}}
	scrubContribution(&lookalike)
	assert.Nil(t, lookalike.Model)
	summary := MemoryProvenanceSummaryDTO{Contributors: []ModelProvenanceDTO{{Provider: "mcp"}}}
	scrubSummary(&summary)
	assert.NotNil(t, summary.Contributors)
	assert.Empty(t, summary.Contributors)
}

// MemoryGraphServiceTests, the pure ones.

func TestPerNodeEdgeCapPrunesExcessEdges(t *testing.T) {
	n := []uuid.UUID{
		uuid.MustParse("00000000-0000-0000-0000-000000000000"),
		uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		uuid.MustParse("33333333-3333-3333-3333-333333333333"),
	}
	e := func(a, b uuid.UUID) edge { return edge{from: a, to: b, kind: edgeSemantic, weight: 1} }
	candidates := []edge{e(n[0], n[1]), e(n[0], n[2]), e(n[0], n[3]), e(n[1], n[2]), e(n[1], n[3]), e(n[2], n[3])}
	pruned := mergeAndCap([][]edge{candidates}, 2, nil)
	degree := map[uuid.UUID]int{}
	for _, p := range pruned {
		degree[p.from]++
		degree[p.to]++
	}
	for id, d := range degree {
		assert.LessOrEqual(t, d, 2, id)
	}
	assert.LessOrEqual(t, len(pruned), 4)
}

func TestSpaceHubsAreExemptFromTheCap(t *testing.T) {
	hub := uuid.MustParse("00000000-0000-0000-0000-0000000000ff")
	var edges []edge
	for range 5 {
		from, to := ordered(uuid.New(), hub)
		edges = append(edges, edge{from: from, to: to, kind: edgeSpace, weight: spaceWeight})
	}
	assert.Len(t, mergeAndCap([][]edge{edges}, 2, map[uuid.UUID]bool{hub: true}), 5)
	assert.Len(t, mergeAndCap([][]edge{edges}, 2, nil), 2)
}

func TestLineageEdgesLinkOnlyWhenThePageIsANode(t *testing.T) {
	mem := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	page := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
	orphan := uuid.MustParse("cccccccc-0000-0000-0000-000000000003")
	rows := []memoryNodeRow{{id: mem, sourceVaultFileID: &page}, {id: orphan, sourceVaultFileID: &orphan}}
	edges := lineageEdges(rows, map[uuid.UUID]bool{page: true})
	require.Len(t, edges, 1)
	assert.Equal(t, edgeWikilink, edges[0].kind)
	assert.InDelta(t, 1.0, edges[0].weight, 0)
	assert.ElementsMatch(t, []uuid.UUID{mem, page}, []uuid.UUID{edges[0].from, edges[0].to})
	assert.Negative(t, memorystore.CompareIDs(edges[0].from, edges[0].to), "undirected invariant")
}

func TestChainEdgesConnectConsecutiveNodesWithinAGroup(t *testing.T) {
	bucket := make([]nodeMeta, 0, 4)
	for i := range 4 {
		at := time.Unix(int64(i)*1000, 0)
		bucket = append(bucket, nodeMeta{id: uuid.New(), createdAt: &at})
	}
	now := time.Now()
	edges := chainEdges(map[int64][]nodeMeta{1: bucket, 2: {{id: uuid.New(), createdAt: &now}}}, edgeSpace, 0.45)
	require.Len(t, edges, 3)
	for _, e := range edges {
		assert.Equal(t, edgeSpace, e.kind)
		assert.InDelta(t, 0.45, e.weight, 0)
		assert.Negative(t, memorystore.CompareIDs(e.from, e.to))
	}
}

func TestMergePrecedenceKeepsTheWikilinkOverAWeakerEdge(t *testing.T) {
	a := uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	b := uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	from, to := ordered(a, b)
	wikilink := edge{from: from, to: to, kind: edgeWikilink, weight: 1}
	temporal := edge{from: from, to: to, kind: edgeTemporal, weight: 0.3}
	merged := mergeAndCap([][]edge{{wikilink}, {temporal}}, 8, nil)
	require.Len(t, merged, 1)
	assert.Equal(t, edgeWikilink, merged[0].kind)
}

func TestTitleFromWiki(t *testing.T) {
	title, blank := "From Metadata", "   "
	assert.Equal(t, "From Metadata", titleFromWiki(&title, "a/b.md"))
	assert.Equal(t, "b", titleFromWiki(&blank, "a/b.md"))
	assert.Equal(t, "b.tar", titleFromWiki(nil, "a/b.tar.gz"))
	assert.Equal(t, ".hidden", titleFromWiki(nil, "a/.hidden"))
	assert.Equal(t, "a.", titleFromWiki(nil, "x/a."))
}

func TestClampAndParseFollowSwift(t *testing.T) {
	assert.InDelta(t, 1.0, clampSwift(5, 0, 1), 0)
	assert.InDelta(t, 0.0, clampSwift(-5, 0, 1), 0)
	nan, ok := parseSwiftDouble("nan")
	require.True(t, ok)
	assert.InDelta(t, 1.0, clampSwift(nan, 0, 1), 0)
	huge, ok := parseSwiftDouble("1e999")
	require.True(t, ok)
	assert.InDelta(t, 1.0, clampSwift(huge, 0, 1), 0)
	_, ok = parseSwiftDouble("abc")
	assert.False(t, ok)
}

func TestRelatedSnippet(t *testing.T) {
	assert.Equal(t, "a b c", relatedSnippet("  a \n\t b c  "))
	long := relatedSnippet(strings.Repeat("x", 200))
	assert.Equal(t, 160, swiftstr.CharacterCount(long))
	assert.True(t, strings.HasSuffix(long, "…"))
	assert.Equal(t, strings.Repeat("y", 160), relatedSnippet(strings.Repeat("y", 160)))
	assert.Empty(t, relatedSnippet(" \n "))
}

func TestReviewActionsApply(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := &memorystore.Memory{ResurfaceMuted: true}
	applyReviewAction(Soon, row, now)
	assert.Equal(t, now.Add(3*24*time.Hour), *row.NextReviewAt)
	assert.False(t, row.ResurfaceMuted)
	applyReviewAction(Later, row, now)
	assert.Equal(t, now.Add(30*24*time.Hour), *row.NextReviewAt)
	applyReviewAction(Never, row, now)
	assert.Nil(t, row.NextReviewAt)
	assert.True(t, row.ResurfaceMuted)
	row.NextReviewAt = &now
	applyReviewAction(Kept, row, now)
	assert.Nil(t, row.NextReviewAt)
	assert.True(t, row.ResurfaceMuted, "kept leaves a mute alone")
}
