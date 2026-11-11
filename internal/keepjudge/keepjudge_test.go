package keepjudge

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/llm"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Ports MemoryKeepGateTests.swift's parser test. Its other test drives
// MemoryIndexWorker, which is W5's job port; see the 7B report.
func TestParseFailsOpenOnAnythingUnusable(t *testing.T) {
	d, ok := Parse(`{"keep":false,"kind":null}`)
	require.True(t, ok)
	require.Equal(t, Decision{Keep: false}, d)
	d, ok = Parse(`Sure. {"keep":true,"kind":"preference"}`)
	require.True(t, ok)
	require.Equal(t, Decision{Keep: true, Kind: "preference"}, d)
	d, ok = Parse(`{"keep":false,"kind":"preference"}`)
	require.True(t, ok)
	require.Empty(t, d.Kind, "a dropped reply has no kind")
	_, ok = Parse("no idea")
	require.False(t, ok)

	d, ok = Parse(`{"keep":1,"kind":"opinion"}`)
	require.True(t, ok)
	require.Equal(t, Decision{Keep: true}, d, "NSNumber 1 bridges to true; an unknown kind is none")
	_, ok = Parse(`{"keep":"yes"}`)
	require.False(t, ok)
	_, ok = Parse(`[{"keep":true}]`)
	require.True(t, ok, "first { to last } is an object even inside an array")
}

type stub struct {
	reply   string
	fail    error
	payload []byte
	key     string
}

func (s *stub) ChatCompletions(_ context.Context, p []byte, key, _ string) (llm.Metadata, error) {
	s.payload, s.key = p, key
	if s.fail != nil {
		return llm.Metadata{}, s.fail
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": s.reply}}}})
	return llm.Metadata{Data: b}, nil
}

func (s *stub) ChatStream(context.Context, []byte, string, string) llm.Stream { return nil }

func TestJudge(t *testing.T) {
	ctx, tenant := context.Background(), uuid.New()

	s := &stub{reply: `{"keep":true,"kind":"decision"}`}
	d, err := Judge{Transport: s, Model: "m"}.Judge(ctx, strings.Repeat("é", 5000), tenant)
	require.NoError(t, err)
	require.Equal(t, Decision{Keep: true, Kind: "decision"}, d)
	require.Equal(t, strings.ToUpper(tenant.String()), s.key)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(s.payload, &sent))
	require.NotContains(t, sent, "temperature", "Haiku 5.5 rejects sampling parameters")
	require.Equal(t, false, sent["stream"])
	msgs := sent["messages"].([]any)
	require.Len(t, []rune(msgs[1].(map[string]any)["content"].(string)), MaxChars)

	d, err = Judge{Transport: &stub{reply: "dunno"}}.Judge(ctx, "x", tenant)
	require.NoError(t, err)
	require.Equal(t, KeepUnclassified, d)

	_, err = Judge{Transport: &stub{fail: errors.New("down")}}.Judge(ctx, "x", tenant)
	require.Error(t, err)

	byok := &stub{reply: `{"keep":false}`}
	d, err = Judge{Transport: byok, IsBYOK: func(context.Context, uuid.UUID) bool { return true }}.Judge(ctx, "x", tenant)
	require.NoError(t, err)
	require.Equal(t, KeepUnclassified, d)
	require.Nil(t, byok.payload, "a BYOK tenant's reply never reaches the managed model")
}
