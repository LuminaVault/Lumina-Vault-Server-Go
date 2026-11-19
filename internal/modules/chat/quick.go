package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/connectedapps"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/quickassistant"
	"time"

	"github.com/google/uuid"
)

// OriginConnectedApp is conversation_messages.origin for both rows of a turn
// that ran a connected-app tool. Such a turn can quote another app's data, so
// it is never replayed to a model: historyMessages skips it. The user still
// reads it in their own history.
const OriginConnectedApp = "connected_app"

// ConnectedApps is the user's connected first-party apps as tool sources
// (connectedapps.Service). user is always the person asking, never a vault.
type ConnectedApps interface {
	Tools(ctx context.Context, user uuid.UUID, onlyApps []string) ([]connectedapps.AppTool, error)
	Call(ctx context.Context, user uuid.UUID, name string, args map[string]any) (string, error)
}

// connectedTools is the apps' tools as the loop offers them. A tool whose
// input schema is not a JSON object of type "object" is left out: Anthropic
// answers 400 to such a tool, which would fail every turn that offers it.
func connectedTools(tools []connectedapps.AppTool) []quickassistant.Tool {
	var out []quickassistant.Tool
	for _, tool := range tools {
		if !isObjectSchema(tool.Remote.InputSchema) {
			continue
		}
		out = append(out, quickassistant.Tool{
			Name: tool.Name, Title: tool.App.Name, Description: tool.Remote.Description,
			InputSchema: tool.Remote.InputSchema, Personal: true,
		})
	}
	return out
}

func isObjectSchema(raw json.RawMessage) bool {
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil {
		return false
	}
	var kind string
	return json.Unmarshal(schema["type"], &kind) == nil && kind == "object"
}

// connectedAppRunner runs a connected-app tool as the person asking. It hands
// the model text and nothing else: an app can never put a chip or an "already
// done" line in Lumina. A failure is the service's own fixed sentence, as
// content. Nothing is logged here; the service logs app, tool and latency.
type connectedAppRunner struct {
	apps  ConnectedApps
	actor uuid.UUID
}

func (r connectedAppRunner) Run(ctx context.Context, name string, args map[string]any) quickassistant.ToolResult {
	out, err := r.apps.Call(ctx, r.actor, name, args)
	if err != nil {
		return quickassistant.ToolResult{ModelContent: err.Error()}
	}
	return quickassistant.ToolResult{ModelContent: out}
}

// connectedAppOptions is the turn's connected-app tools, loaded for the
// person asking (t.actor), whichever vault the turn reads. No service, no
// connected app or a failed load all give the zero options: an ordinary turn,
// not pinned to Anthropic.
func (m *Module) connectedAppOptions(ctx context.Context, t turn) quickassistant.TurnOptions {
	apps := m.opts.ConnectedApps
	if apps == nil {
		return quickassistant.TurnOptions{}
	}
	loaded, err := apps.Tools(ctx, t.actor, nil)
	if err != nil {
		// The kind only: the error text can hold store or network detail.
		m.log.WarnContext(ctx, "connected app tools not loaded; turn runs without them", "kind", fmt.Sprintf("%T", err))
		return quickassistant.TurnOptions{}
	}
	tools := connectedTools(loaded)
	if len(tools) == 0 {
		return quickassistant.TurnOptions{}
	}
	return quickassistant.TurnOptions{
		ExtraTools: tools, ExtraRunner: connectedAppRunner{apps: apps, actor: t.actor}, AnthropicOnly: true,
	}
}

// quickTurn is QuickAssistantLoop: one Quick Assistant turn, ready to run
// inside the stream's task.
type quickTurn struct {
	service  *quickassistant.Service
	messages []llm.ChatMessage
	runner   *quickassistant.Runner
	// pinned is a BYOK user's own model, bound as the loop's forced route only.
	pinned *llm.WireRoute
}

// quickInputs is what streamReply already worked out for the classic prompt.
type quickInputs struct {
	prior    []llm.ChatMessage
	hits     []memorystore.SearchResult
	schedule *string
	pref     *llm.Preference
	turnLoc  *time.Location // nil when neither the device nor the account names a zone
	// chat is a plain chat turn that asked for an app change, not a card turn.
	chat bool
	// appActions offers the space, file and memory_update tools.
	appActions bool
}

// newQuickTurn assembles the loop's prompt and runner, as ConversationController
// does before its stream task starts.
func (m *Module) newQuickTurn(ctx context.Context, c quickassistant.Context, in quickInputs, t turn) *quickTurn {
	qa := m.opts.QuickAssistant
	service, deps := qa.Service, qa.Deps
	if in.appActions {
		service, deps = qa.AppActions.Service, qa.AppActions.Deps
	}
	spaceName := quickassistant.ActiveSpaceName(ctx, m.pool, t.vault, c)
	// With no zone known, turnLoc is nil: System then shows a UTC clock and asks
	// for an explicit offset, the shape the runner takes.
	system := quickassistant.SystemFor(quickassistant.SystemOptions{Chat: in.chat, AppActions: in.appActions}, c, spaceName, m.now(), in.turnLoc)
	// As classic does: a BYOK user's pinned model, with no fallbacks. A pinned
	// model that rejects tools fails round 1 and lands on classic. A provider
	// with no Shared id, or a preference that could not be read, leaves the
	// loop unpinned, as Swift's ConversationController does (its `try?` read
	// and `toShared()` guard).
	var pinned *llm.WireRoute
	if p := in.pref; p != nil && p.Mode == llm.ModeBYOK && p.PrimaryModel != "" {
		if id, ok := p.PrimaryProvider.ToShared(); ok {
			pinned = &llm.WireRoute{Provider: id, Model: p.PrimaryModel}
		}
	}
	// Always carries SOUL: these turns never see Hermes, whichever brain mode
	// the user is on.
	prompt := BuildPrompt(in.prior, in.hits, in.schedule, soulExcerpt(m.opts.Soul, t.actor, t.user.Username), t.disclosure == llm.DisclosureHidden)
	conv := t.conv.ID
	return &quickTurn{
		service:  service,
		messages: quickassistant.AddingSystemText(system, prompt, true),
		// A nil zone stays nil (ruling 80): chips and offset-less times then
		// read as unknown rather than as UTC.
		runner: quickassistant.NewRunner(deps, t.actor, t.vault, in.turnLoc, &conv, m.log),
		pinned: pinned,
	}
}

// answerQuick runs the loop under the turn's routing binding, plus the BYOK
// pin on this binding only: ordinary and fallback classic turns keep
// resolving BYOK in the stream service. Picking happens inside, so a
// free-lane grant is claimed once for the question. The turn also offers the
// asker's connected-app tools; the loop then keeps it on Anthropic.
func (m *Module) answerQuick(ctx context.Context, t turn, bind func(*llm.Values), yield func(StreamEvent) bool) quickassistant.Outcome {
	loopCtx := llm.WithValues(ctx, func(v *llm.Values) {
		bind(v)
		if t.qa.pinned != nil {
			v.ForcedRoute = t.qa.pinned
		}
	})
	return t.qa.service.AnswerWith(loopCtx, t.qa.messages, t.sessionKey, t.sessionID, t.qa.runner, func(e StreamEvent) { yield(e) },
		m.connectedAppOptions(ctx, t))
}
