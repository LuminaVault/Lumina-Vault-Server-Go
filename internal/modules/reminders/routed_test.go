package reminders_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/llm"
	"lumina/internal/modules/reminders"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// routedStub is an in-process llm.ChatTransport (never a real key).
type routedStub struct {
	reply              string
	err                error
	sessionKey, sessID string
}

func (s *routedStub) ChatCompletions(_ context.Context, _ []byte, sessionKey, sessionID string) (llm.Metadata, error) {
	s.sessionKey, s.sessID = sessionKey, sessionID
	if s.err != nil {
		return llm.Metadata{}, s.err
	}
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": s.reply}}}})
	return llm.Metadata{Data: data, Headers: map[string]string{"x-lv-provider": "stub"}}, nil
}

func (s *routedStub) ChatStream(context.Context, []byte, string, string) llm.Stream { return nil }

// The routed adapter hands the classifier the response body and passes the
// session key through with no session id, as Swift's routedTransport does.
func TestRoutedTransportFeedsClassifier(t *testing.T) {
	stub := &routedStub{reply: `{"isReminder":true,"title":"Call mom","fireAt":"2026-06-09T17:00:00Z","recurrenceCron":"","scheduleHuman":"Today at 5:00 PM"}`}
	c := reminders.Classifier{Transport: reminders.RoutedTransport{LLM: stub}, Model: "hermes-3"}
	got := c.Classify(context.Background(), "remind me to call mom at 5pm", "A11CE000-0000-4000-8000-000000000001", time.Now())
	require.True(t, got.IsReminder)
	require.Equal(t, "A11CE000-0000-4000-8000-000000000001", stub.sessionKey)
	require.Empty(t, stub.sessID)
}

func TestRoutedTransportErrorFailsClosed(t *testing.T) {
	stub := &routedStub{err: errors.New("every provider failed")}
	raw, err := reminders.RoutedTransport{LLM: stub}.ChatCompletions(context.Background(), []byte(`{}`), "k")
	require.Error(t, err)
	require.Nil(t, raw)
	c := reminders.Classifier{Transport: reminders.RoutedTransport{LLM: stub}, Model: "hermes-3"}
	require.False(t, c.Classify(context.Background(), "remind me", "k", time.Now()).IsReminder)
}
