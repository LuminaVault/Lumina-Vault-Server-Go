package quickassistant

import (
	"context"
	"fmt"
	"lumina/internal/devicerpc"
	"lumina/internal/ids"
	"lumina/internal/modules/quickassistant/db"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"maps"
	"regexp"
	"strconv"
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

	appleRemindersLimit = 100
	deviceOffline       = "device did not respond (offline or timed out)"
)

// Apple data domains (AppleDataDomain raw values).
const (
	domainHealth    = "health"
	domainCalendar  = "calendar"
	domainReminders = "reminders"
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
	var raw map[string]any
	switch name {
	case "health_query":
		days, ok := intArg(args, "days")
		raw = r.healthQuery(ctx, stringArg(args, "metric"), days, ok)
	case "calendar_query":
		days, ok := intArg(args, "days")
		raw = r.calendarQuery(ctx, days, ok)
	case "reminders_list":
		raw = r.remindersList(ctx)
	default:
		return modelOnly(notAvailable), nil
	}
	return jsonResult(toolResult(raw))
}

// toolResult is MCPService.toolResult(fromPersonalJSON:): a refused or failed
// call also carries isError and the reason as message.
func toolResult(raw map[string]any) map[string]any {
	if raw["status"] == "error" {
		raw["isError"] = true
		raw["message"] = raw["reason"]
	}
	return raw
}

func errorResult(reason string) map[string]any {
	return map[string]any{"status": "error", "reason": reason}
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
	if r.turn.Writes() >= maxWritesPerTurn {
		// The prefix twice, as Swift's Refusal(message: tooManyWrites) reads.
		return ToolResult{}, refuse(tooManyWrites)
	}

	reminder, err := r.Reminders.Create(ctx, r.Actor, title, body, fireAt, nil)
	if err != nil {
		return ToolResult{}, err
	}
	r.turn.noteWrite()
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
// its failure never fails the reminder that already exists. One attempt, never
// queued: a write the phone cannot take now must not land a day later behind
// a chip that never mentioned Apple (ruling 22).
func (r *Runner) mirrorToApple(ctx context.Context, title, body string, fireAt time.Time) bool {
	if r.Devices == nil || r.Consent == nil {
		return false
	}
	if allowed, writes := r.Consent.IsAllowed(ctx, r.Actor, domainReminders); !allowed || !writes {
		return false
	}
	cmd := devicerpc.NewCommand(devicerpc.KindReminderCreate, domainReminders, map[string]string{
		"title": title, "notes": body, "due": isoString(fireAt),
	})
	ctx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()
	result, err := r.Devices.Request(ctx, r.Actor, cmd)
	if err != nil || !result.OK {
		// The status only, as Swift logs it: never the title.
		r.logger().WarnContext(ctx, "quick assistant reminder mirror failed", "status", "error")
		return false
	}
	return true
}

func (r *Runner) listReminders(ctx context.Context) (ToolResult, error) {
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

// MARK: - Personal reads (PersonalDataTools)

// healthQuery is daily aggregates of synced HealthKit samples, optionally one
// metric, over the last days (default 30, 1 to 365).
func (r *Runner) healthQuery(ctx context.Context, metric string, days int, daysGiven bool) map[string]any {
	if allowed, _ := r.consent(ctx, domainHealth); !allowed {
		return errorResult("health access not allowed by the user")
	}
	if !daysGiven {
		days = 30
	}
	days = max(1, min(days, 365))
	params := db.HealthDailyParams{TenantID: r.Actor, Days: int32(days)} //nolint:gosec // clamped to 1..365
	if metric != "" {
		params.Metric = &metric
	}
	rows, err := r.queries().HealthDaily(ctx, params)
	if err != nil {
		return errorResult("health_query failed: " + err.Error())
	}
	points := make([]any, 0, len(rows))
	for _, row := range rows {
		unit := ""
		if row.Unit != nil {
			unit = *row.Unit
		}
		points = append(points, map[string]any{
			"metric": row.EventType,
			"unit":   unit,
			"day":    row.Day.UTC().Format("2006-01-02"),
			"total":  fmt.Sprintf("%.2f", row.Total),
			"avg":    fmt.Sprintf("%.2f", row.Avg),
		})
	}
	return map[string]any{"status": "ok", "days": strconv.Itoa(days), "points": points}
}

// calendarQuery is upcoming events from the synced cache (Apple and Google),
// with a live device read on a cache miss or database error.
func (r *Runner) calendarQuery(ctx context.Context, days int, daysGiven bool) map[string]any {
	if !daysGiven {
		days = 7
	}
	days = max(1, min(days, 90))
	if allowed, _ := r.consent(ctx, domainCalendar); !allowed {
		return errorResult("calendar access not allowed by the user")
	}
	payload := map[string]string{"days": strconv.Itoa(days)}
	rows, err := r.queries().CalendarWindow(ctx, db.CalendarWindowParams{TenantID: r.Actor, Days: int32(days)}) //nolint:gosec // clamped to 1..90
	if err != nil || len(rows) == 0 {
		return r.deviceRead(ctx, domainCalendar, payload)
	}
	events := make([]any, 0, len(rows))
	for _, row := range rows {
		location := ""
		if row.Location != nil {
			location = *row.Location
		}
		events = append(events, map[string]any{
			"title": row.Title, "start": isoString(row.StartsAt), "end": isoString(row.EndsAt), "location": location,
		})
	}
	return map[string]any{"status": "ok", "items": events}
}

// remindersList is the synced Apple Reminders cache: open ones, soonest due
// first, undated last. items is a JSON string inside the JSON, as Swift
// encodes it.
func (r *Runner) remindersList(ctx context.Context) map[string]any {
	if allowed, _ := r.consent(ctx, domainReminders); !allowed {
		return errorResult("reminders access not allowed by the user")
	}
	rows, err := r.queries().OpenAppleReminders(ctx, db.OpenAppleRemindersParams{TenantID: r.Actor, Limit: appleRemindersLimit})
	if err != nil {
		return errorResult("reminders_list failed: " + err.Error())
	}
	if len(rows) == 0 {
		return r.deviceRead(ctx, domainReminders, map[string]string{})
	}
	items := make([]map[string]string, 0, len(rows))
	for _, row := range rows {
		item := map[string]string{"title": row.Title}
		if row.DueAt != nil {
			item["due"] = isoString(*row.DueAt)
		}
		if row.Notes != nil && *row.Notes != "" {
			item["notes"] = *row.Notes
		}
		items = append(items, item)
	}
	encoded, err := encodeJSON(items)
	if err != nil {
		return errorResult("could not encode tool result")
	}
	return map[string]any{"status": "ok", "items": encoded}
}

// deviceRead asks the phone for a fresh read. Without a device requester
// (until P5) it is Swift's offline answer at once, with no wait.
func (r *Runner) deviceRead(ctx context.Context, domain string, payload map[string]string) map[string]any {
	if allowed, _ := r.consent(ctx, domain); !allowed {
		return errorResult(domain + " access not allowed by the user")
	}
	if r.Devices == nil {
		return errorResult(deviceOffline)
	}
	ctx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()
	result, err := r.Devices.Request(ctx, r.Actor, devicerpc.NewCommand(devicerpc.KindDeviceFetch, domain, maps.Clone(payload)))
	if err != nil {
		return errorResult(deviceOffline)
	}
	if !result.OK {
		reason := "device reported failure"
		if result.Error != nil {
			reason = *result.Error
		}
		return errorResult(reason)
	}
	items, ok := result.Payload["items"]
	if !ok {
		items = "[]"
	}
	return map[string]any{"status": "ok", "items": items}
}

// consent is the actor's consent for domain; no reader is no consent.
func (r *Runner) consent(ctx context.Context, domain string) (allowed, writes bool) {
	if r.Consent == nil {
		return false, false
	}
	return r.Consent.IsAllowed(ctx, r.Actor, domain)
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
