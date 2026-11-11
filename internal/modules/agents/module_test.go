package agents_test

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/modules/agents"
	"lumina/internal/ratelimit"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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

// Fixture users from parity/seed/agents.sql.
var (
	ann  = uuid.MustParse("A9E70000-0000-4000-8000-0000000000A1") // research, ops, Mac MCP; three conversations
	ben  = uuid.MustParse("A9E70000-0000-4000-8000-0000000000A2") // one conversation, no personas
	cy   = uuid.MustParse("A9E70000-0000-4000-8000-0000000000A3") // default, stocks
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

	annPlans = "A9E7C000-0000-4000-8000-000000000001"
)

// slowChat is the central agent's voice: it answers after a pause, so a second post
// to the same room lands while the first chain runs.
type slowChat struct {
	mu    sync.Mutex
	delay time.Duration
	calls int
}

func (s *slowChat) Chat(ctx context.Context, _, _ string, _ llm.CompletionRequest) (*llm.ChatResponse, error) {
	s.mu.Lock()
	s.calls++
	delay := s.delay
	s.mu.Unlock()
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}
	return &llm.ChatResponse{Message: llm.CompletionMessage{Role: "assistant", Content: "on it"}}, nil
}

func (s *slowChat) set(delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = delay
}

func (s *slowChat) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
	chat *slowChat
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "agents.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	chat := &slowChat{}
	// No resolver: no BYO gateway, as Swift without a SecretBox.
	m := agents.New(pool, agents.Options{
		Identify: auth.Identify(keys, auth.PGUsers(pool)), Limits: ratelimit.NewStore(), Chat: chat,
		KeepAlive: time.Hour,
	})
	return &env{t: t, pool: pool, chat: chat, e: httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{m}})}
}

type resp struct {
	code int
	ct   string
	body string
}

func (v *env) request(user uuid.UUID, method, path, body string) *http.Request {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req
}

func (v *env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, v.request(user, method, path, body))
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()}
}

func decode[T any](t *testing.T, r resp) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal([]byte(r.body), &out), r.body)
	return out
}

// MARK: - Agents/AgentsControllerTests.swift (1)

func TestAUserSeesOnlyTheirOwnCentralSessionsAndLogs(t *testing.T) {
	v := setup(t)

	r := v.do(ann, "GET", "/v1/agents/instances", "")
	require.Equal(t, 200, r.code)
	instances := decode[agents.AgentInstancesResponse](t, r)
	require.Len(t, instances.Instances, 1)
	require.Equal(t, "central", instances.Instances[0].Id)

	r = v.do(ben, "GET", "/v1/agents/sessions", "")
	require.Equal(t, 200, r.code)
	var sessions struct {
		Sessions []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Source   string `json:"source"`
			IsActive bool   `json:"isActive"`
		} `json:"sessions"`
		Errors []any `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &sessions))
	require.Len(t, sessions.Sessions, 1)
	require.Equal(t, "Ben plans", sessions.Sessions[0].Title)
	require.Equal(t, "app", sessions.Sessions[0].Source)
	require.False(t, sessions.Sessions[0].IsActive, "updated in February, not in the last five minutes")
	require.Empty(t, sessions.Errors)

	// A profile filter can only match a Hermes profile, never central.
	r = v.do(ann, "GET", "/v1/agents/sessions?profile=default", "")
	require.JSONEq(t, `{"sessions":[],"errors":[]}`, r.body)

	log := "/v1/agents/instances/central/sessions/" + annPlans + "/messages"
	r = v.do(ann, "GET", log, "")
	require.Equal(t, 200, r.code)
	require.Equal(t, annPlans, decode[agents.AgentSessionMessagesResponse](t, r).SessionID)
	// Ben cannot read Ann's log by guessing its id.
	require.Equal(t, 404, v.do(ben, "GET", log, "").code)
	// Lowercase ids parse as UUID(uuidString:) does; a central id that is no UUID is
	// simply not found.
	require.Equal(t, 200, v.do(ann, "GET", strings.ToLower(log), "").code)
	require.Equal(t, 404, v.do(ann, "GET", "/v1/agents/instances/central/sessions/nope/messages", "").code)

	// No BYO gateway configured: the byo instance does not exist.
	require.Equal(t, 404, v.do(ann, "GET", "/v1/agents/instances/byo/sessions/s1/messages", "").code)
	require.Equal(t, 400, v.do(ann, "GET", "/v1/agents/sessions?instance=nope", "").code)
	require.Equal(t, 401, v.do(uuid.Nil, "GET", "/v1/agents/sessions", "").code)
}

// MARK: - Agents/AgentRoomsControllerTests.swift (2)

func TestARoomSeatsOnlyTheCallersOwnPersonasAndOnlyTheCallerCanOpenIt(t *testing.T) {
	v := setup(t)

	r := v.do(ann, "GET", "/v1/agents/room-candidates", "")
	require.Equal(t, 200, r.code)
	var candidates struct {
		Candidates []struct {
			InstanceID string  `json:"instanceID"`
			Profile    *string `json:"profile"`
		} `json:"candidates"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &candidates))
	profiles := map[string]bool{}
	for _, c := range candidates.Candidates {
		require.NotEqual(t, "byo", c.InstanceID)
		if c.Profile != nil {
			profiles[*c.Profile] = true
		}
	}
	require.True(t, profiles["research"] && profiles["ops"])

	r = v.do(ann, "POST", "/v1/agents/rooms", `{"title":"Trip planning","members":[
	  {"instanceID":"central","profile":"research","respondMode":"every_human_message"},
	  {"instanceID":"central","profile":"ops"}]}`)
	require.Equal(t, 200, r.code, r.body)
	room := decode[agents.AgentRoomDetailResponse](t, r).Room
	handles, modes := []string{}, []agents.AgentRoomRespondMode{}
	for _, m := range room.Members {
		handles = append(handles, m.Handle)
		modes = append(modes, m.RespondMode)
	}
	require.Equal(t, []string{"research", "ops"}, handles)
	require.Equal(t, []agents.AgentRoomRespondMode{"every_human_message", "mention"}, modes)
	require.Zero(t, room.SpentTokens)

	// Ben cannot seat Ann's persona, open, post to, stop or delete her room.
	require.Equal(t, 400, v.do(ben, "POST", "/v1/agents/rooms", `{"title":"x","members":[{"instanceID":"central","profile":"research"}]}`).code)
	path := "/v1/agents/rooms/" + room.Id.String()
	require.Equal(t, 404, v.do(ben, "GET", path, "").code)
	require.Equal(t, 404, v.do(ben, "POST", path+"/messages", `{"body":"hi"}`).code)
	require.Equal(t, 404, v.do(ben, "POST", path+"/stop", "").code)
	require.Equal(t, 404, v.do(ben, "DELETE", path, "").code)

	// Ann sees it, can stop it, and an empty post is refused before any agent runs.
	r = v.do(ann, "GET", "/v1/agents/rooms", "")
	rooms := decode[agents.AgentRoomsResponse](t, r).Rooms
	require.Len(t, rooms, 1)
	require.Equal(t, room.Id, rooms[0].Id)
	require.Equal(t, 202, v.do(ann, "POST", path+"/stop", "").code)
	require.Equal(t, 400, v.do(ann, "POST", path+"/messages", `{"body":"   "}`).code)
	require.Equal(t, 204, v.do(ann, "DELETE", path, "").code)
	require.Equal(t, 404, v.do(ann, "GET", path, "").code)
	require.Zero(t, v.chat.count())
}

func TestInvalidRoomsAreRefused(t *testing.T) {
	v := setup(t)
	for _, body := range []string{
		`{"title":"","members":[{"instanceID":"central","profile":"default"}]}`,
		`{"title":"No one","members":[]}`,
		`{"title":"Ghost","members":[{"instanceID":"byo"}]}`,
	} {
		require.Equal(t, 400, v.do(cy, "POST", "/v1/agents/rooms", body).code, body)
	}
}

// Beyond the Swift files: the stream's frames, and a second post while a chain runs.

func TestPostStreamsTheChainAndASecondPostIsBusy(t *testing.T) {
	v := setup(t)
	r := v.do(cy, "POST", "/v1/agents/rooms", `{"title":"Desk","members":[{"instanceID":"central","profile":"stocks"}]}`)
	require.Equal(t, 200, r.code, r.body)
	path := "/v1/agents/rooms/" + decode[agents.AgentRoomDetailResponse](t, r).Room.Id.String() + "/messages"

	v.chat.set(300 * time.Millisecond)
	srv := httptest.NewServer(v.e)
	t.Cleanup(srv.Close)
	first := v.request(cy, "POST", srv.URL+path, `{"body":"@stocks quote"}`)
	first.RequestURI = ""
	res, err := http.DefaultClient.Do(first)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "text/event-stream", res.Header.Get("Content-Type"))

	// The chain is thinking: the room is busy.
	time.Sleep(100 * time.Millisecond)
	busy := v.do(cy, "POST", path, `{"body":"again"}`)
	require.Equal(t, 409, busy.code)
	require.JSONEq(t, `{"error":{"message":"room_busy"}}`, busy.body)

	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	var kinds []string
	for _, block := range strings.Split(strings.TrimSpace(string(raw)), "\n\n") {
		lines := strings.Split(block, "\n")
		require.Equal(t, "event: agent.room", lines[0], block)
		var ev agents.AgentRoomStreamEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &ev))
		kinds = append(kinds, string(ev.Kind))
		if ev.Kind == agents.Done {
			require.Equal(t, agents.Idle, *ev.Reason)
		}
	}
	require.Equal(t, []string{"message", "thinking", "message", "done"}, kinds)
	require.Equal(t, 1, v.chat.count())

	// Free again once the chain is done.
	v.chat.set(0)
	require.Equal(t, 200, v.do(cy, "POST", path, `{"body":"thanks"}`).code)
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	r := v.do(cy, "POST", "/v1/agents/rooms", `{"title":"Spec","members":[{"instanceID":"central","profile":"stocks","displayName":"S"}]}`)
	require.Equal(t, 200, r.code, r.body)
	room := "/v1/agents/rooms/" + decode[agents.AgentRoomDetailResponse](t, r).Room.Id.String()

	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
	}{
		{ann, "GET", "/v1/agents/instances", ""},
		{ann, "GET", "/v1/agents/sessions?live=1&limit=2", ""},
		{ann, "GET", "/v1/agents/instances/central/sessions/" + annPlans + "/messages", ""},
		{ann, "GET", "/v1/agents/room-candidates", ""},
		{cy, "GET", "/v1/agents/rooms", ""},
		{cy, "POST", "/v1/agents/rooms", `{"title":"Two","members":[{"instanceID":"central","profile":"default"}]}`},
		{cy, "GET", room, ""},
		{cy, "GET", "/v1/agents/rooms/" + uuid.NewString(), ""},
		{cy, "POST", "/v1/agents/rooms", `{"title":"","members":[]}`},
	} {
		got := v.do(c.user, c.method, c.path, c.body)
		req := v.request(c.user, c.method, c.path, c.body)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: got.code, Header: http.Header{"Content-Type": {got.ct}}}
		out.SetBodyBytes([]byte(got.body))
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, got.body)
	}
}
