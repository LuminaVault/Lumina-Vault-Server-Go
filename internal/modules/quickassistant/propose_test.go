package quickassistant

import (
	"context"
	"encoding/json"
	"lumina/internal/devicetools"
	"lumina/internal/ids"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Ports the proposal half of QuickAssistantConfirmTests: the assistant
// proposes, a row and a card appear, and nothing runs until the user confirms.

const calendarStart = "2030-01-01T00:00:00Z"

func calendarArgs(title string) map[string]any {
	return map[string]any{"title": title, "start": calendarStart}
}

func calendarCall(title string) string {
	raw, _ := json.Marshal(calendarArgs(title))
	return string(raw)
}

// conversation makes a conversation owned by user.
func (g rig) conversation(user uuid.UUID) uuid.UUID {
	g.t.Helper()
	id := uuid.New()
	g.exec(`INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 't', now(), now())`, id, user)
	return id
}

// proposer is a turn's runner with a phone and somewhere to show a card.
func (g rig) proposer(actor, vault uuid.UUID, phone DeviceRequester, loc *time.Location, conv uuid.UUID) *Runner {
	d := g.deps
	d.Devices = phone
	return NewRunner(d, actor, vault, "qa-user", loc, &conv, nil)
}

type pendingRow struct {
	ID             uuid.UUID
	VaultID        uuid.UUID
	Tool           string
	Status         string
	Summary        string
	Args           string
	Expires        time.Time
	ConversationID uuid.UUID
}

func (g rig) pending(tenant uuid.UUID) []pendingRow {
	g.t.Helper()
	rows, err := g.pool.Query(context.Background(), `SELECT id, vault_id, tool, status, summary, args::text, expires_at, conversation_id
		FROM assistant_pending_actions WHERE tenant_id = $1 ORDER BY created_at`, tenant)
	require.NoError(g.t, err)
	defer rows.Close()
	var out []pendingRow
	for rows.Next() {
		var r pendingRow
		require.NoError(g.t, rows.Scan(&r.ID, &r.VaultID, &r.Tool, &r.Status, &r.Summary, &r.Args, &r.Expires, &r.ConversationID))
		out = append(out, r)
	}
	require.NoError(g.t, rows.Err())
	return out
}

func (g rig) storedArg(id uuid.UUID, key string) string {
	g.t.Helper()
	var v *string
	require.NoError(g.t, g.pool.QueryRow(context.Background(),
		`SELECT args->>$2 FROM assistant_pending_actions WHERE id = $1`, id, key).Scan(&v))
	if v == nil {
		return ""
	}
	return *v
}

// loopTurn runs one assistant turn through the real loop and runner, with
// calendar_create offered.
func loopTurn(t *testing.T, runner *Runner, steps ...step) (*scriptedTransport, *eventLog, Reply) {
	t.Helper()
	tr := newTransport(paidDecision(1000), steps...)
	h := newHarness(tr)
	h.service.Tools = Definitions(runner.definitionOptions())
	log := &eventLog{}
	reply := mustReply(t, h.answer(context.Background(), runner, log))
	return tr, log, reply
}

func cards(log *eventLog, kind ActionKind) []Action {
	var out []Action
	for _, a := range log.actions() {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func lastToolContent(t *testing.T, payload []byte) string {
	t.Helper()
	tools := toolMessages(t, payload)
	require.NotEmpty(t, tools)
	content, _ := tools[len(tools)-1]["content"].(string)
	return content
}

func hasControlOrFormat(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) }) >= 0
}

// MARK: - Proposal

func TestCalendarCreateDoesNotRunBeforeConfirm(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	phone := &fakePhone{}
	r := g.proposer(me, me, phone, nil, g.conversation(me))

	_, log, _ := loopTurn(t, r,
		toolsStep(call("c1", "calendar_create", calendarCall("Dentist"))),
		textStep("Waiting on you."))

	shown := cards(log, KindNeedsConfirm)
	require.Len(t, shown, 1)
	rows := g.pending(me)
	require.Len(t, rows, 1)
	require.Equal(t, "pending", rows[0].Status)
	require.Equal(t, "calendar_create", rows[0].Tool)
	require.Equal(t, rows[0].ID, shown[0].ID.UUID)
	require.Equal(t, rows[0].Summary, shown[0].Summary)
	require.Contains(t, shown[0].Summary, "Dentist")
	require.NotNil(t, shown[0].ExpiresAt)
	require.Empty(t, phone.sent())
}

func TestTheModelIsToldACardIsShowingAndNothingIsDone(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	tr, _, _ := loopTurn(t, r,
		toolsStep(call("c1", "calendar_create", calendarCall("Dentist"))),
		textStep("Waiting on you."))

	require.Equal(t, "A confirmation card is showing for the user. Nothing has been done yet: it only happens if the user confirms. Do not say it is done; tell the user it is waiting for their confirmation.",
		lastToolContent(t, tr.sent()[1]))
}

func TestSummaryComesFromArgsNotModelText(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	_, log, _ := loopTurn(t, r,
		toolsStep(call("c1", "calendar_create", calendarCall("Dentist"))),
		textStep("This is harmless, it only reads your calendar."))

	shown := cards(log, KindNeedsConfirm)
	require.Len(t, shown, 1)
	require.NotContains(t, shown[0].Summary, "harmless")
	require.Equal(t, "Add to Calendar: Dentist, Tue 1 Jan 2030, 00:00 UTC", shown[0].Summary)
}

func TestForgetSummaryShowsTheStoredMemoryNotModelText(t *testing.T) {
	g := newRig(t)
	me := g.user()
	memoryID := g.memory(me, "Allergic to penicillin", nil, time.Now())
	r := g.proposer(me, me, nil, nil, g.conversation(me))

	result := r.Run(context.Background(), "memory_forget", map[string]any{"memoryID": memoryID.String()})

	require.NotNil(t, result.Action)
	require.Equal(t, KindNeedsConfirm, result.Action.Kind)
	require.Equal(t, "Forget this memory: “Allergic to penicillin”", result.Action.Summary)
	rows := g.pending(me)
	require.Len(t, rows, 1)
	require.Equal(t, me, rows[0].VaultID)
	require.Equal(t, "memory_forget", rows[0].Tool)
	require.Equal(t, ids.Upper(memoryID), g.storedArg(rows[0].ID, "memoryID"))
	// Nothing is forgotten until confirm.
	require.Equal(t, 1, g.memories(me))
}

func TestForgetWithForeignMemoryMakesNoCard(t *testing.T) {
	g := newRig(t)
	me, other := g.user(), g.user()
	foreign := g.memory(other, "theirs", nil, time.Now())
	r := g.proposer(me, me, nil, nil, g.conversation(me))

	result := r.Run(context.Background(), "memory_forget", map[string]any{"memoryID": foreign.String()})

	require.Nil(t, result.Action)
	require.Equal(t, "The tool did not run: there is no memory with that ID in this vault.", result.ModelContent)
	require.Empty(t, g.pending(me))
}

func TestForgetWithMalformedIDMakesNoCard(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.proposer(me, me, nil, nil, g.conversation(me))

	for _, value := range []any{"not-a-uuid", nil, float64(7)} {
		result := r.Run(context.Background(), "memory_forget", map[string]any{"memoryID": value})
		require.Nil(t, result.Action)
		require.Equal(t, "The tool did not run: memoryID must be a memory UUID from search.", result.ModelContent)
	}
	missing := r.Run(context.Background(), "memory_forget", map[string]any{})
	require.Nil(t, missing.Action)
	require.True(t, strings.HasPrefix(missing.ModelContent, "The tool did not run"))
	require.Empty(t, g.pending(me))
}

func TestUnparseableDateMakesNoCard(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	result := r.Run(context.Background(), "calendar_create", map[string]any{"title": "Dentist", "start": "friday-ish"})
	require.Nil(t, result.Action)
	require.Equal(t, "The tool did not run: start must be an ISO 8601 date and time with an offset, like 2030-01-01T09:00:00+01:00.", result.ModelContent)

	badEnd := r.Run(context.Background(), "calendar_create", map[string]any{"title": "x", "start": calendarStart, "end": "later"})
	require.Nil(t, badEnd.Action)
	require.True(t, strings.HasPrefix(badEnd.ModelContent, "The tool did not run: end must be "))

	backwards := r.Run(context.Background(), "calendar_create", map[string]any{"title": "x", "start": calendarStart, "end": calendarStart})
	require.Nil(t, backwards.Action)
	require.Equal(t, "The tool did not run: end must be after start.", backwards.ModelContent)
	require.Empty(t, g.pending(me))
}

func TestCalendarWithoutConsentGivesPermissionFrame(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	// No consent row at all, then consent that does not allow changes.
	none := r.Run(context.Background(), "calendar_create", calendarArgs("Dentist"))
	require.NotNil(t, none.Action)
	require.Equal(t, KindNeedsPermission, none.Action.Kind)
	require.Equal(t, "Allow Calendar changes in Data Access to add events.", none.Action.Summary)
	require.NotNil(t, none.Action.Screen)
	require.Equal(t, ScreenDataAccess, *none.Action.Screen)
	require.Nil(t, none.Action.ExpiresAt)
	require.Equal(t, "The user has not allowed Calendar changes, so a card is showing that points them to Data Access. Nothing has been done. Tell the user.", none.ModelContent)

	g.allow(me, devicetools.DomainCalendar, false)
	readOnly := r.Run(context.Background(), "calendar_create", calendarArgs("Other"))
	require.NotNil(t, readOnly.Action)
	require.Equal(t, KindNeedsPermission, readOnly.Action.Kind)
	require.Empty(t, g.pending(me))
	// A permission card is not a confirm card.
	require.Zero(t, r.state().Proposals())
}

func TestEventTitleIsCappedAt200(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	ok := r.Run(context.Background(), "calendar_create", calendarArgs(strings.Repeat("a", 200)))
	require.NotNil(t, ok.Action)
	require.Equal(t, KindNeedsConfirm, ok.Action.Kind)
	tooLong := r.Run(context.Background(), "calendar_create", calendarArgs(strings.Repeat("a", 201)))
	require.Nil(t, tooLong.Action)
	require.Equal(t, "The tool did not run: title must be at most 200 characters.", tooLong.ModelContent)
	empty := r.Run(context.Background(), "calendar_create", calendarArgs("   "))
	require.Equal(t, "The tool did not run: title is required and must be a non-empty string.", empty.ModelContent)
	place := r.Run(context.Background(), "calendar_create", map[string]any{"title": "x", "start": calendarStart, "location": strings.Repeat("b", 201)})
	require.Equal(t, "The tool did not run: location must be a string of at most 200 characters.", place.ModelContent)
	require.Len(t, g.pending(me), 1)
}

func TestAtMostThreeCardsPerTurnHeldAcrossCalls(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	for i := 1; i <= 3; i++ {
		result := r.Run(context.Background(), "calendar_create", calendarArgs("Event "+string(rune('0'+i))))
		require.NotNil(t, result.Action, "card %d", i)
		require.Equal(t, KindNeedsConfirm, result.Action.Kind)
	}
	fourth := r.Run(context.Background(), "calendar_create", calendarArgs("Event 4"))
	require.Nil(t, fourth.Action)
	require.Equal(t, tooManyCards, fourth.ModelContent)
	// A forget proposal draws on the same allowance.
	memoryID := g.memory(me, "I like tea", nil, time.Now())
	forget := r.Run(context.Background(), "memory_forget", map[string]any{"memoryID": memoryID.String()})
	require.Nil(t, forget.Action)
	require.Len(t, g.pending(me), 3)
}

func TestAnIdenticalProposalReplaysWithNoSecondCard(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))
	args := calendarCall("Dentist")

	_, log, _ := loopTurn(t, r,
		toolsStep(call("c1", "calendar_create", args), call("c2", "calendar_create", args)),
		toolsStep(call("c3", "calendar_create", args), call("c4", "calendar_create", args)),
		textStep("Waiting."))

	require.Len(t, cards(log, KindNeedsConfirm), 1)
	require.Len(t, g.pending(me), 1)
}

func TestAProposedCalendarEventMakesTheTurnPersonal(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	_, log, reply := loopTurn(t, r,
		toolsStep(call("c1", "calendar_create", calendarCall("Dentist"))),
		textStep("Waiting."))

	require.Len(t, cards(log, KindNeedsConfirm), 1)
	require.True(t, reply.RanPersonalTool)
	require.False(t, reply.Indexable())
	require.True(t, r.state().PersonalRan())

	// The control: a vault read leaves the turn indexable.
	control := g.user()
	_, _, plain := loopTurn(t, g.proposer(control, control, &fakePhone{}, nil, g.conversation(control)),
		toolsStep(call("c1", "recent", "{}")),
		textStep("Nothing new."))
	require.False(t, plain.RanPersonalTool)
}

func TestViewerCannotProposeAForget(t *testing.T) {
	g := newRig(t)
	owner, viewer := g.user(), g.user()
	vault := g.sharedVault(owner, viewer, "viewer")
	memoryID := g.memory(vault, "team note", nil, time.Now())
	r := g.proposer(viewer, vault, nil, nil, g.conversation(viewer))

	tr, log, _ := loopTurn(t, r,
		toolsStep(call("c1", "memory_forget", `{"memoryID":"`+memoryID.String()+`"}`)),
		textStep("Sorry."))

	require.Empty(t, cards(log, KindNeedsConfirm))
	require.Empty(t, g.pending(viewer))
	require.Contains(t, lastToolContent(t, tr.sent()[1]), "doesn't have access")
	require.Equal(t, 1, g.memories(vault))
}

func TestACalendarCardReadsAndStoresTheUsersClock(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, mustZone(t, "America/Santiago"), g.conversation(me))

	// No offset: 15:00 in Santiago (UTC-03:00 on 1 March 2030), 18:00 UTC.
	local := r.Run(context.Background(), "calendar_create", map[string]any{
		"title": "Dentist", "start": "2030-03-01T15:00:00", "end": "2030-03-01T16:00",
	})
	require.NotNil(t, local.Action)
	require.Equal(t, "Add to Calendar: Dentist, Fri 1 Mar 2030, 15:00", local.Action.Summary)
	require.Equal(t, "2030-03-01T18:00:00Z", g.storedArg(local.Action.ID.UUID, "start"))
	require.Equal(t, "2030-03-01T19:00:00Z", g.storedArg(local.Action.ID.UUID, "end"))

	// An offset the model chose, stored as the instant it names.
	offset := r.Run(context.Background(), "calendar_create", map[string]any{
		"title": "Call", "start": "2030-03-01T19:00:00.000+01:00",
	})
	require.NotNil(t, offset.Action)
	require.Equal(t, "Add to Calendar: Call, Fri 1 Mar 2030, 15:00", offset.Action.Summary)
	require.Equal(t, "2030-03-01T18:00:00Z", g.storedArg(offset.Action.ID.UUID, "start"))
	require.Empty(t, g.storedArg(offset.Action.ID.UUID, "end"))
}

func TestTheConfirmWindowIsTenMinutes(t *testing.T) {
	require.Equal(t, 10*time.Minute, ConfirmWindow)
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	result := r.Run(context.Background(), "calendar_create", calendarArgs("Dentist"))

	require.NotNil(t, result.Action)
	require.NotNil(t, result.Action.ExpiresAt)
	seconds := time.Until(result.Action.ExpiresAt.Time).Seconds()
	require.Greater(t, seconds, 590.0)
	require.LessOrEqual(t, seconds, 600.0)
	rows := g.pending(me)
	require.Len(t, rows, 1)
	require.WithinDuration(t, result.Action.ExpiresAt.Time, rows[0].Expires, time.Millisecond)
}

func TestAProposalDeletesThisUsersRowsExpiredOverADayAgo(t *testing.T) {
	g := newRig(t)
	me, other := g.user(), g.user()
	myConv, otherConv := g.conversation(me), g.conversation(other)
	insert := func(tenant, conv uuid.UUID, ago time.Duration) uuid.UUID {
		id := uuid.New()
		require.NoError(t, g.deps.Store.Insert(context.Background(), PendingAction{
			ID: id, TenantID: tenant, VaultID: tenant, ConversationID: conv, Tool: "memory_forget",
			Args: map[string]any{"memoryID": ids.Upper(uuid.New())}, Summary: "Forget this memory: “old”",
			ExpiresAt: time.Now().Add(-ago),
		}))
		return id
	}
	old := insert(me, myConv, 48*time.Hour)
	recent := insert(me, myConv, time.Hour)
	othersOld := insert(other, otherConv, 48*time.Hour)
	g.allow(me, devicetools.DomainCalendar, true)

	result := g.proposer(me, me, &fakePhone{}, nil, myConv).Run(context.Background(), "calendar_create", calendarArgs("Dentist"))

	require.NotNil(t, result.Action)
	exists := func(id uuid.UUID) bool {
		return g.count(`SELECT count(*) FROM assistant_pending_actions WHERE id = $1`, id) == 1
	}
	require.False(t, exists(old))
	require.True(t, exists(recent))
	require.True(t, exists(othersOld))
}

func TestCardsStripControlAndFormatCharacters(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, &fakePhone{}, nil, g.conversation(me))

	event := r.Run(context.Background(), "calendar_create", map[string]any{
		"title": "Pay‮evil\nrent", "start": calendarStart, "location": "  Room\x1b[31m 5  ",
	})
	require.NotNil(t, event.Action)
	require.False(t, hasControlOrFormat(event.Action.Summary), event.Action.Summary)
	require.Contains(t, event.Action.Summary, "Payevil rent")
	require.Contains(t, event.Action.Summary, "at Room[31m 5")
	// The stored arguments are what the phone gets: trimmed, not cleaned.
	require.Equal(t, "Room\x1b[31m 5", g.storedArg(event.Action.ID.UUID, "location"))

	memoryID := g.memory(me, "red \x1b[31mtext‮", nil, time.Now())
	forget := r.Run(context.Background(), "memory_forget", map[string]any{"memoryID": memoryID.String()})
	require.NotNil(t, forget.Action)
	require.Contains(t, forget.Action.Summary, "red [31mtext")
	require.False(t, hasControlOrFormat(forget.Action.Summary))
}

// Review Focus 3: with no device requester calendar_create is not offered, and
// a forced call answers at once with no row and no card.
func TestCalendarCreateWithoutDevicesIsNotAvailable(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.allow(me, devicetools.DomainCalendar, true)
	r := g.proposer(me, me, nil, nil, g.conversation(me))

	start := time.Now()
	result := r.Run(context.Background(), "calendar_create", calendarArgs("Dentist"))

	require.Less(t, time.Since(start), time.Second)
	require.Nil(t, result.Action)
	require.Equal(t, notAvailable, result.ModelContent)
	require.Empty(t, g.pending(me))
	for _, tool := range Definitions(r.definitionOptions()) {
		require.NotEqual(t, "calendar_create", tool.Name)
	}
}
