package quickassistant

import (
	"context"
	"encoding/json"
	"lumina/internal/devicerpc"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakePhone is the device requester: it records every command and answers ok,
// or fails like a phone that never answered.
type fakePhone struct {
	mu       sync.Mutex
	commands []devicerpc.Command
	fail     bool
	payload  map[string]string
}

func (p *fakePhone) Request(_ context.Context, _ uuid.UUID, cmd devicerpc.Command) (devicerpc.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.commands = append(p.commands, cmd)
	if p.fail {
		return devicerpc.Result{}, devicerpc.ErrTimeout
	}
	return devicerpc.Result{ID: cmd.ID, OK: true, Payload: p.payload}, nil
}

func (p *fakePhone) sent() []devicerpc.Command {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]devicerpc.Command(nil), p.commands...)
}

func (g rig) allow(user uuid.UUID, domain string, writes bool) {
	g.exec(`INSERT INTO apple_consent (tenant_id, domain, allowed, allow_writes, updated_at) VALUES ($1, $2, TRUE, $3, now())`,
		user, domain, writes)
}

// phoneRunner is a runner for one turn with phone as its device requester.
func (g rig) phoneRunner(actor, vault uuid.UUID, phone DeviceRequester, loc *time.Location) *Runner {
	d := g.deps
	d.Devices = phone
	return NewRunner(d, actor, vault, "qa-user", loc, nil, nil)
}

type storedReminder struct {
	Title  string
	Body   string
	FireAt time.Time
}

func (g rig) reminders(tenant uuid.UUID) []storedReminder {
	g.t.Helper()
	rows, err := g.pool.Query(context.Background(), `SELECT title, body, fire_at FROM reminders WHERE tenant_id = $1 ORDER BY created_at, title`, tenant)
	require.NoError(g.t, err)
	defer rows.Close()
	var out []storedReminder
	for rows.Next() {
		var r storedReminder
		require.NoError(g.t, rows.Scan(&r.Title, &r.Body, &r.FireAt))
		out = append(out, r)
	}
	require.NoError(g.t, rows.Err())
	return out
}

func reminderArgs(title string, at time.Time) map[string]any {
	return map[string]any{"title": title, "fireAt": at.UTC().Format(time.RFC3339)}
}

func inAnHour() time.Time { return time.Now().Add(time.Hour) }

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

// 2030-01-01 09:00 in Santiago (UTC-03:00 in January) is 12:00 UTC.
var nineInSantiago = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

// MARK: - app_reminder_create and the Apple mirror

func TestReminderWithConsentOffCreatesOwnOnly(t *testing.T) {
	g := newRig(t)
	me := g.user()
	phone := &fakePhone{}

	result := g.phoneRunner(me, me, phone, nil).Run(context.Background(), "app_reminder_create", reminderArgs("Call mum", inAnHour()))

	require.Len(t, g.reminders(me), 1)
	require.Equal(t, "Call mum", g.reminders(me)[0].Title)
	require.Empty(t, phone.sent())
	require.Equal(t, KindDone, result.Action.Kind)
	require.Contains(t, result.Action.Summary, "Call mum")
	require.NotContains(t, result.Action.Summary, "Apple")
}

func TestConsentWithoutWritesDoesNotMirror(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "reminders", false)
	phone := &fakePhone{}

	result := g.phoneRunner(me, me, phone, nil).Run(context.Background(), "app_reminder_create", reminderArgs("Call mum", inAnHour()))

	require.Len(t, g.reminders(me), 1)
	require.Empty(t, phone.sent())
	require.Equal(t, KindDone, result.Action.Kind)
	require.NotContains(t, result.Action.Summary, "Apple")
}

func TestReminderWithConsentOnAlsoMirrors(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "reminders", true)
	phone := &fakePhone{}
	at := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

	result := g.phoneRunner(me, me, phone, nil).Run(context.Background(), "app_reminder_create",
		map[string]any{"title": "Call mum", "fireAt": "2030-01-01T12:00:00Z", "body": "about Sunday"})

	require.Len(t, g.reminders(me), 1)
	sent := phone.sent()
	require.Len(t, sent, 1)
	require.Equal(t, devicerpc.KindReminderCreate, sent[0].Kind)
	require.Equal(t, "reminders", sent[0].Domain)
	require.Equal(t, map[string]string{"title": "Call mum", "notes": "about Sunday", "due": "2030-01-01T12:00:00Z"}, sent[0].Payload)
	require.Equal(t, "Reminder set: Call mum, Tue 1 Jan 2030, 12:00 UTC (also in Apple Reminders)", result.Action.Summary)
	out := decodeObject(t, result.ModelContent)
	require.Equal(t, true, out["alsoAddedToAppleReminders"])
	require.Equal(t, "ok", out["status"])
	require.Equal(t, at.Format(time.RFC3339), out["fireAt"])
	id, _ := out["reminderID"].(string)
	require.Equal(t, strings.ToUpper(id), id)
	require.Equal(t, 1, g.count(`SELECT count(*) FROM reminders WHERE id = $1::uuid AND tenant_id = $2`, id, me))
}

func TestFailedMirrorStillSucceeds(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "reminders", true)
	phone := &fakePhone{fail: true}

	result := g.phoneRunner(me, me, phone, nil).Run(context.Background(), "app_reminder_create", reminderArgs("Call mum", inAnHour()))

	require.Len(t, g.reminders(me), 1)
	require.Len(t, phone.sent(), 1)
	require.Equal(t, KindDone, result.Action.Kind)
	require.NotContains(t, result.Action.Summary, "Apple")
	require.NotContains(t, result.ModelContent, "Apple")
}

func TestFailedMirrorNeverQueuesAnAppleWrite(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "reminders", true)

	result := g.phoneRunner(me, me, &fakePhone{fail: true}, nil).Run(context.Background(), "app_reminder_create", reminderArgs("Call mum", inAnHour()))

	require.Equal(t, KindDone, result.Action.Kind)
	require.Len(t, g.reminders(me), 1)
	require.Zero(t, g.count(`SELECT count(*) FROM device_command_queue`))
}

func TestNilDevicesSkipsTheMirror(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "reminders", true)

	result := g.runner(me, me).Run(context.Background(), "app_reminder_create", reminderArgs("Call mum", inAnHour()))

	require.Len(t, g.reminders(me), 1)
	require.Equal(t, KindDone, result.Action.Kind)
	require.NotContains(t, result.Action.Summary, "Apple")
	require.NotContains(t, result.ModelContent, "Apple")
	require.Zero(t, g.count(`SELECT count(*) FROM device_command_queue`))
}

// MARK: - Times and zones (ruling 50)

func TestReminderTimeIsShownInTheUsersTimeZone(t *testing.T) {
	g := newRig(t)
	me := g.user()
	// 2030-01-01 00:00 UTC is 09:00 in Tokyo, and a Tuesday.
	args := map[string]any{"title": "Stretch", "fireAt": "2030-01-01T00:00:00Z"}

	tokyo := g.phoneRunner(me, me, nil, mustZone(t, "Asia/Tokyo")).Run(context.Background(), "app_reminder_create", args)
	require.Equal(t, "Reminder set: Stretch, Tue 1 Jan 2030, 09:00", tokyo.Action.Summary)

	unknown := g.runner(me, me).Run(context.Background(), "app_reminder_create", args)
	require.Equal(t, "Reminder set: Stretch, Tue 1 Jan 2030, 00:00 UTC", unknown.Action.Summary)
}

func TestATimeWithNoOffsetIsReadInTheUsersZoneNotUTC(t *testing.T) {
	g := newRig(t)
	me := g.user()
	santiago := mustZone(t, "America/Santiago")

	for _, fireAt := range []string{"2030-01-01T09:00:00", "2030-01-01T09:00", "2030-01-01T09:00:00.500"} {
		result := g.phoneRunner(me, me, nil, santiago).Run(context.Background(), "app_reminder_create",
			map[string]any{"title": "Stretch " + fireAt, "fireAt": fireAt})
		require.NotNil(t, result.Action, "%s: %s", fireAt, result.ModelContent)
		require.Equal(t, "Reminder set: Stretch "+fireAt+", Tue 1 Jan 2030, 09:00", result.Action.Summary, fireAt)
	}
	stored := g.reminders(me)
	require.Len(t, stored, 3)
	for _, r := range stored {
		require.True(t, r.FireAt.Equal(nineInSantiago), "%s at %s", r.Title, r.FireAt)
	}
}

func TestAnyOffsetTheModelWritesIsStoredAsThatInstant(t *testing.T) {
	g := newRig(t)
	me := g.user()
	santiago := mustZone(t, "America/Santiago")
	sameInstant := []string{
		"2030-01-01T12:00:00Z", "2030-01-01T09:00:00-03:00", "2030-01-01T13:00:00+01:00",
		"2030-01-01T12:00:00.000Z", "2030-01-01T17:45:00+05:45", " 2030-01-01T12:00:00Z\n",
	}

	for i, fireAt := range sameInstant {
		// A turn per write: six in one turn would meet the cap.
		result := g.phoneRunner(me, me, nil, santiago).Run(context.Background(), "app_reminder_create", map[string]any{"title": "Call " + string(rune('a'+i)), "fireAt": fireAt})
		require.NotNil(t, result.Action, "%q: %s", fireAt, result.ModelContent)
		// The chip shows the user's own clock, whatever offset the model chose.
		require.Equal(t, "Reminder set: Call "+string(rune('a'+i))+", Tue 1 Jan 2030, 09:00", result.Action.Summary, fireAt)
		require.Equal(t, "2030-01-01T12:00:00Z", decodeObject(t, result.ModelContent)["fireAt"], fireAt)
	}
	stored := g.reminders(me)
	require.Len(t, stored, len(sameInstant))
	for _, s := range stored {
		require.True(t, s.FireAt.Equal(nineInSantiago))
	}
}

func TestNoZoneRefusesATimeWithoutOffset(t *testing.T) {
	g := newRig(t)
	me := g.user()

	noZone := g.runner(me, me).Run(context.Background(), "app_reminder_create",
		map[string]any{"title": "Stretch", "fireAt": "2030-01-01T09:00:00"})
	require.Nil(t, noZone.Action)
	require.Equal(t, "The tool did not run: fireAt must be an ISO 8601 date and time with an offset, like 2030-01-01T09:00:00+01:00.", noZone.ModelContent)

	withZone := g.phoneRunner(me, me, nil, mustZone(t, "Europe/Lisbon")).Run(context.Background(), "app_reminder_create",
		map[string]any{"title": "Stretch", "fireAt": "soon"})
	require.Equal(t, "The tool did not run: fireAt must be an ISO 8601 date and time, like 2030-01-01T09:00:00, read in the user's time zone.", withZone.ModelContent)

	// 02:30 on 10 March 2030 does not exist in New York; it is refused, not moved.
	gap := g.phoneRunner(me, me, nil, mustZone(t, "America/New_York")).Run(context.Background(), "app_reminder_create",
		map[string]any{"title": "Stretch", "fireAt": "2030-03-10T02:30:00"})
	require.Nil(t, gap.Action)
	require.True(t, strings.HasPrefix(gap.ModelContent, "The tool did not run: fireAt must be"))
	require.Empty(t, g.reminders(me))
}

func TestSummaryIsBuiltFromTheTrimmedTitleOnOneLine(t *testing.T) {
	g := newRig(t)
	me := g.user()

	result := g.runner(me, me).Run(context.Background(), "app_reminder_create", reminderArgs("  Pay rent\nnow  ", inAnHour()))

	require.True(t, strings.HasPrefix(result.Action.Summary, "Reminder set: Pay rent now, "), result.Action.Summary)
	require.Equal(t, "Pay rent\nnow", g.reminders(me)[0].Title)
}

func TestInvalidFireAtIsRejected(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	for _, fireAt := range []any{"soon", "2030-01-01", "2030-01-01T09:00:00", "2001-01-01T09:00:00Z", "2030-01-01T09:00:00+0100", 1893499200, nil} {
		result := r.Run(context.Background(), "app_reminder_create", map[string]any{"title": "Call mum", "fireAt": fireAt})
		require.Nil(t, result.Action, "%v", fireAt)
		require.True(t, strings.HasPrefix(result.ModelContent, "The tool did not run"), "%v: %s", fireAt, result.ModelContent)
	}
	blank := r.Run(context.Background(), "app_reminder_create", reminderArgs("   ", inAnHour()))
	require.Nil(t, blank.Action)
	require.Equal(t, "The tool did not run: title is required and must be a non-empty string.", blank.ModelContent)
	require.Empty(t, g.reminders(me))
}

func TestPastFireAtIsRejected(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	past := r.Run(context.Background(), "app_reminder_create", reminderArgs("Late", time.Now().Add(-2*time.Minute)))
	require.Nil(t, past.Action)
	require.Equal(t, "The tool did not run: fireAt is in the past. Ask the user, or work out the right date.", past.ModelContent)

	// Within the 60 s slack is still now.
	justNow := r.Run(context.Background(), "app_reminder_create", reminderArgs("Now", time.Now().Add(-30*time.Second)))
	require.NotNil(t, justNow.Action)
	require.Len(t, g.reminders(me), 1)
}

func TestTitleAndBodyLimitsHoldAtTheBoundary(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)
	args := func(title, body int) map[string]any {
		a := reminderArgs(strings.Repeat("t", title), inAnHour())
		a["body"] = strings.Repeat("b", body)
		return a
	}

	longTitle := r.Run(context.Background(), "app_reminder_create", args(201, 1))
	require.Nil(t, longTitle.Action)
	require.Equal(t, "The tool did not run: title must be at most 200 characters.", longTitle.ModelContent)
	longBody := r.Run(context.Background(), "app_reminder_create", args(1, 2001))
	require.Nil(t, longBody.Action)
	require.Equal(t, "The tool did not run: body must be at most 2000 characters.", longBody.ModelContent)
	notText := r.Run(context.Background(), "app_reminder_create", map[string]any{"title": "x", "fireAt": inAnHour().UTC().Format(time.RFC3339), "body": 3})
	require.Equal(t, "The tool did not run: body must be a string.", notText.ModelContent)
	require.Empty(t, g.reminders(me))

	// Characters, as Swift counts them: 200 of "é" spelled with a combining mark.
	atLimit := r.Run(context.Background(), "app_reminder_create", map[string]any{
		"title": strings.Repeat("é", 200), "fireAt": inAnHour().UTC().Format(time.RFC3339), "body": strings.Repeat("b", 2000),
	})
	require.NotNil(t, atLimit.Action, atLimit.ModelContent)
	require.Len(t, g.reminders(me), 1)
}

func TestFiveWritesPerTurnThenRefusedAcrossReminderAndMemory(t *testing.T) {
	g := newRig(t)
	me := g.user()
	// One runner is one turn, however many rounds call it.
	r := g.runner(me, me)

	// Failures do not count.
	for range 3 {
		bad := r.Run(context.Background(), "app_reminder_create", map[string]any{"title": "x", "fireAt": "soon"})
		require.Nil(t, bad.Action)
	}
	// Neither does a duplicate memory.
	first := r.Run(context.Background(), "memory_save", map[string]any{"content": "fact one"})
	duplicate := r.Run(context.Background(), "memory_save", map[string]any{"content": "fact one"})
	require.NotNil(t, first.Action)
	require.Nil(t, duplicate.Action)

	for n := 1; n <= 4; n++ {
		result := r.Run(context.Background(), "app_reminder_create", reminderArgs("r"+string(rune('0'+n)), inAnHour()))
		require.NotNil(t, result.Action, "write %d: %s", n+1, result.ModelContent)
	}
	// The sixth change is refused, whichever tool it is.
	sixthReminder := r.Run(context.Background(), "app_reminder_create", reminderArgs("r5", inAnHour()))
	sixthMemory := r.Run(context.Background(), "memory_save", map[string]any{"content": "fact two"})
	for _, refused := range []ToolResult{sixthReminder, sixthMemory} {
		require.Nil(t, refused.Action)
		// Swift throws Refusal(message: tooManyWrites), so the prefix appears twice.
		require.Equal(t, "The tool did not run: The tool did not run: too many changes in one turn. Tell the user what is done and ask them to send the rest separately.", refused.ModelContent)
	}
	require.Len(t, g.reminders(me), 4)
	require.Equal(t, 1, g.memories(me))
}

// MARK: - app_reminders_list

func (g rig) reminder(tenant uuid.UUID, title string, fireAt time.Time, fired bool, cron *string) {
	var firedAt *time.Time
	if fired {
		now := time.Now()
		firedAt = &now
	}
	g.exec(`INSERT INTO reminders (id, tenant_id, title, body, fire_at, recurrence_cron, fired_at, created_at)
		VALUES ($1, $2, $3, '', $4, $5, $6, now())`, uuid.New(), tenant, title, fireAt, cron, firedAt)
}

func listedTitles(t *testing.T, content string) []string {
	t.Helper()
	var titles []string
	for _, item := range decodeObject(t, content)["reminders"].([]any) {
		titles = append(titles, item.(map[string]any)["title"].(string))
	}
	return titles
}

func TestRemindersListIsTheActorsOwnUpcomingSoonestFirst(t *testing.T) {
	g := newRig(t)
	mine, other := g.user(), g.user()
	now := time.Now()
	weekly := "0 9 * * 1"
	g.reminder(mine, "later", now.Add(2*time.Hour), false, &weekly)
	g.reminder(mine, "sooner", now.Add(time.Hour), false, nil)
	g.reminder(mine, "already fired", now.Add(-time.Hour), true, nil)
	g.reminder(other, "not mine", now.Add(30*time.Minute), false, nil)

	result := g.phoneRunner(mine, mine, nil, mustZone(t, "Asia/Tokyo")).Run(context.Background(), "app_reminders_list", map[string]any{})

	require.Nil(t, result.Action)
	require.Equal(t, []string{"sooner", "later"}, listedTitles(t, result.ModelContent))
	items := decodeObject(t, result.ModelContent)["reminders"].([]any)
	later := items[1].(map[string]any)
	require.Equal(t, true, later["repeats"])
	require.Nil(t, items[0].(map[string]any)["repeats"])
	require.Nil(t, items[0].(map[string]any)["body"])
	require.Equal(t, now.Add(2*time.Hour).In(mustZone(t, "Asia/Tokyo")).Format("Mon 2 Jan 2006, 15:04"), later["when"])
}

func TestRemindersBelongToTheActorEvenUnderASharedVault(t *testing.T) {
	g := newRig(t)
	owner, viewer := g.user(), g.user()
	vault := g.sharedVault(owner, viewer, "viewer")
	g.reminder(owner, "owner's", time.Now().Add(time.Hour), false, nil)
	r := g.runner(viewer, vault)

	created := r.Run(context.Background(), "app_reminder_create", reminderArgs("Call mum", inAnHour()))
	listed := r.Run(context.Background(), "app_reminders_list", map[string]any{})

	require.NotNil(t, created.Action)
	require.Len(t, g.reminders(viewer), 1)
	require.Empty(t, g.reminders(vault))
	require.Len(t, g.reminders(owner), 1)
	require.Equal(t, []string{"Call mum"}, listedTitles(t, listed.ModelContent))
}

// MARK: - Personal reads

func (g rig) steps(user uuid.UUID, value float64) {
	g.exec(`INSERT INTO health_events (id, tenant_id, event_type, value_numeric, unit, recorded_at, created_at)
		VALUES ($1, $2, 'steps', $3, 'count', now(), now())`, uuid.New(), user, value)
}

func TestViewerCannotReachAnotherMembersHealth(t *testing.T) {
	g := newRig(t)
	owner, viewer := g.user(), g.user()
	vault := g.sharedVault(owner, viewer, "viewer")
	// Both allowed Health and both have data, so only tenancy separates them.
	for user, value := range map[uuid.UUID]float64{owner: 7777, viewer: 1234} {
		g.allow(user, "health", false)
		g.steps(user, value)
	}

	result := g.runner(viewer, vault).Run(context.Background(), "health_query", map[string]any{"metric": "steps"})

	require.Contains(t, result.ModelContent, "1234")
	require.NotContains(t, result.ModelContent, "7777")
	out := decodeObject(t, result.ModelContent)
	require.Equal(t, "ok", out["status"])
	require.Equal(t, "30", out["days"])
	point := out["points"].([]any)[0].(map[string]any)
	require.Equal(t, map[string]any{
		"metric": "steps", "unit": "count", "day": time.Now().UTC().Format("2006-01-02"),
		"total": "1234.00", "avg": "1234.00",
	}, point)
}

func TestHealthDaysAreClamped(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "health", false)
	r := g.runner(me, me)

	for days, want := range map[any]string{0: "1", 1000: "365", 7: "7", "x": "30"} {
		out := decodeObject(t, r.Run(context.Background(), "health_query", map[string]any{"days": days}).ModelContent)
		require.Equal(t, want, out["days"], "%v", days)
	}
}

func TestPersonalReadsNeedConsent(t *testing.T) {
	g := newRig(t)
	me := g.user()
	phone := &fakePhone{}
	r := g.phoneRunner(me, me, phone, nil)

	for tool, reason := range map[string]string{
		"health_query":   "health access not allowed by the user",
		"calendar_query": "calendar access not allowed by the user",
		"reminders_list": "reminders access not allowed by the user",
	} {
		out := decodeObject(t, r.Run(context.Background(), tool, map[string]any{}).ModelContent)
		require.Equal(t, map[string]any{"status": "error", "reason": reason, "isError": true, "message": reason}, out, tool)
	}
	require.Empty(t, phone.sent())
}

func TestCalendarCacheMissWithoutDevicesAnswersAtOnce(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "calendar", false)
	g.allow(me, "reminders", false)
	r := g.runner(me, me)

	for _, tool := range []string{"calendar_query", "reminders_list"} {
		start := time.Now()
		out := decodeObject(t, r.Run(context.Background(), tool, map[string]any{}).ModelContent)
		require.Less(t, time.Since(start), time.Second, tool)
		require.Equal(t, "error", out["status"], tool)
		require.Equal(t, "device did not respond (offline or timed out)", out["reason"], tool)
		require.Equal(t, true, out["isError"], tool)
	}
}

func TestCalendarCacheMissAsksTheDevice(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "calendar", false)
	phone := &fakePhone{payload: map[string]string{"items": `[{"title":"Dentist"}]`}}

	out := decodeObject(t, g.phoneRunner(me, me, phone, nil).Run(context.Background(), "calendar_query", map[string]any{"days": 3}).ModelContent)

	require.Equal(t, map[string]any{"status": "ok", "items": `[{"title":"Dentist"}]`}, out)
	sent := phone.sent()
	require.Len(t, sent, 1)
	require.Equal(t, devicerpc.KindDeviceFetch, sent[0].Kind)
	require.Equal(t, "calendar", sent[0].Domain)
	require.Equal(t, map[string]string{"days": "3"}, sent[0].Payload)
}

func TestCalendarReadsUpcomingNonCancelledEventsInTheWindow(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "calendar", false)
	start := time.Now().Add(2 * time.Hour).Truncate(time.Second).UTC()
	event := func(title, status string, at time.Time) {
		g.exec(`INSERT INTO calendar_events (tenant_id, source, external_id, title, starts_at, ends_at, status, location)
			VALUES ($1, 'apple_eventkit', $2, $3, $4, $5, $6, NULL)`, me, uuid.NewString(), title, at, at.Add(time.Hour), status)
	}
	event("Dentist", "confirmed", start)
	event("Cancelled", "cancelled", start)
	event("Past", "confirmed", start.Add(-5*time.Hour))
	event("Next month", "confirmed", start.Add(40*24*time.Hour))
	phone := &fakePhone{}

	out := decodeObject(t, g.phoneRunner(me, me, phone, nil).Run(context.Background(), "calendar_query", map[string]any{}).ModelContent)

	require.Equal(t, map[string]any{"status": "ok", "items": []any{map[string]any{
		"title": "Dentist", "start": isoString(start), "end": isoString(start.Add(time.Hour)), "location": "",
	}}}, out)
	require.Empty(t, phone.sent())
}

func TestAppleRemindersAreOpenOnesSoonestDueFirstAsAString(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, "reminders", false)
	due := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	add := func(title string, dueAt *time.Time, notes *string, completed bool) {
		g.exec(`INSERT INTO apple_reminders (id, tenant_id, external_id, title, notes, due_at, completed, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())`, uuid.New(), me, uuid.NewString(), title, notes, dueAt, completed)
	}
	later := due.Add(time.Hour)
	notes := "bring card"
	add("no date", nil, nil, false)
	add("later", &later, &notes, false)
	add("first", &due, nil, false)
	add("done", &due, nil, true)

	out := decodeObject(t, g.runner(me, me).Run(context.Background(), "reminders_list", map[string]any{}).ModelContent)

	require.Equal(t, "ok", out["status"])
	items, ok := out["items"].(string)
	require.True(t, ok, "items is a JSON string, as Swift encodes it")
	var decoded []map[string]any
	require.NoError(t, json.Unmarshal([]byte(items), &decoded))
	require.Equal(t, []map[string]any{
		{"title": "first", "due": "2030-01-01T12:00:00Z"},
		{"title": "later", "due": "2030-01-01T13:00:00Z", "notes": "bring card"},
		{"title": "no date"},
	}, decoded)
}

func TestPersonalToolsMarkTheTurnPersonal(t *testing.T) {
	g := newRig(t)
	me := g.user()

	vaultOnly := g.runner(me, me)
	vaultOnly.Run(context.Background(), "recent", map[string]any{})
	vaultOnly.Run(context.Background(), "open_screen", map[string]any{"screen": "home"})
	require.False(t, vaultOnly.turn.PersonalRan())

	for _, tool := range []string{"health_query", "calendar_query", "reminders_list", "app_reminders_list", "app_reminder_create"} {
		r := g.runner(me, me)
		r.Run(context.Background(), tool, map[string]any{})
		require.True(t, r.turn.PersonalRan(), tool)
	}
}
