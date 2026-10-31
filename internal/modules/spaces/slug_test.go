package spaces

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Ports Tests/AppTests/Spaces/SpaceSlugDerivationTests.swift.

func TestDeriveOrdinaryNames(t *testing.T) {
	require.Equal(t, "capture", deriveSlug("Capture"))
	require.Equal(t, "aliceonly", deriveSlug("AliceOnly"))
	require.Equal(t, "reading-list", deriveSlug("Reading List"))
}

func TestDeriveCollapsesPunctuation(t *testing.T) {
	require.Equal(t, "work-notes", deriveSlug("  Work / Notes  "))
	require.Equal(t, "ideas", deriveSlug("!!!Ideas!!!"))
	require.Equal(t, "a-b", deriveSlug("a—b"))
}

func TestEveryDerivedSlugValidates(t *testing.T) {
	names := []string{
		"Capture", "AliceOnly", "Reading List", "  Work / Notes  ",
		"!!!Ideas!!!", "a—b", "日本語", "🎉🎉", "-leading-dash",
		strings.Repeat("long", 40), strings.Repeat("ab-", 20),
		"x", "", "   ", "raw", "trash",
		// Go-only: clusters that Swift treats as one non-ASCII Character.
		"café", "İstanbul", "1️⃣x",
	}
	for _, n := range names {
		d := deriveSlug(n)
		_, err := validateSlug(d)
		require.NoError(t, err, "derive(%q) = %q, which the policy rejects", n, d)
	}
}

func TestExplicitSlugWins(t *testing.T) {
	s := func(v string) *string { return &v }
	require.Equal(t, "chosen", resolveSlug(s("chosen"), "Ignored Name"))
	require.Equal(t, "fallback-name", resolveSlug(s("  "), "Fallback Name"))
	require.Equal(t, "fallback-name", resolveSlug(nil, "Fallback Name"))
}

func TestReservedDerivationsAreNudged(t *testing.T) {
	for _, r := range []string{"raw", "compiled", "trash", "tmp"} {
		d := deriveSlug(r)
		require.NotEqual(t, r, d)
		_, err := validateSlug(d)
		require.NoError(t, err)
	}
}

// Grapheme clusters: an ASCII letter with a combining mark is one non-ASCII
// Character in Swift, so it separates.
func TestDeriveClusters(t *testing.T) {
	require.Equal(t, "caf", deriveSlug("café"))
	require.Equal(t, "stanbul", deriveSlug("İstanbul"))
	require.Equal(t, "space", deriveSlug("1️⃣"))
}

func TestValidateSlug(t *testing.T) {
	got, err := validateSlug("  Crypto ")
	require.NoError(t, err)
	require.Equal(t, "crypto", got)
	for _, bad := range []string{"a", "-ab", "_deleted", "raw", "has space", strings.Repeat("a", 32), "ünï"} {
		_, err := validateSlug(bad)
		require.ErrorIs(t, err, errInvalidSlug, bad)
	}
	_, err = validateSlug(strings.Repeat("a", 31))
	require.NoError(t, err)
}
