package skills

import (
	"encoding/json"
	"lumina/internal/memorycompile"
	"testing"

	"github.com/stretchr/testify/require"
)

// Ports of SkillRunner's static helpers and SkillsController.kbCompileMarkdown.

func TestParseAgentOutputPlainMarkdown(t *testing.T) {
	summary, blocks := parseAgentOutput("  # Hello\nworld ")
	require.Equal(t, "  # Hello\nworld ", summary)
	require.Nil(t, blocks)
}

func TestParseAgentOutputBlocksEnvelope(t *testing.T) {
	summary, blocks := parseAgentOutput("```json\n{\"markdown\":\"short\",\"blocks\":[{\"type\":\"divider\"}]}\n```")
	require.Equal(t, "short", summary)
	require.NotNil(t, blocks)
	require.Len(t, *blocks, 1)
}

func TestParseAgentOutputFallsBackToContent(t *testing.T) {
	for _, content := range []string{
		`{"markdown":"x","blocks":[]}`,
		`{"markdown":"x"}`,
		`{"blocks":[{"no":"type"}]}`,
		`{not json`,
	} {
		summary, blocks := parseAgentOutput(content)
		require.Equal(t, content, summary, content)
		require.Nil(t, blocks, content)
	}
	// No markdown: the whole content is the summary.
	content := `{"blocks":[{"type":"divider"}]}`
	summary, blocks := parseAgentOutput(content)
	require.Equal(t, content, summary)
	require.NotNil(t, blocks)
}

func TestUsageDeltaPrefersHeaders(t *testing.T) {
	p, c := 7, 9
	resp := &agentResponse{}
	resp.Usage = &struct {
		PromptTokens     *int `json:"prompt_tokens,omitempty"`
		CompletionTokens *int `json:"completion_tokens,omitempty"`
	}{&p, &c}
	in, out := usageDelta(map[string]string{"X-Usage-Mtok-In": "100", "x-mtok-out": "nope"}, resp)
	require.Equal(t, 100, in)
	require.Equal(t, 9, out)
	in, out = usageDelta(nil, &agentResponse{})
	require.Zero(t, in)
	require.Zero(t, out)
}

func TestInputDescription(t *testing.T) {
	require.Empty(t, inputDescription(nil, nil))
	empty := ""
	require.Empty(t, inputDescription(&empty, map[string]string{}))
	in := "topic"
	require.Equal(t, "\nInput: topic\n\nArguments:\na=1\nb=2", inputDescription(&in, map[string]string{"b": "2", "a": "1"}))
}

func TestSkillSlug(t *testing.T) {
	require.Equal(t, "daily-brief", skillSlug("Daily  Brief"))
	require.Equal(t, "skill", skillSlug("--!!"))
	require.Equal(t, "a-b", skillSlug("a_-_b--"))
}

func TestValidateRelativePath(t *testing.T) {
	got, err := validateRelativePath("  memos/a.md \n")
	require.NoError(t, err)
	require.Equal(t, "memos/a.md", got)
	for _, bad := range []string{"", "  ", "/etc/passwd", "a/../b", ".."} {
		_, err := validateRelativePath(bad)
		require.Error(t, err, bad)
	}
}

func TestKBCompileMarkdown(t *testing.T) {
	require.Equal(t, "KB compile finished. No pending vault files needed ingestion.", kbCompileMarkdown(memorycompile.Response{}))
	u, d := 2, 40
	require.Equal(t, "KB compile finished.\n\n- Memories ingested: 3\n- Memories updated: 2\n- Duration: 40 ms",
		kbCompileMarkdown(memorycompile.Response{MemoriesIngested: 3, MemoriesUpdated: &u, DurationMs: &d}))
	require.Equal(t, "KB compile finished.\n\n- Memories ingested: 1", kbCompileMarkdown(memorycompile.Response{MemoriesIngested: 1}))
}

func TestToolDefinitionsShape(t *testing.T) {
	require.Len(t, availableTools, 13)
	b, err := json.Marshal(availableTools[0].definition())
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"function","function":{"name":"memory_upsert","description":"Persist a memory for the current tenant.",
		"parameters":{"type":"object","properties":{"content":{"type":"string","description":"Memory text to persist."},
		"source_vault_file_id":{"type":"string","description":"Optional source vault file UUID."}},"required":["content"]}}}`, string(b))
	b, err = json.Marshal(availableTools[7].definition())
	require.NoError(t, err)
	require.Contains(t, string(b), `"properties":{},"required":[]`)
}

func TestSwiftFloat(t *testing.T) {
	require.Equal(t, "1.0", swiftFloat(1))
	require.Equal(t, "0.25", swiftFloat(0.25))
}

func TestToolErrorJSON(t *testing.T) {
	require.JSONEq(t, `{"status":"error","reason":"nope"}`, toolErrorJSON("nope"))
}
