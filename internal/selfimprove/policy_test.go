package selfimprove

import (
	"fmt"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func numberedLines(prefix string, n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("%s-%d", prefix, i+1)
	}
	return strings.Join(lines, "\n")
}

// SelfImprovementPolicyTests: "conservative change accepts small patch" and
// "rejects broad rewrite".
func TestIsConservativeChange(t *testing.T) {
	old := numberedLines("line", 30)
	assert.True(t, IsConservativeChange(old, old+"\nline-extra"))

	assert.False(t, IsConservativeChange(numberedLines("keep", 40), numberedLines("rewrite", 40)))

	// The limit, in both directions: 20 differing lines pass and 21 don't.
	assert.True(t, IsConservativeChange("", numberedLines("new", 20)))
	assert.False(t, IsConservativeChange("", numberedLines("new", 21)))
	assert.True(t, IsConservativeChange(numberedLines("old", 20), ""))
	// A third of the old lines beats 20 once there are more than 62 of them.
	long := numberedLines("old", 90)
	assert.True(t, IsConservativeChange(long, numberedLines("old", 75)))
	assert.False(t, IsConservativeChange(long, numberedLines("old", 59)))

	// Blank lines and repeats don't count; canonically equal lines are equal.
	assert.True(t, IsConservativeChange("a\n\n\nb\na", "a\nb"))
	assert.Empty(t, symmetric("café", "café"))
}

func symmetric(a, b string) []string {
	var out []string
	x, y := lineSet(a), lineSet(b)
	for l := range x {
		if !y[l] {
			out = append(out, l)
		}
	}
	for l := range y {
		if !x[l] {
			out = append(out, l)
		}
	}
	return out
}

// "sha256 is stable for approve base comparison".
func TestSHA256(t *testing.T) {
	soul := "# SOUL.md\n\nHello\n"
	assert.Equal(t, SHA256(soul), SHA256(soul))
	assert.NotEqual(t, SHA256(soul), SHA256(soul+"x"))
	assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", SHA256("abc"))
}

// "valid resource name rejects path traversal and empties".
func TestValidResourceName(t *testing.T) {
	for _, name := range []string{"my-skill_1", "a", strings.Repeat("a", 80), "café", "café", "日本語", "٣", "Ⅷ", "½"} {
		assert.True(t, ValidResourceName(name), name)
	}
	for _, name := range []string{"", "../etc", "a/b", "a.b", "a b", "a%20b", strings.Repeat("a", 81), "-́", "😀"} {
		assert.False(t, ValidResourceName(name), name)
	}
	// The limit counts Characters, not bytes or scalars.
	assert.True(t, ValidResourceName(strings.Repeat("é", 80)))
}

// "decodeJSON strips markdown fences".
func TestDecodeJSON(t *testing.T) {
	type envelope struct {
		Needed  bool   `json:"needed"`
		Summary string `json:"summary"`
	}
	for _, raw := range []string{
		"```json\n{\"needed\":false,\"summary\":\"no drift\"}\n```",
		"  \n```\n{\"needed\":false,\"summary\":\"no drift\"}\n```\n",
		`{"needed":false,"summary":"no drift"}`,
	} {
		got, err := DecodeJSON[envelope](raw)
		require.NoError(t, err, raw)
		assert.Equal(t, envelope{Needed: false, Summary: "no drift"}, got)
	}
	for _, raw := range []string{"", "not json", "```", "```json"} {
		_, err := DecodeJSON[envelope](raw)
		var wireErr *wire.Error
		require.ErrorAs(t, err, &wireErr, raw)
		assert.Equal(t, http.StatusBadGateway, wireErr.Status)
		assert.Equal(t, "review_output_invalid_json", wireErr.Message)
	}
}
