package llmchat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Ports Tests/AppTests/LLM/URLExtractorTests.swift.

func TestExtractSingleURL(t *testing.T) {
	assert.Equal(t, []string{"https://example.com/article"}, ExtractURLs("Check https://example.com/article"))
}

func TestExtractMultipleDistinctURLsInOrder(t *testing.T) {
	assert.Equal(t, []string{"https://a.com", "https://b.com/x"}, ExtractURLs("see https://a.com and https://b.com/x"))
}

func TestExtractDedupesIdenticalURLs(t *testing.T) {
	assert.Len(t, ExtractURLs("https://a.com twice https://a.com"), 1)
}

func TestExtractCapsAtFive(t *testing.T) {
	many := make([]string, 10)
	for i := range many {
		many[i] = fmt.Sprintf("https://e%d.com", i)
	}
	assert.Len(t, ExtractURLs(strings.Join(many, " ")), 5)
}

func TestExtractStripsTrailingPunctuation(t *testing.T) {
	assert.Equal(t, []string{"https://x.com/path"}, ExtractURLs("ref https://x.com/path. end"))
}

func TestExtractIgnoresNonHTTPSchemes(t *testing.T) {
	assert.Equal(t, []string{"https://ok.com"}, ExtractURLs("javascript:alert(1) ftp://x and https://ok.com"))
}

func TestExtractEmptyInput(t *testing.T) {
	assert.Empty(t, ExtractURLs(""))
	assert.Empty(t, ExtractURLs("no urls here"))
}

func TestExtractStripsTrailingParenAndBracket(t *testing.T) {
	got := ExtractURLs("(https://a.com) [https://b.com]")
	assert.Contains(t, got, "https://a.com")
	assert.Contains(t, got, "https://b.com")
}

func TestContextBlockEscaping(t *testing.T) {
	assert.Equal(t, "a &amp; &lt;b&gt;", escapeXML("a & <b>"))
	assert.Equal(t, "&quot;x&quot; &amp;", escapeAttr(`"x" &`))
}
