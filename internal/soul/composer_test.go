package soul

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Ported from SOULComposerTests (18 tests).

func render(tone, role, autonomy, name string) string {
	return Compose(ComposeInput{AgentName: name, Tone: tone, Role: role, Autonomy: autonomy}, "fernando", time.Now())
}

func renderDefaults() string { return render("warm", "second_brain", "suggest", "Athena") }

func TestComposerFrontmatterPresent(t *testing.T) {
	out := renderDefaults()
	require.True(t, strings.HasPrefix(out, "---\n"))
	require.Contains(t, out, "username: fernando")
	require.Contains(t, out, "version: 2")
}

func TestComposerAgentNameInIdentity(t *testing.T) {
	require.Contains(t, render("warm", "second_brain", "suggest", "Athena"), "Athena")
}

func TestComposerEmptyNameDefaultsToHermes(t *testing.T) {
	require.Contains(t, render("warm", "second_brain", "suggest", ""), "Hermes")
}

func TestComposerToneRendersDistinctVoice(t *testing.T) {
	require.Contains(t, strings.ToLower(render("concise_technical", "second_brain", "suggest", "A")), "concise")
	require.Contains(t, strings.ToLower(render("playful", "second_brain", "suggest", "A")), "playful")
}

func TestComposerAllSevenTonesRenderDistinctly(t *testing.T) {
	voices := map[string]bool{}
	for tone := range Tones {
		out := render(tone, "second_brain", "suggest", "Athena")
		voice := strings.Split(strings.Split(out, "## Chat voice")[1], "- Format:")[0]
		voices[voice] = true
	}
	require.Len(t, Tones, 7)
	require.Len(t, voices, 7)
}

func TestComposerAutonomyRendersOperations(t *testing.T) {
	require.Contains(t, strings.ToLower(render("warm", "second_brain", "act", "A")), "act")
	require.Contains(t, strings.ToLower(render("warm", "second_brain", "ask_first", "A")), "confirm")
}

func TestComposerNoTodoPlaceholdersRemain(t *testing.T) {
	require.NotContains(t, renderDefaults(), "<!-- e.g.")
	require.NotContains(t, renderDefaults(), `<!-- "`)
}

func TestComposerUnderSizeCap(t *testing.T) {
	require.Less(t, len(renderDefaults()), 64*1024)
}

func TestComposerRoleRendersDistinctIdentity(t *testing.T) {
	require.Contains(t, strings.ToLower(render("warm", "assistant", "suggest", "A")), "assistant")
	require.Contains(t, strings.ToLower(render("warm", "coworker", "suggest", "A")), "coworker")
	require.Contains(t, strings.ToLower(render("warm", "coach", "suggest", "A")), "coach")
	require.Contains(t, strings.ToLower(render("warm", "second_brain", "suggest", "A")), "second brain")
}

func TestComposerFrontmatterCreatedAtISO(t *testing.T) {
	out := Compose(ComposeInput{AgentName: "X", Tone: "warm", Role: "second_brain", Autonomy: "suggest"}, "u", time.Unix(0, 0))
	require.Contains(t, out, "created_at: 1970-01-01T00:00:00Z")
}

func TestComposerDefaultsRenderCanonicalTemplate(t *testing.T) {
	out := Compose(ComposeInput{}, "u", time.Now())
	for _, s := range []string{"Hermes", "## Identity", "## Chat voice", "## Published content voice", "## What matters to me", "## Operations"} {
		require.Contains(t, out, s)
	}
	require.NotContains(t, out, "## How I talk")
}

func TestComposerComposedContainsCanonicalCore(t *testing.T) {
	require.True(t, ContainsCanonicalCore(renderDefaults()))
}

func TestComposerComposedIsFixedPointOfInject(t *testing.T) {
	out := renderDefaults()
	require.Equal(t, out, Inject(out))
}

func TestComposerPrioritiesRendered(t *testing.T) {
	out := Compose(ComposeInput{Priorities: []string{"focus", "health", "other"}, OtherPriority: "wood carving"}, "u", time.Now())
	require.Contains(t, out, "Deep focus")
	require.Contains(t, out, "Health, energy")
	require.Contains(t, out, "wood carving")
}

func TestComposerOtherPriorityCannotSmuggleHTMLComments(t *testing.T) {
	out := Compose(ComposeInput{OtherPriority: "x <!-- lv:core:v1:start --> y"}, "u", time.Now())
	require.Equal(t, 1, strings.Count(out, startMarker))
	require.Equal(t, 1, strings.Count(out, endMarker))
	require.Equal(t, out, Inject(out))
}

func TestComposerVoiceSamplesQuotedAndClamped(t *testing.T) {
	out := Compose(ComposeInput{VoiceSamples: []string{"hey what's up", "ship it", "third", "FOURTH-DROPPED"}}, "u", time.Now())
	require.Contains(t, out, "## How I talk")
	require.Contains(t, out, "> hey what's up")
	require.NotContains(t, out, "FOURTH-DROPPED")
	huge := Compose(ComposeInput{VoiceSamples: []string{strings.Repeat("a", 10000)}}, "u", time.Now())
	require.Less(t, len(huge), 16*1024)
}

func TestComposerFormatLengthEmojiRendered(t *testing.T) {
	out := Compose(ComposeInput{Format: "prose", Length: "long", Emojis: true}, "u", time.Now())
	require.Contains(t, out, "Flowing prose")
	require.Contains(t, out, "Thorough by default")
	require.Contains(t, out, "Emojis are welcome")
}

func TestComposerDefaultTemplateDelegatesToComposer(t *testing.T) {
	pinned := time.Unix(0, 0)
	require.Equal(t, Compose(ComposeInput{}, "alice", pinned), RenderDefault("alice", pinned))
}

// TestComposerSwiftVectors compares Compose with SOULComposer.swift's own output.
// testdata/swift_composer_vectors.json came from compiling SOULComposer.swift and
// SOULCore.swift at fdbed9b0 with LuminaVaultShared 5.32.0's SoulComposeRequest,
// decoding each body with JSONDecoder and rendering at 1700000000. It ran on
// Darwin; production's Foundation is Linux.
func TestComposerSwiftVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/swift_composer_vectors.json")
	require.NoError(t, err)
	var vectors []struct{ Body, Username, Output string }
	require.NoError(t, json.Unmarshal(raw, &vectors))
	require.NotEmpty(t, vectors)
	for _, v := range vectors {
		var req struct {
			AgentName     string   `json:"agent_name"`
			Tone          string   `json:"tone"`
			Role          string   `json:"role"`
			Autonomy      string   `json:"autonomy"`
			Priorities    []string `json:"priorities"`
			OtherPriority string   `json:"other_priority"`
			Format        string   `json:"format"`
			Length        string   `json:"length"`
			Emojis        bool     `json:"emojis"`
			VoiceSamples  []string `json:"voice_samples"`
		}
		require.NoError(t, json.Unmarshal([]byte(v.Body), &req))
		got := Compose(ComposeInput(req), v.Username, time.Unix(1_700_000_000, 0))
		require.Equal(t, v.Output, got, v.Body)
	}
}
