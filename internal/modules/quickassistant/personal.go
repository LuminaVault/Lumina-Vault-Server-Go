package quickassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/devicerpc"
	"lumina/internal/devicetools"
	"lumina/internal/ids"
	"lumina/internal/modules/mcp"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"regexp"
	"time"

	"github.com/google/uuid"
)

const (
	// futureSlack is how far in the past a fire time may be and still be now.
	futureSlack = 60 * time.Second
	// listLimit is app_reminders_list's cap.
	listLimit = 20
	// deviceTimeout bounds one phone round trip on top of the broker's own
	// 12 s wait, so a broker that never answers cannot hold the turn.
	deviceTimeout = 15 * time.Second
)

// personal runs a tool on the actor's own account. It never asks for a vault,
// so a shared vault cannot reach a co-member's data (Decision 2).
func (r *Runner) personal(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	switch name {
	case "app_reminder_create":
		return r.createReminder(ctx, args)
	case "app_reminders_list":
		return r.listReminders(ctx)
	}
	if r.Pool == nil {
		return ToolResult{}, errMissingDependency
	}
	// MCPService.callPersonal's argument reading, on the actor's own account.
	var raw string
	switch name {
	case "health_query":
		var metric *string
		if m, ok := args["metric"].(string); ok {
			metric = &m
		}
		raw = r.devices().HealthQuery(ctx, r.Actor, metric, intArgPtr(args, "days"))
	case "calendar_query":
		raw = r.devices().CalendarQuery(ctx, r.Actor, intArgPtr(args, "days"))
	case "reminders_list":
		raw = r.devices().RemindersList(ctx, r.Actor)
	default:
		return modelOnly(notAvailable), nil
	}
	return jsonResult(mcp.PersonalResult(raw))
}

// MARK: - LuminaVault's own reminders

// createReminder files the reminder under the actor, never the turn's vault,
// and mirrors it to Apple Reminders once when the user allows it (ruling 22).
func (r *Runner) createReminder(ctx context.Context, args map[string]any) (ToolResult, error) {
	title := swiftstr.TrimWhitespacesAndNewlines(stringArg(args, "title"))
	if title == "" {
		return ToolResult{}, refuse("title is required and must be a non-empty string.")
	}
	fireAt, ok := r.parseDate(args["fireAt"])
	if !ok {
		return ToolResult{}, refuse("fireAt must be " + r.isoHint())
	}
	if swiftstr.Count(title) > maxTitleLength {
		return ToolResult{}, refuse(fmt.Sprintf("title must be at most %d characters.", maxTitleLength))
	}
	if fireAt.Sub(r.now()) <= -futureSlack {
		return ToolResult{}, refuse("fireAt is in the past. Ask the user, or work out the right date.")
	}
	body := ""
	if value, present := args["body"]; present && value != nil {
		text, isString := value.(string)
		if !isString {
			return ToolResult{}, refuse("body must be a string.")
		}
		if swiftstr.Count(text) > maxBodyLength {
			return ToolResult{}, refuse(fmt.Sprintf("body must be at most %d characters.", maxBodyLength))
		}
		body = text
	}
	if r.state().Writes() >= maxWritesPerTurn {
		// The prefix twice, as Swift's Refusal(message: tooManyWrites) reads.
		return ToolResult{}, refuse(tooManyWrites)
	}

	if r.Reminders == nil {
		return ToolResult{}, errMissingDependency
	}
	reminder, err := r.Reminders.Create(ctx, r.Actor, title, body, fireAt, nil)
	if err != nil {
		return ToolResult{}, err
	}
	r.state().noteWrite()
	mirrored := r.mirrorToApple(ctx, title, body, fireAt)

	summary := "Reminder set: " + Shortened(title, 60) + ", " + r.formatted(fireAt)
	content := map[string]any{
		"status":     "ok",
		"reminderID": ids.Upper(reminder.ID),
		"fireAt":     isoString(fireAt),
	}
	if mirrored {
		summary += " (also in Apple Reminders)"
		content["alsoAddedToAppleReminders"] = true
	}
	result, err := jsonResult(content)
	if err != nil {
		return ToolResult{}, err
	}
	result.Action = &Action{ID: wire.UUID{UUID: uuid.New()}, Kind: KindDone, Summary: summary}
	return result, nil
}

// mirrorToApple is a courtesy: it needs the user's consent for writes, and
// its failure never fails the reminder that already exists. One attempt through
// devicetools with no queue: a write the phone cannot take now must not land a
// day later behind a chip that never mentioned Apple (ruling 22). With no
// device requester it is skipped before any read (ruling 61).
func (r *Runner) mirrorToApple(ctx context.Context, title, body string, fireAt time.Time) bool {
	if r.Devices == nil || r.Consent == nil || r.Pool == nil {
		return false
	}
	if allowed, writes := r.Consent.IsAllowed(ctx, r.Actor, devicetools.DomainReminders); !allowed || !writes {
		return false
	}
	due := isoString(fireAt)
	raw := r.devices().ReminderCreate(ctx, r.Actor, title, &body, &due)
	var out struct {
		Status *string `json:"status"`
	}
	status := "unreadable"
	if json.Unmarshal([]byte(raw), &out) == nil && out.Status != nil {
		status = *out.Status
	}
	if status != "ok" {
		// The status only, as Swift logs it: never the title.
		r.logger().WarnContext(ctx, "quick assistant reminder mirror failed", "status", status)
		return false
	}
	return true
}

func (r *Runner) listReminders(ctx context.Context) (ToolResult, error) {
	if r.Reminders == nil {
		return ToolResult{}, errMissingDependency
	}
	rows, err := r.Reminders.Upcoming(ctx, r.Actor, listLimit)
	if err != nil {
		return ToolResult{}, err
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"title":  row.Title,
			"fireAt": isoString(row.FireAt),
			"when":   r.formatted(row.FireAt),
		}
		if row.Body != "" {
			item["body"] = row.Body
		}
		if row.RecurrenceCron != nil {
			item["repeats"] = true
		}
		items = append(items, item)
	}
	return jsonResult(map[string]any{"reminders": items})
}

// MARK: - Personal reads

// devices is PersonalDataTools on the actor's phone: no queue, so a write the
// phone misses is never stored for later (ruling 22), and every round trip
// bounded by deviceTimeout. With no device requester (until P5) the phone is
// offline: a read answers Swift's offline reason at once, with no wait.
func (r *Runner) devices() *devicetools.Tools {
	return devicetools.New(r.Pool, boundedDevice{r.Devices}, nil)
}

// boundedDevice is the turn's device requester behind devicetools.Broker.
type boundedDevice struct{ requester DeviceRequester }

// errNoDevice is a phone that cannot be reached because none is wired.
var errNoDevice = errors.New("no device requester")

func (d boundedDevice) Request(ctx context.Context, tenant uuid.UUID, cmd devicerpc.Command) (devicerpc.Result, error) {
	if d.requester == nil {
		return devicerpc.Result{}, errNoDevice
	}
	ctx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()
	return d.requester.Request(ctx, tenant, cmd)
}

// MARK: - Dates

// localDateTime is a date and time with no offset: the whole string, so an
// offset RFC 3339 refused ("+0100") is never dropped and read as local time.
var localDateTime = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2})(:[0-9]{2})?(\.[0-9]+)?$`)

// parseDate is the instant an ISO 8601 date and time names. With an offset or
// "Z" it is exactly that instant. With none it is a time on the user's clock,
// read in the turn's zone; with no zone known it is refused rather than taken
// as UTC (ruling 50). A wall time the zone skips (a DST gap) is refused.
func (r *Runner) parseDate(value any) (time.Time, bool) {
	raw, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	text := swiftstr.TrimWhitespacesAndNewlines(raw)
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, true
	}
	if r.Loc == nil {
		return time.Time{}, false
	}
	m := localDateTime.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	seconds := m[2]
	if seconds == "" {
		seconds = ":00"
	}
	const layout = "2006-01-02T15:04:05"
	wall := m[1] + seconds
	t, err := time.ParseInLocation(layout, wall, r.Loc)
	if err != nil || t.In(r.Loc).Format(layout) != wall {
		return time.Time{}, false
	}
	return t, true
}

func (r *Runner) isoHint() string {
	if r.Loc == nil {
		return "an ISO 8601 date and time with an offset, like 2030-01-01T09:00:00+01:00."
	}
	return "an ISO 8601 date and time, like 2030-01-01T09:00:00, read in the user's time zone."
}

// formatted is "EEE d MMM yyyy, HH:mm" in the turn's zone, or in UTC marked
// " UTC" when no zone is known.
func (r *Runner) formatted(t time.Time) string {
	const layout = "Mon 2 Jan 2006, 15:04"
	if r.Loc == nil {
		return t.UTC().Format(layout) + " UTC"
	}
	return t.In(r.Loc).Format(layout)
}

// isoString is ISO8601DateFormatter with .withInternetDateTime: UTC, whole
// seconds, "Z".
func isoString(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z07:00") }
