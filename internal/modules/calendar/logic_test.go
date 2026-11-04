package calendar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/modules/calendar/db"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// CalendarWebReturnTests.

var allowedOrigins = []string{"https://app.luminavault.fyi", "http://localhost:5173"}

func TestValidatedReturnAcceptsAPageOnAnAllowedOrigin(t *testing.T) {
	url := "https://app.luminavault.fyi/settings/integrations?tab=calendar"
	got, ok := validatedReturn(url, allowedOrigins)
	require.True(t, ok)
	require.Equal(t, url, got)
	_, ok = validatedReturn("http://localhost:5173/settings", allowedOrigins)
	require.True(t, ok)
	// Origins compare case-insensitively and ignore a trailing slash in config.
	_, ok = validatedReturn("https://APP.luminavault.fyi/x", []string{"https://app.luminavault.fyi/"})
	require.True(t, ok)
}

func TestValidatedReturnRefusesAnythingOffTheList(t *testing.T) {
	for _, bad := range []string{
		"https://evil.example/settings",
		"https://app.luminavault.fyi.evil.example/",
		"https://user:pw@app.luminavault.fyi/",
		"https://app.luminavault.fyi/#frag",
		"https://app.luminavault.fyi/#",
		"http://app.luminavault.fyi/", // scheme is part of the origin
		"https://app.luminavault.fyi:8443/",
		"javascript:alert(1)",
		"/settings",
		"luminavault://oauth/google-calendar",
		"https://app.luminavault.fyi/" + strings.Repeat("x", 2048),
	} {
		_, ok := validatedReturn(bad, allowedOrigins)
		require.False(t, ok, bad)
	}
	// An empty allow-list (dev) accepts no web return at all.
	_, ok := validatedReturn("https://app.luminavault.fyi/", nil)
	require.False(t, ok)
}

func TestRedirectAppendsStatusAndReason(t *testing.T) {
	require.Equal(t, "https://app.luminavault.fyi/settings?status=ok", redirectTo("https://app.luminavault.fyi/settings", "ok", nil))
	require.Equal(t, "https://app.luminavault.fyi/s?tab=cal&status=error&reason=access_denied", redirectTo("https://app.luminavault.fyi/s?tab=cal", "error", ptr("access_denied")))
	require.Equal(t, "luminavault://oauth/google-calendar?status=ok", redirectTo(appCallbackBase, "ok", nil))
	// urlQueryAllowed: a space is escaped, `&` and `=` are not.
	require.Equal(t, "x?status=error&reason=a%20b&c=d", redirectTo("x", "error", ptr("a b&c=d")))
}

// MeetingBriefWordingTests. Push title and body are the scheduler's and stay in Swift.

func TestQueryTextIsTitleNamesWithoutTheUserThenNotes(t *testing.T) {
	ana := CalendarAttendeeDTO{Name: ptr("Ana Silva"), Email: ptr("ana@example.com")}
	rui := CalendarAttendeeDTO{Name: ptr("Rui"), Email: ptr("rui@example.com")}
	me := CalendarAttendeeDTO{Name: ptr("Fernando"), Email: ptr("Me@Example.com")}
	got := queryText(briefEvent{title: "  Q4   plan ", notes: ptr("Bring the\nhiring numbers"), attendees: []CalendarAttendeeDTO{ana, me, rui, ana}}, ptr("me@example.com"))
	require.Equal(t, "Q4 plan\nAna Silva, Rui\nBring the hiring numbers", got)
	require.Empty(t, queryText(briefEvent{}, nil))
}

func TestLongNotesAreCut(t *testing.T) {
	got := queryText(briefEvent{title: "Q4 plan", notes: ptr(strings.Repeat("dial in ", 500))}, nil)
	require.LessOrEqual(t, len(got), len("Q4 plan\n")+maxNotesInQuery)
}

func TestNamesCountNotesByWholeWordIgnoringCaseAndAccents(t *testing.T) {
	attendees := []CalendarAttendeeDTO{
		{Name: ptr("Rui"), Email: ptr("rui@example.com")},
		{Name: ptr("Ana Silva"), Email: ptr("ana@example.com")},
		{Name: ptr("Fernando"), Email: ptr("Me@Example.com")},
	}
	got := mentionedNames(attendees, ptr("me@example.com"), []string{"ana wants hiring in Q4", "Banana bread for Rui", "Ána said yes", "Fernando owes Ana the deck"})
	require.Equal(t, []mention{{"Ana", 3}, {"Rui", 1}}, got)
	require.False(t, containsWord("ana", "banana"))
	require.True(t, containsWord("ana", "(ana)"))
	require.True(t, containsWord("ana", "banana ana"), "a later whole word still counts")
	require.False(t, containsWord("", "ana"))
}

func TestWhyLines(t *testing.T) {
	require.Equal(t, "You mentioned Ana in 2 notes", whyLine(3, []mention{{"Ana", 2}}))
	require.Equal(t, "You mentioned Ana in 1 note", whyLine(1, []mention{{"Ana", 1}}))
	require.Equal(t, "3 notes match this meeting", whyLine(3, nil))
	require.Equal(t, "1 note matches this meeting", whyLine(1, nil))
	require.Equal(t, "Nothing in your notes matches this meeting yet", whyLine(0, nil))
}

func TestFloorKeepsTheNearestFiveWithinTheDistanceOnePerMemory(t *testing.T) {
	ids := make([]uuid.UUID, 8)
	for i := range ids {
		ids[i] = uuid.New()
	}
	hits := []searchHit{{ids[0], 0.39}, {ids[1], 0.41}, {ids[2], 0.1}, {ids[2], 0.05}}
	for _, id := range ids[3:] {
		hits = append(hits, searchHit{id, 0.2})
	}
	kept := floorHits(hits, DefaultBriefMaxDistance)
	require.Len(t, kept, 5)
	require.Equal(t, briefMatch{ids[2], 1 - float64(float32(0.05))}, kept[0])
	require.Less(t, float32(DefaultBriefMaxDistance), float32(0.45), "tighter than MemoryController.defaultRelatedMaxDistance")
	for _, m := range kept {
		require.NotEqual(t, ids[1], m.id)
	}
}

func TestFloorBreaksTiesByUppercaseIDString(t *testing.T) {
	a, b := uuid.MustParse("ca1d3000-0000-4000-8000-00000000000a"), uuid.MustParse("ca1d3000-0000-4000-8000-00000000000b")
	kept := floorHits([]searchHit{{b, 0}, {a, 0}}, 0.4)
	require.Equal(t, []uuid.UUID{a, b}, []uuid.UUID{kept[0].id, kept[1].id})
}

func TestSyncAttendeesAreCleanedIntoTheStoredShape(t *testing.T) {
	require.Nil(t, attendeesJSON(nil))
	got := attendeesJSON(&[]syncAttendee{
		{Name: ptr("  Ana "), Email: ptr(" ")},
		{},
		{Email: ptr("rui@example.com")},
	})
	require.NotNil(t, got)
	parsed, err := attendeesFromJSON(*got)
	require.NoError(t, err)
	require.Equal(t, []CalendarAttendeeDTO{{Name: ptr("Ana")}, {Email: ptr("rui@example.com")}}, parsed)
	require.JSONEq(t, `{"list":[{"displayName":"Ana"},{"email":"rui@example.com"}]}`, *got)

	many := make([]syncAttendee, 150)
	for i := range many {
		many[i] = syncAttendee{Name: ptr("P")}
	}
	parsed, err = attendeesFromJSON(*attendeesJSON(&many))
	require.NoError(t, err)
	require.Len(t, parsed, maxAttendees)

	empty := []syncAttendee{}
	require.JSONEq(t, `{"list":[]}`, *attendeesJSON(&empty), "an empty array clears, not omits")
}

func TestStatusTrimIsNarrowerThanWhitespacesAndNewlines(t *testing.T) {
	require.Equal(t, "cancelled", trimWhitespaces(" \tcancelled "))
	require.Equal(t, "cancelled\n", trimWhitespaces("cancelled\n"))
}

func TestBalancedCapsModelAnswersAndBackfills(t *testing.T) {
	row := func(id int, kind string, d float64) db.SearchDocumentsRow {
		return db.SearchDocumentsRow{ID: uuid.NewMD5(uuid.Nil, []byte{byte(id)}), OriginKind: kind, Distance: d}
	}
	// Answers are nearest, but the quota keeps four of them.
	var rows []db.SearchDocumentsRow
	for i := range 6 {
		rows = append(rows, row(i, []string{"chat", "query"}[i%2], float64(i)/100))
	}
	for i := range 7 {
		rows = append(rows, row(10+i, "legacy", 0.1+float64(i)/100))
	}
	got := balanced(rows, 10)
	require.Len(t, got, 10)
	require.Equal(t, rows[3].ID, got[3].id)
	require.Equal(t, rows[6].ID, got[4].id, "the fifth nearest is a fact, not a fifth answer")

	// Few facts: unused slots are backfilled from answers.
	got = balanced(rows[:6], 10)
	require.Len(t, got, 6)
}

func TestMergePrefersTheChunkHitEvenWhenFurther(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	got := mergeHits([]searchHit{{a, 1.0}}, []searchHit{{a, 0}, {b, 0.1}}, 10)
	require.Equal(t, []searchHit{{a, 1.0}, {b, 0.1}}, got)
	require.Empty(t, floorHits(got[:1], DefaultBriefMaxDistance))
	require.Len(t, mergeHits([]searchHit{{a, 0}}, []searchHit{{b, 0}}, 1), 1)
}

// GmailClientTests, scope bookkeeping. The rest of that suite tests the inbox
// reader, which no route of this module uses.

func TestScopeChecksReadTheSpaceSeparatedGrant(t *testing.T) {
	both := "openid https://www.googleapis.com/auth/calendar.events https://www.googleapis.com/auth/gmail.readonly email"
	require.True(t, grants(&both, scopeGmailReadonly))
	require.True(t, grants(&both, scopeCalendarEvents))
	require.False(t, grants(ptr(scopeCalendarConnect), scopeGmailReadonly))
	require.False(t, grants(nil, scopeGmailReadonly))
	// A prefix is not a grant.
	require.False(t, grants(ptr("https://www.googleapis.com/auth/gmail.readonlyX"), scopeGmailReadonly))
	require.True(t, grants(ptr("https://mail.google.com/"), scopeGmailReadonly))
	require.True(t, grants(ptr("https://www.googleapis.com/auth/gmail.modify"), scopeGmailReadonly))
	require.True(t, grants(ptr("https://www.googleapis.com/auth/calendar"), scopeCalendarEvents))

	calendarOnly := withoutGmail(both)
	require.False(t, grants(&calendarOnly, scopeGmailReadonly))
	require.True(t, grants(&calendarOnly, scopeCalendarEvents))
	gmailOnly := withoutCalendar(both)
	require.True(t, grants(&gmailOnly, scopeGmailReadonly))
	require.False(t, grants(&gmailOnly, scopeCalendarEvents))
	require.Equal(t, "a b", removeScope("x", "a  x b"), "split collapses runs of spaces")
}

func TestTokenResponseWithoutScopeKeepsTheOldGrantAndAddsTheNewOne(t *testing.T) {
	merged := effectiveScope(nil, ptr(scopeCalendarConnect), scopeGmailConnect)
	require.True(t, grants(&merged, scopeCalendarEvents))
	require.True(t, grants(&merged, scopeGmailReadonly))
	require.Equal(t, 1, strings.Count(merged, "openid"))
	require.Equal(t, "from-google", effectiveScope(ptr("from-google"), ptr("old"), "new"))
	require.Equal(t, "old new", effectiveScope(ptr(""), ptr("old"), "new"))
	require.Equal(t, "new", effectiveScope(nil, nil, "new"))
}

func TestEmailFromIDToken(t *testing.T) {
	seg := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"me@gmail.test","sub":"1"}`))
	require.Equal(t, "me@gmail.test", *emailFromIDToken("h." + seg + ".s"))
	require.Nil(t, emailFromIDToken("nodots"))
	require.Nil(t, emailFromIDToken("h.!!!.s"))
	require.Nil(t, emailFromIDToken("h."+base64.RawURLEncoding.EncodeToString([]byte(`{"email":1}`))+".s"))
}

func TestAuthorizeURLEncoding(t *testing.T) {
	o := oauthClient{clientID: "id.apps", redirectURI: "https://api.test/v1/calendar/oauth/callback"}
	require.Equal(t, authorizeEndpoint+"?client_id=id.apps&redirect_uri=https://api.test/v1/calendar/oauth/callback&response_type=code"+
		"&scope=https://www.googleapis.com/auth/calendar.events%20openid%20email&access_type=offline&prompt=consent&include_granted_scopes=true&state=A.B",
		o.authorizeURL("A.B", scopeCalendarConnect))
}

func TestParseEvent(t *testing.T) {
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := calendarClient{now: func() time.Time { return fixed }}
	parse := func(raw string) (remoteEvent, bool) {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &m))
		return g.parseEvent(m)
	}
	_, ok := parse(`{"summary":"no id"}`)
	require.False(t, ok)

	// A tombstone may omit everything but its id.
	ev, ok := parse(`{"id":"x","status":"cancelled"}`)
	require.True(t, ok)
	require.True(t, ev.cancelled)
	require.Equal(t, "(no title)", ev.title)
	require.Equal(t, int64(0), ev.startsAt.Unix())
	require.Equal(t, ev.startsAt, ev.endsAt)
	require.Equal(t, fixed, ev.updatedAt, "no updated stamp means now")

	ev, ok = parse(`{"id":"x","summary":"All day","start":{"date":"2026-03-01"},"end":{"date":"2026-03-02"},"updated":"2026-02-01T10:00:00.123Z",
		"organizer":{"email":"o@x.test"},"attendees":[{"email":"a@x.test","displayName":"A","responseStatus":"accepted"}],"recurrence":["RRULE:FREQ=DAILY"],"htmlLink":"h","etag":"e"}`)
	require.True(t, ok)
	require.True(t, ev.allDay)
	require.Equal(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), ev.startsAt)
	require.Equal(t, "o@x.test", *ev.organizer)
	require.Equal(t, "A", *ev.attendees[0].DisplayName)
	require.Equal(t, []string{"RRULE:FREQ=DAILY"}, ev.recurrence)
	require.Equal(t, int64(1769940000), ev.updatedAt.Unix())

	// dateTime wins over date; casts fail as a whole.
	ev, ok = parse(`{"id":"x","start":{"dateTime":"2026-03-01T10:00:00+02:00","date":"2026-03-01"},"attendees":[{"email":"a"},1],"recurrence":["a",1]}`)
	require.True(t, ok)
	require.False(t, ev.allDay)
	require.Equal(t, int64(1772352000), ev.startsAt.Unix())
	require.Empty(t, ev.attendees)
	require.Empty(t, ev.recurrence)
}

// The Swift loop's `continue` on 410 lands on the repeat-while condition, so a
// stale sync_token ends the call with nothing fetched and stays stored. Swift's
// CalendarSyncWorker takes this path. Only the callback reaches it in Go, and
// that clears the token first.
func TestSyncKeepsAnExpiredTokenAsSwiftDoes(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "stale", r.URL.Query().Get("syncToken"))
		w.WriteHeader(http.StatusGone)
	}))
	t.Cleanup(srv.Close)

	tenant := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at) VALUES ($1, $2, 'x', true, $3, now(), now())`,
		tenant, tenant.String()+"@x.test", "u"+tenant.String()[:8])
	require.NoError(t, err)
	g := NewGoogle(pool, GoogleOptions{Box: testBox(t), Endpoints: Endpoints{CalendarBase: srv.URL, Token: srv.URL, Revoke: srv.URL}})
	sealed, err := testBox(t).Seal("access", tenant)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO calendar_accounts (tenant_id, provider, scope, access_ciphertext, access_nonce, access_expires_at, sync_token)
		VALUES ($1, 'google', $2, $3, $4, now() + interval '1 hour', 'stale')`, tenant, scopeCalendarConnect, sealed.Ciphertext, sealed.Nonce)
	require.NoError(t, err)

	n, err := g.sync.sync(ctx, tenant)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, int32(1), calls.Load(), "no full window re-sync in the same call")
	var token string
	var synced *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT sync_token, last_synced_at FROM calendar_accounts WHERE tenant_id = $1`, tenant).Scan(&token, &synced))
	require.Equal(t, "stale", token)
	require.NotNil(t, synced, "and it still counts as synced")
}
