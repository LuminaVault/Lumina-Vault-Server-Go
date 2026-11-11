package hermesruns

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Ported from AppTests/Hermes/Runs/HermesRunEventTests.swift (15 tests).

func decodeOK(t *testing.T, name *string, data string) Frame {
	t.Helper()
	f, ok := DecodeFrame(name, data, time.Unix(0, 0))
	require.True(t, ok, data)
	return f
}

func sp(s string) *string { return &s }

func TestParserBuffersPartialRecordsAcrossFeeds(t *testing.T) {
	var p Parser
	require.Empty(t, p.Feed(`data: {"a":`))
	require.Equal(t, []Record{{Data: `{"a":1}`}}, p.Feed("1}\n\n"))
}

func TestParserDropsKeepaliveCommentsAndReadsTheEventName(t *testing.T) {
	var p Parser
	require.Equal(t, []Record{{Event: sp("run.started"), Data: "{}"}}, p.Feed(": keepalive\n\nevent: run.started\ndata: {}\n\n"))
}

func TestParserJoinsMultiLineDataAndHandlesCRLFRecords(t *testing.T) {
	var p Parser
	require.Equal(t, []Record{{Data: "line1\nline2"}}, p.Feed("data: line1\r\ndata: line2\r\n\r\n"))
}

func TestParserFlushSurfacesARecordTheUpstreamNeverTerminated(t *testing.T) {
	var p Parser
	require.Empty(t, p.Feed(`data: {"event":"error"}`))
	r, ok := p.Flush()
	require.True(t, ok)
	require.Equal(t, Record{Data: `{"event":"error"}`}, r)
	_, ok = p.Flush()
	require.False(t, ok, "flushing twice must not replay it")
}

func TestApprovalRequestCarriesTheRedactedCommandTheChoicesAndTheExtras(t *testing.T) {
	f := decodeOK(t, nil, `{"event":"approval.request","run_id":"run_1","command":"rm -rf ***",
		"choices":["once","always","deny"],"tool":"shell","timestamp":1000}`)
	require.Equal(t, "approval.request", f.Name)
	require.True(t, f.At.Equal(time.Unix(1000, 0)))
	require.Equal(t, KindApprovalRequest, f.Event.Kind)
	require.Equal(t, "rm -rf ***", *f.Event.Command)
	require.Equal(t, []Choice{Once, Always, Deny}, f.Event.Choices)
	require.Equal(t, "shell", f.Event.Extra["tool"])
	require.NotContains(t, f.Event.Extra, "run_id")
}

func TestApprovalRequestWithNoChoicesFallsBackToTheFullSet(t *testing.T) {
	f := decodeOK(t, nil, `{"event":"approval.request"}`)
	require.Equal(t, AllChoices(), f.Event.Choices)
}

func TestEveryModelledHermesEventNameDecodesToItsCase(t *testing.T) {
	cases := map[string]Kind{
		`{"event":"run.started"}`:                                  KindRunStarted,
		`{"event":"message.started"}`:                              KindMessageStarted,
		`{"event":"message.delta","delta":"hi"}`:                   KindMessageDelta,
		`{"event":"tool.completed","tool":"shell","duration":1.5}`: KindToolCompleted,
		`{"event":"tool.failed","tool_name":"web","error":"boom"}`: KindToolFailed,
		`{"event":"hermes.tool.progress","text":"reading"}`:        KindToolProgress,
		`{"event":"approval.responded","choice":"once"}`:           KindApprovalResponded,
		`{"event":"run.failed","error":"nope"}`:                    KindRunFailed,
		`{"event":"run.cancelled"}`:                                KindRunCancelled,
		`{"event":"error","message":"bad"}`:                        KindError,
	}
	for data, kind := range cases {
		require.Equal(t, kind, decodeOK(t, nil, data).Event.Kind, data)
	}
	require.Equal(t, "hi", decodeOK(t, nil, `{"event":"message.delta","delta":"hi"}`).Event.Text)
	failed := decodeOK(t, nil, `{"event":"tool.failed","tool_name":"web","error":"boom"}`).Event
	require.Equal(t, "web", *failed.Tool)
	require.Equal(t, "boom", failed.Text)
	progress := decodeOK(t, nil, `{"event":"hermes.tool.progress","text":"reading"}`).Event
	require.Nil(t, progress.Tool)
	require.Equal(t, "reading", progress.Text)
	require.Equal(t, "nope", decodeOK(t, nil, `{"event":"run.failed","error":"nope"}`).Event.Text)
	require.Equal(t, "bad", decodeOK(t, nil, `{"event":"error","message":"bad"}`).Event.Text)
}

func TestAnUnknownEventSurvivesWithItsPayloadSoReplayIsLossless(t *testing.T) {
	f := decodeOK(t, sp("brand.new"), `{"weird":true}`)
	require.Equal(t, "brand.new", f.Name)
	_, ok := f.Event.ImpliedStatus()
	require.False(t, ok)
	require.Equal(t, KindUnknown, f.Event.Kind)
	require.Equal(t, "brand.new", f.Event.Name)
	require.Equal(t, true, f.Event.Payload.(map[string]any)["weird"])
}

func TestDecodeRejectsARecordThatNamesNoEvent(t *testing.T) {
	_, ok := DecodeFrame(nil, `{"no":"name"}`, time.Now())
	require.False(t, ok)
	_, ok = DecodeFrame(nil, "not json", time.Now())
	require.False(t, ok)
}

func TestAPendingApprovalSurvivesStrayDeltasAndClearsOnAToolEvent(t *testing.T) {
	status := Transition(Running, Event{Kind: KindApprovalRequest})
	require.Equal(t, WaitingForApproval, status)
	status = Transition(status, Event{Kind: KindMessageDelta, Text: "thinking"})
	require.Equal(t, WaitingForApproval, status)
	status = Transition(status, Event{Kind: KindToolStarted, Tool: sp("shell")})
	require.Equal(t, Running, status)
}

func TestTerminalStatesAreAbsorbing(t *testing.T) {
	terminal := Transition(Running, Event{Kind: KindRunCompleted, Summary: sp("ok")})
	require.Equal(t, Completed, terminal)
	require.Equal(t, Completed, Transition(terminal, Event{Kind: KindRunStarted}))
}

func TestAPolledStatusChangeIsSynthesisedAsTheEventHermesWouldHaveSent(t *testing.T) {
	f := Synthesise(Completed, Snapshot{Status: "completed", LastEvent: sp("run.completed"), Output: sp("finished")}, time.Now())
	require.Equal(t, "run.completed", f.Name)
	require.Equal(t, KindRunCompleted, f.Event.Kind)
	require.Equal(t, "finished", *f.Event.Summary)
	require.Equal(t, "poll", f.Payload.(map[string]any)["source"])
}

func TestPushBodiesAreSingleLineRedactedAndCapped(t *testing.T) {
	body, ok := BodyText(sp("  first line \n\n second line  "))
	require.True(t, ok)
	require.Equal(t, "first line second line", body)
	_, ok = BodyText(sp("   \n  "))
	require.False(t, ok)
	_, ok = BodyText(nil)
	require.False(t, ok)
	long, _ := BodyText(sp(strings.Repeat("x", 500)))
	require.Len(t, []rune(long), bodyLimit)
	require.True(t, strings.HasSuffix(long, "…"))
	redacted, _ := BodyText(sp("token Bearer abc.def-1 leaked"))
	require.Equal(t, "token Bearer *** leaked", redacted)
}

func TestTerminalOutputIsRelayedVerbatimAndChangesNoStatus(t *testing.T) {
	f := decodeOK(t, nil, `{"event":"terminal.output","run_id":"run_1","timestamp":1700000000,"process_id":"proc_9","chunk":"server listening\n"}`)
	require.Equal(t, "terminal.output", f.Name)
	_, ok := f.Event.ImpliedStatus()
	require.False(t, ok)
	payload := f.Payload.(map[string]any)
	require.Equal(t, "proc_9", payload["process_id"])
	require.Equal(t, "server listening\n", payload["chunk"])
}

func TestTerminalFieldsOnToolEventsSurviveIntoThePersistedPayload(t *testing.T) {
	started := decodeOK(t, nil, `{"event":"tool.started","tool":"terminal","preview":"ls","command":"ls -la","background":false}`)
	completed := decodeOK(t, nil, `{"event":"tool.completed","tool":"terminal","duration":0.2,"error":false,"output":"a.txt","exit_code":0,"output_truncated":false}`)
	require.Equal(t, KindToolStarted, started.Event.Kind)
	require.Equal(t, "terminal", *started.Event.Tool)
	require.Equal(t, "ls -la", started.Payload.(map[string]any)["command"])
	require.Equal(t, KindToolCompleted, completed.Event.Kind)
	require.Equal(t, "a.txt", completed.Payload.(map[string]any)["output"])
	require.InDelta(t, 0.0, completed.Payload.(map[string]any)["exit_code"], 0)
}

func TestToolNameCountsStartedEventsOnly(t *testing.T) {
	name, ok := ToolName("tool.started", map[string]any{"tool_name": "web"})
	require.True(t, ok)
	require.Equal(t, "web", name)
	name, ok = ToolName("tool.started", map[string]any{})
	require.True(t, ok)
	require.Equal(t, "tool", name)
	_, ok = ToolName("tool.completed", map[string]any{"tool": "web"})
	require.False(t, ok)
}
