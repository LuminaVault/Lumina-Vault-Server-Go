package quickassistant

import (
	"context"
	"fmt"
	"lumina/internal/devicetools"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"time"

	"github.com/google/uuid"
)

// ConfirmWindow is how long a confirm card can be confirmed (ruling 32).
const ConfirmWindow = 10 * time.Minute

// proposeEvent is QuickAssistantToolRunner.proposeEvent. Calendar is the
// user's own account data, so the pending action is filed against their
// personal vault whichever vault the turn is in. It is reachable only with a
// device to write to: without one calendar_create is not offered (Decision 3).
func (r *Runner) proposeEvent(ctx context.Context, args map[string]any) (ToolResult, error) {
	title := swiftstr.TrimWhitespacesAndNewlines(stringArg(args, "title"))
	if title == "" {
		return ToolResult{}, refuse("title is required and must be a non-empty string.")
	}
	if swiftstr.Count(title) > maxTitleLength {
		return ToolResult{}, refuse(fmt.Sprintf("title must be at most %d characters.", maxTitleLength))
	}
	start, ok := r.parseDate(args["start"])
	if !ok {
		return ToolResult{}, refuse("start must be " + r.isoHint())
	}
	// Stored as the instant, in UTC: the phone reads it with a plain ISO 8601
	// parser, and must add the event the card showed whatever the model wrote.
	stored := map[string]any{"title": title, "start": isoString(start)}
	if value, present := args["end"]; present && value != nil {
		end, ok := r.parseDate(value)
		if !ok {
			return ToolResult{}, refuse("end must be " + r.isoHint())
		}
		if !end.After(start) {
			return ToolResult{}, refuse("end must be after start.")
		}
		stored["end"] = isoString(end)
	}
	place := ""
	if value, present := args["location"]; present && value != nil {
		text, isString := value.(string)
		if !isString || swiftstr.Count(text) > maxTitleLength {
			return ToolResult{}, refuse(fmt.Sprintf("location must be a string of at most %d characters.", maxTitleLength))
		}
		place = swiftstr.TrimWhitespacesAndNewlines(text)
		stored["location"] = place
	}

	// Ask for consent now, so the user is not shown a card that could never work.
	if r.Consent == nil {
		return ToolResult{}, errMissingDependency
	}
	if allowed, writes := r.Consent.IsAllowed(ctx, r.Actor, devicetools.DomainCalendar); !allowed || !writes {
		screen := ScreenDataAccess
		return ToolResult{
			ModelContent: permissionCard,
			Action: &Action{
				ID: wire.UUID{UUID: uuid.New()}, Kind: KindNeedsPermission,
				Summary: "Allow Calendar changes in Data Access to add events.", Screen: &screen,
			},
		}, nil
	}

	summary := "Add to Calendar: " + Shortened(title, 60) + ", " + r.formatted(start)
	if place != "" {
		summary += " at " + Shortened(place, 40)
	}
	return r.propose(ctx, "calendar_create", r.Actor, stored, summary)
}

// proposeForget is QuickAssistantToolRunner.proposeForget. The excerpt on the
// card is read from the vault-scoped row, so a memory in another vault can
// neither be proposed nor described.
func (r *Runner) proposeForget(ctx context.Context, args map[string]any) (ToolResult, error) {
	raw, isString := args["memoryID"].(string)
	memoryID, err := ids.Parse(raw)
	if !isString || err != nil {
		return ToolResult{}, refuse("memoryID must be a memory UUID from search.")
	}
	vault, err := r.resolveVault(ctx, vaultaccess.Write)
	if err != nil {
		return ToolResult{}, err
	}
	if r.Pool == nil {
		return ToolResult{}, errMissingDependency
	}
	memory, err := memorystore.New(r.Pool, nil).Find(ctx, vault, memoryID)
	if err != nil {
		return ToolResult{}, err
	}
	if memory == nil {
		return ToolResult{}, refuse("there is no memory with that ID in this vault.")
	}
	return r.propose(ctx, "memory_forget", vault,
		map[string]any{"memoryID": ids.Upper(memoryID)},
		"Forget this memory: \u201C"+Shortened(memory.Content, 80)+"\u201D")
}

// propose stores a pending action and returns its card, at most three per
// turn (ruling 27). Only the user's Confirm runs it.
func (r *Runner) propose(ctx context.Context, tool string, vault uuid.UUID, args map[string]any, summary string) (ToolResult, error) {
	if r.ConversationID == nil {
		return modelOnly(noConversation), nil
	}
	if r.Store == nil {
		return ToolResult{}, errMissingDependency
	}
	if r.state().Proposals() >= maxConfirmCardsPerTurn {
		return modelOnly(tooManyCards), nil
	}
	// Housekeeping only: a sweep that fails must not cost the user the card.
	if err := r.Store.DeleteLongExpired(ctx, r.Actor); err != nil {
		r.logger().WarnContext(ctx, "quick assistant expired actions not swept", "error_type", fmt.Sprintf("%T", err))
	}
	id := uuid.New()
	expiresAt := r.now().Add(ConfirmWindow)
	if err := r.Store.Insert(ctx, PendingAction{
		ID: id, TenantID: r.Actor, VaultID: vault, ConversationID: *r.ConversationID,
		Tool: tool, Args: args, Summary: summary, ExpiresAt: expiresAt,
	}); err != nil {
		return ToolResult{}, err
	}
	r.state().noteProposal()
	return ToolResult{
		ModelContent: proposalPending,
		Action: &Action{
			ID: wire.UUID{UUID: id}, Kind: KindNeedsConfirm, Summary: summary,
			ExpiresAt: &wire.Time{Time: expiresAt},
		},
	}, nil
}
