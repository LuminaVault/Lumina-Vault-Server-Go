package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports HermesToolErrorClassifierTests (15 cases), names as in Swift.
func TestHermesToolErrorClassifier(t *testing.T) {
	t.Run("tool returned error parses tool name and message", func(t *testing.T) {
		raw := "Working on it…\n" +
			`Tool terminal returned error (0.23s): {"output": "/usr/bin/bash: line 3: pip: command not found", "exit_code": 127, "error": null}`
		errs := ClassifyToolErrors(raw)
		require.Len(t, errs, 1)
		assert.Equal(t, "terminal", errs[0].ToolName)
		assert.Equal(t, ToolErrorNotInstalled, errs[0].Category)
		assert.False(t, errs[0].IsRetryable)
	})
	t.Run("same_tool_failure_warning parses loop exhausted", func(t *testing.T) {
		errs := ClassifyToolErrors("[Tool loop warning: same_tool_failure_warning; count=3; terminal has failed 3 times this turn]")
		require.Len(t, errs, 1)
		assert.Equal(t, "terminal", errs[0].ToolName)
		assert.Equal(t, ToolErrorLoopExhausted, errs[0].Category)
		assert.Equal(t, "Tool failed repeatedly and was disabled for this turn.", errs[0].Message)
	})
	categories := []struct {
		name, raw string
		want      ToolErrorCategory
	}{
		{"youtube-transcript-api missing maps to notInstalled", `Tool terminal returned error (0.50s): {"output": "Error: youtube-transcript-api not installed. Run: pip install youtube-transcript-api", "exit_code": 1}`, ToolErrorNotInstalled},
		{"tirith spawn error maps to notInstalled", "Tool tirith_security returned error (0.10s): tirith spawn failed: [Errno 2] No such file or directory: 'tirith'", ToolErrorNotInstalled},
		{"timeout error maps to timeout category", "Tool web_search returned error (30.0s): request timed out after 30s", ToolErrorTimeout},
		{"permission denied maps to permissionDenied", "Tool file_write returned error (0.01s): EACCES: permission denied, open '/etc/hosts'", ToolErrorPermissionDenied},
		{"anything else is other", "Tool web_search returned error (1.0s): HTTP 500", ToolErrorOther},
	}
	for _, c := range categories {
		t.Run(c.name, func(t *testing.T) {
			errs := ClassifyToolErrors(c.raw)
			require.NotEmpty(t, errs)
			assert.Equal(t, c.want, errs[0].Category)
		})
	}
	t.Run("message is capped at 200 characters and single-lined", func(t *testing.T) {
		long := ""
		for range 300 {
			long += "é"
		}
		errs := ClassifyToolErrors("Tool x returned error (1s): " + long + "  ")
		require.Len(t, errs, 1)
		assert.Equal(t, 200, len([]rune(errs[0].Message)))
	})
	t.Run("empty content returns empty array", func(t *testing.T) {
		assert.Empty(t, ClassifyToolErrors(""))
	})
	t.Run("clean assistant content returns empty array", func(t *testing.T) {
		assert.Empty(t, ClassifyToolErrors("Hi! I'd suggest reviewing your notes from last week."))
	})

	t.Run("bash stderr line is stripped", func(t *testing.T) {
		out, ok := SanitizeToolOutput("Trying that now.\n/usr/bin/bash: line 3: pip: command not found\nI'll fall back to a manual approach.")
		require.True(t, ok)
		assert.Equal(t, "Trying that now.\nI'll fall back to a manual approach.", out)
	})
	t.Run("python no module named line is stripped", func(t *testing.T) {
		out, ok := SanitizeToolOutput("Running setup.\n/opt/hermes/.venv/bin/python: No module named pip\nLet me try another tool.")
		require.True(t, ok)
		assert.Equal(t, "Running setup.\nLet me try another tool.", out)
	})
	t.Run("multiple stderr lines all stripped", func(t *testing.T) {
		out, ok := SanitizeToolOutput("Hello.\n/usr/bin/bash: line 1: pip: command not found\n/usr/bin/bash: line 2: pip3: command not found\nDone.")
		require.True(t, ok)
		assert.Equal(t, "Hello.\nDone.", out)
	})
	t.Run("traceback errno and missing file lines are stripped", func(t *testing.T) {
		out, ok := SanitizeToolOutput("A\nTraceback (most recent call last):\nErrno 13 nope\nNo such file or directory: x\nB")
		require.True(t, ok)
		assert.Equal(t, "A\nB", out)
	})
	t.Run("sanitize collapses to nil when content was only noise", func(t *testing.T) {
		_, ok := SanitizeToolOutput("/usr/bin/bash: line 1: pip: command not found\n/usr/bin/bash: line 2: pip: command not found")
		assert.False(t, ok)
	})
	t.Run("sanitize preserves nil and empty inputs", func(t *testing.T) {
		out, ok := SanitizeToolOutput("")
		assert.True(t, ok)
		assert.Empty(t, out)
	})
	t.Run("CRLF does not split, as a Swift Character", func(t *testing.T) {
		// "\r\n" is one Character in Swift, so split(separator: "\n") keeps the
		// whole text as one line, and that line matches the bash pattern.
		_, ok := SanitizeToolOutput("Hello.\r\n/usr/bin/bash: line 1: pip: command not found")
		assert.False(t, ok)
	})

	t.Run("sanitize message keeps role and tool_calls", func(t *testing.T) {
		clean := SanitizeAssistantMessage(CompletionMessage{
			Role:      "assistant",
			Content:   "Working on it.\n/usr/bin/bash: line 1: pip: command not found\nDone.",
			ToolCalls: []ChatToolCall{{ID: "call_1", Type: "function", Function: ChatToolCallFunction{Name: "search", Arguments: "{}"}}},
		})
		assert.Equal(t, "assistant", clean.Role)
		require.Len(t, clean.ToolCalls, 1)
		assert.Equal(t, "call_1", clean.ToolCalls[0].ID)
		assert.Equal(t, "Working on it.\nDone.", clean.Content)
	})
	t.Run("sanitize message collapses empty content to empty string", func(t *testing.T) {
		clean := SanitizeAssistantMessage(CompletionMessage{Role: "assistant", Content: "/usr/bin/bash: line 1: pip: command not found"})
		assert.Empty(t, clean.Content)
	})
}
