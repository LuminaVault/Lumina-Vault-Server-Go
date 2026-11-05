package chunkindex

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WikilinkParsingTests.

func slugs(links []ParsedWikilink) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.TargetSlug
	}
	return out
}

func TestABareLinkYieldsItsTarget(t *testing.T) {
	links := ExtractWikilinks("See [[hermes]] for details.")
	require.Len(t, links, 1)
	assert.Equal(t, "hermes", links[0].TargetSlug)
	assert.Equal(t, 1, links[0].Line)
	assert.Nil(t, links[0].Label)
	assert.Nil(t, links[0].TargetHeading)
}

func TestAliasHeadingAndPathAreSplitApart(t *testing.T) {
	links := ExtractWikilinks("[[projects/hermes#Fallbacks|how routing fails]]")
	require.Len(t, links, 1)
	assert.Equal(t, "projects/hermes", links[0].TargetSlug)
	assert.Equal(t, "Fallbacks", *links[0].TargetHeading)
	assert.Equal(t, "how routing fails", *links[0].Label)
	assert.Equal(t, "projects/hermes#Fallbacks|how routing fails", links[0].RawTarget)
}

func TestSeveralLinksOnOneLineGetTheRightLineNumber(t *testing.T) {
	links := ExtractWikilinks("intro\n[[a]] and [[b]] and [[c]]\noutro")
	assert.Equal(t, []string{"a", "b", "c"}, slugs(links))
	for _, l := range links {
		assert.Equal(t, 2, l.Line)
	}
}

func TestLinksInsideFencedCodeAreIgnored(t *testing.T) {
	assert.Equal(t, []string{"real", "alsoreal"}, slugs(ExtractWikilinks("[[real]]\n\n```\n[[not-a-link]]\n```\n\n[[alsoreal]]")))
}

func TestLinksInsideInlineCodeSpansAreIgnored(t *testing.T) {
	assert.Equal(t, []string{"real"}, slugs(ExtractWikilinks("type `[[literal]]` to make [[real]]")))
}

func TestASameDocumentHeadingAnchorIsNotAnEdge(t *testing.T) {
	assert.Empty(t, ExtractWikilinks("jump to [[#Section]]"))
}

func TestUnterminatedAndEmptyBracketsProduceNothing(t *testing.T) {
	assert.Empty(t, ExtractWikilinks("open [[never closed"))
	assert.Empty(t, ExtractWikilinks("[[]]"))
	assert.Empty(t, ExtractWikilinks("[[   ]]"))
}

func TestASingleBracketPairIsNotAWikilink(t *testing.T) {
	assert.Empty(t, ExtractWikilinks("a [markdown](link) and [ref]"))
}

func TestAnAbsurdlyLongTargetIsRejectedRatherThanIndexed(t *testing.T) {
	assert.Empty(t, ExtractWikilinks("[["+strings.Repeat("x", MaxTargetLength+1)+"]]"))
}

func TestPathSpellingsAreNormalizedButCaseIsPreserved(t *testing.T) {
	assert.Equal(t, "Notes/A.md", NormalizeSlug("./Notes/A.md"))
	assert.Equal(t, "Notes/A.md", NormalizeSlug("/Notes/A.md"))
	assert.Equal(t, "Notes/A.md", NormalizeSlug(`Notes\A.md`))
	assert.NotEqual(t, NormalizeSlug("Notes/A.md"), NormalizeSlug("notes/a.md"))
}

// WikilinkResolverTests.

func target(path string) WikilinkTarget {
	return WikilinkTarget{VaultFileID: uuid.New(), Path: path}
}

func link(t *testing.T, inner string) ParsedWikilink {
	t.Helper()
	parsed := ParseWikilink(inner, 1)
	require.NotNil(t, parsed)
	return *parsed
}

func TestAnExactPathResolves(t *testing.T) {
	hermes := target("projects/hermes.md")
	resolved := ResolveWikilinks([]ParsedWikilink{link(t, "projects/hermes")}, []WikilinkTarget{hermes, target("notes/other.md")})
	assert.Equal(t, Resolved, resolved[0].State)
	assert.Equal(t, hermes.VaultFileID, *resolved[0].TargetVaultFileID)
}

func TestAnExactPathWithItsExtensionResolves(t *testing.T) {
	hermes := target("projects/hermes.md")
	resolved := ResolveWikilinks([]ParsedWikilink{link(t, "projects/hermes.md")}, []WikilinkTarget{hermes})
	assert.Equal(t, hermes.VaultFileID, *resolved[0].TargetVaultFileID)
}

func TestAUniqueFilenameStemResolvesWithoutAPath(t *testing.T) {
	hermes := target("deep/nested/hermes.md")
	resolved := ResolveWikilinks([]ParsedWikilink{link(t, "hermes")}, []WikilinkTarget{hermes, target("notes/unrelated.md")})
	assert.Equal(t, Resolved, resolved[0].State)
	assert.Equal(t, hermes.VaultFileID, *resolved[0].TargetVaultFileID)
}

func TestAStemMatchingTwoDocumentsIsAmbiguous(t *testing.T) {
	resolved := ResolveWikilinks([]ParsedWikilink{link(t, "index")}, []WikilinkTarget{target("a/index.md"), target("b/index.md")})
	assert.Equal(t, Ambiguous, resolved[0].State)
	assert.Nil(t, resolved[0].TargetVaultFileID, "an arbitrary pick would draw a wrong edge and cite a wrong file")
}

func TestAnExactPathWinsOverAnAmbiguousStem(t *testing.T) {
	a := target("a/index.md")
	resolved := ResolveWikilinks([]ParsedWikilink{link(t, "a/index")}, []WikilinkTarget{a, target("b/index.md")})
	assert.Equal(t, Resolved, resolved[0].State)
	assert.Equal(t, a.VaultFileID, *resolved[0].TargetVaultFileID)
}

func TestALinkToNothingIsUnresolved(t *testing.T) {
	resolved := ResolveWikilinks([]ParsedWikilink{link(t, "does-not-exist")}, []WikilinkTarget{target("a.md")})
	assert.Equal(t, Unresolved, resolved[0].State)
	assert.Nil(t, resolved[0].TargetVaultFileID)
}

func TestAHeadingFragmentDoesNotChangeWhichDocumentIsChosen(t *testing.T) {
	hermes := target("projects/hermes.md")
	withHeading := ResolveWikilinks([]ParsedWikilink{link(t, "hermes#Fallbacks")}, []WikilinkTarget{hermes})
	without := ResolveWikilinks([]ParsedWikilink{link(t, "hermes")}, []WikilinkTarget{hermes})
	assert.Equal(t, *without[0].TargetVaultFileID, *withHeading[0].TargetVaultFileID)
	assert.Equal(t, "Fallbacks", *withHeading[0].Link.TargetHeading)
}

func TestAnAliasDoesNotChangeWhichDocumentIsChosen(t *testing.T) {
	hermes := target("projects/hermes.md")
	resolved := ResolveWikilinks([]ParsedWikilink{link(t, "hermes|the router")}, []WikilinkTarget{hermes})
	assert.Equal(t, hermes.VaultFileID, *resolved[0].TargetVaultFileID)
	assert.Equal(t, "the router", *resolved[0].Link.Label)
}

func TestLongerSuffixesAreStrippedBeforeShorterOnes(t *testing.T) {
	assert.Equal(t, "note", LinkStem("a/note.markdown"))
	assert.Equal(t, "note", LinkStem("a/note.md"))
	assert.Equal(t, "note", LinkStem("a/note"))
}

func TestResolutionOfAnEmptyLinkListDoesNoWork(t *testing.T) {
	assert.Empty(t, ResolveWikilinks(nil, []WikilinkTarget{target("a.md")}))
}

// Swift's Dictionary compares keys by canonical equivalence.
func TestPathsResolveAcrossUnicodeNormalizationForms(t *testing.T) {
	nfd := target("notas/café.md")
	resolved := ResolveWikilinks([]ParsedWikilink{link(t, "notas/café"), link(t, "café")}, []WikilinkTarget{nfd})
	assert.Equal(t, nfd.VaultFileID, *resolved[0].TargetVaultFileID)
	assert.Equal(t, nfd.VaultFileID, *resolved[1].TargetVaultFileID)
}
