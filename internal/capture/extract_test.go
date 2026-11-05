package capture

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func normalized(urls []ExtractedURL) []string {
	out := []string{}
	for _, u := range urls {
		out = append(out, u.Normalized)
	}
	return out
}

// URLExtractionServiceTests.swift.
func TestExtractSingleBareURL(t *testing.T) {
	urls := ExtractURLs("check out https://news.ycombinator.com today")
	require.Len(t, urls, 1)
	require.Equal(t, "https://news.ycombinator.com", urls[0].Raw)
	require.Equal(t, "https://news.ycombinator.com", urls[0].Normalized)
}

func TestExtractDedupesRepeats(t *testing.T) {
	urls := ExtractURLs("https://example.com/foo and again https://example.com/foo")
	require.Equal(t, []string{"https://example.com/foo"}, normalized(urls))
}

func TestExtractDedupesFragments(t *testing.T) {
	require.Len(t, ExtractURLs("https://example.com/post#a vs https://example.com/post#b"), 1)
}

func TestExtractKeepsDifferentQueries(t *testing.T) {
	require.Len(t, ExtractURLs("https://example.com/q?a=1 vs https://example.com/q?a=2"), 2)
}

func TestExtractLowercasesSchemeAndHost(t *testing.T) {
	urls := ExtractURLs("HTTPS://EXAMPLE.COM/Path is the same as https://example.com/Path")
	require.Equal(t, []string{"https://example.com/Path"}, normalized(urls))
}

func TestExtractStripsDefaultPorts(t *testing.T) {
	require.Len(t, ExtractURLs("https://example.com:443/x and https://example.com/x"), 1)
}

func TestExtractKeepsOrder(t *testing.T) {
	urls := ExtractURLs("first https://a.com then https://b.com finally https://c.com")
	require.Equal(t, []string{"https://a.com", "https://b.com", "https://c.com"}, normalized(urls))
}

func TestExtractDropsNonHTTP(t *testing.T) {
	require.Empty(t, ExtractURLs("ftp://example.com/x and mailto:a@b.com"))
}

func TestExtractDropsNonPublicHosts(t *testing.T) {
	require.Empty(t, ExtractURLs("internal http://localhost:8080/admin or http://192.168.1.1"))
}

func TestExtractEmpty(t *testing.T) {
	require.Empty(t, ExtractURLs(""))
}

func TestExtractCap(t *testing.T) {
	parts := []string{}
	for i := range MaxURLsPerExtraction + 10 {
		parts = append(parts, fmt.Sprintf("https://example%d.com", i))
	}
	require.Len(t, ExtractURLs(strings.Join(parts, " ")), MaxURLsPerExtraction)
}
