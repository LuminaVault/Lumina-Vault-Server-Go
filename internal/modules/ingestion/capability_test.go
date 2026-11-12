package ingestion

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// MultimodalIngestionCapabilityTests: "matches exact and wildcard MIME capabilities".
func TestSupportsContentTypeMatchesExactAndWildcard(t *testing.T) {
	require.True(t, SupportsContentType("application/pdf", []string{"application/pdf"}))
	require.True(t, SupportsContentType("image/heic", []string{"image/*"}))
	require.True(t, SupportsContentType("IMAGE/JPEG", []string{"image/*"}))
	require.False(t, SupportsContentType("video/mp4", []string{"image/*", "audio/*"}))
}

// MultimodalIngestionCapabilityTests: "BYO ingestion source byte ranges are
// bounded and support suffixes".
func TestSourceRangesAreBoundedAndSupportSuffixes(t *testing.T) {
	type r struct{ first, last int64 }
	for value, want := range map[string]r{
		"bytes=0-99":     {0, 99},
		"bytes=900-":     {900, 999},
		"bytes=-100":     {900, 999},
		"bytes=950-2000": {950, 999},
	} {
		first, last, err := parseRange(value, 1000)
		require.NoError(t, err, value)
		require.Equal(t, want, r{first, last}, value)
	}
	_, _, err := parseRange("bytes=1000-1001", 1000)
	require.Error(t, err)
	_, _, err = parseRange("bytes=0-1,4-5", 1000)
	require.Error(t, err)
}

// MultimodalIngestionCapabilityTests: "source tokens are stored as deterministic hashes".
func TestSourceTokensAreStoredAsDeterministicHashes(t *testing.T) {
	token := strings.Repeat("a", 64)
	hash := sourceTokenHash(token)
	require.Len(t, hash, 64)
	require.Equal(t, hash, sourceTokenHash(token))
	require.NotEqual(t, token, hash)
}
