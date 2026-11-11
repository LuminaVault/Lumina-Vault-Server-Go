// Package hermesruns is the Go port of Swift's Hermes runs stack
// (Sources/App/Hermes/Runs): the /v1/runs gateway client, the per-run watcher,
// the run store and HermesRunsService. The routes live in
// internal/modules/hermesruns.
package hermesruns

import (
	"encoding/json"
	"lumina/internal/wire"
	"reflect"
	"strings"
	"time"
)

// Status is HermesRunStatus.
type Status string

const (
	Queued             Status = "queued"
	Running            Status = "running"
	WaitingForApproval Status = "waiting_for_approval"
	Completed          Status = "completed"
	Failed             Status = "failed"
	Stopped            Status = "stopped"
	Lost               Status = "lost"
)

// IsTerminal is HermesRunStatus.isTerminal.
func (s Status) IsTerminal() bool {
	switch s {
	case Completed, Failed, Stopped, Lost:
		return true
	}
	return false
}

// parseStatus is HermesRun.runStatus: an unknown string reads as lost, so a
// corrupt row never masquerades as active.
func parseStatus(s string) Status {
	switch Status(s) {
	case Queued, Running, WaitingForApproval, Completed, Failed, Stopped, Lost:
		return Status(s)
	}
	return Lost
}

// Choice is HermesApprovalChoice.
type Choice string

const (
	Once    Choice = "once"
	Session Choice = "session"
	Always  Choice = "always"
	Deny    Choice = "deny"
)

// AllChoices is HermesApprovalChoice.allCases.
func AllChoices() []Choice { return []Choice{Once, Session, Always, Deny} }

func validChoice(s string) bool {
	switch Choice(s) {
	case Once, Session, Always, Deny:
		return true
	}
	return false
}

// UnmarshalJSON accepts only the four raw values, as the Swift enum's decoder.
func (c *Choice) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !validChoice(s) {
		return &wire.CorruptedError{Message: "Cannot initialize HermesApprovalChoice from invalid String value " + s}
	}
	*c = Choice(s)
	return nil
}

// Kind is the case of HermesRunEvent.
type Kind int

const (
	KindUnknown Kind = iota
	KindRunStarted
	KindMessageStarted
	KindMessageDelta
	KindToolStarted
	KindToolCompleted
	KindToolFailed
	KindToolProgress
	KindApprovalRequest
	KindApprovalResponded
	KindRunCompleted
	KindRunFailed
	KindRunCancelled
	KindError
)

// Event is HermesRunEvent: the typed view of one Hermes run event. Only the
// fields a case carries are set.
type Event struct {
	Kind Kind
	// Tool is the tool name for tool events.
	Tool *string
	// Text is the delta, progress text, failure message or error message.
	Text string
	// Summary is run.completed's output.
	Summary *string
	// Command, Choices and Extra are approval.request's.
	Command *string
	Choices []Choice
	Extra   map[string]any
	// Name and Payload are an unknown event's.
	Name    string
	Payload any
}

// ImpliedStatus is HermesRunEvent.impliedStatus; ok is false for an unknown event.
func (e Event) ImpliedStatus() (Status, bool) {
	switch e.Kind {
	case KindRunStarted, KindMessageStarted, KindMessageDelta, KindToolStarted, KindToolCompleted,
		KindToolFailed, KindToolProgress, KindApprovalResponded:
		return Running, true
	case KindApprovalRequest:
		return WaitingForApproval, true
	case KindRunCompleted:
		return Completed, true
	case KindRunFailed, KindError:
		return Failed, true
	case KindRunCancelled:
		return Stopped, true
	}
	return "", false
}

// Frame is HermesRunEventFrame: the typed event and the raw payload that is
// persisted and replayed verbatim.
type Frame struct {
	Name    string
	Event   Event
	Payload any
	At      time.Time
}

func str(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func strPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func firstStrPtr(vs ...any) *string {
	for _, v := range vs {
		if s, ok := v.(string); ok {
			return &s
		}
	}
	return nil
}

func firstStr(fallback string, vs ...any) string {
	if p := firstStrPtr(vs...); p != nil {
		return *p
	}
	return fallback
}

// DecodeFrame is HermesRunEvent.decode: the event name comes from the JSON
// `event` field, falling back to the SSE `event:` line. now stands in for a
// missing timestamp.
func DecodeFrame(eventName *string, data string, now time.Time) (Frame, bool) {
	payload, ok := decodeJSON([]byte(data))
	if !ok {
		return Frame{}, false
	}
	object, _ := payload.(map[string]any)
	if object == nil {
		object = map[string]any{}
	}
	var name string
	if s, ok := str(object["event"]); ok {
		name = s
	} else if eventName != nil {
		name = *eventName
	}
	if name == "" {
		return Frame{}, false
	}
	at := now
	if ts, ok := object["timestamp"].(float64); ok {
		at = unixDouble(ts)
	}
	return Frame{Name: name, Event: ParseEvent(name, object, payload), Payload: payload, At: at}, true
}

// decodeJSON is JSONDecoder().decode(AnyJSONValue.self): any JSON value, with
// numbers as Double.
func decodeJSON(b []byte) (any, bool) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, false
	}
	return v, true
}

func unixDouble(ts float64) time.Time {
	sec := int64(ts)
	if float64(sec) > ts {
		sec--
	}
	return time.Unix(sec, int64((ts-float64(sec))*1e9)).UTC()
}

// ParseEvent is HermesRunEvent.parse.
func ParseEvent(name string, object map[string]any, payload any) Event {
	tool := func() *string { return firstStrPtr(object["tool"], object["tool_name"]) }
	switch name {
	case "run.started":
		return Event{Kind: KindRunStarted}
	case "message.started":
		return Event{Kind: KindMessageStarted}
	case "message.delta", "assistant.delta":
		return Event{Kind: KindMessageDelta, Text: firstStr("", object["delta"])}
	case "tool.started":
		return Event{Kind: KindToolStarted, Tool: tool()}
	case "tool.completed":
		return Event{Kind: KindToolCompleted, Tool: tool()}
	case "tool.failed":
		return Event{Kind: KindToolFailed, Tool: tool(), Text: firstStr("", object["error"], object["preview"])}
	case "hermes.tool.progress", "tool.progress", "reasoning.available":
		return Event{Kind: KindToolProgress, Tool: tool(), Text: firstStr("", object["text"], object["delta"], object["preview"])}
	case "approval.request":
		extra := map[string]any{}
		for k, v := range object {
			switch k {
			case "event", "run_id", "timestamp", "command", "choices":
				continue
			}
			extra[k] = v
		}
		return Event{Kind: KindApprovalRequest, Command: strPtr(object["command"]), Choices: parseChoices(object["choices"]), Extra: extra}
	case "approval.responded":
		return Event{Kind: KindApprovalResponded}
	case "run.completed":
		return Event{Kind: KindRunCompleted, Summary: strPtr(object["output"])}
	case "run.failed":
		return Event{Kind: KindRunFailed, Text: firstStr("agent run failed", object["error"])}
	case "run.cancelled":
		return Event{Kind: KindRunCancelled}
	case "error":
		return Event{Kind: KindError, Text: firstStr("hermes error", object["message"], object["error"])}
	}
	return Event{Kind: KindUnknown, Name: name, Payload: payload}
}

// parseChoices keeps the known choices in order; none known means all four.
func parseChoices(v any) []Choice {
	arr, _ := v.([]any)
	var out []Choice
	for _, item := range arr {
		if s, ok := item.(string); ok && validChoice(s) {
			out = append(out, Choice(s))
		}
	}
	if len(out) == 0 {
		return AllChoices()
	}
	return out
}

// Transition is HermesRunWatcher.transition. A run waiting on approval only
// leaves that state on approval.responded, a tool event or a terminal event.
func Transition(current Status, e Event) Status {
	if current.IsTerminal() {
		return current
	}
	implied, ok := e.ImpliedStatus()
	if !ok {
		return current
	}
	if implied.IsTerminal() || implied == WaitingForApproval {
		return implied
	}
	if current == WaitingForApproval {
		switch e.Kind {
		case KindApprovalResponded, KindToolStarted, KindToolCompleted, KindToolFailed:
			return Running
		}
		return current
	}
	return Running
}

// Snapshot is HermesRunStatusSnapshot, from GET /v1/runs/{id}.
type Snapshot struct {
	Status    string
	LastEvent *string
	Output    *string
	Error     *string
	SessionID *string
}

// Mapped is HermesRunStatusSnapshot.mapped: `stopping` stays active until the
// gateway confirms `cancelled`.
func (s Snapshot) Mapped() (Status, bool) {
	switch s.Status {
	case "queued":
		return Queued, true
	case "running", "stopping":
		return Running, true
	case "waiting_for_approval":
		return WaitingForApproval, true
	case "completed":
		return Completed, true
	case "failed":
		return Failed, true
	case "cancelled", "stopped":
		return Stopped, true
	}
	return "", false
}

// Synthesise is HermesRunWatcher.synthesise: a status change seen while
// polling, as the event Hermes would have streamed for it.
func Synthesise(status Status, snap Snapshot, now time.Time) Frame {
	var event Event
	var name string
	switch status {
	case WaitingForApproval:
		event, name = Event{Kind: KindApprovalRequest, Choices: AllChoices(), Extra: map[string]any{}}, "approval.request"
	case Completed:
		event, name = Event{Kind: KindRunCompleted, Summary: snap.Output}, "run.completed"
	case Failed:
		msg := "agent run failed"
		if snap.Error != nil {
			msg = *snap.Error
		}
		event, name = Event{Kind: KindRunFailed, Text: msg}, "run.failed"
	case Stopped:
		event, name = Event{Kind: KindRunCancelled}, "run.cancelled"
	case Lost:
		event, name = Event{Kind: KindUnknown, Name: "watcher.lost"}, "watcher.lost"
	default:
		event, name = Event{Kind: KindRunStarted}, "run.started"
	}
	payload := map[string]any{"event": name, "status": snap.Status, "source": "poll"}
	if snap.LastEvent != nil {
		payload["last_event"] = *snap.LastEvent
	}
	if snap.Output != nil {
		payload["output"] = *snap.Output
	}
	if snap.Error != nil {
		payload["error"] = *snap.Error
	}
	return Frame{Name: name, Event: event, Payload: payload, At: now}
}

// ToolName is HermesRunWatcher.toolName: the tool a tool.started event names,
// "tool" when it names none.
func ToolName(name string, payload map[string]any) (string, bool) {
	if name != "tool.started" {
		return "", false
	}
	for _, k := range []string{"tool", "tool_name"} {
		if s, ok := payload[k].(string); ok && s != "" {
			return s, true
		}
	}
	return "tool", true
}

// trimWhitespacesAndNewlines is trimmingCharacters(in: .whitespacesAndNewlines).
func trimWhitespacesAndNewlines(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f', 0x85, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000:
			return true
		}
		return r >= 0x2000 && r <= 0x200A
	})
}
