package embedding

import (
	"lumina/internal/swiftstr"
	"strings"
)

// Kind is EmbeddingProviderKind. The values are Swift's raw values, which only
// appear in logs.
type Kind string

const (
	KindOpenAI        Kind = "openai"
	KindOpenRouter    Kind = "openRouter"
	KindHermesLocal   Kind = "hermesLocal"
	KindNomic         Kind = "nomic"
	KindDeterministic Kind = "deterministic"
)

// ParseKind is EmbeddingProviderKind(rawConfigValue:).
func ParseKind(raw string) (Kind, bool) {
	switch strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(raw)) {
	case "openai":
		return KindOpenAI, true
	case "openrouter", "open_router", "open-router":
		return KindOpenRouter, true
	case "hermeslocal", "hermes_local", "hermes-local", "local", "localhermes":
		return KindHermesLocal, true
	case "nomic":
		return KindNomic, true
	case "deterministic", "dev", "stub":
		return KindDeterministic, true
	}
	return "", false
}
