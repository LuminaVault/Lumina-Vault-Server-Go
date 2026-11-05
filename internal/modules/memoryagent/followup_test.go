package memoryagent

// Ports Tests/AppTests/Memory/FollowUpGeneratorTests.swift

import (
	"context"
	"errors"
	"lumina/internal/wire"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func followUpHit(content string) HitDTO {
	return HitDTO{ID: wire.UUID{UUID: uuid.New()}, Content: content, Distance: 0.1}
}

func generator(transport *scriptedTransport, maxFollowUps int) *FollowUpGenerator {
	return &FollowUpGenerator{Transport: transport, DefaultModel: "test-model", MaxFollowUps: maxFollowUps}
}

func TestFollowUpsParsesJSONArrayFromAssistantMessage(t *testing.T) {
	transport := &scriptedTransport{steps: []step{plainStep(`{"follow_ups": ["Go deeper", "Compare with last month", "Save as memo"]}`)}}
	out := generator(transport, 0).Generate(context.Background(), "ferocious-fox", "", "You slept 8h this week.", []HitDTO{followUpHit("slept 9h Tuesday")})
	assert.Equal(t, []string{"Go deeper", "Compare with last month", "Save as memo"}, out)
}

func TestFollowUpsCapsAtSoftCeiling(t *testing.T) {
	transport := &scriptedTransport{steps: []step{plainStep(`{"follow_ups": ["a","b","c","d","e","f","g"]}`)}}
	out := generator(transport, 3).Generate(context.Background(), "u", "", "s", []HitDTO{followUpHit("hit")})
	assert.Equal(t, []string{"a", "b", "c"}, out)
}

func TestFollowUpsTrimsWhitespaceAndDropsEmptyEntries(t *testing.T) {
	transport := &scriptedTransport{steps: []step{plainStep(`{"follow_ups": ["  Go deeper ", "", "   ", "Save"]}`)}}
	out := generator(transport, 0).Generate(context.Background(), "u", "", "s", nil)
	assert.Equal(t, []string{"Go deeper", "Save"}, out)
}

func TestFollowUpsEmptyOnTransportFailure(t *testing.T) {
	transport := &scriptedTransport{fail: errors.New("upstream")}
	assert.Empty(t, generator(transport, 0).Generate(context.Background(), "u", "", "s", []HitDTO{followUpHit("h")}))
}

func TestFollowUpsEmptyOnMalformedAssistantJSON(t *testing.T) {
	transport := &scriptedTransport{steps: []step{plainStep("not json at all")}}
	assert.Empty(t, generator(transport, 0).Generate(context.Background(), "u", "", "s", nil))
}

func TestFollowUpsEmptyOnEmptyAssistantContent(t *testing.T) {
	transport := &scriptedTransport{steps: []step{plainStep("")}}
	assert.Empty(t, generator(transport, 0).Generate(context.Background(), "u", "", "s", nil))
}

func TestFollowUpsShortCircuitsWithoutUpstreamCall(t *testing.T) {
	transport := &scriptedTransport{steps: []step{plainStep(`{"follow_ups": ["should not appear"]}`)}}
	assert.Empty(t, generator(transport, 0).Generate(context.Background(), "u", "", "   ", nil))
	assert.Empty(t, transport.calls)
}

func TestFollowUpPromptTruncatesLongSources(t *testing.T) {
	messages := BuildFollowUpPrompt("s", []HitDTO{followUpHit(strings.Repeat("a", 500))}, 4)
	require.Len(t, messages, 2)
	user := messages[1].Content
	assert.Contains(t, user, "[1]")
	assert.Contains(t, user, strings.Repeat("a", 120))
	assert.NotContains(t, user, strings.Repeat("a", 121))
}

func TestFollowUpPromptWithNoSources(t *testing.T) {
	messages := BuildFollowUpPrompt("summary", nil, 4)
	require.Len(t, messages, 2)
	assert.Contains(t, messages[1].Content, "no source notes")
}
