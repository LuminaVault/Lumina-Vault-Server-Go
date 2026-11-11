package hermesruns

import (
	"context"
	"lumina/internal/apns"
	"lumina/internal/wire"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Ported from AppTests/Hermes/Runs/HermesRunPushNotifierTests.swift (3 of 4; the
// prefs-table test is waived: apns_test.go covers category gating).

type recordingPusher struct {
	mu    sync.Mutex
	sends []apns.Message
}

func (p *recordingPusher) Notify(_ context.Context, _ uuid.UUID, m apns.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sends = append(p.sends, m)
	return nil
}

func pushRun(status Status, pending *PendingApprovalDTO, summary, errText *string) DTO {
	return DTO{
		ID: wire.UUID{UUID: uuid.New()}, HermesRunID: "run_push", Status: status, Prompt: "clean the temp dir",
		StartedAt: wire.Time{Time: time.Now()}, LastSeq: 1, PendingApproval: pending, Summary: summary, Error: errText,
	}
}

func TestAnApprovalRequestPushesTheActionableApprovalCategory(t *testing.T) {
	p := &recordingPusher{}
	cmd := "rm -rf /tmp/build"
	run := pushRun(WaitingForApproval, &PendingApprovalDTO{Command: &cmd, Choices: []Choice{Once, Deny}}, nil, nil)
	APNSNotifier{Push: p}.ApprovalRequested(context.Background(), uuid.New(), run)
	require.Len(t, p.sends, 1)
	send := p.sends[0]
	require.Equal(t, apns.CategoryApproval, send.Category)
	require.Equal(t, approvalTitle, send.Title)
	require.Equal(t, cmd, send.Body)
	require.Equal(t, run.ID.String(), send.Payload["runID"])
	require.Equal(t, "run_push", send.Payload["hermesRunID"])
	require.Equal(t, "once,deny", send.Payload["choices"])
}

func TestAFinishedRunPushesTheRunCompletedCategoryWithItsOutcome(t *testing.T) {
	p := &recordingPusher{}
	n := APNSNotifier{Push: p}
	n.RunFinished(context.Background(), uuid.New(), pushRun(Completed, nil, sp("removed 12 files"), nil))
	n.RunFinished(context.Background(), uuid.New(), pushRun(Failed, nil, nil, sp("shell exited 1")))
	require.Len(t, p.sends, 2)
	for _, s := range p.sends {
		require.Equal(t, apns.CategoryRunCompleted, s.Category)
	}
	require.Equal(t, "removed 12 files", p.sends[0].Body)
	require.Equal(t, "Hermes finished a run", p.sends[0].Title)
	require.Equal(t, "shell exited 1", p.sends[1].Body)
	require.Equal(t, "Hermes run failed", p.sends[1].Title)
}

func TestAnApprovalWithNoCommandStillSaysSomethingUseful(t *testing.T) {
	p := &recordingPusher{}
	APNSNotifier{Push: p}.ApprovalRequested(context.Background(), uuid.New(), pushRun(WaitingForApproval, nil, nil, nil))
	require.Equal(t, "A tool call is waiting for your approval", p.sends[0].Body)
	require.Equal(t, "once,session,always,deny", p.sends[0].Payload["choices"])
}
