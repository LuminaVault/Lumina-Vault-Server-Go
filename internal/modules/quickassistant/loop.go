package quickassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/llm"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/swiftstr"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Limits and model-facing lines, Swift's QuickAssistantService verbatim.
const (
	maxRounds            = 4
	maxToolCallsPerRound = 8 // ruling 14

	ranOutLead   = "I ran out of steps before I could finish."
	failedLead   = "Something went wrong before I could finish."
	stoppedLead  = "This request was stopped before it finished."
	refusal      = "That tool is not available. Do not try it again."
	badArguments = "The tool did not run: its arguments were not valid JSON."
	tooManyCalls = "The tool did not run: too many tools were called in one step."
)

// ToolResult is QuickAssistantToolResult: what the model reads back, and the
// chip or card the app shows, if any.
type ToolResult struct {
	ModelContent string
	Action       *Action
}

// ToolRunner is QuickAssistantToolRunning: runs one tool the policy allows.
// Run gets a context that is not cancelled with the turn, so a started tool
// finishes; a runner that needs a bound sets its own timeout.
// The loop has already refused what the policy refuses, capped the round and
// parsed the arguments; the runner does the rest (Task 8).
type ToolRunner interface {
	Run(ctx context.Context, name string, args map[string]any) ToolResult
}

// Service is QuickAssistantService: the in-process tool loop behind the Quick
// Assistant card.
//
// One question is one route decision, however many rounds it takes, so it is
// one free-lane grant and one budget reservation. The rounds are buffered and
// the final text goes out as ordinary tokens. The app shows actions only from
// assistant_action frames, so model text can never produce a chip.
type Service struct {
	Transport llm.DecidedCompletionTransport
	Model     string
	// Release gives back a reservation the transport will never settle,
	// because the turn ended before reaching it. Nil releases nothing.
	Release func(ctx context.Context, tenant uuid.UUID, micros int64)
	// Tools are the definitions offered to the model on every round.
	Tools []Tool
	Log   *slog.Logger
}

// Reply is QuickAssistantReply: a turn that ended with something for the
// conversation.
type Reply struct {
	Text string
	// IsAnswer is true when the model wrote it. The server's canned lines are
	// not answers: they do not latch, count or get indexed (ruling 13).
	IsAnswer bool
	// ToolCallCount counts calls that reached the runner. Refused ones never did.
	ToolCallCount int
	// RanPersonalTool: a Health, Calendar or Reminders tool ran, so the reply
	// can quote the caller's own data and must never become a shared memory.
	RanPersonalTool bool
	// RanAppAction: a tool that changes spaces, files or a memory was asked
	// for, so the reply reports a change and is not worth remembering.
	RanAppAction bool
	// ShouldPersist is false only for a stopped turn with nothing done: nobody
	// saw it, and there is nothing to keep a record of.
	ShouldPersist bool
	// UsedConnectedApp: a connected-app tool ran, so the reply can quote
	// another app's data. It always comes with RanPersonalTool.
	UsedConnectedApp bool
}

// Indexable reports whether the reply may be turned into a memory.
func (r Reply) Indexable() bool {
	return r.IsAnswer && !r.RanPersonalTool && !r.RanAppAction && !r.UsedConnectedApp
}

// Outcome is QuickAssistantTurn. Reply nil means answer through the classic
// path; PreDecided is then the free-lane decision this question already paid
// its one grant for, which the classic path has to reuse, or nil for the
// classic path to pick its own route.
type Outcome struct {
	Reply      *Reply
	PreDecided *llm.Decision
}

// TurnOptions is what one turn adds to the service's own tools: the caller's
// connected-app tools.
//
// A turn that offers them goes to Anthropic's own endpoint and nowhere else.
// When the route has no Anthropic candidate, is the free lane, or would send
// the request to a base URL the user stored with their own key, the turn runs
// as if no options were given: the extras are not offered and cannot run.
type TurnOptions struct {
	// ExtraTools are offered after the service's tools. Each is named
	// "<app>__<tool>"; a name without "__" is dropped, so an extra can never
	// stand in for one of the service's tools.
	ExtraTools []Tool
	// ExtraRunner runs the ExtraTools names and nothing else. Nil drops them.
	ExtraRunner ToolRunner
	// AnthropicOnly pins the turn to its Anthropic route. Offering extras pins
	// the turn whether or not this is set.
	AnthropicOnly bool
}

// connectedToolSeparator is what an extra tool's name has and none of the
// service's tools do.
const connectedToolSeparator = "__"

// extras is the offerable ExtraTools, first of each name.
func (o TurnOptions) extras() []Tool {
	if o.ExtraRunner == nil {
		return nil
	}
	var out []Tool
	seen := map[string]bool{}
	for _, tool := range o.ExtraTools {
		if !strings.Contains(tool.Name, connectedToolSeparator) || ActionFor(tool.Name) != Refused || seen[tool.Name] {
			continue
		}
		seen[tool.Name] = true
		out = append(out, tool)
	}
	return out
}

// Answer picks the route once and runs the loop on it. It must run under the
// turn's llm.WithValues binding, since picking reads it.
func (s *Service) Answer(ctx context.Context, msgs []llm.ChatMessage, sessionKey, sessionID string, tools ToolRunner, emit func(memoryagent.StreamEvent)) Outcome {
	return s.AnswerWith(ctx, msgs, sessionKey, sessionID, tools, emit, TurnOptions{})
}

// AnswerWith is Answer with this turn's connected-app tools. It must run under
// the same llm.WithValues binding as the pick: the transport applies the
// binding's forced route again on every round.
func (s *Service) AnswerWith(ctx context.Context, msgs []llm.ChatMessage, sessionKey, sessionID string, tools ToolRunner, emit func(memoryagent.StreamEvent), opts TurnOptions) Outcome {
	turnStart := time.Now()
	transcript := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		transcript = append(transcript, wireMessage{Role: m.Role, Content: &m.Content})
	}
	// The pick reads the model and the prompt only, and the pin is not known
	// yet, so this payload carries no extras.
	payload, err := s.payload(transcript, nil)
	if err != nil {
		s.logTurn(ctx, "fallback", 0, turnStart)
		return Outcome{}
	}
	picked := s.Transport.PickDecision(ctx, payload)
	var preDecided *llm.Decision
	if picked.Cerberus != nil && picked.Cerberus.IsFreeLane {
		p := picked
		preDecided = &p
	}

	// The gateway ignores tools, and Ollama and Gemini cannot carry this loop
	// (rulings 15, 51).
	decision, ok := picked.Removing(llm.HermesGateway)
	if ok {
		decision, ok = decision.Removing(llm.Ollama)
	}
	if ok {
		decision, ok = decision.Removing(llm.Gemini)
	}
	if !ok {
		s.releaseUnsettled(ctx, picked)
		s.logTurn(ctx, "no_tool_route", 0, turnStart)
		return Outcome{PreDecided: preDecided}
	}

	// Connected-app tools go to Anthropic or are not offered. The pin comes
	// after the removals above and before the loop, so every round runs on it.
	// When it cannot hold, decision stays exactly as it was: a free-lane or
	// other-provider turn never carries an extra.
	extras := opts.extras()
	var unpinned *llm.Decision
	if opts.AnthropicOnly || len(extras) > 0 {
		pinned, canPin := decision.OnlyProvider(llm.Anthropic)
		// preDecided is set exactly for a free-lane pick, which carries no
		// extras even when a trial route sits in front of it.
		freeLane := preDecided != nil
		// "Anthropic" means Anthropic's endpoint: a user's own key can carry a
		// base URL of their choosing, and app data must not go there.
		if canPin && !freeLane && len(extras) > 0 && !s.Transport.DispatchesToProviderEndpoint(ctx, pinned) {
			canPin = false
		}
		if canPin && !freeLane {
			before := decision
			unpinned = &before
			decision = pinned
		} else {
			extras = nil
		}
	}

	if emit == nil {
		emit = func(memoryagent.StreamEvent) {}
	}
	t := &turn{
		service: s, ctx: ctx, sessionKey: sessionKey, sessionID: sessionID,
		tools: tools, emit: emit, turnStart: turnStart,
		transcript: transcript, finishedWrites: map[string]string{},
		unpinned: unpinned,
	}
	if len(extras) > 0 {
		t.extraTools, t.extraRunner = extras, opts.ExtraRunner
		t.extraNames = make(map[string]bool, len(extras))
		for _, tool := range extras {
			t.extraNames[tool.Name] = true
		}
	}
	if reply := t.run(decision); reply != nil {
		return Outcome{Reply: reply}
	}
	return Outcome{PreDecided: preDecided}
}

// turn is one question's loop state.
type turn struct {
	service    *Service
	ctx        context.Context
	sessionKey string
	sessionID  string
	tools      ToolRunner
	emit       func(memoryagent.StreamEvent)
	turnStart  time.Time

	transcript      []wireMessage
	done            []string
	actionEmitted   bool
	toolCallCount   int
	ranPersonalTool bool
	ranAppAction    bool
	// extraTools is the turn's connected-app tools as they are really offered:
	// empty unless the turn is pinned to Anthropic. extraNames is their names,
	// the one set the offer, the policy check and the dispatch all read.
	extraTools       []Tool
	extraNames       map[string]bool
	extraRunner      ToolRunner
	usedConnectedApp bool
	// unpinned is the decision as it was before the Anthropic pin, kept until
	// round 1 answers; nil on a turn that is not pinned.
	unpinned *llm.Decision
	// finishedWrites is the first result of each write that took effect, so a
	// repeat of the same call (a retry, or the model saying it twice) does not
	// do it twice (ruling 19).
	finishedWrites map[string]string
}

// run is the loop on a decision already made; nil means fall back to classic.
//
// Round 1 uses decision as picked, so the transport settles its reservation
// there, on success or failure alike. Later rounds use the stripped decision,
// so the reservation is subtracted once per question while each round still
// records its own cost (ruling 10).
func (t *turn) run(decision llm.Decision) *Reply {
	s := t.service
	laterRounds := decision.WithoutBudgetReservation()

	for round := 1; round <= maxRounds; round++ {
		var payload []byte
		err := t.ctx.Err()
		if err == nil {
			payload, err = s.payload(t.transcript, t.extraTools)
		}
		if err != nil {
			// Round 1 never reached the transport, so nothing settled.
			if round == 1 {
				s.releaseUnsettled(t.ctx, decision)
				return t.fallBack(0)
			}
			return t.stopped(round - 1)
		}

		roundStart := time.Now()
		roundDecision := laterRounds
		if round == 1 {
			roundDecision = decision
		}
		var message *wireMessage
		meta, err := s.Transport.ChatCompletionsDecided(t.ctx, payload, t.sessionKey, t.sessionID, roundDecision)
		if err == nil {
			message, err = decodeCompletion(meta.Data)
		}
		if err != nil {
			// The type only: a provider error's text can echo the prompt.
			s.logger().WarnContext(t.ctx, "quick assistant round failed",
				"round", round,
				"error_type", fmt.Sprintf("%T", err),
				"duration_ms", time.Since(roundStart).Milliseconds())
			// On round 1 the transport already settled the reservation.
			if round == 1 {
				if t.unpinned != nil && t.ctx.Err() == nil {
					return t.rerunUnpinned()
				}
				return t.fallBack(1)
			}
			if t.ctx.Err() != nil {
				return t.stopped(round)
			}
			return t.failed(round)
		}

		var calls []wireToolCall
		if message != nil {
			calls = message.ToolCalls
		}
		s.logger().InfoContext(t.ctx, "quick assistant round",
			"round", round,
			"tool_calls", len(calls),
			"duration_ms", time.Since(roundStart).Milliseconds())

		if message == nil || len(calls) == 0 {
			text := ""
			if message != nil && message.Content != nil {
				text = swiftstr.TrimWhitespacesAndNewlines(*message.Content)
			}
			if text != "" {
				return t.reply(text, true, "answer", round, true)
			}
			if round == 1 {
				return t.fallBack(1)
			}
			return t.failed(round)
		}

		// The last round's calls do not run: the model could never read their
		// results, and a write it cannot follow up on is worse than one the
		// user can simply ask for again. tool_choice "none" is not sent
		// instead (ruling 16).
		if round == maxRounds {
			return t.ranOut(round)
		}

		t.transcript = append(t.transcript, *message)
		// Every call gets a result, run or not: providers reject a transcript
		// with an unanswered tool call.
		for index, c := range calls {
			if t.ctx.Err() != nil {
				return t.stopped(round)
			}
			content := t.runCall(index, c)
			t.transcript = append(t.transcript, wireMessage{
				Role: "tool", Content: &content, ToolCallID: c.ID, Name: c.Function.Name,
			})
		}
	}
	// Unreachable: the last round always returns above.
	return t.ranOut(maxRounds)
}

// runCall is one call's model-facing result, running the tool when allowed.
func (t *turn) runCall(index int, c wireToolCall) string {
	name := c.Function.Name
	args, ok := parseArguments(c.Function.Arguments)
	extra := t.extraNames[name]
	if rejection := rejection(index, name, ok, extra); rejection != "" {
		return rejection
	}
	writeKey := ""
	if IsWrite(name) {
		writeKey = writeKeyFor(name, args)
		if first, seen := t.finishedWrites[writeKey]; seen {
			return first
		}
	}
	t.toolCallCount++
	t.ranPersonalTool = t.ranPersonalTool || TouchesPersonalData(name)
	t.ranAppAction = t.ranAppAction || ChangesApp(name)
	runner := t.tools
	if extra {
		// Another app's data about the caller: personal, whatever the tool.
		t.ranPersonalTool, t.usedConnectedApp = true, true
		runner = t.extraRunner
	}
	// A started tool runs to its result, as under Swift's cooperative
	// cancellation: a disconnect must not abort a write half-way or let it
	// commit without its chip. Cancellation is checked between calls instead.
	result := runner.Run(context.WithoutCancel(t.ctx), name, args)
	if a := result.Action; a != nil {
		t.actionEmitted = true
		t.emit(Frame(*a))
		if a.Kind == KindDone {
			t.done = append(t.done, a.Summary)
		}
		// A write shows a chip only when it took effect.
		if writeKey != "" {
			t.finishedWrites[writeKey] = result.ModelContent
		}
	}
	return result.ModelContent
}

// rerunUnpinned is a pinned turn whose first round failed: nothing has run and
// no app data exists, so the question starts over as an ordinary turn, on the
// decision from before the pin and with no connected-app tools. Round 1
// settled the reservation, so the rerun carries none. It happens once: with
// unpinned cleared, a second round-1 failure falls back to the classic path.
// From round 2 on a pinned turn never gets here; it fails (failedLead).
func (t *turn) rerunUnpinned() *Reply {
	decision := t.unpinned.WithoutBudgetReservation()
	t.unpinned = nil
	t.extraTools, t.extraNames, t.extraRunner = nil, nil, nil
	t.service.logger().InfoContext(t.ctx, "quick assistant rerun without connected apps")
	return t.run(decision)
}

func (t *turn) fallBack(rounds int) *Reply {
	t.service.logTurn(t.ctx, "fallback", rounds, t.turnStart)
	return nil
}

func (t *turn) reply(text string, isAnswer bool, outcome string, rounds int, shouldPersist bool) *Reply {
	for _, chunk := range WordChunks(text) {
		t.emit(memoryagent.StreamEvent{Type: "token", Payload: chunk})
	}
	t.service.logTurn(t.ctx, outcome, rounds, t.turnStart)
	return &Reply{
		Text:             text,
		IsAnswer:         isAnswer,
		ToolCallCount:    t.toolCallCount,
		RanPersonalTool:  t.ranPersonalTool,
		RanAppAction:     t.ranAppAction,
		ShouldPersist:    shouldPersist,
		UsedConnectedApp: t.usedConnectedApp,
	}
}

func (t *turn) failed(rounds int) *Reply {
	return t.reply(partialReply(failedLead, t.done), false, "failed", rounds, true)
}

func (t *turn) ranOut(rounds int) *Reply {
	return t.reply(partialReply(ranOutLead, t.done), false, "ran_out", rounds, true)
}

// stopped: nobody is listening any more, so with nothing done there is nothing
// to say or to keep. With an action the line is its record.
func (t *turn) stopped(rounds int) *Reply {
	text := ""
	if t.actionEmitted {
		text = partialReply(stoppedLead, t.done)
	}
	return t.reply(text, false, "stopped", rounds, t.actionEmitted)
}

// WordChunks splits text into word-sized tokens, each keeping its trailing
// whitespace, so the tokens concatenate back to exactly text. It walks
// Characters, as Swift does, so "\r\n" stays whole.
func WordChunks(text string) []string {
	var chunks []string
	var current strings.Builder
	afterSpace := false
	for _, character := range swiftstr.Characters(text) {
		space := isWhitespaceCharacter(character)
		if !space && afterSpace {
			chunks = append(chunks, current.String())
			current.Reset()
			afterSpace = false
		}
		current.WriteString(character)
		if space {
			afterSpace = true
		}
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

// isWhitespaceCharacter is Character.isWhitespace: the first scalar has the
// Unicode White_Space property.
func isWhitespaceCharacter(character string) bool {
	for _, r := range character {
		return unicode.Is(unicode.White_Space, r)
	}
	return false
}

// rejection is why a call must not run, or "". The policy is checked here, not
// trusted to the offer: the model can name a tool it was never offered. extra
// says the name is one of this turn's offered connected-app tools, which the
// policy does not know; any other "<app>__<tool>" name is refused with the rest.
func rejection(index int, name string, argsOK, extra bool) string {
	switch {
	case index >= maxToolCallsPerRound:
		return tooManyCalls
	case !extra && ActionFor(name) == Refused:
		return refusal
	case !argsOK:
		return badArguments
	}
	return ""
}

// parseArguments reads a call's arguments: blank means none, anything but a
// JSON object is invalid.
func parseArguments(raw string) (map[string]any, bool) {
	if swiftstr.TrimWhitespacesAndNewlines(raw) == "" {
		return map[string]any{}, true
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
		return nil, false
	}
	return args, true
}

// writeKeyFor is the same tool and the same arguments, whatever order the
// model wrote the keys in: encoding/json sorts map keys at every depth.
func writeKeyFor(name string, args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return name + "\n"
	}
	return name + "\n" + string(b)
}

// partialReply reports only what a tool reported as done, never anything the
// model said.
func partialReply(lead string, done []string) string {
	if len(done) == 0 {
		return lead
	}
	return lead + " Already done: " + strings.Join(done, "; ") + "."
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) logTurn(ctx context.Context, outcome string, rounds int, start time.Time) {
	s.logger().InfoContext(ctx, "quick assistant turn",
		"outcome", outcome,
		"rounds", rounds,
		"duration_ms", time.Since(start).Milliseconds())
}

// releaseUnsettled gives back a reservation no completion will settle. The
// turn may already be cancelled, which must not stop the release.
func (s *Service) releaseUnsettled(ctx context.Context, d llm.Decision) {
	if s.Release == nil || d.Cerberus == nil || d.Cerberus.BudgetReservationUsdMicros <= 0 {
		return
	}
	s.Release(context.WithoutCancel(ctx), d.Cerberus.TenantID, d.Cerberus.BudgetReservationUsdMicros)
}

// MARK: - Wire format

// wireRequest is the OpenAI-shape request every routed adapter speaks. There is
// deliberately no sampling field: Haiku 5.5 answers 400 to any of them.
type wireRequest struct {
	Model      string           `json:"model"`
	Messages   []wireMessage    `json:"messages"`
	Tools      []map[string]any `json:"tools"`
	ToolChoice string           `json:"tool_choice"`
	Stream     bool             `json:"stream"`
}

// payload offers the service's tools, then extras, the turn's connected-app
// tools.
func (s *Service) payload(transcript []wireMessage, extras []Tool) ([]byte, error) {
	return json.Marshal(wireRequest{
		Model:      s.Model,
		Messages:   transcript,
		Tools:      append(OpenAITools(s.Tools), OpenAITools(extras)...),
		ToolChoice: "auto",
		Stream:     false,
	})
}

// wireMessage is a chat message with tool fields, which llm.ChatMessage lacks.
// Content nil is left out, as Swift's encoder does.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     *string          `json:"type,omitempty"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// errUnreadableCompletion is a completion missing a field Swift's decoder
// requires.
var errUnreadableCompletion = errors.New("unreadable completion")

// decodeCompletion is Swift's WireCompletion decode: the first choice's
// message, nil when there are no choices. The fields Swift requires (choices,
// each choice's message and role, each call's id, function name and string
// arguments) fail the round when missing or null.
func decodeCompletion(data []byte) (*wireMessage, error) {
	var raw struct {
		Choices *[]struct {
			Message *struct {
				Role       *string        `json:"role"`
				Content    *string        `json:"content"`
				ToolCalls  *[]rawToolCall `json:"tool_calls"`
				ToolCallID *string        `json:"tool_call_id"`
				Name       *string        `json:"name"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.Choices == nil {
		return nil, errUnreadableCompletion
	}
	var first *wireMessage
	for i, choice := range *raw.Choices {
		m := choice.Message
		if m == nil || m.Role == nil {
			return nil, errUnreadableCompletion
		}
		var calls []wireToolCall
		if m.ToolCalls != nil {
			for _, c := range *m.ToolCalls {
				if c.ID == nil || c.Function == nil || c.Function.Name == nil || c.Function.Arguments == nil {
					return nil, errUnreadableCompletion
				}
				calls = append(calls, wireToolCall{
					ID:       *c.ID,
					Type:     c.Type,
					Function: wireFunctionCall{Name: *c.Function.Name, Arguments: *c.Function.Arguments},
				})
			}
		}
		if i == 0 {
			first = &wireMessage{Role: *m.Role, Content: m.Content, ToolCalls: calls}
			if m.ToolCallID != nil {
				first.ToolCallID = *m.ToolCallID
			}
			if m.Name != nil {
				first.Name = *m.Name
			}
		}
	}
	return first, nil
}

type rawToolCall struct {
	ID       *string `json:"id"`
	Type     *string `json:"type"`
	Function *struct {
		Name      *string `json:"name"`
		Arguments *string `json:"arguments"`
	} `json:"function"`
}
