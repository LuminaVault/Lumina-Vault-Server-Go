package hermesruns

import (
	"context"
	"log/slog"
	"lumina/internal/apns"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Notifier is HermesRunPushNotifying. Implementations never fail the watcher.
type Notifier interface {
	ApprovalRequested(ctx context.Context, tenant uuid.UUID, run DTO)
	RunFinished(ctx context.Context, tenant uuid.UUID, run DTO)
}

// Pusher is the slice of apns.Service the notifier needs.
type Pusher interface {
	Notify(ctx context.Context, tenant uuid.UUID, m apns.Message) error
}

// APNSNotifier is APNSHermesRunPushNotifier.
type APNSNotifier struct {
	Push Pusher
	Log  *slog.Logger
}

const (
	approvalTitle = "Hermes needs approval"
	bodyLimit     = 200
)

// ApprovalRequested pushes the actionable approval category.
func (n APNSNotifier) ApprovalRequested(ctx context.Context, tenant uuid.UUID, run DTO) {
	var command *string
	choices := AllChoices()
	if run.PendingApproval != nil {
		command = run.PendingApproval.Command
		choices = run.PendingApproval.Choices
	}
	body, ok := BodyText(command)
	if !ok {
		body = "A tool call is waiting for your approval"
	}
	payload := basePayload(run)
	names := make([]string, len(choices))
	for i, c := range choices {
		names[i] = string(c)
	}
	payload["choices"] = strings.Join(names, ",")
	n.deliver(ctx, tenant, approvalTitle, body, apns.CategoryApproval, payload)
}

// RunFinished pushes the runCompleted category with the outcome.
func (n APNSNotifier) RunFinished(ctx context.Context, tenant uuid.UUID, run DTO) {
	var title string
	switch run.Status {
	case Completed:
		title = "Hermes finished a run"
	case Failed:
		title = "Hermes run failed"
	case Stopped:
		title = "Hermes run stopped"
	case Lost:
		title = "Hermes run was lost"
	default:
		title = "Hermes run update"
	}
	body, ok := BodyText(run.Summary)
	if !ok {
		body, ok = BodyText(run.Error)
	}
	if !ok {
		body, ok = BodyText(&run.Prompt)
	}
	if !ok {
		body = string(run.Status)
	}
	n.deliver(ctx, tenant, title, body, apns.CategoryRunCompleted, basePayload(run))
}

func (n APNSNotifier) deliver(ctx context.Context, tenant uuid.UUID, title, body string, category apns.Category, payload map[string]string) {
	if n.Push == nil {
		return
	}
	if err := n.Push.Notify(ctx, tenant, apns.Message{Title: title, Body: body, Category: category, Payload: payload}); err != nil {
		log := n.Log
		if log == nil {
			log = slog.Default()
		}
		log.WarnContext(ctx, "hermes runs push failed", "category", string(category), "run", payload["runID"], "error", redact(err.Error()))
	}
}

func basePayload(run DTO) map[string]string {
	return map[string]string{"runID": run.ID.String(), "hermesRunID": run.HermesRunID, "status": string(run.Status)}
}

var bearerRe = regexp.MustCompile(`Bearer\s+[A-Za-z0-9._\-]+`)

// redact is Logger.redact: bearer tokens become "Bearer ***".
func redact(s string) string { return bearerRe.ReplaceAllString(s, "Bearer ***") }

// BodyText is APNSHermesRunPushNotifier.bodyText: single line, bearer-redacted,
// capped at 200 characters with an ellipsis.
func BodyText(raw *string) (string, bool) {
	if raw == nil {
		return "", false
	}
	lines := strings.FieldsFunc(redact(*raw), isNewline)
	kept := lines[:0]
	for _, l := range lines {
		if t := strings.Trim(l, " \t"); t != "" {
			kept = append(kept, t)
		}
	}
	one := strings.Join(kept, " ")
	if one == "" {
		return "", false
	}
	runes := []rune(one)
	if len(runes) <= bodyLimit {
		return one, true
	}
	return string(runes[:bodyLimit-1]) + "…", true
}

func isNewline(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}
