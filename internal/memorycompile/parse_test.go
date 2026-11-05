package memorycompile

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsesCanonicalMemories(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, ParseExtractedMemories(`{"memories":["a","b"]}`))
}

func TestParsesAnArrayOfContentObjects(t *testing.T) {
	assert.Equal(t, []string{"x", "y"}, ParseExtractedMemories(`{"memories":[{"content":"x"},{"content":"y"}]}`))
}

func TestParsesABareArray(t *testing.T) {
	assert.Equal(t, []string{"one", "two"}, ParseExtractedMemories(`["one","two"]`))
}

func TestStripsCodeFencesAndProse(t *testing.T) {
	assert.Equal(t, []string{"z"}, ParseExtractedMemories("```json\n{\"memories\":[\"z\"]}\n```"))
	assert.Equal(t, []string{"p"}, ParseExtractedMemories("Here you go: {\"memories\":[\"p\"]} — that's all."))
}

func TestEmptyMemoriesYieldsEmpty(t *testing.T) {
	assert.Empty(t, ParseExtractedMemories(`{"memories":[]}`))
	assert.Empty(t, ParseExtractedMemories("garbage"))
}

func TestExtractedItemsCarryKindAndSource(t *testing.T) {
	items := ParseExtractedItems(`{"memories":[{"text":"a","kind":"fact","source":"x.md"},{"content":"b","kind":"nope"},"c"]}`)
	require.Len(t, items, 3)
	assert.Equal(t, "a", items[0].Text)
	require.NotNil(t, items[0].Kind)
	assert.Equal(t, "fact", *items[0].Kind)
	require.NotNil(t, items[0].Source)
	assert.Equal(t, "x.md", *items[0].Source)
	assert.Equal(t, "b", items[1].Text)
	assert.Nil(t, items[1].Kind)
	assert.Nil(t, items[1].Source)
	assert.Equal(t, "c", items[2].Text)
	assert.Nil(t, items[2].Kind)
	assert.Nil(t, items[2].Source)
}

func TestWikiSlugSanitizesAPathBasename(t *testing.T) {
	assert.Equal(t, "2026-05-30-foo-bar", WikiSlug("captures/2026-05-30-Foo Bar.md"))
	assert.NotEmpty(t, WikiSlug("ai/note.md"))
}

func commitmentTexts(items []ExtractedCommitment) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Text
	}
	return out
}

func TestValidCommitmentsKeepTextDueDayAndSource(t *testing.T) {
	items := ParseExtractedCommitments(`{"memories": [], "commitments": [
	  {"text": "Send Ana the deck", "due": "2026-10-23", "source": "notes/a.md"},
	  {"text": "  Renew passport  ", "due": null},
	  {"text": "Call the bank", "due": "2026-10-24T09:30:00Z"}
	]}`)
	assert.Equal(t, []string{"Send Ana the deck", "Renew passport", "Call the bank"}, commitmentTexts(items))
	require.NotNil(t, items[0].Source)
	assert.Equal(t, "notes/a.md", *items[0].Source)
	require.NotNil(t, items[0].Due)
	assert.True(t, items[0].Due.Equal(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)))
	assert.Nil(t, items[1].Due)
	require.NotNil(t, items[2].Due)
	assert.True(t, items[2].Due.Equal(time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)))
}

func TestMissingOrMalformedCommitmentsKeyIsNoCommitments(t *testing.T) {
	assert.Empty(t, ParseExtractedCommitments(`{"memories": ["a"]}`))
	assert.Empty(t, ParseExtractedCommitments(`{"memories": [], "commitments": "soon"}`))
	assert.Empty(t, ParseExtractedCommitments(`["a", "b"]`))
	assert.Empty(t, ParseExtractedCommitments("not json at all"))
	assert.Empty(t, ParseExtractedCommitments(""))
	mixed := ParseExtractedCommitments(`{"commitments": [{"due": "2026-10-23"}, {"text": "  "}, 7, {"text": "Book the dentist"}]}`)
	assert.Equal(t, []string{"Book the dentist"}, commitmentTexts(mixed))
}

func TestUnparsableDueIsNilAndTheCommitmentIsKept(t *testing.T) {
	items := ParseExtractedCommitments("```json\n" + `{"commitments": [
	  {"text": "Pay rent", "due": "Friday"},
	  {"text": "Water plants", "due": "2026-02-30"},
	  {"text": "Fix bike", "due": 20261023}
	]}` + "\n```")
	assert.Equal(t, []string{"Pay rent", "Water plants", "Fix bike"}, commitmentTexts(items))
	for _, item := range items {
		assert.Nil(t, item.Due, item.Text)
	}
}

func TestProseAroundTheCommitmentJSONIsTolerated(t *testing.T) {
	items := ParseExtractedCommitments(`Here you go: {"memories": [], "commitments": [{"text": "Email Joe"}]} hope that helps`)
	assert.Equal(t, []string{"Email Joe"}, commitmentTexts(items))
}

func TestNormalizationFoldsCaseSpacingAndTrailingPunctuation(t *testing.T) {
	assert.Equal(t, "i'll call mum", NormalizeCommitment("  I'll   Call Mum. "))
	assert.Equal(t, "i'll call mum", NormalizeCommitment("i'll call mum!"))
}
