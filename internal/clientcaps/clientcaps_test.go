package clientcaps_test

import (
	"lumina/internal/clientcaps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// HTTP/ClientCapabilitiesTests.swift.
func TestAbsentHeaderSupportsNothing(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	require.False(t, clientcaps.FromRequest(r).Supports(clientcaps.ChatHermesRun),
		"an old iOS build never sends the header and dies on an unknown frame")
	require.False(t, clientcaps.Capabilities{}.Supports(clientcaps.ChatHermesRun))
}

func TestEmptyHeaderSupportsNothing(t *testing.T) {
	for _, value := range []string{"", "   ", ",,"} {
		require.False(t, clientcaps.Parse(value).Supports(clientcaps.ChatHermesRun), value)
	}
}

func TestDeclaredCapabilityIsRecognised(t *testing.T) {
	require.True(t, clientcaps.Parse("chat.hermes_run").Supports(clientcaps.ChatHermesRun))
}

func TestOrderSpacingAndCaseDoNotMatter(t *testing.T) {
	for _, value := range []string{
		"chat.hermes_run",
		" chat.hermes_run ",
		"CHAT.HERMES_RUN",
		"something.else, chat.hermes_run",
		"chat.hermes_run,something.else",
	} {
		require.True(t, clientcaps.Parse(value).Supports(clientcaps.ChatHermesRun), value)
	}
}

func TestUnknownTokensAreIgnored(t *testing.T) {
	require.True(t, clientcaps.Parse("chat.future_thing, chat.hermes_run, nonsense").Supports(clientcaps.ChatHermesRun))
}

func TestNearMissDoesNotCount(t *testing.T) {
	for _, value := range []string{"chat.hermes", "chat.hermes_runs", "hermes_run"} {
		require.False(t, clientcaps.Parse(value).Supports(clientcaps.ChatHermesRun), value)
	}
}

// Swift trims .whitespaces, which doesn't include newlines.
func TestNewlineIsNotTrimmed(t *testing.T) {
	require.False(t, clientcaps.Parse("chat.hermes_run\n").Supports(clientcaps.ChatHermesRun))
}

// HTTPFields' subscript joins repeated lines, so the token can be on any of them.
func TestRepeatedHeaderLinesAreJoined(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Add(clientcaps.HeaderName, "something.else")
	r.Header.Add(clientcaps.HeaderName, "chat.hermes_run")
	require.True(t, clientcaps.FromRequest(r).Supports(clientcaps.ChatHermesRun))
}
