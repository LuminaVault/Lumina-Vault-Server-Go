package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ProviderStreamKitTests cases with no exact Go counterpart.
func TestAuditDProviderStreamKit(t *testing.T) {
	t.Run("named non-content event is ignored", func(t *testing.T) {
		chunks, done, err := collectRecord(t, "event: hermes.tool.progress\ndata: {\"tool\":\"search\"}")
		require.NoError(t, err)
		assert.Empty(t, chunks)
		assert.False(t, done)
	})
	t.Run("OpenAI inline error throws", func(t *testing.T) {
		_, _, err := collectRecord(t, `data: {"error":{"message":"rate limited"}}`)
		var perr *ProviderError
		require.ErrorAs(t, err, &perr)
		assert.Equal(t, "rate limited", *perr.Body)
		assert.Zero(t, perr.Status)
	})
	t.Run("Anthropic content_block_delta yields text", func(t *testing.T) {
		var state AnthropicStreamState
		var chunks []Chunk
		done, err := ProcessAnthropicRecord("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}",
			&state, func(c Chunk) { chunks = append(chunks, c) })
		require.NoError(t, err)
		assert.Equal(t, []Chunk{{Delta: "Hi"}}, chunks)
		assert.False(t, done)
	})
	t.Run("Anthropic message_stop ends the stream", func(t *testing.T) {
		var state AnthropicStreamState
		var chunks []Chunk
		done, err := ProcessAnthropicRecord("event: message_stop\ndata: {\"type\":\"message_stop\"}",
			&state, func(c Chunk) { chunks = append(chunks, c) })
		require.NoError(t, err)
		assert.Empty(t, chunks)
		assert.True(t, done)
	})
}

// OpenAICompletionReaderTests' AgentToolCallsShapeTests: decoding follows
// AgentToolCalls.init(from:).
func TestAuditDAgentToolCallsDecode(t *testing.T) {
	t.Run("a legacy record without a count infers it from names", func(t *testing.T) {
		var decoded AgentToolCalls
		require.NoError(t, json.Unmarshal([]byte(`{"names":["a","b","c"]}`), &decoded))
		assert.Equal(t, 3, decoded.Count)
	})
	t.Run("an empty record means the turn ran without tools", func(t *testing.T) {
		var decoded AgentToolCalls
		require.NoError(t, json.Unmarshal([]byte(`{"names":[],"count":0}`), &decoded))
		assert.Empty(t, decoded.Names)
		assert.Equal(t, len(decoded.Names), decoded.Count)
	})
	t.Run("a count without names decodes to an empty list", func(t *testing.T) {
		var decoded AgentToolCalls
		require.NoError(t, json.Unmarshal([]byte(`{"count":3}`), &decoded))
		assert.Equal(t, AgentToolCalls{Names: []string{}, Count: 3}, decoded)
	})
	t.Run("names imply a matching count", func(t *testing.T) {
		assert.Equal(t, &AgentToolCalls{Names: []string{"a", "b"}, Count: 2}, ToolRecord([]string{"a", "b"}, nil))
	})
	t.Run("the reader sees a tool call where the probe would", func(t *testing.T) {
		withTools := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"t","arguments":"{}"}}]}}]}`)
		withProse := []byte(`{"choices":[{"message":{"role":"assistant","content":"no tools here"}}]}`)
		assert.NotEmpty(t, ToolCallNamesFrom(withTools))
		assert.Empty(t, ToolCallNamesFrom(withProse))
	})
}
