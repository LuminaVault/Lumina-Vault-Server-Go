package chat

import (
	"lumina/internal/memorystore"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports Tests/AppTests/Chat/ChatAttachmentPromptTests.swift.

func ptr[T any](v T) *T { return &v }

func TestAttachmentsNoneLeavesContentUntouched(t *testing.T) {
	assert.Equal(t, "hello", ComposeAttachments("hello", nil))
	assert.Equal(t, "hello", ComposeAttachments("hello", []Attachment{}))
}

func TestAttachmentTextBlockShape(t *testing.T) {
	got := ComposeAttachments("what does this say?", []Attachment{{Kind: "text", Name: "notes.txt", Text: ptr("the contents")}})
	assert.Equal(t, "[Attached: notes.txt]\n\"\"\"\nthe contents\n\"\"\"\n\nwhat does this say?", got)
}

func TestAttachmentEachKindGetsItsOwnLabel(t *testing.T) {
	assert.Contains(t, ComposeAttachments("x", []Attachment{{Kind: "text", Name: "a", Text: ptr("1")}}), "[Attached: a]")
	assert.Contains(t, ComposeAttachments("x", []Attachment{{Kind: "vault_file", Name: "b", Text: ptr("2")}}), "[Attached file: b]")
	assert.Contains(t, ComposeAttachments("x", []Attachment{{Kind: "link", Name: "c", URL: ptr("https://example.com")}}), "[Attached link: c]")
}

func TestAttachmentWithoutMessageGetsAnInstruction(t *testing.T) {
	got := ComposeAttachments("", []Attachment{{Kind: "text", Name: "a", Text: ptr("body")}})
	assert.True(t, strings.HasSuffix(got, "Please use the attached context."))
}

func TestAttachmentsKeepTheirOrder(t *testing.T) {
	got := ComposeAttachments("compare", []Attachment{
		{Kind: "text", Name: "first", Text: ptr("one")},
		{Kind: "text", Name: "second", Text: ptr("two")},
	})
	assert.Less(t, strings.Index(got, "first"), strings.Index(got, "second"))
	assert.True(t, strings.HasSuffix(got, "compare"))
}

func TestAttachmentOversizedIsTruncatedNotDropped(t *testing.T) {
	huge := strings.Repeat("x", maxCharactersPerAttachment+5000)
	got := ComposeAttachments("summarise", []Attachment{{Kind: "text", Name: "big", Text: &huge}})
	assert.Contains(t, got, truncationNotice)
	assert.Contains(t, got, "[Attached: big]")
	assert.Less(t, len(got), len(huge))
}

func TestAttachmentTotalBudgetBoundsTheTurn(t *testing.T) {
	each := strings.Repeat("y", maxCharactersPerAttachment)
	many := make([]Attachment, 10)
	for i := range many {
		many[i] = Attachment{Kind: "text", Name: "f", Text: &each}
	}
	assert.Less(t, len(ComposeAttachments("go", many)), maxTotalAttachmentChars+2000)
}

func TestAttachmentsWithNothingUsableAreSkipped(t *testing.T) {
	got := ComposeAttachments("hello", []Attachment{
		{Kind: "text", Name: "empty", Text: ptr("")},
		{Kind: "text", Name: "blank", Text: ptr("   ")},
		{Kind: "link", Name: "nolink"},
	})
	assert.Equal(t, "hello", got)
}

func TestAttachmentVaultReferenceFallsBackToPath(t *testing.T) {
	got := ComposeAttachments("x", []Attachment{{Kind: "vault_file", Name: "plan", VaultPath: ptr("notes/plan.md")}})
	assert.Contains(t, got, "notes/plan.md")
}

// Ports Tests/AppTests/Chat/HermesEscalationPolicyTests.swift.

func TestEscalationOffNeverEscalates(t *testing.T) {
	for _, c := range []string{"commit this", "deploy to prod", "delete the branch"} {
		assert.False(t, DecideEscalation(c, ModeOff, true), c)
	}
}

func TestEscalationForceAlwaysEscalates(t *testing.T) {
	for _, c := range []string{"what is in my vault?", "", "tell me about swift"} {
		assert.True(t, DecideEscalation(c, ModeForce, true), c)
	}
}

func TestEscalationUnavailableFallsBack(t *testing.T) {
	for _, mode := range []string{ModeOff, ModeAuto, ModeForce} {
		assert.False(t, DecideEscalation("commit this", mode, false), mode)
	}
}

func TestEscalationAutoEscalatesActions(t *testing.T) {
	for _, c := range []string{
		"commit the staged changes", "deploy the api to staging", "refactor this into a service",
		"delete the merged branches", "please commit this", "could you deploy the api", "Fix the failing test",
	} {
		assert.True(t, DecideEscalation(c, ModeAuto, true), c)
	}
}

func TestEscalationAutoLeavesQuestionsAlone(t *testing.T) {
	for _, c := range []string{
		"what did I write about deployment last week?", "how does the commit flow work?",
		"summarise my notes on refactoring", "remind me why we deleted that", "", "   ",
	} {
		assert.False(t, DecideEscalation(c, ModeAuto, true), c)
	}
}

func TestEscalationVerbMustLead(t *testing.T) {
	assert.False(t, LooksLikeAction("how does deploy work"))
	assert.False(t, LooksLikeAction("notes about commit hygiene"))
	assert.True(t, LooksLikeAction("deploy the thing"))
}

func TestEscalationVerbFollowedByMeIsAQuestion(t *testing.T) {
	assert.False(t, LooksLikeAction("run me through the auth flow"))
	assert.False(t, LooksLikeAction("walk me through this"))
	assert.True(t, LooksLikeAction("run the migration"))
}

func TestEscalationPolitenessIsStripped(t *testing.T) {
	assert.True(t, LooksLikeAction("please deploy"))
	assert.True(t, LooksLikeAction("hey, can you deploy the api"))
	assert.True(t, LooksLikeAction("okay please build it"))
}

func TestEscalationTokenizationIsRobust(t *testing.T) {
	assert.True(t, LooksLikeAction("COMMIT!"))
	assert.True(t, LooksLikeAction("  commit,  the changes"))
	assert.True(t, LooksLikeAction("Delete — the old branch"))
}

// Ports Tests/AppTests/Chat/ChatGroundingTests.swift.

func groundingHit(distance float32, lexical *float32) memorystore.SearchResult {
	return memorystore.SearchResult{ID: uuid.New(), TenantID: uuid.New(), Content: "x", Distance: distance, LexicalScore: lexical}
}

func TestGroundingKeepsCloseHitsDropsFar(t *testing.T) {
	kept := Relevant([]memorystore.SearchResult{groundingHit(0.2, nil), groundingHit(0.9, nil)}, 0.6)
	require.Len(t, kept, 1)
	assert.InDelta(t, 0.2, kept[0].Distance, 1e-6)
}

func TestGroundingKeepsExactKeywordMatch(t *testing.T) {
	assert.Len(t, Relevant([]memorystore.SearchResult{groundingHit(1.0, ptr[float32](0.1))}, 0.6), 1)
}

func TestGroundingNothingRelevantIsEmpty(t *testing.T) {
	assert.Empty(t, Relevant([]memorystore.SearchResult{groundingHit(0.8, nil), groundingHit(0.95, nil)}, 0.6))
}

// Ports Tests/AppTests/Chat/ChatSoulPromptTests.swift.

func TestPromptCarriesSoulOnlyWhenGiven(t *testing.T) {
	with := BuildPrompt(nil, nil, nil, ptr("Prefers British spelling."), false)
	without := BuildPrompt(nil, nil, nil, nil, false)
	assert.Contains(t, with[0].Content, "Prefers British spelling.")
	assert.NotContains(t, without[0].Content, "SOUL.md")
}

func TestLongSoulIsCappedAndSaysSo(t *testing.T) {
	long := strings.Repeat("a", maxSoulCharacters+50)
	got := capped(long, maxSoulCharacters)
	assert.True(t, strings.HasSuffix(got, "…(truncated)"))
	assert.Less(t, len(got), len(long))
	assert.Equal(t, "short", capped("short", 10))
}
