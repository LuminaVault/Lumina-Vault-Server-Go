package hermesruns_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/dbtest"
	"lumina/internal/hermesresolve"
	"lumina/internal/hermesruns"
	"lumina/internal/hermesruns/hermesrunstest"
	"lumina/internal/migrations"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Ported from AppTests/Hermes/Runs/HermesRunsServiceTests.swift (17 tests),
// HermesRunConversationPersistenceTests.swift (5), HermesRunsListFilterTests.swift
// (4), HermesRunPersistenceTests.swift (8, the 2 migration cases against the goose
// baseline) and HermesRunPushNotifierTests.swift's prefs-table case.

func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), p, migrations.SQL()))
	return p
}

type harness struct {
	t           *testing.T
	pool        *pgxpool.Pool
	gateway     *hermesrunstest.Gateway
	notifier    *hermesrunstest.Notifier
	service     *hermesruns.Service
	tenant      uuid.UUID
	hermesRunID string
}

func newUser(t *testing.T, p *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	suffix := strings.ReplaceAll(id.String(), "-", "")[:12]
	_, err := p.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'stub', $3, now(), now())`, id, "runs-"+suffix+"@test.luminavault", "runs"+suffix)
	require.NoError(t, err)
	return id
}

func withHarness(t *testing.T, limits hermesruns.Limits, p *pgxpool.Pool) *harness {
	t.Helper()
	h := &harness{t: t, pool: p, gateway: hermesrunstest.Accepting(t, "run_1"), notifier: &hermesrunstest.Notifier{}, hermesRunID: "run_1"}
	h.tenant = newUser(t, p)
	base, _ := url.Parse("http://hermes.test")
	h.service = hermesruns.NewService(hermesruns.Options{
		Pool: p, Notifier: h.notifier, Limits: limits,
		Resolve: func(context.Context, uuid.UUID) (hermesresolve.Resolution, error) {
			return hermesresolve.Resolution{BaseURL: base}, nil
		},
		MakeClient: func(_ hermesresolve.Resolution, sessionKey string) *hermesruns.Client {
			return h.gateway.Client(sessionKey)
		},
	})
	t.Cleanup(func() {
		h.gateway.FinishEvents()
		h.service.StopAll()
	})
	return h
}

func (h *harness) start(prompt string) hermesruns.DTO {
	h.t.Helper()
	run, err := h.service.Start(context.Background(), h.tenant, hermesruns.StartRequest{Prompt: prompt}, "")
	require.NoError(h.t, err)
	return run
}

func (h *harness) waitForRun(id uuid.UUID, cond func(hermesruns.DTO) bool) hermesruns.DTO {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.service.Get(context.Background(), h.tenant, id)
		require.NoError(h.t, err)
		if cond(run) {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatal("run never reached the expected state")
	return hermesruns.DTO{}
}

func (h *harness) insertRun(r *hermesruns.Run) {
	h.t.Helper()
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.Tenant == uuid.Nil {
		r.Tenant = h.tenant
	}
	require.NoError(h.t, h.service.Store().Insert(context.Background(), r))
}

// isolate settles other cases' still-active runs: re-attach is process-wide and
// the cases share one database.
func (h *harness) isolate() {
	h.t.Helper()
	_, err := h.pool.Exec(context.Background(), `UPDATE hermes_runs SET status = 'completed'
		WHERE tenant_id <> $1 AND status IN ('queued', 'running', 'waiting_for_approval')`, h.tenant)
	require.NoError(h.t, err)
}

func serviceErr(t *testing.T, err error, want hermesruns.ServiceError) {
	t.Helper()
	se, ok := hermesruns.IsServiceError(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, want, se)
}

func eventNames(events []hermesruns.EventDTO) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Event
	}
	return out
}

func caseStartPersistsTheRunAndTheWatcherRecordsEveryEventInOrder(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	run, err := h.service.Start(context.Background(), h.tenant, hermesruns.StartRequest{Prompt: "audit the vault"}, "profile:default")
	require.NoError(t, err)
	require.Equal(t, "run_1", run.HermesRunID)
	require.Equal(t, hermesruns.Running, run.Status)
	require.Equal(t, 0, run.LastSeq)
	require.Equal(t, "profile:default", h.gateway.RequestsMatching("v1/runs")[0].SessionKey)
	h.gateway.WaitForEventSubscription(t)
	h.gateway.Emit(`{"event":"run.started"}`)
	h.gateway.Emit(`{"event":"tool.started","tool":"shell"}`)
	h.gateway.Emit(`{"event":"tool.completed","tool":"shell"}`)
	h.gateway.Emit(`{"event":"run.completed","output":"vault is clean"}`)
	h.gateway.FinishEvents()
	finished := h.waitForRun(run.ID.UUID, func(r hermesruns.DTO) bool { return r.Status == hermesruns.Completed })
	require.Equal(t, "vault is clean", *finished.Summary)
	require.NotNil(t, finished.FinishedAt)
	require.Equal(t, "run.completed", *finished.LastEvent)
	require.Equal(t, 4, finished.LastSeq)
	events, err := h.service.Events(context.Background(), h.tenant, run.ID.UUID, 0, 500)
	require.NoError(t, err)
	require.Equal(t, []string{"run.started", "tool.started", "tool.completed", "run.completed"}, eventNames(events))
	require.Equal(t, 4, events[3].Seq)
	tail, err := h.service.Events(context.Background(), h.tenant, run.ID.UUID, 2, 500)
	require.NoError(t, err)
	require.Len(t, tail, 2)
	require.Equal(t, 3, tail[0].Seq)
	hermesrunstest.WaitFor(t, "a completion push", func() bool { return len(h.notifier.Completions()) == 1 })
	require.Equal(t, hermesruns.Completed, h.notifier.Completions()[0].Status)
}

func caseARunTheTenantDoesNotOwnIsNotReadable(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	run := h.start("mine")
	_, err := h.service.Get(context.Background(), uuid.New(), run.ID.UUID)
	serviceErr(t, err, hermesruns.ErrRunNotFound)
}

func caseAnEmptyPromptIsRejectedBeforeHermesIsCalled(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	_, err := h.service.Start(context.Background(), h.tenant, hermesruns.StartRequest{Prompt: "   \n "}, "")
	serviceErr(t, err, hermesruns.ErrEmptyPrompt)
	require.Empty(t, h.gateway.Requests())
}

func caseAHermesWithoutTheRunsCapabilityRejectsTheStart(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	h.gateway.Stub("GET v1/capabilities", http.StatusOK, `{"features":{"approval_events":false,"run_events_sse":true}}`)
	_, err := h.service.Start(context.Background(), h.tenant, hermesruns.StartRequest{Prompt: "go"}, "")
	var ce *hermesruns.ClientError
	require.True(t, errors.As(err, &ce))
	require.Equal(t, hermesruns.ErrUnsupported, ce.Kind)
	require.Empty(t, h.gateway.RequestsMatching("v1/runs"))
}

func caseAnApprovalRequestSetsPendingApprovalPushesOnceAndApprovingClearsIt(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	run := h.start("delete the temp files")
	h.gateway.WaitForEventSubscription(t)
	h.gateway.Emit(`{"event":"run.started"}`)
	h.gateway.Emit(`{"event":"approval.request","command":"rm -rf /tmp/x","choices":["once","session","always","deny"],"tool":"shell"}`)
	waiting := h.waitForRun(run.ID.UUID, func(r hermesruns.DTO) bool { return r.Status == hermesruns.WaitingForApproval })
	require.NotNil(t, waiting.PendingApproval)
	require.Equal(t, "rm -rf /tmp/x", *waiting.PendingApproval.Command)
	require.Equal(t, hermesruns.AllChoices(), waiting.PendingApproval.Choices)
	require.Equal(t, "shell", waiting.PendingApproval.Extra["tool"])
	hermesrunstest.WaitFor(t, "an approval push", func() bool { return len(h.notifier.Approvals()) == 1 })
	require.Equal(t, "rm -rf /tmp/x", *h.notifier.Approvals()[0].PendingApproval.Command)

	approved, err := h.service.Approve(context.Background(), h.tenant, run.ID.UUID, hermesruns.Session)
	require.NoError(t, err)
	require.Nil(t, approved.PendingApproval)
	require.Equal(t, hermesruns.Running, approved.Status)
	require.JSONEq(t, `{"choice":"session"}`, *h.gateway.RequestsMatching("v1/runs/run_1/approval")[0].Body)

	h.gateway.Emit(`{"event":"run.completed","output":"cleaned"}`)
	h.gateway.FinishEvents()
	h.waitForRun(run.ID.UUID, func(r hermesruns.DTO) bool { return r.Status == hermesruns.Completed })
	require.Len(t, h.notifier.Approvals(), 1)
}

func caseApprovingARunWithNothingPendingIsRejected(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	run := h.start("go")
	_, err := h.service.Approve(context.Background(), h.tenant, run.ID.UUID, hermesruns.Once)
	serviceErr(t, err, hermesruns.ErrApprovalNotPending)
	require.Empty(t, h.gateway.RequestsMatching("v1/runs/run_1/approval"))
}

func caseAnApprovalHermesAlreadyAnsweredElsewhereClearsTheLocalPendingState(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	h.gateway.Stub("POST v1/runs/run_1/approval", http.StatusConflict, `{"error":{"code":"approval_not_pending"}}`)
	run := h.start("go")
	h.gateway.WaitForEventSubscription(t)
	h.gateway.Emit(`{"event":"approval.request","command":"ls"}`)
	h.waitForRun(run.ID.UUID, func(r hermesruns.DTO) bool { return r.PendingApproval != nil })
	_, err := h.service.Approve(context.Background(), h.tenant, run.ID.UUID, hermesruns.Once)
	serviceErr(t, err, hermesruns.ErrApprovalNotPending)
	cleared, err := h.service.Get(context.Background(), h.tenant, run.ID.UUID)
	require.NoError(t, err)
	require.Nil(t, cleared.PendingApproval)
}

func caseStopAsksHermesToCancelAndTheWatcherRecordsTheCancellation(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	run := h.start("long job")
	h.gateway.WaitForEventSubscription(t)
	h.gateway.Emit(`{"event":"run.started"}`)
	_, err := h.service.Stop(context.Background(), h.tenant, run.ID.UUID)
	require.NoError(t, err)
	require.Len(t, h.gateway.RequestsMatching("v1/runs/run_1/stop"), 1)
	h.gateway.Emit(`{"event":"run.cancelled"}`)
	h.gateway.FinishEvents()
	stopped := h.waitForRun(run.ID.UUID, func(r hermesruns.DTO) bool { return r.Status == hermesruns.Stopped })
	require.NotNil(t, stopped.FinishedAt)
	hermesrunstest.WaitFor(t, "a completion push", func() bool { return len(h.notifier.Completions()) == 1 })
}

func caseStoppingARunHermesHasAlreadyForgottenMarksItStoppedLocally(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	run := h.start("long job")
	h.gateway.WaitForEventSubscription(t)
	h.gateway.Stub("POST v1/runs/run_1/stop", http.StatusNotFound, `{"error":{"code":"run_not_found"}}`)
	stopped, err := h.service.Stop(context.Background(), h.tenant, run.ID.UUID)
	require.NoError(t, err)
	require.Equal(t, hermesruns.Stopped, stopped.Status)
	require.NotNil(t, stopped.FinishedAt)
	hermesrunstest.WaitFor(t, "a completion push", func() bool { return len(h.notifier.Completions()) == 1 })
}

func caseStoppingAnAlreadyTerminalRunIsANoOp(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	run := h.start("quick")
	h.gateway.WaitForEventSubscription(t)
	h.gateway.Emit(`{"event":"run.completed","output":"ok"}`)
	h.gateway.FinishEvents()
	h.waitForRun(run.ID.UUID, func(r hermesruns.DTO) bool { return r.Status == hermesruns.Completed })
	again, err := h.service.Stop(context.Background(), h.tenant, run.ID.UUID)
	require.NoError(t, err)
	require.Equal(t, hermesruns.Completed, again.Status)
	require.Empty(t, h.gateway.RequestsMatching("v1/runs/run_1/stop"))
}

func fastPoll() hermesruns.Limits {
	l := hermesruns.DefaultLimits()
	l.Watcher.PollInterval = 20 * time.Millisecond
	return l
}

func caseReattachPollsHermesForARunThatOutlivedTheProcess(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, fastPoll(), p)
	r := &hermesruns.Run{HermesRunID: "run_1", Status: hermesruns.Running, Prompt: "survived a restart", StartedAt: time.Now()}
	h.insertRun(r)
	h.gateway.Stub("GET v1/runs/run_1", http.StatusOK, `{"status":"completed","output":"finished while we were down"}`)
	h.isolate()
	h.service.Reattach(context.Background())
	finished := h.waitForRun(r.ID, func(d hermesruns.DTO) bool { return d.Status == hermesruns.Completed })
	require.Equal(t, "finished while we were down", *finished.Summary)
	require.Equal(t, 0, h.gateway.EventsSubscriptionCount(), "re-attach polls, never re-subscribes")
	hermesrunstest.WaitFor(t, "a completion push", func() bool { return len(h.notifier.Completions()) == 1 })
}

func caseReattachMarksARunHermesHasExpiredAsLost(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, fastPoll(), p)
	r := &hermesruns.Run{HermesRunID: "run_gone", Status: hermesruns.Running, Prompt: "hermes forgot me", StartedAt: time.Now()}
	h.insertRun(r)
	h.isolate()
	h.service.Reattach(context.Background())
	lost := h.waitForRun(r.ID, func(d hermesruns.DTO) bool { return d.Status == hermesruns.Lost })
	require.NotNil(t, lost.Error)
	hermesrunstest.WaitFor(t, "a completion push", func() bool { return len(h.notifier.Completions()) == 1 })
}

func caseARunOlderThanTheHermesTTLIsMarkedLostWithoutContactingHermes(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.DefaultLimits(), p)
	r := &hermesruns.Run{
		HermesRunID: "run_ancient", Status: hermesruns.WaitingForApproval, Prompt: "started an hour ago",
		StartedAt:       time.Now().Add(-time.Hour),
		PendingApproval: &hermesruns.PendingApproval{Choices: hermesruns.AllChoices(), RequestedAt: time.Now().Add(-time.Hour)},
	}
	h.insertRun(r)
	h.isolate()
	h.service.Reattach(context.Background())
	require.False(t, h.service.IsWatching(r.ID))
	lost, err := h.service.Get(context.Background(), h.tenant, r.ID)
	require.NoError(t, err)
	require.Equal(t, hermesruns.Lost, lost.Status)
	require.Nil(t, lost.PendingApproval)
	require.NotNil(t, lost.FinishedAt)
	require.Empty(t, h.gateway.Requests())
	events, err := h.service.Events(context.Background(), h.tenant, r.ID, 0, 500)
	require.NoError(t, err)
	require.Equal(t, []string{"watcher.lost"}, eventNames(events))
	require.Len(t, h.notifier.Completions(), 1)
}

func caseReattachSkipsRunsThatAlreadyHaveAWatcher(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	run := h.start("already watched")
	h.gateway.WaitForEventSubscription(t)
	h.isolate()
	h.service.Reattach(context.Background())
	require.Equal(t, 1, h.service.ActiveWatcherCount())
	require.Equal(t, 1, h.gateway.EventsSubscriptionCount())
	h.gateway.Emit(`{"event":"run.completed"}`)
	h.gateway.FinishEvents()
	h.waitForRun(run.ID.UUID, func(d hermesruns.DTO) bool { return d.Status == hermesruns.Completed })
}

func caseThePerTenantWatcherBoundRejectsFurtherConcurrentRuns(t *testing.T, p *pgxpool.Pool) {
	limits := hermesruns.DefaultLimits()
	limits.MaxWatchersPerTenant = 2
	h := withHarness(t, limits, p)
	for _, id := range []string{"run_0", "run_1"} {
		h.gateway.Stub("POST v1/runs", http.StatusAccepted, `{"run_id":"`+id+`"}`)
		h.start("job " + id)
	}
	require.Equal(t, 2, h.service.ActiveWatcherCount())
	_, err := h.service.Start(context.Background(), h.tenant, hermesruns.StartRequest{Prompt: "one too many"}, "")
	serviceErr(t, err, hermesruns.ErrTooManyActiveRuns)
}

func caseListReturnsTheTenantsRunsNewestFirstAndCapsTheLimit(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	now := time.Now()
	for i := range 3 {
		h.insertRun(&hermesruns.Run{
			HermesRunID: "run_list_" + string(rune('0'+i)), Status: hermesruns.Completed,
			Prompt: "job " + string(rune('0'+i)), StartedAt: now.Add(time.Duration(i) * time.Second),
		})
	}
	runs, err := h.service.List(context.Background(), h.tenant, 100, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"job 2", "job 1", "job 0"}, []string{runs[0].Prompt, runs[1].Prompt, runs[2].Prompt})
	limited, err := h.service.List(context.Background(), h.tenant, 1, nil)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	require.Equal(t, "job 2", limited[0].Prompt)
	other, err := h.service.List(context.Background(), uuid.New(), 10, nil)
	require.NoError(t, err)
	require.Empty(t, other)
}

func caseEveryPersistedEventIsPublishedOnTheBusForTheSSEFeed(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	sub, unsubscribe := h.service.Bus().Subscribe()
	defer unsubscribe()
	run := h.start("watch the bus")
	h.gateway.WaitForEventSubscription(t)
	h.gateway.Emit(`{"event":"run.started"}`)
	h.gateway.Emit(`{"event":"run.completed","output":"ok"}`)
	h.gateway.FinishEvents()
	h.waitForRun(run.ID.UUID, func(d hermesruns.DTO) bool { return d.Status == hermesruns.Completed })
	var names []string
	for len(names) < 3 {
		select {
		case p := <-sub:
			require.Equal(t, run.ID.UUID, p.RunID)
			names = append(names, p.Event)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v arrived", names)
		}
	}
	require.ElementsMatch(t, []string{"run.accepted", "run.started", "run.completed"}, names)
}

// MARK: - Conversation persistence (HermesRunConversationPersistenceTests)

func (h *harness) conversation() uuid.UUID {
	h.t.Helper()
	id := uuid.New()
	_, err := h.pool.Exec(context.Background(), `INSERT INTO conversations (id, tenant_id, title, pinned_memory_ids, created_at, updated_at) VALUES ($1, $2, 'Agent', '{}', now(), now())`, id, h.tenant)
	require.NoError(h.t, err)
	return id
}

type message struct {
	role, content string
	tools         int
	runID         *uuid.UUID
}

func (h *harness) messages(conversation uuid.UUID) []message {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `SELECT role, content, tool_call_count, hermes_run_id FROM conversation_messages
		WHERE conversation_id = $1 ORDER BY created_at, role DESC`, conversation)
	require.NoError(h.t, err)
	defer rows.Close()
	var out []message
	for rows.Next() {
		var m message
		require.NoError(h.t, rows.Scan(&m.role, &m.content, &m.tools, &m.runID))
		out = append(out, m)
	}
	return out
}

func (h *harness) terminalRun(conversation *uuid.UUID, status hermesruns.Status, summary *string) *hermesruns.Run {
	h.t.Helper()
	r := &hermesruns.Run{
		HermesRunID: "run_" + uuid.NewString()[:8], Status: status, Prompt: "p", StartedAt: time.Now(),
		ConversationID: conversation, Summary: summary,
	}
	h.insertRun(r)
	return r
}

func caseACompletedRunWritesItsAnswerIntoTheConversation(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	conv := h.conversation()
	run, err := h.service.Start(context.Background(), h.tenant, hermesruns.StartRequest{Prompt: "audit", ConversationID: &conv}, "")
	require.NoError(t, err)
	h.gateway.WaitForEventSubscription(t)
	h.gateway.Emit(`{"event":"tool.started","tool":"shell"}`)
	h.gateway.Emit(`{"event":"tool.started","tool_name":"web"}`)
	h.gateway.Emit(`{"event":"run.completed","output":"  all clear \n"}`)
	h.gateway.FinishEvents()
	h.waitForRun(run.ID.UUID, func(d hermesruns.DTO) bool { return d.Status == hermesruns.Completed })
	hermesrunstest.WaitFor(t, "the assistant turn", func() bool { return len(h.messages(conv)) == 2 })
	msgs := h.messages(conv)
	require.Equal(t, "system", msgs[0].role)
	require.Equal(t, hermesruns.ConversationMarker(run.ID.UUID, "audit"), msgs[0].content)
	require.Equal(t, "assistant", msgs[1].role)
	require.Equal(t, "all clear", msgs[1].content)
	require.Equal(t, 2, msgs[1].tools)
	require.Equal(t, run.ID.UUID, *msgs[1].runID)
}

func caseCommittingTheAnswerTwiceIsANoOp(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	conv := h.conversation()
	r := h.terminalRun(&conv, hermesruns.Completed, ptr("done"))
	hermesruns.CommitConversationTurn(context.Background(), h.service.Store(), r, slogDiscard())
	hermesruns.CommitConversationTurn(context.Background(), h.service.Store(), r, slogDiscard())
	require.Len(t, h.messages(conv), 1)
}

func caseAFailedRunWritesNoAssistantTurn(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	conv := h.conversation()
	hermesruns.CommitConversationTurn(context.Background(), h.service.Store(), h.terminalRun(&conv, hermesruns.Failed, ptr("x")), slogDiscard())
	require.Empty(t, h.messages(conv))
}

func caseACompletedRunWithAnEmptySummaryWritesNothing(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	conv := h.conversation()
	hermesruns.CommitConversationTurn(context.Background(), h.service.Store(), h.terminalRun(&conv, hermesruns.Completed, ptr("  \n")), slogDiscard())
	require.Empty(t, h.messages(conv))
}

func caseARunWithNoConversationWritesNothing(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	r := h.terminalRun(nil, hermesruns.Completed, ptr("done"))
	hermesruns.CommitConversationTurn(context.Background(), h.service.Store(), r, slogDiscard())
	var n int
	require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT count(*) FROM conversation_messages WHERE hermes_run_id = $1`, r.ID).Scan(&n))
	require.Zero(t, n)
}

// MARK: - List filter (HermesRunsListFilterTests)

func caseListFilter(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	a, b, empty := h.conversation(), h.conversation(), h.conversation()
	h.terminalRun(&a, hermesruns.Completed, nil)
	h.terminalRun(&a, hermesruns.Completed, nil)
	h.terminalRun(&b, hermesruns.Completed, nil)
	h.terminalRun(nil, hermesruns.Completed, nil)

	t.Run("Filtering by conversation returns only that conversation's runs", func(t *testing.T) {
		runs, err := h.service.List(context.Background(), h.tenant, 50, &a)
		require.NoError(t, err)
		require.Len(t, runs, 2)
		for _, r := range runs {
			require.Equal(t, a, r.ConversationID.UUID)
		}
	})
	t.Run("Omitting the filter still returns every run for the tenant", func(t *testing.T) {
		runs, err := h.service.List(context.Background(), h.tenant, 50, nil)
		require.NoError(t, err)
		require.Len(t, runs, 4)
	})
	t.Run("A conversation with no runs returns empty rather than everything", func(t *testing.T) {
		runs, err := h.service.List(context.Background(), h.tenant, 50, &empty)
		require.NoError(t, err)
		require.Empty(t, runs)
	})
	t.Run("Standalone runs never match a conversation filter", func(t *testing.T) {
		runs, err := h.service.List(context.Background(), h.tenant, 50, &b)
		require.NoError(t, err)
		require.Len(t, runs, 1)
		require.NotNil(t, runs[0].ConversationID)
	})
}

// MARK: - Persistence (HermesRunPersistenceTests)

func caseANestedEventPayloadRoundTripsThroughJsonbAsAnObject(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	r := h.terminalRun(nil, hermesruns.Running, nil)
	_, err := h.service.Store().Append(context.Background(), r.ID, "tool.completed",
		map[string]any{"event": "tool.completed", "result": map[string]any{"rows": []any{1.0, 2.5}, "ok": true}}, time.Now(), nil)
	require.NoError(t, err)
	events, err := h.service.Events(context.Background(), h.tenant, r.ID, 0, 10)
	require.NoError(t, err)
	body, err := json.Marshal(events[0].Payload)
	require.NoError(t, err)
	require.JSONEq(t, `{"event":"tool.completed","result":{"rows":[1,2.5],"ok":true}}`, string(body))
}

func caseAScalarPayloadIsWrappedRatherThanLost(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	r := h.terminalRun(nil, hermesruns.Running, nil)
	_, err := h.service.Store().Append(context.Background(), r.ID, "odd", "just text", time.Now(), nil)
	require.NoError(t, err)
	events, err := h.service.Events(context.Background(), h.tenant, r.ID, 0, 10)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"value": "just text"}, events[0].Payload)
}

func caseAPendingApprovalRoundTripsThroughJsonbWithItsExtras(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	at := time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)
	r := &hermesruns.Run{
		HermesRunID: "run_p", Status: hermesruns.WaitingForApproval, Prompt: "p", StartedAt: at,
		PendingApproval: &hermesruns.PendingApproval{
			Command: ptr("ls"), Choices: []hermesruns.Choice{hermesruns.Once, hermesruns.Deny},
			RequestedAt: at, Extra: map[string]any{"tool": "shell"},
		},
	}
	h.insertRun(r)
	got, err := h.service.Get(context.Background(), h.tenant, r.ID)
	require.NoError(t, err)
	require.Equal(t, "ls", *got.PendingApproval.Command)
	require.Equal(t, []hermesruns.Choice{hermesruns.Once, hermesruns.Deny}, got.PendingApproval.Choices)
	require.True(t, got.PendingApproval.RequestedAt.Equal(at))
	require.Equal(t, "shell", got.PendingApproval.Extra["tool"])
	// Stored as Fluent writes a Date in jsonb: seconds since 2001-01-01.
	var raw []byte
	require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT pending_approval FROM hermes_runs WHERE id = $1`, r.ID).Scan(&raw))
	var stored map[string]any
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.InDelta(t, float64(at.Unix()-978307200), stored["requestedAt"], 0.001)
}

func caseAnUnknownStatusStringReadsAsLostRatherThanAsAnActiveRun(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	r := h.terminalRun(nil, hermesruns.Running, nil)
	_, err := h.pool.Exec(context.Background(), `UPDATE hermes_runs SET status = 'exploded' WHERE id = $1`, r.ID)
	require.NoError(t, err)
	got, err := h.service.Get(context.Background(), h.tenant, r.ID)
	require.NoError(t, err)
	require.Equal(t, hermesruns.Lost, got.Status)
}

func caseTheSameSeqCannotBePersistedTwiceForOneRun(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	r := h.terminalRun(nil, hermesruns.Running, nil)
	insert := `INSERT INTO hermes_run_events (id, run_id, seq, event, payload, at) VALUES ($1, $2, 1, 'x', '{}', now())`
	_, err := h.pool.Exec(context.Background(), insert, uuid.New(), r.ID)
	require.NoError(t, err)
	_, err = h.pool.Exec(context.Background(), insert, uuid.New(), r.ID)
	require.Error(t, err)
}

func caseDeletingARunCascadesToItsEvents(t *testing.T, p *pgxpool.Pool) {
	h := withHarness(t, hermesruns.Limits{}, p)
	r := h.terminalRun(nil, hermesruns.Running, nil)
	_, err := h.service.Store().Append(context.Background(), r.ID, "run.started", map[string]any{}, time.Now(), nil)
	require.NoError(t, err)
	_, err = h.pool.Exec(context.Background(), `DELETE FROM hermes_runs WHERE id = $1`, r.ID)
	require.NoError(t, err)
	var n int
	require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT count(*) FROM hermes_run_events WHERE run_id = $1`, r.ID).Scan(&n))
	require.Zero(t, n)
}

func ptr(s string) *string { return &s }

// TestHermesRuns shares one Postgres container across every case.
func TestHermesRuns(t *testing.T) {
	p := testPool(t)
	t.Run("StartPersistsTheRunAndTheWatcherRecordsEveryEventInOrder", func(t *testing.T) { caseStartPersistsTheRunAndTheWatcherRecordsEveryEventInOrder(t, p) })
	t.Run("ARunTheTenantDoesNotOwnIsNotReadable", func(t *testing.T) { caseARunTheTenantDoesNotOwnIsNotReadable(t, p) })
	t.Run("AnEmptyPromptIsRejectedBeforeHermesIsCalled", func(t *testing.T) { caseAnEmptyPromptIsRejectedBeforeHermesIsCalled(t, p) })
	t.Run("AHermesWithoutTheRunsCapabilityRejectsTheStart", func(t *testing.T) { caseAHermesWithoutTheRunsCapabilityRejectsTheStart(t, p) })
	t.Run("AnApprovalRequestSetsPendingApprovalPushesOnceAndApprovingClearsIt", func(t *testing.T) { caseAnApprovalRequestSetsPendingApprovalPushesOnceAndApprovingClearsIt(t, p) })
	t.Run("ApprovingARunWithNothingPendingIsRejected", func(t *testing.T) { caseApprovingARunWithNothingPendingIsRejected(t, p) })
	t.Run("AnApprovalHermesAlreadyAnsweredElsewhereClearsTheLocalPendingState", func(t *testing.T) { caseAnApprovalHermesAlreadyAnsweredElsewhereClearsTheLocalPendingState(t, p) })
	t.Run("StopAsksHermesToCancelAndTheWatcherRecordsTheCancellation", func(t *testing.T) { caseStopAsksHermesToCancelAndTheWatcherRecordsTheCancellation(t, p) })
	t.Run("StoppingARunHermesHasAlreadyForgottenMarksItStoppedLocally", func(t *testing.T) { caseStoppingARunHermesHasAlreadyForgottenMarksItStoppedLocally(t, p) })
	t.Run("StoppingAnAlreadyTerminalRunIsANoOp", func(t *testing.T) { caseStoppingAnAlreadyTerminalRunIsANoOp(t, p) })
	t.Run("ReattachPollsHermesForARunThatOutlivedTheProcess", func(t *testing.T) { caseReattachPollsHermesForARunThatOutlivedTheProcess(t, p) })
	t.Run("ReattachMarksARunHermesHasExpiredAsLost", func(t *testing.T) { caseReattachMarksARunHermesHasExpiredAsLost(t, p) })
	t.Run("ARunOlderThanTheHermesTTLIsMarkedLostWithoutContactingHermes", func(t *testing.T) { caseARunOlderThanTheHermesTTLIsMarkedLostWithoutContactingHermes(t, p) })
	t.Run("ReattachSkipsRunsThatAlreadyHaveAWatcher", func(t *testing.T) { caseReattachSkipsRunsThatAlreadyHaveAWatcher(t, p) })
	t.Run("ThePerTenantWatcherBoundRejectsFurtherConcurrentRuns", func(t *testing.T) { caseThePerTenantWatcherBoundRejectsFurtherConcurrentRuns(t, p) })
	t.Run("ListReturnsTheTenantsRunsNewestFirstAndCapsTheLimit", func(t *testing.T) { caseListReturnsTheTenantsRunsNewestFirstAndCapsTheLimit(t, p) })
	t.Run("EveryPersistedEventIsPublishedOnTheBusForTheSSEFeed", func(t *testing.T) { caseEveryPersistedEventIsPublishedOnTheBusForTheSSEFeed(t, p) })
	t.Run("ACompletedRunWritesItsAnswerIntoTheConversation", func(t *testing.T) { caseACompletedRunWritesItsAnswerIntoTheConversation(t, p) })
	t.Run("CommittingTheAnswerTwiceIsANoOp", func(t *testing.T) { caseCommittingTheAnswerTwiceIsANoOp(t, p) })
	t.Run("AFailedRunWritesNoAssistantTurn", func(t *testing.T) { caseAFailedRunWritesNoAssistantTurn(t, p) })
	t.Run("ACompletedRunWithAnEmptySummaryWritesNothing", func(t *testing.T) { caseACompletedRunWithAnEmptySummaryWritesNothing(t, p) })
	t.Run("ARunWithNoConversationWritesNothing", func(t *testing.T) { caseARunWithNoConversationWritesNothing(t, p) })
	t.Run("ListFilter", func(t *testing.T) { caseListFilter(t, p) })
	t.Run("ANestedEventPayloadRoundTripsThroughJsonbAsAnObject", func(t *testing.T) { caseANestedEventPayloadRoundTripsThroughJsonbAsAnObject(t, p) })
	t.Run("AScalarPayloadIsWrappedRatherThanLost", func(t *testing.T) { caseAScalarPayloadIsWrappedRatherThanLost(t, p) })
	t.Run("APendingApprovalRoundTripsThroughJsonbWithItsExtras", func(t *testing.T) { caseAPendingApprovalRoundTripsThroughJsonbWithItsExtras(t, p) })
	t.Run("AnUnknownStatusStringReadsAsLostRatherThanAsAnActiveRun", func(t *testing.T) { caseAnUnknownStatusStringReadsAsLostRatherThanAsAnActiveRun(t, p) })
	t.Run("TheSameSeqCannotBePersistedTwiceForOneRun", func(t *testing.T) { caseTheSameSeqCannotBePersistedTwiceForOneRun(t, p) })
	t.Run("DeletingARunCascadesToItsEvents", func(t *testing.T) { caseDeletingARunCascadesToItsEvents(t, p) })
	t.Run("M117AndM118CreateTheTablesIndexesAndCategoryColumns", func(t *testing.T) { caseM117AndM118CreateTheTablesIndexesAndCategoryColumns(t, p) })
	t.Run("M119DefaultsBothNewPushCategoriesToEnabled", func(t *testing.T) { caseM119DefaultsBothNewPushCategoriesToEnabled(t, p) })
	t.Run("EachNewCategoryIsIndependentlyMutableThroughThePrefsTable", func(t *testing.T) { caseEachNewCategoryIsIndependentlyMutableThroughThePrefsTable(t, p) })
}

// MARK: - Migrations (HermesRunPersistenceTests): Go's schema is the goose baseline,
// so these check the baseline carries what M117–M119 created.

func strings1(t *testing.T, p *pgxpool.Pool, sql string) []string {
	t.Helper()
	rows, err := p.Query(context.Background(), sql)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func caseM117AndM118CreateTheTablesIndexesAndCategoryColumns(t *testing.T, p *pgxpool.Pool) {
	runIndexes := strings1(t, p, `SELECT indexname FROM pg_indexes WHERE tablename = 'hermes_runs'`)
	require.Contains(t, runIndexes, "idx_hermes_runs_tenant_started_at")
	require.Contains(t, runIndexes, "idx_hermes_runs_active")
	require.Contains(t, runIndexes, "uq_hermes_runs_tenant_hermes_run_id")
	require.Contains(t, strings1(t, p, `SELECT indexname FROM pg_indexes WHERE tablename = 'hermes_run_events'`), "uq_hermes_run_events_run_seq")
	require.Equal(t, []string{"jsonb"}, strings1(t, p, `SELECT data_type FROM information_schema.columns
		WHERE table_name = 'hermes_run_events' AND column_name = 'payload'`))
	require.ElementsMatch(t, []string{"approval_enabled", "run_completed_enabled"}, strings1(t, p, `SELECT column_name FROM information_schema.columns
		WHERE table_name = 'apns_category_prefs' AND column_name IN ('approval_enabled', 'run_completed_enabled')`))
}

func caseM119DefaultsBothNewPushCategoriesToEnabled(t *testing.T, p *pgxpool.Pool) {
	tenant := newUser(t, p)
	_, err := p.Exec(context.Background(), `INSERT INTO apns_category_prefs (tenant_id) VALUES ($1)`, tenant)
	require.NoError(t, err)
	var approval, completed bool
	require.NoError(t, p.QueryRow(context.Background(), `SELECT approval_enabled, run_completed_enabled FROM apns_category_prefs
		WHERE tenant_id = $1`, tenant).Scan(&approval, &completed))
	require.True(t, approval)
	require.True(t, completed)
}

// MARK: - Push notifier through apns.Service (HermesRunPushNotifierTests)

type recordingSender struct {
	mu    sync.Mutex
	sends []apns.Delivery
}

func (s *recordingSender) Send(_ context.Context, d apns.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends = append(s.sends, d)
	return nil
}

func caseEachNewCategoryIsIndependentlyMutableThroughThePrefsTable(t *testing.T, p *pgxpool.Pool) {
	ctx := context.Background()
	tenant := newUser(t, p)
	_, err := p.Exec(ctx, `INSERT INTO device_tokens (id, tenant_id, token, platform, bundle_id, created_at, updated_at)
		VALUES ($1, $2, $3, 'ios', 'com.lumina.test', now(), now())`, uuid.New(), tenant, uuid.NewString())
	require.NoError(t, err)
	_, err = p.Exec(ctx, `INSERT INTO apns_category_prefs (tenant_id, approval_enabled, run_completed_enabled) VALUES ($1, false, true)`, tenant)
	require.NoError(t, err)
	sender := &recordingSender{}
	notifier := hermesruns.APNSNotifier{Push: apns.NewWithSender(p, "com.lumina.test", sender)}
	summary := "done"
	notifier.ApprovalRequested(ctx, tenant, hermesruns.DTO{HermesRunID: "run_push", Status: hermesruns.WaitingForApproval, Prompt: "p"})
	notifier.RunFinished(ctx, tenant, hermesruns.DTO{HermesRunID: "run_push", Status: hermesruns.Completed, Prompt: "p", Summary: &summary})
	sender.mu.Lock()
	defer sender.mu.Unlock()
	require.Len(t, sender.sends, 1)
	require.Equal(t, apns.CategoryRunCompleted, sender.sends[0].Message.Category)
}
