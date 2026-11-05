package llm

import (
	"lumina/internal/swiftstr"
	"slices"
	"strings"
)

// ClassifyTask is RouterTaskClassifier.classify. Keyword order decides ties.
func ClassifyTask(prompt string, surface RouterSurface) RouterTaskType {
	if surface == SurfaceJob || surface == SurfaceSkill {
		return TaskAutomation
	}
	value := strings.ToLower(prompt)
	switch {
	case containsAny(value, "search", "latest", "today", "news", "find online", "look up", "research"):
		return TaskSearch
	case containsAny(value, "code", "swift", "typescript", "python", "sql", "debug", "compile", "api", "function"):
		return TaskCoding
	case containsAny(value, "summarize", "summary", "tl;dr", "condense", "key points"):
		return TaskSummarization
	case containsAny(value, "extract", "parse", "fields", "entities", "json from"):
		return TaskExtraction
	case containsAny(value, "write a story", "poem", "brainstorm", "creative", "tagline", "imagine"):
		return TaskCreative
	case containsAny(value, "reason", "analyze", "compare", "trade-off", "prove", "why", "plan"):
		return TaskReasoning
	}
	return TaskGeneral
}

func containsAny(value string, needles ...string) bool {
	return slices.ContainsFunc(needles, func(n string) bool { return strings.Contains(value, n) })
}

// ClassifyComplexity is ComplexityClassifier.classify: a heuristic for Auto's tier
// floor, biased up when uncertain. Jobs and skills floor at medium.
func ClassifyComplexity(prompt string, surface RouterSurface) RouterComplexity {
	floor := ComplexityLow
	if surface == SurfaceJob || surface == SurfaceSkill {
		floor = ComplexityMedium
	}
	trimmed := swiftstr.TrimWhitespacesAndNewlines(prompt)
	if trimmed == "" {
		return floor
	}
	lower := strings.ToLower(trimmed)
	tokenEstimate := max(1, swiftstr.CharacterCount(trimmed)/4)
	hasCodeFence := strings.Count(trimmed, "```") >= 2
	lineCount := swiftLineCount(trimmed)

	score := 0
	switch {
	case tokenEstimate >= 2000 || lineCount >= 40:
		score += 3
	case tokenEstimate >= 400 || lineCount >= 12:
		score++
	}
	if hasCodeFence {
		score += 2
	}
	if swiftNewlineCharacters(trimmed) > 20 {
		score++
	}
	if containsAny(lower, "debug", "refactor", "architecture", "prove", "optimize algorithm",
		"lock-free", "race condition", "distributed", "multi-step",
		"deep analysis", "trade-off", "tradeoff", "implement end-to-end",
		"production-ready", "security audit", "performance profile") {
		score += 3
	}
	if containsAny(lower, "plan", "compare", "design", "implement", "write a", "explain why",
		"analyze", "review", "migrate", "api", "function", "class ") {
		score++
	}

	// "hi" is a substring match, so most short prompts count as trivial. Swift's
	// hasPrefix-or-contains reduces to contains.
	lowSignal := containsAny(lower, "hi", "hello", "thanks", "thank you", "what is ", "what's ",
		"define ", "tl;dr", "summarize this", "2+2", "quick question")
	words := strings.FieldsFunc(lower, func(r rune) bool { return r == ' ' })
	if tokenEstimate < 80 && !hasCodeFence && (lowSignal || len(words) <= 12) && score == 0 {
		return floor
	}

	raw := ComplexityLow
	switch {
	case score >= 4:
		raw = ComplexityHigh
	case score >= 1:
		raw = ComplexityMedium
	}
	return maxComplexity(floor, raw)
}

// swiftLineCount is split(separator: "\n", omittingEmptySubsequences: false).count.
// A Swift Character "\r\n" is not "\n", so a CRLF does not start a line.
func swiftLineCount(s string) int {
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' && (i == 0 || s[i-1] != '\r') {
			n++
		}
	}
	return n
}

// swiftNewlineCharacters is filter(\.isNewline).count: CRLF is one Character.
func swiftNewlineCharacters(s string) int {
	n := 0
	for i, r := range s {
		switch r {
		case '\n':
			if i == 0 || s[i-1] != '\r' {
				n++
			}
		case '\v', '\f', '\r', 0x85, 0x2028, 0x2029:
			n++
		}
	}
	return n
}
