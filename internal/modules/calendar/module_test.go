package calendar_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/calendar"
	"lumina/internal/modules/mail"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

const (
	scopeCalendar = "https://www.googleapis.com/auth/calendar.events openid email"
	scopeGmail    = "https://www.googleapis.com/auth/gmail.readonly openid email"
	scopeBoth     = "openid https://www.googleapis.com/auth/calendar.events https://www.googleapis.com/auth/gmail.readonly email"
)

// stubGoogle answers like Google's token, revoke and Calendar endpoints.
type stubGoogle struct {
	mu       sync.Mutex
	srv      *httptest.Server
	token    func(form url.Values) (int, string)
	list     func(q url.Values) (int, string)
	insert   func(body map[string]any) (int, string)
	revoked  []string
	bearers  []string
	refreshN atomic.Int32
}

func newStubGoogle(t *testing.T) *stubGoogle {
	t.Helper()
	s := &stubGoogle{
		token:  func(url.Values) (int, string) { return http.StatusInternalServerError, "" },
		list:   func(url.Values) (int, string) { return http.StatusOK, `{"items":[],"nextSyncToken":"tok-1"}` },
		insert: func(map[string]any) (int, string) { return http.StatusInternalServerError, "" },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.PostForm.Get("grant_type") == "refresh_token" {
			s.refreshN.Add(1)
		}
		s.mu.Lock()
		f := s.token
		s.mu.Unlock()
		code, body := f(r.PostForm)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		s.mu.Lock()
		s.revoked = append(s.revoked, r.PostForm.Get("token"))
		s.mu.Unlock()
	})
	mux.HandleFunc("GET /cal/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.bearers = append(s.bearers, r.Header.Get("Authorization"))
		f := s.list
		s.mu.Unlock()
		code, body := f(r.URL.Query())
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("POST /cal/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		s.mu.Lock()
		s.bearers = append(s.bearers, r.Header.Get("Authorization"))
		f := s.insert
		s.mu.Unlock()
		code, resp := f(body)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, resp)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubGoogle) set(f func(*stubGoogle)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *stubGoogle) endpoints() calendar.Endpoints {
	return calendar.Endpoints{Token: s.srv.URL + "/token", Revoke: s.srv.URL + "/revoke", CalendarBase: s.srv.URL + "/cal"}
}

type env struct {
	t      *testing.T
	e      *echo.Echo
	pool   *pgxpool.Pool
	box    *secretbox.Box
	google *stubGoogle
}

func startDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "calendar.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	return pool
}

type opts struct {
	unconfigured bool
	noBox        bool
	now          func() time.Time
}

func newEnv(t *testing.T, pool *pgxpool.Pool, o opts) env {
	t.Helper()
	box, err := secretbox.New(calendar.TestMasterKey)
	require.NoError(t, err)
	stub := newStubGoogle(t)
	var g *calendar.Google
	if !o.noBox {
		gopts := calendar.GoogleOptions{
			Box: box, WebReturnOrigins: []string{"https://app.luminavault.fyi"}, Endpoints: stub.endpoints(), Now: o.now,
		}
		if !o.unconfigured {
			gopts.ClientID, gopts.ClientSecret, gopts.RedirectURI = "client-id", "not-a-secret", "https://api.test/v1/calendar/oauth/callback"
		}
		g = calendar.NewGoogle(pool, gopts)
	}
	identify := auth.Identify(keys, auth.PGUsers(pool))
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{
			calendar.New(pool, identify, ratelimit.NewStore(), calendar.Options{
				Google: g, MaxDistance: calendar.DefaultBriefMaxDistance,
				Embeddings: embedding.New(pool, embedding.Config{Provider: "deterministic", MonthlyCap: 2_000_000}, nil),
			}),
			mail.New(g),
		},
	})
	return env{t: t, e: e, pool: pool, box: box, google: stub}
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (r resp) String() string { return string(r.body) }

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func decode[T any](t *testing.T, r resp) T {
	t.Helper()
	require.Equal(t, http.StatusOK, r.code, r.String())
	var out T
	require.NoError(t, json.Unmarshal(r.body, &out), r.String())
	return out
}

func (v env) exec(sql string, args ...any) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(), sql, args...)
	require.NoError(v.t, err)
}

func (v env) user(tier string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at, tier)
		VALUES ($1, $2, 'x', true, $3, now(), now(), $4)`, id, id.String()+"@test.luminavault", "u"+id.String()[:8], tier)
	return id
}

func (v env) consent(u uuid.UUID) {
	v.t.Helper()
	v.exec(`INSERT INTO apple_consent (tenant_id, domain, allowed, allow_writes, updated_at) VALUES ($1, 'calendar', true, false, now())`, u)
}

// account stores a Google grant sealed as Swift's CalendarTokenStore would.
func (v env) account(u uuid.UUID, scope, status, refresh, access string, expires time.Time) {
	v.t.Helper()
	rs, err := v.box.Seal(refresh, u)
	require.NoError(v.t, err)
	as, err := v.box.Seal(access, u)
	require.NoError(v.t, err)
	v.exec(`INSERT INTO calendar_accounts (tenant_id, provider, account_email, scope, refresh_ciphertext, refresh_nonce, access_ciphertext, access_nonce, access_expires_at, status)
		VALUES ($1, 'google', 'me@gmail.test', $2, $3, $4, $5, $6, $7, $8)`, u, scope, rs.Ciphertext, rs.Nonce, as.Ciphertext, as.Nonce, expires, status)
}

type accountRow struct {
	email, scope, status, syncToken string
	failure                         string
	refresh, access                 string
	expires                         *time.Time
	lastSynced                      *time.Time
}

func (v env) readAccount(u uuid.UUID) (accountRow, bool) {
	v.t.Helper()
	rows, err := v.pool.Query(context.Background(), `SELECT COALESCE(account_email,''), scope, status, COALESCE(sync_token,''), COALESCE(last_failure_code,''),
		refresh_ciphertext, refresh_nonce, access_ciphertext, access_nonce, access_expires_at, last_synced_at
		FROM calendar_accounts WHERE tenant_id = $1 AND provider = 'google'`, u)
	require.NoError(v.t, err)
	defer rows.Close()
	if !rows.Next() {
		return accountRow{}, false
	}
	var a accountRow
	var rc, rn, ac, an []byte
	require.NoError(v.t, rows.Scan(&a.email, &a.scope, &a.status, &a.syncToken, &a.failure, &rc, &rn, &ac, &an, &a.expires, &a.lastSynced))
	// Opening with the shared envelope is what proves Swift's worker can read it.
	if rc != nil {
		a.refresh, err = v.box.Open(secretbox.Sealed{Ciphertext: rc, Nonce: rn}, u)
		require.NoError(v.t, err)
	}
	if ac != nil {
		a.access, err = v.box.Open(secretbox.Sealed{Ciphertext: ac, Nonce: an}, u)
		require.NoError(v.t, err)
	}
	return a, true
}

func (v env) count(sql string, args ...any) int {
	v.t.Helper()
	var n int
	require.NoError(v.t, v.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func syncBody(events ...string) string { return `{"events":[` + strings.Join(events, ",") + `]}` }

func ev(id, title string, extra ...string) string {
	s := fmt.Sprintf(`{"externalID":%q,"calendarID":"work","title":%q,"startsAt":"2026-05-01T10:00:00Z","endsAt":"2026-05-01T10:30:00Z","allDay":false`, id, title)
	for _, e := range extra {
		s += "," + e
	}
	return s + "}"
}

func stamp(t time.Time) string {
	return `"remoteUpdatedAt":"` + t.UTC().Format(time.RFC3339) + `"`
}

func TestAgainstPostgres(t *testing.T) {
	pool := startDB(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, env)
	}{
		// AppleCalendarControllerTests
		{"SyncForbiddenWithoutConsent", testSyncForbidden},
		{"SyncHappyPathInsertsTwo", testSyncInserts},
		{"SyncIdempotentReSyncUpdates", testSyncResync},
		{"SyncLastWriterWins", testSyncLastWriter},
		{"SyncCancelledIsTombstoned", testSyncCancelled},
		{"SyncEventKitRowsDoNotCollideWithGoogle", testSyncSources},
		{"SyncTenantIsolation", testSyncIsolation},
		// MeetingBriefHTTPTests
		{"AttendeesRideTheSync", testSyncAttendees},
		{"EventFromBeforeAttendeesHasNone", testSyncNoAttendees},
		{"BriefIsTheCallersOwn", testBriefOwn},
		{"BriefEmptyPastTheFloor", testBriefEmpty},
		// Beyond the Swift tests: seeds the parity scenarios compare.
		{"BriefSeededSearchArms", testBriefArms},
		{"BriefQuota", testBriefQuota},
		{"BriefEmbeddingCap", testBriefCap},
		{"BriefUsageIsCounted", testBriefUsage},
		{"BriefRateLimitScalesWithTier", testBriefRateLimit},
		// The Google paths parity cannot reach, against a stub.
		{"ConnectBuildsTheConsentURL", testConnectURL},
		{"ConnectRejectsAReturnOffTheList", testConnectBadReturn},
		{"CallbackExchangesStoresAndSyncs", testCallbackHappy},
		{"CallbackKeepsAnEmailWhenTheTokenHasNone", testCallbackRelink},
		{"CallbackDeclineGoesBackToTheWebPage", testCallbackDecline},
		{"QueryPlusStaysAPlus", testQueryPlusStaysAPlus},
		{"CallbackMissingCode", testCallbackMissingCode},
		{"CallbackExchangeFailures", testCallbackExchangeFails},
		{"CallbackInitialSyncFailureStillConnects", testCallbackSyncFails},
		{"CallbackIsOneShotAndPerSession", testCallbackOneShot},
		{"CreateEventHappyPath", testCreateHappy},
		{"CreateEventRefreshesAnExpiredToken", testCreateRefresh},
		{"CreateEventRefreshRotation", testCreateRotation},
		{"CreateEventRejectedRefreshFlipsToNeedsReauth", testCreateRejected},
		{"CreateEventGoogleErrorsAre500", testCreateGoogleErrors},
		{"CreateEventRefreshIsSingleFlight", testRefreshSingleFlight},
		{"DisconnectRevokesAndDeletes", testDisconnectRevokes},
		{"DisconnectDeletesEvenWhenGoogleFails", testDisconnectGoogleDown},
		{"GmailDisconnectRevokesAGmailOnlyGrant", testGmailDisconnectRevokes},
		// MuseProactiveIntegrationTests (Gmail half of the grant)
		{"GmailAndCalendarReportTheirOwnHalf", testGmailHalves},
		{"GmailDisconnectKeepsCalendar", testGmailKeepsCalendar},
		{"CalendarDisconnectKeepsGmailThenGmailDeletes", testCalendarKeepsGmail},
		{"GmailConnectRemembersItsPurpose", testGmailPurpose},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newEnv(t, pool, opts{})) })
	}
}

// MARK: sync

func testSyncForbidden(t *testing.T, v env) {
	r := v.do(v.user("trial"), http.MethodPost, "/v1/calendar/sync", syncBody(ev("e-1", "Standup")))
	require.Equal(t, http.StatusForbidden, r.code)
	require.JSONEq(t, `{"error":{"message":"calendar access not allowed by the user"}}`, r.String())
}

type syncResp = calendar.AppleSyncResponse

func (v env) sync(u uuid.UUID, events ...string) syncResp {
	v.t.Helper()
	return decode[syncResp](v.t, v.do(u, http.MethodPost, "/v1/calendar/sync", syncBody(events...)))
}

func testSyncInserts(t *testing.T, v env) {
	u := v.user("trial")
	v.consent(u)
	now := time.Now()
	require.Equal(t, syncResp{Inserted: 2}, v.sync(u, ev("e-1", "Standup", stamp(now)), ev("e-2", "Standup", stamp(now))))
	require.Equal(t, 2, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1 AND source = 'apple_eventkit' AND calendar_id = 'work'`, u))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM apple_consent WHERE tenant_id = $1 AND last_sync_at IS NOT NULL`, u), "the consent row is stamped")
}

func testSyncResync(t *testing.T, v env) {
	u := v.user("trial")
	v.consent(u)
	t0 := time.Now()
	require.Equal(t, 2, v.sync(u, ev("e-1", "Standup", stamp(t0)), ev("e-2", "Standup", stamp(t0))).Inserted)
	t1 := t0.Add(time.Minute)
	require.Equal(t, syncResp{Updated: 2}, v.sync(u, ev("e-1", "Moved", stamp(t1)), ev("e-2", "Moved", stamp(t1))))
}

func testSyncLastWriter(t *testing.T, v env) {
	u := v.user("trial")
	v.consent(u)
	base := time.Now()
	require.Equal(t, 1, v.sync(u, ev("e-lww", "Original", stamp(base))).Inserted)
	require.Equal(t, syncResp{Skipped: 1}, v.sync(u, ev("e-lww", "Stale", stamp(base.Add(-time.Hour)))))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1 AND title = 'Original'`, u))
	require.Equal(t, syncResp{Updated: 1}, v.sync(u, ev("e-lww", "Fresh", stamp(base.Add(time.Hour)))))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1 AND title = 'Fresh'`, u))
}

func testSyncCancelled(t *testing.T, v env) {
	u := v.user("trial")
	v.consent(u)
	now := time.Now()
	require.Equal(t, syncResp{Inserted: 1}, v.sync(u, ev("e-c", "Gone", `"status":"cancelled"`, stamp(now))))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1 AND status = 'cancelled'`, u))
	require.Equal(t, syncResp{Updated: 1}, v.sync(u, ev("e-c", "Gone", `"status":"cancelled"`, stamp(now.Add(time.Minute)))))
}

func testSyncSources(t *testing.T, v env) {
	u := v.user("trial")
	v.consent(u)
	v.exec(`INSERT INTO calendar_events (tenant_id, source, external_id, title, starts_at, ends_at) VALUES ($1, 'google', 'shared', 'G', now(), now())`, u)
	now := time.Now()
	require.Equal(t, syncResp{Inserted: 1}, v.sync(u, ev("shared", "EK", stamp(now))))
	require.Equal(t, syncResp{Updated: 1}, v.sync(u, ev("shared", "EK2", stamp(now.Add(30*time.Second)))))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1 AND source = 'google' AND title = 'G'`, u))
}

func testSyncIsolation(t *testing.T, v env) {
	a, b := v.user("trial"), v.user("trial")
	v.consent(a)
	v.consent(b)
	now := time.Now()
	require.Equal(t, 1, v.sync(a, ev("shared", "A's", stamp(now))).Inserted)
	require.Equal(t, syncResp{Inserted: 1}, v.sync(b, ev("shared", "B's", stamp(now))))
}

func (v env) storedAttendees(u uuid.UUID, id string) *string {
	v.t.Helper()
	var s *string
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT attendees::text FROM calendar_events WHERE tenant_id = $1 AND external_id = $2`, u, id).Scan(&s))
	return s
}

func testSyncAttendees(t *testing.T, v env) {
	u := v.user("trial")
	v.consent(u)
	v.sync(u, ev("EK-q4", "Q4 plan", `"attendees":[{"name":"Ana Silva","email":"ana@example.com"},{"name":" "}]`))
	require.JSONEq(t, `{"list":[{"displayName":"Ana Silva","email":"ana@example.com"}]}`, *v.storedAttendees(u, "EK-q4"))
	// An older client: no attendees key at all.
	v.sync(u, ev("EK-q4", "Q4 plan (moved)"))
	require.JSONEq(t, `{"list":[{"displayName":"Ana Silva","email":"ana@example.com"}]}`, *v.storedAttendees(u, "EK-q4"))
	v.sync(u, ev("EK-q4", "Q4 plan", `"attendees":[]`))
	require.JSONEq(t, `{"list":[]}`, *v.storedAttendees(u, "EK-q4"))
}

func testSyncNoAttendees(t *testing.T, v env) {
	u := v.user("trial")
	v.consent(u)
	v.sync(u, ev("EK-old", "Standup"))
	require.Nil(t, v.storedAttendees(u, "EK-old"))
}

// MARK: brief

var (
	seedBri = uuid.MustParse("CA1E0000-0000-4000-8000-0000000000DA")
	seedBal = uuid.MustParse("CA1E0000-0000-4000-8000-0000000000EB")
	seedCap = uuid.MustParse("CA1E0000-0000-4000-8000-0000000000FC")
	seedQuo = uuid.MustParse("CA1E0000-0000-4000-8000-0000000000AD")
	seedOff = uuid.MustParse("CA1E0000-0000-4000-8000-0000000000C9")
	seedAnn = uuid.MustParse("CA1E0000-0000-4000-8000-0000000000A1")
)

func briefPath(event string) string { return "/v1/calendar/events/" + event + "/brief" }

func contents(b calendar.MeetingBriefResponse) []string {
	out := make([]string, len(b.Memories))
	for i, m := range b.Memories {
		out[i] = m.Snippet
	}
	return out
}

func testBriefOwn(t *testing.T, v env) {
	const q4, garden = "CA1C0000-0000-4000-8000-000000000001", "CA1C0000-0000-4000-8000-000000000002"
	r := v.do(seedBri, http.MethodGet, briefPath(q4), "")
	brief := decode[calendar.MeetingBriefResponse](t, r)
	require.Contains(t, r.String(), `"id":"`+q4+`"`, "uppercase on the wire")
	require.Equal(t, "Q4 plan", brief.Event.Title)
	require.Equal(t, "Room 1", *brief.Event.Location)
	require.Len(t, brief.Event.Attendees, 4)
	require.Equal(t, "Ana Silva", *brief.Event.Attendees[0].Name)
	require.Contains(t, contents(brief), "Ana wants hiring in the Q4 plan")
	require.NotContains(t, contents(brief), "Rejected: Ana")
	require.Equal(t, "decision", *brief.Memories[0].Kind)
	require.Greater(t, brief.Memories[0].Similarity, 0.99)
	require.Equal(t, "You mentioned Ana in 1 note", brief.Why)

	stored := decode[calendar.MeetingBriefResponse](t, v.do(seedBri, http.MethodGet, briefPath(garden), ""))
	require.Equal(t, []string{"Garden plans"}, contents(stored), "what the push counted, not a fresh match")
	require.InDelta(t, 0.77, stored.Memories[0].Similarity, 0)
	require.Equal(t, "1 note matches this meeting", stored.Why)

	require.Equal(t, http.StatusNotFound, v.do(seedAnn, http.MethodGet, briefPath(q4), "").code)
	notFound := v.do(seedBri, http.MethodGet, briefPath(uuid.NewString()), "")
	require.Equal(t, http.StatusNotFound, notFound.code)
	require.JSONEq(t, `{"error":{"message":"event not found"}}`, notFound.String())
	for _, bad := range []string{"not-a-uuid", "ca1c0000000040008000000000000001", "urn:uuid:ca1c0000-0000-4000-8000-000000000001"} {
		r := v.do(seedBri, http.MethodGet, briefPath(bad), "")
		require.Equal(t, http.StatusBadRequest, r.code, bad)
		require.JSONEq(t, `{"error":{"message":"invalid event id"}}`, r.String())
	}
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, briefPath(q4), "").code)

	// Stored notes: rejected, foreign and unknown ids go; the epoch stands in for no created_at.
	filtered := decode[calendar.MeetingBriefResponse](t, v.do(seedBri, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000003"), ""))
	require.Len(t, filtered.Memories, 1)
	require.Equal(t, "fact", *filtered.Memories[0].Kind)
	require.Equal(t, int64(0), filtered.Memories[0].CreatedAt.Unix())
	require.Equal(t, 160, len([]rune(filtered.Memories[0].Snippet)))
	require.True(t, strings.HasSuffix(filtered.Memories[0].Snippet, "…"))
	require.True(t, strings.HasPrefix(filtered.Memories[0].Snippet, "A long note about"))

	// A row that does not decode fails the request, as a Fluent model decode does.
	require.Equal(t, http.StatusInternalServerError, v.do(seedBri, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000008"), "").code)
}

func testBriefEmpty(t *testing.T, v env) {
	brief := decode[calendar.MeetingBriefResponse](t, v.do(seedOff, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000041"), ""))
	require.Empty(t, brief.Memories)
	require.NotNil(t, brief.Memories, "[] on the wire")
	require.NotNil(t, brief.Event.Attendees, "[] on the wire")
	require.Equal(t, "Nothing in your notes matches this meeting yet", brief.Why)
}

func testBriefArms(t *testing.T, v env) {
	brief := decode[calendar.MeetingBriefResponse](t, v.do(seedBal, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000011"), ""))
	got := contents(brief)
	require.Len(t, got, 5)
	require.Equal(t, []string{"Twin a", "Twin b"}, got[:2], "equal distance orders by id")
	require.Contains(t, got, "Found by its chunk", "the chunk arm finds a memory with no document vector")
	require.NotContains(t, got, "Hidden by its chunk", "a lexical-only chunk hit wins the merge at distance 1.0")
	require.NotContains(t, got, "Rejected chunk owner")
	require.NotContains(t, got, "Far away")
	require.Equal(t, "5 notes match this meeting", brief.Why)

	retro := decode[calendar.MeetingBriefResponse](t, v.do(seedBal, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000012"), ""))
	require.Equal(t, "You mentioned Ána in 2 notes", retro.Why)
}

func testBriefQuota(t *testing.T, v env) {
	brief := decode[calendar.MeetingBriefResponse](t, v.do(seedQuo, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000031"), ""))
	require.Equal(t, []string{"Answer 1", "Answer 2", "Answer 3", "Answer 4", "Fact 1"}, contents(brief))
}

func testBriefCap(t *testing.T, v env) {
	require.Equal(t, http.StatusInternalServerError, v.do(seedCap, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000021"), "").code)
	stored := decode[calendar.MeetingBriefResponse](t, v.do(seedCap, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000022"), ""))
	require.Equal(t, []string{"Stored note"}, contents(stored))
	blank := decode[calendar.MeetingBriefResponse](t, v.do(seedCap, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000023"), ""))
	require.Empty(t, blank.Memories, "a blank event never reaches the embedder")
}

func testBriefUsage(t *testing.T, v env) {
	u := v.user("trial")
	var id uuid.UUID
	require.NoError(t, v.pool.QueryRow(context.Background(), `INSERT INTO calendar_events (tenant_id, source, external_id, title, starts_at, ends_at)
		VALUES ($1, 'apple_eventkit', 'x', 'Twelve chars', now(), now()) RETURNING id`, u).Scan(&id))
	require.Equal(t, http.StatusOK, v.do(u, http.MethodGet, briefPath(id.String()), "").code)
	require.Equal(t, 3, v.count(`SELECT tokens_used::int FROM embedding_usage WHERE tenant_id = $1`, u), "(12+3)/4")
}

func testBriefRateLimit(t *testing.T, v env) {
	for _, c := range []struct {
		tier string
		max  int
	}{{"trial", 30}, {"free", 10}, {"pro", 60}, {"archived", 3}, {"ultimate", 120}} {
		u := v.user(c.tier)
		path := briefPath(uuid.NewString())
		for i := range c.max {
			require.Equal(t, http.StatusNotFound, v.do(u, http.MethodGet, path, "").code, "%s %d", c.tier, i)
		}
		r := v.do(u, http.MethodGet, path, "")
		require.Equal(t, http.StatusTooManyRequests, r.code, c.tier)
		require.JSONEq(t, `{"error":{"message":"rate limit exceeded"}}`, r.String())
		require.NotEmpty(t, r.header.Get("Retry-After"))
	}
	// Unauthenticated: the bucket counts before the 401.
	for range 30 {
		require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, briefPath("nope"), "").code)
	}
	require.Equal(t, http.StatusTooManyRequests, v.do(uuid.Nil, http.MethodGet, briefPath("nope"), "").code)
}

// MARK: connect and callback

func (v env) connect(u uuid.UUID, path string) *url.URL {
	v.t.Helper()
	r := decode[calendar.CalendarConnectStartResponse](v.t, v.do(u, http.MethodPost, path, ""))
	parsed, err := url.Parse(r.AuthorizeURL)
	require.NoError(v.t, err)
	return parsed
}

func (v env) callback(query url.Values) string {
	v.t.Helper()
	r := v.do(uuid.Nil, http.MethodGet, "/v1/calendar/oauth/callback?"+query.Encode(), "")
	require.Equal(v.t, http.StatusSeeOther, r.code, r.String())
	require.Empty(v.t, r.body)
	return r.header.Get("Location")
}

func testConnectURL(t *testing.T, v env) {
	u := v.connect(v.user("trial"), "/v1/calendar/connect")
	require.Equal(t, "accounts.google.com", u.Host)
	q := u.Query()
	require.Equal(t, "client-id", q.Get("client_id"))
	require.Equal(t, "https://api.test/v1/calendar/oauth/callback", q.Get("redirect_uri"))
	require.Equal(t, "code", q.Get("response_type"))
	require.Equal(t, scopeCalendar, q.Get("scope"))
	require.Equal(t, "offline", q.Get("access_type"))
	require.Equal(t, "consent", q.Get("prompt"))
	require.Equal(t, "true", q.Get("include_granted_scopes"))
	first, second, ok := strings.Cut(q.Get("state"), ".")
	require.True(t, ok)
	require.Equal(t, strings.ToUpper(first), first, "UUID().uuidString")
	require.Len(t, first, 36)
	require.Len(t, second, 36)
}

func testConnectBadReturn(t *testing.T, v env) {
	u := v.user("trial")
	for _, path := range []string{"/v1/calendar/connect", "/v1/mail/gmail/connect"} {
		r := v.do(u, http.MethodPost, path+"?returnTo="+url.QueryEscape("https://evil.example/"), "")
		require.Equal(t, http.StatusBadRequest, r.code)
		require.JSONEq(t, `{"error":{"message":"invalid_return_to"}}`, r.String())
	}
}

func idToken(email string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"`+email+`"}`)) + ".s"
}

func testCallbackHappy(t *testing.T, v env) {
	u := v.user("trial")
	var form url.Values
	v.google.set(func(s *stubGoogle) {
		s.token = func(f url.Values) (int, string) {
			form = f
			return http.StatusOK, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":1800,"scope":"` + scopeCalendar + `","id_token":"` + idToken("me@gmail.test") + `"}`
		}
		s.list = func(q url.Values) (int, string) {
			require.NotEmpty(t, q.Get("timeMin"), "first sync is a window")
			require.Equal(t, "startTime", q.Get("orderBy"))
			if q.Get("pageToken") == "" {
				return http.StatusOK, `{"items":[{"id":"g-1","summary":"First","start":{"dateTime":"2099-01-01T10:00:00Z"},"end":{"dateTime":"2099-01-01T11:00:00Z"},
					"attendees":[{"email":"a@x.test","displayName":"A"}],"recurrence":["RRULE:FREQ=DAILY"],"htmlLink":"https://cal/1","etag":"e1","updated":"2026-01-01T00:00:00Z"}],"nextPageToken":"p2"}`
			}
			return http.StatusOK, `{"items":[{"id":"g-2","status":"cancelled"}],"nextSyncToken":"sync-1"}`
		}
	})
	state := v.connect(u, "/v1/calendar/connect").Query().Get("state")
	loc := v.callback(url.Values{"state": {state}, "code": {"auth/code"}})
	require.Equal(t, "luminavault://oauth/google-calendar?status=ok", loc)
	require.Equal(t, "auth/code", form.Get("code"))
	require.Equal(t, "authorization_code", form.Get("grant_type"))
	require.Equal(t, "client-id", form.Get("client_id"))
	require.Equal(t, "not-a-secret", form.Get("client_secret"))
	require.Equal(t, "https://api.test/v1/calendar/oauth/callback", form.Get("redirect_uri"))

	a, ok := v.readAccount(u)
	require.True(t, ok)
	require.Equal(t, "rt-1", a.refresh)
	require.Equal(t, "at-1", a.access)
	require.Equal(t, "me@gmail.test", a.email)
	require.Equal(t, scopeCalendar, a.scope)
	require.Equal(t, "connected", a.status)
	require.Equal(t, "sync-1", a.syncToken)
	require.NotNil(t, a.lastSynced)
	require.WithinDuration(t, time.Now().Add(1800*time.Second), *a.expires, time.Minute)
	require.Contains(t, v.google.bearers, "Bearer at-1")

	var attendees, recurrence string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT attendees::text, recurrence::text FROM calendar_events WHERE tenant_id = $1 AND external_id = 'g-1' AND source = 'google'`, u).Scan(&attendees, &recurrence))
	require.JSONEq(t, `{"list":[{"email":"a@x.test","displayName":"A"}]}`, attendees)
	require.JSONEq(t, `{"rules":["RRULE:FREQ=DAILY"]}`, recurrence)
	require.Equal(t, 1, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1 AND external_id = 'g-2' AND status = 'cancelled' AND starts_at = to_timestamp(0)`, u))

	events := decode[calendar.CalendarEventsResponse](t, v.do(u, http.MethodGet, "/v1/calendar/events", ""))
	require.Len(t, events.Events, 1)
	require.Equal(t, "https://cal/1", *events.Events[0].HtmlLink)
	status := decode[calendar.CalendarStatusResponse](t, v.do(u, http.MethodGet, "/v1/calendar/status", ""))
	require.True(t, status.Connected)
	require.Equal(t, "me@gmail.test", *status.AccountEmail)
}

func testCallbackRelink(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeCalendar, "needs_reauth", "old-rt", "old-at", time.Now())
	v.exec(`UPDATE calendar_accounts SET sync_token = 'old', last_failure_code = 'refresh_rejected', last_failure_at = now() WHERE tenant_id = $1`, u)
	v.google.set(func(s *stubGoogle) {
		s.token = func(url.Values) (int, string) { return http.StatusOK, `{"access_token":"at-2","refresh_token":"rt-2"}` }
		s.list = func(q url.Values) (int, string) {
			require.Empty(t, q.Get("syncToken"), "a re-link re-syncs the window")
			return http.StatusOK, `{"nextSyncToken":"new"}`
		}
	})
	state := v.connect(u, "/v1/calendar/connect").Query().Get("state")
	require.Equal(t, "luminavault://oauth/google-calendar?status=ok", v.callback(url.Values{"state": {state}, "code": {"c"}}))
	a, _ := v.readAccount(u)
	require.Equal(t, "me@gmail.test", a.email, "kept: no id_token")
	require.Equal(t, "connected", a.status)
	require.Empty(t, a.failure)
	require.Equal(t, "rt-2", a.refresh)
	require.Equal(t, scopeCalendar, a.scope, "no scope in the answer: old plus requested, unique")
	require.Equal(t, "new", a.syncToken)
	require.WithinDuration(t, time.Now().Add(time.Hour), *a.expires, time.Minute, "expires_in defaults to 3600")
}

// Hummingbird keeps a `+` in a query value, so a page's own query survives.
func testQueryPlusStaysAPlus(t *testing.T, v env) {
	u := v.user("trial")
	page := "https://app.luminavault.fyi/settings?tab=a+b"
	state := v.connect(u, "/v1/calendar/connect?returnTo=https://app.luminavault.fyi/settings%3Ftab=a+b").Query().Get("state")
	require.Equal(t, page+"&status=error&reason=access_denied", v.callback(url.Values{"state": {state}, "error": {"access_denied"}}))
}

func testCallbackDecline(t *testing.T, v env) {
	u := v.user("trial")
	page := "https://app.luminavault.fyi/settings?tab=cal"
	state := v.connect(u, "/v1/calendar/connect?returnTo="+url.QueryEscape(page)).Query().Get("state")
	require.Equal(t, page+"&status=error&reason=access_denied", v.callback(url.Values{"state": {state}, "error": {"access_denied"}, "code": {"ignored"}}))
	_, ok := v.readAccount(u)
	require.False(t, ok)
}

func testCallbackMissingCode(t *testing.T, v env) {
	u := v.user("trial")
	state := v.connect(u, "/v1/calendar/connect").Query().Get("state")
	require.Equal(t, "luminavault://oauth/google-calendar?status=error&reason=missing_code", v.callback(url.Values{"state": {state}}))
}

func testCallbackExchangeFails(t *testing.T, v env) {
	for _, c := range []struct {
		name string
		code int
		body string
	}{
		{"google says no", http.StatusBadRequest, `{"error":"invalid_grant"}`},
		{"not json", http.StatusOK, `nope`},
		{"no access token", http.StatusOK, `{"refresh_token":"r"}`},
		{"no refresh token, so no offline access", http.StatusOK, `{"access_token":"a"}`},
	} {
		u := v.user("trial")
		v.google.set(func(s *stubGoogle) { s.token = func(url.Values) (int, string) { return c.code, c.body } })
		state := v.connect(u, "/v1/calendar/connect").Query().Get("state")
		require.Equal(t, "luminavault://oauth/google-calendar?status=error&reason=exchange_failed", v.callback(url.Values{"state": {state}, "code": {"c"}}), c.name)
		_, ok := v.readAccount(u)
		require.False(t, ok, c.name)
	}
}

func testCallbackSyncFails(t *testing.T, v env) {
	u := v.user("trial")
	v.google.set(func(s *stubGoogle) {
		s.token = func(url.Values) (int, string) { return http.StatusOK, `{"access_token":"a","refresh_token":"r"}` }
		s.list = func(url.Values) (int, string) { return http.StatusForbidden, `` }
	})
	state := v.connect(u, "/v1/calendar/connect").Query().Get("state")
	require.Equal(t, "luminavault://oauth/google-calendar?status=ok", v.callback(url.Values{"state": {state}, "code": {"c"}}))
	a, ok := v.readAccount(u)
	require.True(t, ok)
	require.Nil(t, a.lastSynced)
}

func testCallbackOneShot(t *testing.T, v env) {
	notFound := "luminavault://oauth/google-calendar?status=error&reason=session_not_found"
	u := v.user("trial")
	state := v.connect(u, "/v1/calendar/connect").Query().Get("state")
	require.NotEqual(t, notFound, v.callback(url.Values{"state": {state}}))
	require.Equal(t, notFound, v.callback(url.Values{"state": {state}}), "consumed")
	require.Equal(t, notFound, v.callback(url.Values{"state": {"nope"}, "code": {"c"}}))

	// Sessions live 600 s.
	now := time.Now()
	clock := func() time.Time { return now }
	w := newEnv(t, v.pool, opts{now: clock})
	s1 := w.connect(u, "/v1/calendar/connect").Query().Get("state")
	s2 := w.connect(u, "/v1/calendar/connect").Query().Get("state")
	now = now.Add(600 * time.Second)
	require.NotEqual(t, notFound, w.callback(url.Values{"state": {s1}}), "the boundary still counts")
	now = now.Add(time.Second)
	require.Equal(t, notFound, w.callback(url.Values{"state": {s2}}))
}

// MARK: create event

const createBody = `{"title":" Lunch ","startsAt":"2099-01-01T10:00:00.500Z","endsAt":"2099-01-01T12:00:00+01:00","location":"Cafe","notes":"n","attendees":["a@x.test"]}`

func insertOK(t *testing.T, got *map[string]any) func(map[string]any) (int, string) {
	t.Helper()
	return func(body map[string]any) (int, string) {
		*got = body
		return http.StatusOK, `{"id":"g-new","summary":"Lunch","location":"Cafe","description":"n","start":{"dateTime":"2099-01-01T10:00:00Z"},"end":{"dateTime":"2099-01-01T11:30:00Z"},"htmlLink":"https://cal/new","organizer":{"email":"me@gmail.test"},"updated":"2026-01-01T00:00:00Z"}`
	}
}

func testCreateHappy(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeCalendar, "connected", "rt", "at", time.Now().Add(time.Hour))
	var got map[string]any
	v.google.set(func(s *stubGoogle) { s.insert = insertOK(t, &got) })
	r := v.do(u, http.MethodPost, "/v1/calendar/events", createBody)
	dto := decode[calendar.CalendarEventDTO](t, r)
	require.Equal(t, "g-new", dto.ExternalID)
	require.Equal(t, "google", dto.Source)
	require.Equal(t, "confirmed", dto.Status)
	require.Equal(t, "https://cal/new", *dto.HtmlLink)
	require.Equal(t, strings.ToUpper(dto.Id.String()), dto.Id.String())
	require.Contains(t, r.String(), `"startsAt":"2099-01-01T10:00:00Z"`)

	require.Equal(t, "Lunch", got["summary"], "trimmed")
	require.Equal(t, map[string]any{"dateTime": "2099-01-01T10:00:00Z"}, got["start"], "UTC, whole seconds")
	require.Equal(t, map[string]any{"dateTime": "2099-01-01T11:00:00Z"}, got["end"])
	require.Equal(t, "Cafe", got["location"])
	require.Equal(t, "n", got["description"])
	require.Equal(t, []any{map[string]any{"email": "a@x.test"}}, got["attendees"])
	require.Contains(t, v.google.bearers, "Bearer at")
	require.Zero(t, v.google.refreshN.Load(), "the cached token was still good")
	require.Equal(t, 1, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1 AND external_id = 'g-new' AND source = 'google'`, u))

	// The same event again upserts, keeping id.
	again := decode[calendar.CalendarEventDTO](t, v.do(u, http.MethodPost, "/v1/calendar/events", `{"title":"x","startsAt":"2099-01-01T10:00:00Z","endsAt":"2099-01-01T12:00:00Z"}`))
	require.Equal(t, dto.Id, again.Id)
	require.NotContains(t, got, "attendees", "none sent when empty")
	require.NotContains(t, got, "location")
}

func testCreateRefresh(t *testing.T, v env) {
	u := v.user("trial")
	// Within the 120 s skew counts as expired.
	v.account(u, scopeCalendar, "connected", "rt", "old", time.Now().Add(100*time.Second))
	var form url.Values
	var got map[string]any
	v.google.set(func(s *stubGoogle) {
		s.token = func(f url.Values) (int, string) {
			form = f
			return http.StatusOK, `{"access_token":"fresh","expires_in":900}`
		}
		s.insert = insertOK(t, &got)
	})
	require.Equal(t, http.StatusOK, v.do(u, http.MethodPost, "/v1/calendar/events", createBody).code)
	require.Equal(t, "refresh_token", form.Get("grant_type"))
	require.Equal(t, "rt", form.Get("refresh_token"))
	require.Contains(t, v.google.bearers, "Bearer fresh")
	a, _ := v.readAccount(u)
	require.Equal(t, "fresh", a.access)
	require.Equal(t, "rt", a.refresh, "not rotated, so kept")
	require.WithinDuration(t, time.Now().Add(900*time.Second), *a.expires, time.Minute)
}

func testCreateRotation(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeCalendar, "connected", "rt", "old", time.Now().Add(-time.Hour))
	var got map[string]any
	v.google.set(func(s *stubGoogle) {
		s.token = func(url.Values) (int, string) {
			return http.StatusOK, `{"access_token":"fresh","refresh_token":"rt-2"}`
		}
		s.insert = insertOK(t, &got)
	})
	require.Equal(t, http.StatusOK, v.do(u, http.MethodPost, "/v1/calendar/events", createBody).code)
	a, _ := v.readAccount(u)
	require.Equal(t, "rt-2", a.refresh)
}

func testCreateRejected(t *testing.T, v env) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		u := v.user("trial")
		v.account(u, scopeCalendar, "connected", "rt", "old", time.Now().Add(-time.Hour))
		v.google.set(func(s *stubGoogle) {
			s.token = func(url.Values) (int, string) { return code, `{"error":"invalid_grant"}` }
		})
		r := v.do(u, http.MethodPost, "/v1/calendar/events", createBody)
		require.Equal(t, http.StatusConflict, r.code)
		require.JSONEq(t, `{"error":{"message":"Google Calendar is not connected"}}`, r.String())
		a, _ := v.readAccount(u)
		require.Equal(t, "needs_reauth", a.status)
		require.Equal(t, "refresh_rejected", a.failure)
		status := decode[calendar.CalendarStatusResponse](t, v.do(u, http.MethodGet, "/v1/calendar/status", ""))
		require.True(t, status.NeedsReauth)
	}
	// Anything else from the token endpoint is not a dead grant.
	u := v.user("trial")
	v.account(u, scopeCalendar, "connected", "rt", "old", time.Now().Add(-time.Hour))
	v.google.set(func(s *stubGoogle) { s.token = func(url.Values) (int, string) { return http.StatusBadGateway, `` } })
	require.Equal(t, http.StatusInternalServerError, v.do(u, http.MethodPost, "/v1/calendar/events", createBody).code)
	a, _ := v.readAccount(u)
	require.Equal(t, "connected", a.status)
}

func testCreateGoogleErrors(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeCalendar, "connected", "rt", "at", time.Now().Add(time.Hour))
	for _, c := range []struct {
		code int
		body string
	}{
		{http.StatusUnauthorized, ""},
		{http.StatusForbidden, ""},
		{http.StatusTooManyRequests, ""},
		{http.StatusBadGateway, "boom"},
		{http.StatusOK, "not json"},
		{http.StatusOK, `{"summary":"no id"}`},
	} {
		v.google.set(func(s *stubGoogle) { s.insert = func(map[string]any) (int, string) { return c.code, c.body } })
		r := v.do(u, http.MethodPost, "/v1/calendar/events", createBody)
		require.Equal(t, http.StatusInternalServerError, r.code, "%d %s", c.code, c.body)
		require.Empty(t, r.body, "nothing of Google's answer reaches the client")
	}
	// A connected row with no refresh token and no usable access token.
	w := v.user("trial")
	v.exec(`INSERT INTO calendar_accounts (tenant_id, provider, scope) VALUES ($1, 'google', $2)`, w, scopeCalendar)
	require.Equal(t, http.StatusInternalServerError, v.do(w, http.MethodPost, "/v1/calendar/events", createBody).code)
}

func testRefreshSingleFlight(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeCalendar, "connected", "rt", "old", time.Now().Add(-time.Hour))
	release := make(chan struct{})
	v.google.set(func(s *stubGoogle) {
		s.token = func(url.Values) (int, string) {
			<-release
			return http.StatusOK, `{"access_token":"fresh"}`
		}
		s.insert = func(map[string]any) (int, string) {
			return http.StatusOK, `{"id":"g-flight","summary":"Lunch","start":{"dateTime":"2099-01-01T10:00:00Z"},"end":{"dateTime":"2099-01-01T11:00:00Z"}}`
		}
	})
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Go(func() { codes[i] = v.do(u, http.MethodPost, "/v1/calendar/events", createBody).code })
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, c := range codes {
		require.Equal(t, http.StatusOK, c)
	}
	require.Equal(t, int32(1), v.google.refreshN.Load())
}

// MARK: disconnect

func testDisconnectRevokes(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeCalendar, "connected", "rt", "at", time.Now().Add(time.Hour))
	v.exec(`INSERT INTO calendar_events (tenant_id, source, external_id, title, starts_at, ends_at) VALUES ($1, 'google', 'g', 'G', now(), now()), ($1, 'apple_eventkit', 'a', 'A', now(), now())`, u)
	r := v.do(u, http.MethodPost, "/v1/calendar/disconnect", "")
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	require.Equal(t, []string{"at"}, v.google.revoked)
	_, ok := v.readAccount(u)
	require.False(t, ok)
	require.Equal(t, 1, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1`, u))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM calendar_events WHERE tenant_id = $1 AND source = 'apple_eventkit'`, u))
}

func testDisconnectGoogleDown(t *testing.T, v env) {
	u := v.user("trial")
	// Expired, and the refresh fails: nothing to revoke, the row goes anyway.
	v.account(u, scopeCalendar, "connected", "rt", "at", time.Now().Add(-time.Hour))
	v.google.set(func(s *stubGoogle) { s.token = func(url.Values) (int, string) { return http.StatusBadGateway, `` } })
	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodPost, "/v1/calendar/disconnect", "").code)
	require.Empty(t, v.google.revoked)
	_, ok := v.readAccount(u)
	require.False(t, ok)
}

func testGmailDisconnectRevokes(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeGmail, "connected", "rt", "at", time.Now().Add(time.Hour))
	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodPost, "/v1/mail/gmail/disconnect", "").code)
	require.Equal(t, []string{"at"}, v.google.revoked)
	_, ok := v.readAccount(u)
	require.False(t, ok)
}

// MARK: Gmail, MuseProactiveIntegrationTests

func testGmailHalves(t *testing.T, v env) {
	gmail := func(u uuid.UUID) resp { return v.do(u, http.MethodGet, "/v1/mail/gmail/status", "") }
	u := v.user("trial")
	require.JSONEq(t, `{"connected":false,"needsReauth":false,"calendarConnected":false}`, gmail(u).String())

	v.account(u, scopeCalendar, "connected", "r", "a", time.Now().Add(time.Hour))
	require.JSONEq(t, `{"connected":false,"needsReauth":false,"calendarConnected":true}`, gmail(u).String())

	// Gmail only: Calendar must not claim to be connected.
	v.exec(`UPDATE calendar_accounts SET scope = $2 WHERE tenant_id = $1`, u, scopeGmail)
	require.JSONEq(t, `{"connected":true,"needsReauth":false,"accountEmail":"me@gmail.test","calendarConnected":false}`, gmail(u).String())
	require.JSONEq(t, `{"connected":false,"needsReauth":false}`, v.do(u, http.MethodGet, "/v1/calendar/status", "").String())
}

func testGmailKeepsCalendar(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeBoth, "connected", "r", "a", time.Now().Add(time.Hour))
	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodPost, "/v1/mail/gmail/disconnect", "").code)
	a, _ := v.readAccount(u)
	require.Equal(t, "openid https://www.googleapis.com/auth/calendar.events email", a.scope)
	require.JSONEq(t, `{"connected":false,"needsReauth":false,"calendarConnected":true}`, v.do(u, http.MethodGet, "/v1/mail/gmail/status", "").String())
	require.Contains(t, v.do(u, http.MethodGet, "/v1/calendar/status", "").String(), `"connected":true`)
	require.Empty(t, v.google.revoked, "Google has no per-scope revoke")
}

func testCalendarKeepsGmail(t *testing.T, v env) {
	u := v.user("trial")
	v.account(u, scopeBoth, "connected", "r", "a", time.Now().Add(time.Hour))
	v.exec(`UPDATE calendar_accounts SET sync_token = 'tok' WHERE tenant_id = $1`, u)
	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodPost, "/v1/calendar/disconnect", "").code)
	a, ok := v.readAccount(u)
	require.True(t, ok)
	require.Empty(t, a.syncToken)
	require.Contains(t, v.do(u, http.MethodGet, "/v1/calendar/status", "").String(), `"connected":false`)
	require.Contains(t, v.do(u, http.MethodGet, "/v1/mail/gmail/status", "").String(), `"connected":true`)

	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodPost, "/v1/mail/gmail/disconnect", "").code)
	_, ok = v.readAccount(u)
	require.False(t, ok)
}

func testGmailPurpose(t *testing.T, v env) {
	u := v.user("trial")
	authorize := v.connect(u, "/v1/mail/gmail/connect")
	require.Equal(t, scopeGmail, authorize.Query().Get("scope"), "asks only for gmail.readonly")
	require.Equal(t, "true", authorize.Query().Get("include_granted_scopes"), "and keeps earlier grants")
	require.Equal(t, "offline", authorize.Query().Get("access_type"))
	require.Equal(t, "https://api.test/v1/calendar/oauth/callback", authorize.Query().Get("redirect_uri"))
	// Declining lands the iOS app on the Gmail deep link, not Calendar's.
	require.Equal(t, "luminavault://oauth/google-gmail?status=error&reason=access_denied",
		v.callback(url.Values{"state": {authorize.Query().Get("state")}, "error": {"access_denied"}}))

	// Granting folds in the Calendar scope already held when Google omits scope.
	v.account(u, scopeCalendar, "connected", "r", "a", time.Now().Add(time.Hour))
	v.google.set(func(s *stubGoogle) {
		s.token = func(url.Values) (int, string) { return http.StatusOK, `{"access_token":"a2","refresh_token":"r2"}` }
	})
	state := v.connect(u, "/v1/mail/gmail/connect").Query().Get("state")
	require.Equal(t, "luminavault://oauth/google-gmail?status=ok", v.callback(url.Values{"state": {state}, "code": {"c"}}))
	require.JSONEq(t, `{"connected":true,"needsReauth":false,"accountEmail":"me@gmail.test","calendarConnected":true}`,
		v.do(u, http.MethodGet, "/v1/mail/gmail/status", "").String())

	// A Gmail-only grant skips the initial Calendar sync: it would only collect 403s.
	w := v.user("trial")
	var listed atomic.Int32
	v.google.set(func(s *stubGoogle) {
		s.token = func(url.Values) (int, string) {
			return http.StatusOK, `{"access_token":"a","refresh_token":"r","scope":"` + scopeGmail + `"}`
		}
		s.list = func(url.Values) (int, string) {
			listed.Add(1)
			return http.StatusForbidden, ""
		}
	})
	state = v.connect(w, "/v1/mail/gmail/connect").Query().Get("state")
	require.Equal(t, "luminavault://oauth/google-gmail?status=ok", v.callback(url.Values{"state": {state}, "code": {"c"}}))
	require.Zero(t, listed.Load())
}

// MARK: wiring

func TestDisabledPaths(t *testing.T) {
	pool := startDB(t)

	t.Run("not configured answers 503 before looking at returnTo", func(t *testing.T) {
		v := newEnv(t, pool, opts{unconfigured: true})
		u := v.user("trial")
		r := v.do(u, http.MethodPost, "/v1/calendar/connect?returnTo=https://evil.example/", "")
		require.Equal(t, http.StatusServiceUnavailable, r.code)
		require.JSONEq(t, `{"error":{"message":"Google Calendar is not configured on this server"}}`, r.String())
		r = v.do(u, http.MethodPost, "/v1/mail/gmail/connect", "")
		require.Equal(t, http.StatusServiceUnavailable, r.code)
		require.JSONEq(t, `{"error":{"message":"Google sign-in is not configured on this server"}}`, r.String())
		require.Equal(t, http.StatusOK, v.do(u, http.MethodGet, "/v1/calendar/status", "").code)
	})

	t.Run("without a SecretBox only sync and brief exist", func(t *testing.T) {
		v := newEnv(t, pool, opts{noBox: true})
		u := v.user("trial")
		for _, c := range [][2]string{
			{http.MethodGet, "/v1/calendar/status"},
			{http.MethodPost, "/v1/calendar/connect"},
			{http.MethodPost, "/v1/calendar/disconnect"},
			{http.MethodGet, "/v1/calendar/events"},
			{http.MethodGet, "/v1/calendar/oauth/callback"},
			{http.MethodGet, "/v1/mail/gmail/status"},
			{http.MethodPost, "/v1/mail/gmail/connect"},
			{http.MethodPost, "/v1/mail/gmail/disconnect"},
		} {
			require.Equal(t, http.StatusNotFound, v.do(u, c[0], c[1], "").code, c[1])
		}
		require.Equal(t, http.StatusForbidden, v.do(u, http.MethodPost, "/v1/calendar/sync", syncBody(ev("e", "t"))).code)
		require.Equal(t, http.StatusNotFound, v.do(u, http.MethodGet, briefPath(uuid.NewString()), "").code)
	})
}

// TestSpecConformance checks responses against api/openapi.yaml, including the
// Google success paths parity cannot compare.
func TestSpecConformance(t *testing.T) {
	pool := startDB(t)
	v := newEnv(t, pool, opts{})
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	u := v.user("trial")
	v.consent(u)
	v.account(u, scopeBoth, "connected", "r", "a", time.Now().Add(time.Hour))
	var got map[string]any
	v.google.set(func(s *stubGoogle) { s.insert = insertOK(t, &got) })

	for _, c := range []struct {
		user         uuid.UUID
		method, path string
		body         string
	}{
		{u, http.MethodGet, "/v1/calendar/status", ""},
		{seedAnn, http.MethodGet, "/v1/calendar/status", ""},
		{u, http.MethodPost, "/v1/calendar/connect", ""},
		{seedAnn, http.MethodGet, "/v1/calendar/events", ""},
		{u, http.MethodPost, "/v1/calendar/events", createBody},
		{u, http.MethodPost, "/v1/calendar/sync", syncBody(ev("spec", "Spec"))},
		{seedBri, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000001"), ""},
		{seedBri, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000002"), ""},
		{seedOff, http.MethodGet, briefPath("CA1C0000-0000-4000-8000-000000000041"), ""},
		{u, http.MethodGet, "/v1/mail/gmail/status", ""},
		{u, http.MethodPost, "/v1/mail/gmail/connect", ""},
		{u, http.MethodPost, "/v1/mail/gmail/disconnect", ""},
		{u, http.MethodPost, "/v1/calendar/disconnect", ""},
	} {
		r := v.do(c.user, c.method, c.path, c.body)
		req := httptest.NewRequest(c.method, strings.Split(c.path, "?")[0], nil)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			},
			Status: r.code, Header: http.Header{"Content-Type": {r.header.Get("Content-Type")}},
		}
		in.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(context.Background(), in), "%s %s -> %d %s", c.method, c.path, r.code, r.String())
	}
}

// TestParityScenariosRunOnGo runs the scenario files against this server: no
// step may error or answer 5xx except the ones Swift answers 5xx too.
func TestParityScenariosRunOnGo(t *testing.T) {
	ctx := context.Background()
	pool := startDB(t)
	v := newEnv(t, pool, opts{unconfigured: true})
	srv := httptest.NewServer(v.e)
	t.Cleanup(srv.Close)

	rawUsers, err := os.ReadFile("../../../parity/seed/users.yaml")
	require.NoError(t, err)
	var names map[string]string
	require.NoError(t, yaml.Unmarshal(rawUsers, &names))
	users := map[string]uuid.UUID{}
	for k, id := range names {
		users[k] = uuid.MustParse(id)
	}
	allowed5xx := map[string]bool{"brief undecodable attendees": true, "brief over the embedding cap": true}
	var all []scenario.Transcript
	for _, dir := range []string{"calendar", "mail"} {
		scs, err := scenario.Load("../../../parity/scenarios/" + dir)
		require.NoError(t, err)
		require.NotEmpty(t, scs)
		target := scenario.Target{Name: "go", BaseURL: srv.URL, Keys: keys, Users: users, Snapshot: scenario.PGSnapshotter(pool)}
		for _, sc := range scs {
			tr, err := scenario.Run(ctx, sc, target)
			require.NoError(t, err, sc.File)
			for _, st := range tr.Steps {
				if !allowed5xx[st.Name] && st.Status != http.StatusServiceUnavailable {
					require.Less(t, st.Status, 500, "%s/%s", sc.File, st.Name)
				}
			}
			all = append(all, tr)
		}
	}
	if out := os.Getenv("CALENDAR_TRANSCRIPT"); out != "" {
		raw, err := json.MarshalIndent(all, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(out, raw, 0o600))
	}
}
