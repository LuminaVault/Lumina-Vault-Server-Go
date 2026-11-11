package llm

import (
	"context"
	"log/slog"
	"lumina/internal/swiftstr"
	"lumina/internal/telemetry"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ToolErrorCategory is ChatToolError.Category.
type ToolErrorCategory string

const (
	ToolErrorNotInstalled     ToolErrorCategory = "notInstalled"
	ToolErrorPermissionDenied ToolErrorCategory = "permissionDenied"
	ToolErrorTimeout          ToolErrorCategory = "timeout"
	ToolErrorLoopExhausted    ToolErrorCategory = "loopExhausted"
	ToolErrorOther            ToolErrorCategory = "other"
)

// ChatToolError is one Hermes tool-call failure found in assistant content. It is
// for logs and metrics only; it never reaches the wire.
type ChatToolError struct {
	ToolName    string
	Category    ToolErrorCategory
	Message     string // first line of the error, already single-lined
	IsRetryable bool
}

// notNewline stands in for Swift Regex's ".": without dotMatchesNewlines it
// matches no newline character, \r and U+2028 included, where Go's "." matches
// everything but \n.
const notNewline = `[^\n\r\x{0B}\x{0C}\x{85}\x{2028}\x{2029}]`

var (
	toolReturnedErrorRegex = regexp.MustCompile(`Tool ([A-Za-z0-9_.-]+) returned error \(` + notNewline + `*?\): (` + notNewline + `+)`)
	loopWarningRegex       = regexp.MustCompile(`same_tool_failure_warning; count=(\p{Nd}+); ([A-Za-z0-9_.-]+) has failed`)
	// stderrPatterns: a line matching any of these anywhere is dropped. "^" is the
	// start of the line, since each line is matched on its own.
	stderrPatterns = []*regexp.Regexp{
		regexp.MustCompile(`/usr/bin/bash: line \p{Nd}+: ` + notNewline + `*`),
		regexp.MustCompile(`/bin/bash: line \p{Nd}+: ` + notNewline + `*`),
		regexp.MustCompile(`^` + notNewline + `+?: command not found` + notNewline + `*`),
		regexp.MustCompile(`^No such file or directory` + notNewline + `*`),
		regexp.MustCompile(`^` + notNewline + `*: No module named ` + notNewline + `*`),
		regexp.MustCompile(`^Errno \p{Nd}+` + notNewline + `*`),
		regexp.MustCompile(`^Traceback \(most recent call last\):` + notNewline + `*`),
	}
)

// ClassifyToolErrors is HermesToolErrorClassifier.classify(content:): every
// "Tool X returned error (…): msg" line, then every same_tool_failure_warning.
func ClassifyToolErrors(content string) []ChatToolError {
	if content == "" {
		return nil
	}
	var out []ChatToolError
	for _, m := range toolReturnedErrorRegex.FindAllStringSubmatch(content, -1) {
		message := swiftstr.Prefix(m[2], 200)
		out = append(out, ChatToolError{
			ToolName: m[1], Category: categoriseToolError(message),
			Message: sanitizeSingleLine(message),
		})
	}
	for _, m := range loopWarningRegex.FindAllStringSubmatch(content, -1) {
		out = append(out, ChatToolError{
			ToolName: m[2], Category: ToolErrorLoopExhausted,
			Message: "Tool failed repeatedly and was disabled for this turn.",
		})
	}
	return out
}

// SanitizeToolOutput is HermesToolErrorClassifier.sanitize(content:): it drops
// stderr-shaped lines and trims the rest. ok is false when nothing is left (Swift's
// nil). An empty input comes back empty with ok true, as Swift returns it as is.
// Lines split as Swift's split(separator: "\n") does: a "\r\n" is one Character
// and does not split.
func SanitizeToolOutput(content string) (string, bool) {
	if content == "" {
		return content, true
	}
	lines := swiftstr.SplitLines(content)
	kept := lines[:0:0]
	for _, line := range lines {
		if !matchesStderr(line) {
			kept = append(kept, line)
		}
	}
	joined := swiftstr.TrimWhitespacesAndNewlines(strings.Join(kept, "\n"))
	if joined == "" {
		return "", false
	}
	return joined, true
}

// SanitizeAssistantMessage is HermesToolErrorClassifier.sanitize(message:): the
// content sanitized, collapsing to "" when only noise was left; role and tool
// calls kept.
func SanitizeAssistantMessage(m CompletionMessage) CompletionMessage {
	content, _ := SanitizeToolOutput(m.Content)
	m.Content = content
	return m
}

var toolFailureCounter = telemetry.Global.LLM.ToolFailure

// ObserveToolErrors is HermesToolErrorClassifier.observe: one warning and one
// counter increment per error.
func ObserveToolErrors(ctx context.Context, errs []ChatToolError, model, profile string) {
	for _, e := range errs {
		slog.WarnContext(ctx, "hermes.tool_failure", "tool", e.ToolName, "category", string(e.Category),
			"model", model, "profile", profile, "retryable", e.IsRetryable, "msg", e.Message)
		if toolFailureCounter != nil {
			toolFailureCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("tool", e.ToolName), attribute.String("category", string(e.Category))))
		}
	}
}

func matchesStderr(line string) bool {
	for _, p := range stderrPatterns {
		if p.MatchString(line) {
			return true
		}
	}
	return false
}

func categoriseToolError(raw string) ToolErrorCategory {
	m := strings.ToLower(raw)
	switch {
	case strings.Contains(m, "not installed"), strings.Contains(m, "command not found"),
		strings.Contains(m, "no such file or directory"), strings.Contains(m, "no module named"):
		return ToolErrorNotInstalled
	case strings.Contains(m, "permission denied"), strings.Contains(m, "eaccess"):
		return ToolErrorPermissionDenied
	case strings.Contains(m, "timed out"), strings.Contains(m, "timeout"), strings.Contains(m, "etimedout"):
		return ToolErrorTimeout
	}
	return ToolErrorOther
}

// sanitizeSingleLine replaces \n and \r with spaces, then trims.
func sanitizeSingleLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return swiftstr.TrimWhitespacesAndNewlines(s)
}
