package community_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/community"
	"lumina/internal/ratelimit"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Fixture members from parity/seed/community.sql.
var (
	ann  = member("A1")
	ben  = member("A2")
	mod  = member("A5")
	rep1 = member("A9")
	rep2 = member("AA")
	rep3 = member("AB")
	nw   = member("AC")
	nw2  = member("B6")
	blk  = member("AF")
	read = member("B2")
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

func member(suffix string) uuid.UUID {
	return uuid.MustParse("C0AA0000-0000-4000-8000-0000000000" + suffix)
}

func profileOf(user uuid.UUID) string {
	return "C0AB0000" + strings.ToUpper(user.String())[8:]
}

// allowAll is what Swift's tests get from llm.provider=stub.
type allowAll struct{}

func (allowAll) Review(_ context.Context, title *string, body string) community.Verdict {
	text := body
	if title != nil {
		text = *title + "\n" + body
	}
	if category := community.FilterCheck(text); category != "" {
		return community.Verdict{Kind: community.Rejected, Category: category}
	}
	return community.Verdict{Kind: community.Allowed}
}

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setupWith(t *testing.T, cfg community.Config, moderator community.Moderator) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "community.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	return env{t: t, pool: pool, e: serve(pool, cfg, moderator)}
}

func serve(pool *pgxpool.Pool, cfg community.Config, moderator community.Moderator) *echo.Echo {
	identify := auth.Identify(keys, auth.PGUsers(pool))
	return httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{community.New(pool, cfg, identify, ratelimit.NewStore(), moderator)},
	})
}

func setup(t *testing.T) env {
	t.Helper()
	return setupWith(t, community.Config{Enabled: true, PostingEnabled: true, AdminToken: "secret"}, allowAll{})
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (r resp) obj(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.body, &m), string(r.body))
	return m
}

func (v env) do(user uuid.UUID, method, path, body string, hdr ...string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

func (v env) errIs(r resp, status int, msg string) {
	v.t.Helper()
	require.Equal(v.t, status, r.code, string(r.body))
	m, err := json.Marshal(msg)
	require.NoError(v.t, err)
	require.JSONEq(v.t, `{"error":{"message":`+string(m)+`}}`, string(r.body))
}

func (v env) thread(user uuid.UUID, title, body string) map[string]any {
	v.t.Helper()
	r := v.do(user, http.MethodPost, "/v1/community/posts", fmt.Sprintf(`{"title":%q,"body":%q}`, title, body))
	require.Equal(v.t, http.StatusCreated, r.code, string(r.body))
	return r.obj(v.t)
}

func (v env) feedIDs(user uuid.UUID) []string {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/community/posts?limit=50", "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	ids := []string{}
	items, ok := r.obj(v.t)["items"].([]any)
	require.True(v.t, ok)
	for _, it := range items {
		m, ok := it.(map[string]any)
		require.True(v.t, ok)
		id, ok := m["id"].(string)
		require.True(v.t, ok)
		ids = append(ids, id)
	}
	return ids
}

func TestBoardNeverRevealsEmailUsernameOrUserID(t *testing.T) {
	v := setup(t)
	thread := v.thread(ann, "Paper or app?", "I still sketch on paper first.")
	require.Equal(t, "visible", thread["status"])
	require.Equal(t, true, thread["isOwn"])

	r := v.do(ben, http.MethodGet, "/v1/community/posts?limit=50", "")
	require.Equal(t, http.StatusOK, r.code)
	raw := string(r.body)
	require.Contains(t, raw, thread["id"])
	require.NotContains(t, raw, "comm_ann@parity.test")
	require.NotContains(t, raw, "comm_ann")
	require.NotContains(t, strings.ToLower(raw), ann.String())
	items, ok := r.obj(t)["items"].([]any)
	require.True(t, ok)
	for _, it := range items {
		m, ok := it.(map[string]any)
		require.True(t, ok)
		if m["id"] == thread["id"] {
			require.Equal(t, false, m["isOwn"])
		}
	}
}

func TestPostingNeedsProfileWithAcceptedTermsAndHandlesAreUnique(t *testing.T) {
	v := setup(t)
	v.errIs(v.do(nw, http.MethodPost, "/v1/community/posts", `{"title":"Hello there","body":"First!"}`), http.StatusForbidden, "community_profile_required")
	v.errIs(v.do(nw, http.MethodPut, "/v1/community/profile", `{"handle":"good_one","displayName":"A","acceptTerms":false,"termsVersion":1}`), http.StatusUnprocessableEntity, "terms_required")

	r := v.do(nw, http.MethodPut, "/v1/community/profile", `{"handle":"good_one","displayName":"A","acceptTerms":true,"termsVersion":1}`)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	v.errIs(v.do(nw2, http.MethodPut, "/v1/community/profile", `{"handle":"GOOD_ONE","displayName":"B","acceptTerms":true,"termsVersion":1}`), http.StatusConflict, "handle_taken")

	me := v.do(nw, http.MethodGet, "/v1/community/me", "").obj(t)
	require.Equal(t, true, me["enabled"])
	profile, ok := me["profile"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "good_one", profile["handle"])
	require.Equal(t, false, me["isModerator"])
}

func TestPreFilterRejectsPersonalData(t *testing.T) {
	v := setup(t)
	v.errIs(v.do(ann, http.MethodPost, "/v1/community/posts", `{"title":"Contact","body":"Email her at someone@example.com"}`),
		http.StatusUnprocessableEntity, "moderation_rejected:personal_data")
}

func TestBlockHidesMemberForBlockerOnly(t *testing.T) {
	v := setup(t)
	thread := v.thread(ann, "Morning pages", "Three pages, every day.")
	id, ok := thread["id"].(string)
	require.True(t, ok)

	r := v.do(blk, http.MethodPost, "/v1/community/blocks", `{"profileId":"`+profileOf(ann)+`"}`)
	require.Equal(t, http.StatusNoContent, r.code, string(r.body))
	require.NotContains(t, v.feedIDs(blk), id)
	require.Contains(t, v.feedIDs(read), id)
	require.Equal(t, http.StatusNotFound, v.do(blk, http.MethodGet, "/v1/community/posts/"+id, "").code)

	require.Equal(t, http.StatusNoContent, v.do(blk, http.MethodDelete, "/v1/community/blocks/"+profileOf(ann), "").code)
	require.Contains(t, v.feedIDs(blk), id)
}

func TestRepliesCountAndThreeReportsHideUntilRestored(t *testing.T) {
	v := setup(t)
	thread := v.thread(ann, "Inbox zero?", "Does anyone keep it?")
	id, ok := thread["id"].(string)
	require.True(t, ok)

	r := v.do(rep1, http.MethodPost, "/v1/community/posts/"+id+"/replies", `{"body":"Weekly, not daily."}`)
	require.Equal(t, http.StatusCreated, r.code, string(r.body))
	refreshed := v.do(ann, http.MethodGet, "/v1/community/posts/"+id, "").obj(t)
	require.EqualValues(t, 1, refreshed["replyCount"])

	v.errIs(v.do(ann, http.MethodPost, "/v1/community/posts/"+id+"/report", `{"reason":"spam"}`), http.StatusBadRequest, "cannot_report_own_post")
	for _, m := range []uuid.UUID{rep1, rep1, rep2, rep3} {
		r := v.do(m, http.MethodPost, "/v1/community/posts/"+id+"/report", `{"reason":"spam","note":"looks like an ad"}`)
		require.Equal(t, http.StatusAccepted, r.code, string(r.body))
	}
	require.NotContains(t, v.feedIDs(rep2), id)

	v.errIs(v.do(ann, http.MethodGet, "/v1/community/admin/queue", ""), http.StatusForbidden, "admin_required")

	queue := v.do(mod, http.MethodGet, "/v1/community/admin/queue?status=hidden", "")
	require.Equal(t, http.StatusOK, queue.code, string(queue.body))
	items, ok := queue.obj(t)["items"].([]any)
	require.True(t, ok)
	found := false
	for _, it := range items {
		m, ok := it.(map[string]any)
		require.True(t, ok)
		post, ok := m["post"].(map[string]any)
		require.True(t, ok)
		if post["id"] == id {
			found = true
			require.EqualValues(t, 3, m["openReports"])
			require.Equal(t, []any{"spam"}, m["reportReasons"])
		}
	}
	require.True(t, found)

	r = v.do(mod, http.MethodPost, "/v1/community/admin/posts/"+id+"/decision", `{"action":"restore"}`)
	require.Equal(t, http.StatusNoContent, r.code, string(r.body))
	require.Contains(t, v.feedIDs(rep2), id)
}

func TestFeedPagesWithStableCursor(t *testing.T) {
	v := setup(t)
	created := []string{}
	for i := range 5 {
		thread := v.thread(ann, fmt.Sprintf("Thread number %d", i), fmt.Sprintf("Body %d", i))
		id, ok := thread["id"].(string)
		require.True(t, ok)
		created = append(created, id)
		// Fluent writes a fresh Date per insert; keep the order strict.
		time.Sleep(2 * time.Millisecond)
	}
	seen := []string{}
	cursor := ""
	for {
		path := "/v1/community/posts?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := v.do(ann, http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		page := r.obj(t)
		items, ok := page["items"].([]any)
		require.True(t, ok)
		for _, it := range items {
			m, ok := it.(map[string]any)
			require.True(t, ok)
			id, ok := m["id"].(string)
			require.True(t, ok)
			seen = append(seen, id)
		}
		next, ok := page["nextCursor"].(string)
		if !ok {
			break
		}
		cursor = next
	}
	unique := map[string]bool{}
	for _, id := range seen {
		require.False(t, unique[id], "duplicate %s", id)
		unique[id] = true
	}
	mine := []string{}
	isCreated := map[string]bool{}
	for _, id := range created {
		isCreated[id] = true
	}
	for _, id := range seen {
		if isCreated[id] {
			mine = append(mine, id)
		}
	}
	require.Len(t, mine, 5)
	for i, id := range mine {
		require.Equal(t, created[4-i], id)
	}
}

func TestSwitches(t *testing.T) {
	v := setupWith(t, community.Config{Enabled: true, PostingEnabled: false}, allowAll{})
	v.errIs(v.do(ann, http.MethodPost, "/v1/community/posts", `{"title":"Hello there","body":"Anyone?"}`), http.StatusServiceUnavailable, "community_posting_disabled")
	require.Equal(t, http.StatusOK, v.do(ann, http.MethodGet, "/v1/community/posts", "").code)

	// ADMIN_TOKEN empty: the route exists and rejects everything.
	v.errIs(v.do(uuid.Nil, http.MethodPost, "/v1/admin/community/daily/run", "", "X-Admin-Token", "x"), http.StatusNotFound, "admin disabled")

	off := env{t: t, pool: v.pool, e: serve(v.pool, community.Config{}, allowAll{})}
	me := off.do(ann, http.MethodGet, "/v1/community/me", "").obj(t)
	require.Equal(t, false, me["enabled"])
	off.errIs(off.do(ann, http.MethodGet, "/v1/community/posts", ""), http.StatusServiceUnavailable, "community_disabled")
	off.errIs(off.do(uuid.Nil, http.MethodPost, "/v1/community/posts", "{bad"), http.StatusServiceUnavailable, "community_disabled")
	require.Equal(t, http.StatusNotFound, off.do(uuid.Nil, http.MethodPost, "/v1/admin/community/daily/run", "", "X-Admin-Token", "x").code)
}

// Without the LLM moderator, the production default: a post that passes the
// filter is held pending for Swift's sweep, and a reply does not bump the thread.
func TestFilterOnlyHoldsPostsPending(t *testing.T) {
	v := setupWith(t, community.Config{Enabled: true, PostingEnabled: true}, nil)
	thread := v.thread(ann, "Held for review", "Waiting for the sweep.")
	require.Equal(t, "pending", thread["status"])
	id, ok := thread["id"].(string)
	require.True(t, ok)
	require.NotContains(t, v.feedIDs(read), id)
	require.Contains(t, v.feedIDs(ann), id)
	v.errIs(v.do(ann, http.MethodPost, "/v1/community/posts", `{"title":"Contact","body":"a@b.co"}`), http.StatusUnprocessableEntity, "moderation_rejected:personal_data")
}

func TestAllowedPostsAreVisibleAndRepliesBumpTheThread(t *testing.T) {
	v := setup(t)
	thread := v.thread(ann, "Bump me", "Please.")
	id, ok := thread["id"].(string)
	require.True(t, ok)
	var moderatedAt *time.Time
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT moderated_at FROM community_posts WHERE id = $1`, id).Scan(&moderatedAt))
	require.NotNil(t, moderatedAt)
	time.Sleep(2 * time.Millisecond)
	r := v.do(ben, http.MethodPost, "/v1/community/posts/"+id+"/replies", `{"body":"Bumped."}`)
	require.Equal(t, http.StatusCreated, r.code, string(r.body))
	after := v.do(ann, http.MethodGet, "/v1/community/posts/"+id, "").obj(t)
	require.EqualValues(t, 1, after["replyCount"])
	var bumped bool
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT last_activity_at > created_at FROM community_posts WHERE id = $1`, id).Scan(&bumped))
	require.True(t, bumped)
}

func TestWriteBucketIsTenAMinute(t *testing.T) {
	v := setup(t)
	for range 10 {
		r := v.do(nw, http.MethodPut, "/v1/community/profile", `{"handle":"x","displayName":"x","acceptTerms":true,"termsVersion":1}`)
		require.Equal(t, http.StatusUnprocessableEntity, r.code)
	}
	r := v.do(nw, http.MethodPut, "/v1/community/profile", `{"handle":"x","displayName":"x","acceptTerms":true,"termsVersion":1}`)
	require.Equal(t, http.StatusTooManyRequests, r.code)
	require.Equal(t, http.StatusOK, v.do(nw, http.MethodGet, "/v1/community/me", "").code)
}

func TestDailyRun(t *testing.T) {
	v := setup(t)
	v.errIs(v.do(uuid.Nil, http.MethodPost, "/v1/admin/community/daily/run", ""), http.StatusUnauthorized, "admin token invalid")
	for _, want := range []string{"failed", "failed", "skipped", "skipped"} {
		r := v.do(uuid.Nil, http.MethodPost, "/v1/admin/community/daily/run", "", "X-Admin-Token", "secret")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		require.Equal(t, want, r.obj(t)["status"])
	}
	require.Equal(t, http.StatusNoContent, v.do(read, http.MethodGet, "/v1/community/daily/today", "").code)
}

func TestContentFilter(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"reach me at ana.k+notes@example.com please", "personal_data"},
		{"call +351 912 345 678 tonight", "personal_data"},
		{"call (415) 555-0134", "personal_data"},
		{"Since 2026-10-19 I walk 10,000 steps", ""},
		{"https://a.co https://b.co https://c.co", ""},
		{"https://a.co https://b.co https://c.co https://d.co", "spam"},
		{"mail me: a@b.co", "personal_data"},
	} {
		require.Equal(t, c.want, community.FilterCheck(c.text), c.text)
	}
}

func TestHandleRules(t *testing.T) {
	require.True(t, community.IsValidHandle("ana_k", "ana.kowalski"))
	require.False(t, community.IsValidHandle("an", "x"))
	require.False(t, community.IsValidHandle("Ana", "x"))
	require.False(t, community.IsValidHandle("lumina", "x"))
	require.False(t, community.IsValidHandle("ana_k", "Ana_K"))
}

func TestCursorRoundTrip(t *testing.T) {
	id := uuid.New()
	micros, got, err := community.DecodeCursor(community.EncodeCursor(1_761_000_000_123_456, id))
	require.NoError(t, err)
	require.Equal(t, int64(1_761_000_000_123_456), micros)
	require.Equal(t, id, got)
	_, _, err = community.DecodeCursor("not-a-cursor")
	require.Error(t, err)
}

func TestConfigFlags(t *testing.T) {
	require.True(t, community.ParseFlag("true"))
	require.True(t, community.ParseFlag("TRUE"))
	require.False(t, community.ParseFlag(""))
	require.False(t, community.ParseFlag("1"))
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	post := "C0AC0000-0000-4000-8000-000000000001"
	daily := "C0AC0000-0000-4000-8000-000000000003"
	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
	}{
		{nw, http.MethodGet, "/v1/community/me", ""},
		{mod, http.MethodGet, "/v1/community/me", ""},
		{nw, http.MethodPut, "/v1/community/profile", `{"handle":"spec_h","displayName":"Spec","acceptTerms":true,"termsVersion":1}`},
		{read, http.MethodGet, "/v1/community/posts?limit=3", ""},
		{read, http.MethodGet, "/v1/community/posts/" + post, ""},
		{read, http.MethodGet, "/v1/community/posts/" + daily, ""},
		{read, http.MethodGet, "/v1/community/posts/" + post + "/replies", ""},
		{ben, http.MethodPost, "/v1/community/posts", `{"title":"Spec thread","body":"Spec body"}`},
		{ben, http.MethodPost, "/v1/community/posts/" + post + "/replies", `{"body":"Spec reply"}`},
		{read, http.MethodGet, "/v1/community/blocks", ""},
		{blk, http.MethodGet, "/v1/community/blocks", ""},
		{mod, http.MethodGet, "/v1/community/admin/queue", ""},
		{uuid.Nil, http.MethodPost, "/v1/admin/community/daily/run", ""},
	} {
		var r resp
		if c.user == uuid.Nil {
			r = v.do(c.user, c.method, c.path, c.body, "X-Admin-Token", "secret")
		} else {
			r = v.do(c.user, c.method, c.path, c.body)
		}
		require.Less(t, r.code, 300, "%s %s: %s", c.method, c.path, r.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		outp := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		outp.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, outp), "%s %s: %s", c.method, c.path, r.body)
	}
}
