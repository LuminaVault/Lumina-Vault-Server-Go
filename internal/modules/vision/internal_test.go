package vision

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// VisionContentTypeTests in Vision/VisionSearchLimitTests.swift.
func TestContentType(t *testing.T) {
	header := func(values ...string) http.Header { return http.Header{"Content-Type": values} }

	t.Run("parameters and casing are stripped so the allowlist matches", func(t *testing.T) {
		require.Equal(t, "image/png", contentType(header("image/png")))
		require.Equal(t, "image/png", contentType(header("IMAGE/PNG")))
		require.Equal(t, "image/jpeg", contentType(header("image/jpeg; charset=binary")))
		require.Equal(t, "image/webp", contentType(header(" image/webp ")))
	})
	t.Run("a missing content type yields empty rather than crashing", func(t *testing.T) {
		require.Empty(t, contentType(http.Header{}))
		require.False(t, acceptedMimes[""])
	})
	t.Run("split drops empty pieces", func(t *testing.T) {
		require.Equal(t, "image/webp", contentType(header(";image/webp")))
		require.Empty(t, contentType(header(";;")))
		require.Empty(t, contentType(header(" ;image/webp")), "a blank piece isn't empty")
	})
	t.Run("repeated headers are one value, which no allowlist entry matches", func(t *testing.T) {
		require.Equal(t, "image/png, image/jpeg", contentType(header("image/png", "image/jpeg")))
	})
}

func TestPadOrTruncate(t *testing.T) {
	require.Equal(t, []float32{1, 2, 0, 0}, padOrTruncate([]float32{1, 2}, 4))
	require.Equal(t, []float32{1, 2}, padOrTruncate([]float32{1, 2}, 2))
	require.Equal(t, []float32{1, 2}, padOrTruncate([]float32{1, 2, 3}, 2))
}

func TestRegistryPicksOneActiveAdapter(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stub := Config{StubDim: 2, StubFill: 0.5, StubModel: "m"}

	for _, c := range []struct {
		name         string
		cfg          Config
		kind         string
		wantProvider bool
	}{
		{"default with no key", Config{Provider: "cohere"}, kindCohere, false},
		{"cohere with a key", Config{Provider: "cohere", CohereAPIKey: "k"}, kindCohere, true},
		{"trimmed and lowercased", Config{Provider: " COHERE\n", CohereAPIKey: "k"}, kindCohere, true},
		{"unknown reads as cohere", Config{Provider: "bogus", CohereAPIKey: "k"}, kindCohere, true},
		{"empty reads as cohere", Config{CohereAPIKey: "k"}, kindCohere, true},
		{"hermesVision can't match once lowercased", Config{Provider: "hermesVision", CohereAPIKey: "k"}, kindCohere, true},
		{"a kind with no adapter ignores the Cohere key", Config{Provider: "openai", CohereAPIKey: "k"}, kindOpenAI, false},
		{"replicate has no adapter either", Config{Provider: "replicate"}, kindRepl, false},
		{"stub, lowercased", Config{Provider: "STUB", StubDim: stub.StubDim}, kindStub, true},
		{"stub wins over a Cohere key", Config{Provider: "stub", CohereAPIKey: "k"}, kindStub, true},
	} {
		reg := newRegistry(c.cfg, log)
		require.Equal(t, c.kind, reg.kind, c.name)
		require.Equal(t, c.wantProvider, reg.active != nil, c.name)
	}
}

func TestDecodeCohere(t *testing.T) {
	vec, err := decodeCohere([]byte(`{"id":"x","embeddings":{"float":[[0.5,1],[2]]}}`))
	require.NoError(t, err)
	require.Equal(t, []float32{0.5, 1}, vec)
	for _, bad := range []string{
		`not json`, `{}`, `{"embeddings":{}}`, `{"embeddings":{"float":null}}`, `{"embeddings":{"float":[]}}`,
		`{"embeddings":{"float":[[]]}}`, `{"embeddings":{"float":[[0.5,null]]}}`, `{"embeddings":{"float":[[0.5],null]}}`,
		`{"embeddings":{"float":[[1e100]]}}`,
	} {
		_, err := decodeCohere([]byte(bad))
		require.Error(t, err, bad)
	}
}
