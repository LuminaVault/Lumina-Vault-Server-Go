package grok

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// JSONDecoder.KeyDecodingStrategy.convertFromSnakeCase, from swift-foundation.
func TestFromSnakeCase(t *testing.T) {
	for key, want := range map[string]string{
		"model":              "model",
		"promptTokens":       "promptTokens",
		"prompt_tokens":      "promptTokens",
		"published_at":       "publishedAt",
		"PROMPT_TOKENS":      "promptTokens",
		"Completion__Tokens": "completionTokens",
		"_model":             "_model",
		"_prompt_tokens_":    "_promptTokens_",
		"CHOICES":            "CHOICES",
		"_":                  "_",
		"":                   "",
	} {
		require.Equal(t, want, fromSnakeCase(key), key)
	}
}
