package chat

import (
	"context"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/quickassistant"
	"time"
)

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
	prior       []llm.ChatMessage
	hits        []memorystore.SearchResult
	schedule    *string
	pref        *llm.Preference
	turnLoc     *time.Location // nil when neither the device nor the account names a zone
	scheduleLoc *time.Location // turnLoc, or UTC
}

// newQuickTurn assembles the loop's prompt and runner, as ConversationController
// does before its stream task starts.
func (m *Module) newQuickTurn(ctx context.Context, c quickassistant.Context, in quickInputs, t turn) *quickTurn {
	qa := m.opts.QuickAssistant
	spaceName := quickassistant.ActiveSpaceName(ctx, m.pool, t.vault, c)
	system := quickassistant.System(c, spaceName, m.now(), in.scheduleLoc)
	// As classic does: a BYOK user's pinned model, with no fallbacks. A pinned
	// model that rejects tools fails round 1 and lands on classic.
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
		service:  qa.Service,
		messages: quickassistant.AddingSystemText(system, prompt, true),
		// A nil zone stays nil (ruling 80): chips and offset-less times then
		// read as unknown rather than as UTC.
		runner: quickassistant.NewRunner(qa.Deps, t.actor, t.vault, t.user.Username, in.turnLoc, &conv, m.log),
		pinned: pinned,
	}
}

// answerQuick runs the loop under the turn's routing binding, plus the BYOK
// pin on this binding only: ordinary and fallback classic turns keep
// resolving BYOK in the stream service. Picking happens inside, so a
// free-lane grant is claimed once for the question.
func (m *Module) answerQuick(ctx context.Context, t turn, bind func(*llm.Values), yield func(StreamEvent) bool) quickassistant.Outcome {
	loopCtx := llm.WithValues(ctx, func(v *llm.Values) {
		bind(v)
		if t.qa.pinned != nil {
			v.ForcedRoute = t.qa.pinned
		}
	})
	return t.qa.service.Answer(loopCtx, t.qa.messages, t.sessionKey, t.sessionID, t.qa.runner, func(e StreamEvent) { yield(e) })
}
