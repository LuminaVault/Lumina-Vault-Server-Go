package reminders_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/reminders"
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

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

const (
	fired   = "E0000000-0000-4000-8000-000000000001"
	standup = "E0000000-0000-4000-8000-000000000002"
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setup(t *testing.T, transport reminders.ChatTransport) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"../../../parity/seed/base.sql", "../../../parity/seed/apple.sql"} {
		seed, err := os.ReadFile(f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{reminders.New(pool, transport, "hermes-3")},
	})
	return env{t: t, e: e, pool: pool}
}

type resp struct {
	code int
	ct   string
	body []byte
}

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
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

type dto struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	FireAt         string  `json:"fireAt"`
	RecurrenceCron *string `json:"recurrenceCron"`
	FiredAt        *string `json:"firedAt"`
	CreatedAt      string  `json:"createdAt"`
}

func (v env) list(user uuid.UUID, query string) []dto {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/reminders"+query, "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	require.NotContains(v.t, string(r.body), "nextCursor", "nil cursor is omitted")
	var out struct{ Reminders []dto }
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out.Reminders
}

func (v env) one(r resp) dto {
	v.t.Helper()
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var d dto
	require.NoError(v.t, json.Unmarshal(r.body, &d))
	return d
}

func TestListNewestFireAtFirstAndLimit(t *testing.T) {
	v := setup(t, nil)
	v.one(v.do(alice, http.MethodPost, "/v1/reminders", `{"title":"Later","body":"","fireAt":"2027-01-01T00:00:00Z"}`))
	got := v.list(alice, "")
	require.Len(t, got, 3)
	require.Equal(t, "Later", got[0].Title)
	require.Equal(t, standup, got[1].ID)
	require.Equal(t, fired, got[2].ID)
	require.Equal(t, "2026-01-05T09:00:05Z", *got[2].FiredAt)
	require.Len(t, v.list(alice, "?limit=1"), 1)
	require.Len(t, v.list(alice, "?limit=0"), 1)
	require.Len(t, v.list(alice, "?limit=999"), 3)
	require.Len(t, v.list(alice, "?limit=abc"), 3)
	require.Empty(t, v.list(bob, ""))
}

func TestCreateTrimsTitleAndKeepsEmptyCron(t *testing.T) {
	v := setup(t, nil)
	r := v.do(alice, http.MethodPost, "/v1/reminders", `{"title":"  Call mom ","body":" at home ","fireAt":"2026-11-01T09:00:00.500Z","recurrenceCron":""}`)
	require.Equal(t, "application/json; charset=utf-8", r.ct)
	d := v.one(r)
	require.Equal(t, "Call mom", d.Title)
	require.Equal(t, " at home ", d.Body, "body is not trimmed")
	require.Equal(t, "2026-11-01T09:00:00Z", d.FireAt)
	require.Equal(t, "", *d.RecurrenceCron, `"" is valid and stored on create`)
	require.Nil(t, d.FiredAt)
	require.Equal(t, strings.ToUpper(d.ID), d.ID)
	require.NotContains(t, string(r.body), "firedAt")

	d = v.one(v.do(alice, http.MethodPost, "/v1/reminders", `{"title":"Odd","body":"","fireAt":"2026-11-02T11:00:00+02:00","recurrenceCron":" 1,,2 */ 1-31/7 +5 0-6 "}`))
	require.Equal(t, "2026-11-02T09:00:00Z", d.FireAt)
}

func TestPatchSemantics(t *testing.T) {
	v := setup(t, nil)
	d := v.one(v.do(alice, http.MethodPatch, "/v1/reminders/"+fired, `{"fireAt":"2026-12-01T08:00:00Z"}`))
	require.Nil(t, d.FiredAt, "a new fireAt re-arms a fired reminder")
	require.Equal(t, "Seeded fired", d.Title)

	d = v.one(v.do(alice, http.MethodPatch, "/v1/reminders/"+strings.ToLower(standup), `{"recurrenceCron":""}`))
	require.Nil(t, d.RecurrenceCron, `"" clears the cron on PATCH`)

	d = v.one(v.do(alice, http.MethodPatch, "/v1/reminders/"+standup, `{"title":" Daily ","body":"","recurrenceCron":"0 7 * * *"}`))
	require.Equal(t, "Daily", d.Title)
	require.Equal(t, "0 7 * * *", *d.RecurrenceCron)

	d = v.one(v.do(alice, http.MethodPatch, "/v1/reminders/"+standup, `{"title":null}`))
	require.Equal(t, "Daily", d.Title, "null optional = unchanged")

	r := v.do(alice, http.MethodPatch, "/v1/reminders/"+standup, `{"title":"ok","recurrenceCron":"bad"}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	var title string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT title FROM reminders WHERE id = $1`, uuid.MustParse(standup)).Scan(&title))
	require.Equal(t, "Daily", title, "a failed validation saves nothing")
}

func TestDeleteThenDeleteAgain(t *testing.T) {
	v := setup(t, nil)
	r := v.do(alice, http.MethodDelete, "/v1/reminders/"+standup, "")
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	require.Empty(t, r.ct)
	require.Equal(t, http.StatusNotFound, v.do(alice, http.MethodDelete, "/v1/reminders/"+standup, "").code)
}

func TestErrorsMatchSwift(t *testing.T) {
	v := setup(t, nil)
	cases := []struct {
		name, method, path, body string
		user                     uuid.UUID
		status                   int
		message                  string
	}{
		{"no token", http.MethodGet, "/v1/reminders", "", uuid.Nil, 401, "Authenticated identity is unavailable"},
		{"missing body", http.MethodPost, "/v1/reminders", `{"title":"x","fireAt":"2026-11-01T09:00:00Z"}`, alice, 400, "Coding key `body` not found."},
		{"whitespace title", http.MethodPost, "/v1/reminders", `{"title":" ","body":"","fireAt":"2026-11-01T09:00:00Z"}`, alice, 400, "reminder title required"},
		{"bad cron", http.MethodPost, "/v1/reminders", `{"title":"x","body":"","fireAt":"2026-11-01T09:00:00Z","recurrenceCron":"61 * * * *"}`, alice, 400, "invalid recurrence cron expression"},
		{"bad date", http.MethodPost, "/v1/reminders", `{"title":"x","body":"","fireAt":"soon"}`, alice, 400, "The given data was not valid input: Expected date string to be ISO8601-formatted."},
		{"bad id before body", http.MethodPatch, "/v1/reminders/nope", "nope", alice, 400, "invalid reminder id"},
		{"body before lookup", http.MethodPatch, "/v1/reminders/00000000-0000-4000-8000-000000000000", "nope", alice, 400, "The given data was not valid input: The given data was not valid JSON."},
		{"random id", http.MethodPatch, "/v1/reminders/00000000-0000-4000-8000-000000000000", `{}`, alice, 404, "reminder not found"},
		{"empty title", http.MethodPatch, "/v1/reminders/" + standup, `{"title":" "}`, alice, 400, "reminder title cannot be empty"},
		{"other tenant patch", http.MethodPatch, "/v1/reminders/" + standup, `{"title":"x"}`, bob, 404, "reminder not found"},
		{"other tenant delete", http.MethodDelete, "/v1/reminders/" + standup, "", bob, 404, "reminder not found"},
		{"delete bad id", http.MethodDelete, "/v1/reminders/nope", "", alice, 400, "invalid reminder id"},
	}
	for _, tc := range cases {
		r := v.do(tc.user, tc.method, tc.path, tc.body)
		require.Equal(t, tc.status, r.code, tc.name)
		require.JSONEq(t, `{"error":{"message":`+mustJSON(tc.message)+`}}`, string(r.body), tc.name)
	}
}

// stubTransport is an in-process stand-in for the LLM (never a real key).
type stubTransport struct {
	reply   string
	err     error
	payload []byte
	session string
}

func (s *stubTransport) ChatCompletions(_ context.Context, payload []byte, sessionKey string) ([]byte, error) {
	s.payload, s.session = payload, sessionKey
	if s.err != nil {
		return nil, s.err
	}
	return json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": s.reply}}}})
}

func TestDetectFailsClosedWithoutTransport(t *testing.T) {
	v := setup(t, nil)
	r := v.do(alice, http.MethodPost, "/v1/reminders/detect", `{"text":"remind me to call mom at 5pm"}`)
	require.Equal(t, http.StatusOK, r.code)
	require.JSONEq(t, `{"isReminder":false}`, string(r.body))
	r = v.do(alice, http.MethodPost, "/v1/reminders/detect", `{}`)
	require.Equal(t, http.StatusBadRequest, r.code, "the body is decoded even though the classifier will fail")
}

func TestDetectWithStubTransport(t *testing.T) {
	stub := &stubTransport{reply: "```json\n{\"isReminder\":true,\"title\":\"Call mom\",\"fireAt\":\"2026-06-09T17:00:00.250Z\",\"recurrenceCron\":\"\",\"scheduleHuman\":\"Today at 5:00 PM\"}\n```"}
	v := setup(t, stub)
	r := v.do(alice, http.MethodPost, "/v1/reminders/detect", `{"text":"remind me to call mom at 5pm"}`)
	require.Equal(t, http.StatusOK, r.code)
	require.JSONEq(t, `{"isReminder":true,"title":"Call mom","body":"","fireAt":"2026-06-09T17:00:00Z","scheduleHuman":"Today at 5:00 PM"}`, string(r.body))
	require.Equal(t, "A11CE000-0000-4000-8000-000000000001", stub.session, "session key is the tenant's uppercase id")
	var sent struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		Stream      bool    `json:"stream"`
		Messages    []struct{ Role, Content string }
	}
	require.NoError(t, json.Unmarshal(stub.payload, &sent))
	require.Equal(t, "hermes-3", sent.Model)
	require.InDelta(t, 0.1, sent.Temperature, 1e-9)
	require.False(t, sent.Stream)
	require.Equal(t, "system", sent.Messages[0].Role)
	require.Contains(t, sent.Messages[0].Content, "(UTC). Resolve all relative phrasing")
	require.Equal(t, "remind me to call mom at 5pm", sent.Messages[1].Content)
}

func TestParseProposal(t *testing.T) {
	no := `{"isReminder":false}`
	for _, in := range []string{"", "not json", `{"isReminder":false}`, `{"isReminder":true}`, `{"isReminder":true,"fireAt":"tomorrow"}`, `[]`} {
		b, _ := json.Marshal(reminders.ParseProposal(in))
		require.JSONEq(t, no, string(b), in)
	}
	p := reminders.ParseProposal(`  {"isReminder":true,"title":"Stretch","body":"","fireAt":"2026-06-09T15:00:00Z","recurrenceCron":"0 15 * * 1-5"}  `)
	require.True(t, p.IsReminder)
	require.Equal(t, "0 15 * * 1-5", *p.RecurrenceCron)
	require.Equal(t, time.Date(2026, 6, 9, 15, 0, 0, 0, time.UTC), p.FireAt.UTC())
	p = reminders.ParseProposal("```\n{\"isReminder\":true,\"fireAt\":\"2026-06-09T15:00:00+02:00\"}")
	require.True(t, p.IsReminder, "an unterminated fence is tolerated")
	require.Nil(t, p.Title)
	require.Equal(t, "", *p.Body)
}

func TestSystemPromptStampsUTCNow(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 30, 45, 999, time.FixedZone("x", 3600))
	require.Contains(t, reminders.SystemPrompt(now), "The current time is 2026-06-09T11:30:45Z (UTC).")
}

func TestParseCronMatchesSwift(t *testing.T) {
	valid := []string{"0 7 * * *", "*/15 9-17 * * 1-5", "1-30/5 0 1 1 0", "0 9 * * 1-5", " 1,,2 */ 1-31/7 +5 0-6 ", "1--5 * * * *", "0\t9 * *\n*"}
	for _, c := range valid {
		require.NoError(t, reminders.ParseCron(c), c)
	}
	invalid := []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 7",
		"5-1 * * * *", "*/0 * * * *", "*/2/3 * * * *", "a * * * *", "1- * * * *", "-1 * * * *", "1-2-3 * * * *", "/ * * * *",
	}
	for _, c := range invalid {
		require.Error(t, reminders.ParseCron(c), c)
	}
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, nil)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/reminders", ""},
		{http.MethodPost, "/v1/reminders", `{"title":"Spec","body":"","fireAt":"2026-11-01T09:00:00Z","recurrenceCron":"0 9 * * *"}`},
		{http.MethodPatch, "/v1/reminders/" + fired, `{"body":"b"}`},
		{http.MethodPost, "/v1/reminders/detect", `{"text":"hi"}`},
		{http.MethodDelete, "/v1/reminders/" + standup, ""},
	} {
		r := v.do(alice, c.method, c.path, c.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: r.code, Header: http.Header{"Content-Type": {r.ct}}}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
