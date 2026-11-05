package memoryagent

// Ports Tests/AppTests/Memory/QueryStreamTests.swift

import (
	"encoding/json"
	"lumina/internal/memorystore"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFrame(t *testing.T, event any) string {
	t.Helper()
	w := httptest.NewRecorder()
	require.True(t, frame(w, http.NewResponseController(w), event))
	return w.Body.String()
}

func frameJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	require.True(t, strings.HasPrefix(raw, "data: "), raw)
	require.True(t, strings.HasSuffix(raw, "\n\n"), raw)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(raw, "data: "), "\n\n")), &out))
	return out
}

func TestSSETokenEventEncodesAsDataColonJSONDoubleNewline(t *testing.T) {
	decoded := frameJSON(t, writeFrame(t, StreamEvent{Type: "token", Payload: "hello"}))
	assert.Equal(t, "token", decoded["type"])
	assert.Equal(t, "hello", decoded["payload"])
}

func TestSSESourceEventRoundTripsThroughWireFormat(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	hit := HitDTO{ID: wire.UUID{UUID: id}, Content: "slept 9h on Tuesday", Distance: 0.21}
	decoded := frameJSON(t, writeFrame(t, StreamEvent{Type: "source", Payload: hit}))
	assert.Equal(t, "source", decoded["type"])
	payload, ok := decoded["payload"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", payload["id"])
	assert.Equal(t, "slept 9h on Tuesday", payload["content"])
	assert.InDelta(t, 0.21, payload["distance"], 1e-6)
	assert.NotContains(t, payload, "createdAt")
}

func TestDoneEventHasNoPayloadKeyOnWire(t *testing.T) {
	decoded := frameJSON(t, writeFrame(t, StreamEvent{Type: "done"}))
	assert.Equal(t, "done", decoded["type"])
	assert.NotContains(t, decoded, "payload")
}

func TestBuildPromptEmbedsHitsAsNumberedContextBlock(t *testing.T) {
	hits := []memorystore.SearchResult{
		{ID: uuid.New(), Content: "ran 5k Monday", Distance: 0.1, Source: memorystore.SourceLegacy},
		{ID: uuid.New(), Content: "skipped run Tuesday", Distance: 0.2, Source: memorystore.SourceLegacy},
	}
	messages := BuildQueryPrompt("what runs did I do?", hits, false)
	require.Len(t, messages, 2)
	assert.Equal(t, "system", messages[0].Role)
	assert.Equal(t, "user", messages[1].Role)
	assert.Equal(t, "what runs did I do?", messages[1].Content)
	assert.Contains(t, messages[0].Content, "[1] [legacy] ran 5k Monday")
	assert.Contains(t, messages[0].Content, "[2] [legacy] skipped run Tuesday")
}

func TestBuildPromptWithNoHitsSurfacesFallbackContext(t *testing.T) {
	messages := BuildQueryPrompt("what?", nil, false)
	assert.Contains(t, messages[0].Content, "no relevant memories")
}
