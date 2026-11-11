package hermesruns_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/hermesresolve"
	"lumina/internal/hermesruns"
	"lumina/internal/hermesruns/hermesrunstest"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// Ported from AppTests/Hermes/Runs/HermesRunsClientTests.swift (14 tests).

func clientErr(t *testing.T, err error) *hermesruns.ClientError {
	t.Helper()
	var ce *hermesruns.ClientError
	require.True(t, errors.As(err, &ce), "%v", err)
	return ce
}

func TestCapabilitiesRequiresBothApprovalEventsAndRunEventSSE(t *testing.T) {
	require.True(t, hermesruns.ParseCapabilities([]byte(hermesrunstest.SupportedCapabilities)).SupportsRuns())
	sseOnly := hermesruns.ParseCapabilities([]byte(`{"features":{"approval_events":false,"run_events_sse":true}}`))
	require.True(t, sseOnly.RunEventsSSE)
	require.False(t, sseOnly.SupportsRuns())
	garbage := hermesruns.ParseCapabilities([]byte("not json"))
	require.False(t, garbage.ApprovalEvents)
	require.False(t, garbage.SupportsRuns())
}

func TestRequireRunsSupportThrowsUnsupportedWhenTheGatewayLacksTheFlags(t *testing.T) {
	g := hermesrunstest.New(t)
	g.Stub("GET v1/capabilities", http.StatusOK, `{"features":{"approval_events":false,"run_events_sse":false}}`)
	err := g.Client("").RequireRunsSupport(context.Background())
	require.Equal(t, hermesruns.ErrUnsupported, clientErr(t, err).Kind)
}

func TestStartPostsThePromptAndReturnsTheHermesRunID(t *testing.T) {
	g := hermesrunstest.Accepting(t, "run_abc123")
	model, session := "hermes-3", "sess-1"
	id, err := g.Client("profile:stocks").Start(context.Background(), "audit the vault", &session, &model)
	require.NoError(t, err)
	require.Equal(t, "run_abc123", id)
	reqs := g.RequestsMatching("v1/runs")
	require.Len(t, reqs, 1)
	require.Equal(t, http.MethodPost, reqs[0].Method)
	require.Equal(t, "Bearer fake-token", reqs[0].Authorization)
	require.Equal(t, "profile:stocks", reqs[0].SessionKey)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(*reqs[0].Body), &body))
	require.Equal(t, map[string]any{"input": "audit the vault", "session_id": "sess-1", "model": "hermes-3"}, body)
}

func TestStartOmitsEmptyOptionalFields(t *testing.T) {
	g := hermesrunstest.Accepting(t, "run_x")
	empty := ""
	_, err := g.Client("").Start(context.Background(), "go", nil, &empty)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(*g.RequestsMatching("v1/runs")[0].Body), &body))
	require.Equal(t, map[string]any{"input": "go"}, body)
}

func TestStartThrowsInvalidResponseWhenHermesOmitsRunID(t *testing.T) {
	g := hermesrunstest.New(t)
	g.Stub("POST v1/runs", http.StatusAccepted, `{"accepted":true}`)
	_, err := g.Client("").Start(context.Background(), "go", nil, nil)
	require.Equal(t, hermesruns.ErrInvalidResponse, clientErr(t, err).Kind)
}

func TestStatusMapsTheHermesVocabularyOntoRunStatuses(t *testing.T) {
	cases := map[string]hermesruns.Status{
		"queued": hermesruns.Queued, "running": hermesruns.Running, "stopping": hermesruns.Running,
		"waiting_for_approval": hermesruns.WaitingForApproval, "completed": hermesruns.Completed,
		"failed": hermesruns.Failed, "cancelled": hermesruns.Stopped, "stopped": hermesruns.Stopped,
	}
	for raw, want := range cases {
		got, ok := hermesruns.Snapshot{Status: raw}.Mapped()
		require.True(t, ok, raw)
		require.Equal(t, want, got, raw)
	}
	_, ok := hermesruns.Snapshot{Status: "who-knows"}.Mapped()
	require.False(t, ok)
}

func TestStatusReadsTheRunSnapshot(t *testing.T) {
	g := hermesrunstest.New(t)
	g.Stub("GET v1/runs/run_1", http.StatusOK, `{"status":"completed","last_event":"run.completed","output":"done","session_id":"s1"}`)
	snap, err := g.Client("").Status(context.Background(), "run_1")
	require.NoError(t, err)
	mapped, _ := snap.Mapped()
	require.Equal(t, hermesruns.Completed, mapped)
	require.Equal(t, "done", *snap.Output)
	require.Equal(t, "s1", *snap.SessionID)
}

func TestApprovePostsTheChosenAnswer(t *testing.T) {
	g := hermesrunstest.Accepting(t, "run_1")
	require.NoError(t, g.Client("").Approve(context.Background(), "run_1", hermesruns.Session))
	reqs := g.RequestsMatching("v1/runs/run_1/approval")
	require.Len(t, reqs, 1)
	require.Equal(t, http.MethodPost, reqs[0].Method)
	require.JSONEq(t, `{"choice":"session"}`, *reqs[0].Body)
}

func TestStopPostsToTheStopEndpoint(t *testing.T) {
	g := hermesrunstest.Accepting(t, "run_1")
	require.NoError(t, g.Client("").Stop(context.Background(), "run_1"))
	require.Equal(t, http.MethodPost, g.RequestsMatching("v1/runs/run_1/stop")[0].Method)
}

func TestA404BecomesRunNotFoundAndA409BecomesApprovalNotPending(t *testing.T) {
	g := hermesrunstest.New(t)
	g.Stub("POST v1/runs/gone/stop", http.StatusNotFound, `{"error":{"code":"run_not_found"}}`)
	g.Stub("POST v1/runs/busy/approval", http.StatusConflict, `{"error":{"code":"approval_not_pending"}}`)
	require.Equal(t, hermesruns.ErrRunExpired, clientErr(t, g.Client("").Stop(context.Background(), "gone")).Kind)
	require.Equal(t, hermesruns.ErrApprovalNotPendingUpstream, clientErr(t, g.Client("").Approve(context.Background(), "busy", hermesruns.Deny)).Kind)
}

func TestA500BecomesAnUpstreamErrorCarryingTheHermesCode(t *testing.T) {
	g := hermesrunstest.New(t)
	g.Stub("GET v1/runs/boom", http.StatusInternalServerError, `{"error":{"code":"internal_error"}}`)
	_, err := g.Client("").Status(context.Background(), "boom")
	ce := clientErr(t, err)
	require.Equal(t, hermesruns.ErrUpstream, ce.Kind)
	require.Equal(t, http.StatusInternalServerError, ce.Status)
	require.Equal(t, "internal_error", *ce.Code)
}

func TestEventsYieldsTypedFramesInOrderAndFinishesWithTheStream(t *testing.T) {
	g := hermesrunstest.Accepting(t, "run_1")
	client := g.Client("")
	var frames []hermesruns.Frame
	done := make(chan error, 1)
	go func() {
		done <- client.Events(context.Background(), "run_1", func(f hermesruns.Frame) { frames = append(frames, f) })
	}()
	g.WaitForEventSubscription(t)
	g.Emit(`{"event":"run.started","run_id":"run_1"}`)
	g.EmitRaw(": keepalive\n\n")
	g.EmitRaw(`data: {"event":"tool.started",`)
	g.EmitRaw("\"tool\":\"shell\"}\n\n")
	g.Emit(`{"event":"run.completed","output":"all done"}`)
	g.FinishEvents()
	require.NoError(t, <-done)
	require.Len(t, frames, 3)
	require.Equal(t, []string{"run.started", "tool.started", "run.completed"}, []string{frames[0].Name, frames[1].Name, frames[2].Name})
	require.Equal(t, "shell", *frames[1].Event.Tool)
	require.Equal(t, "all done", *frames[2].Event.Summary)
}

func TestEventsThrowsWhenTheGatewayRejectsTheSubscription(t *testing.T) {
	g := hermesrunstest.Accepting(t, "run_1")
	g.FailEvents(http.StatusNotFound, `{"error":{"code":"run_not_found"}}`)
	err := g.Client("").Events(context.Background(), "run_1", func(hermesruns.Frame) {})
	require.Equal(t, hermesruns.ErrRunExpired, clientErr(t, err).Kind)
}

func TestMakeUsesTheManagedBearerAndABYOOverridesOwnHeader(t *testing.T) {
	managedURL, _ := url.Parse("http://managed")
	byoURL, _ := url.Parse("http://byo")
	managed := hermesruns.MakeClient(hermesresolve.Resolution{BaseURL: managedURL}, "central-key", "", nil)
	require.Equal(t, "Bearer central-key", managed.AuthHeader)
	token := "Token abc"
	byo := hermesruns.MakeClient(hermesresolve.Resolution{BaseURL: byoURL, AuthHeader: &token, IsUserOverride: true}, "central-key", "", nil)
	require.Equal(t, "Token abc", byo.AuthHeader)
	empty := ""
	open := hermesruns.MakeClient(hermesresolve.Resolution{BaseURL: byoURL, AuthHeader: &empty, IsUserOverride: true}, "central-key", "", nil)
	require.Empty(t, open.AuthHeader)
}

func TestEventsThrowsStreamIdleWhenTheGatewayStalls(t *testing.T) {
	g := hermesrunstest.Accepting(t, "run_1")
	client := g.Client("")
	client.StreamIdleTimeout = 100_000_000 // 100 ms
	err := client.Events(context.Background(), "run_1", func(hermesruns.Frame) {})
	require.Equal(t, hermesruns.ErrStreamIdle, clientErr(t, err).Kind)
}
