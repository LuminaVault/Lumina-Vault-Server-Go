package agents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/dbtest"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/modules/agents/db"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// MARK: - Agents/AgentGatewayClientTests.swift (8)

// stubGateway is StubHermesHTTP: canned answers by method and path, and the
// requests it saw. An unknown route is the plain 404 of an older Hermes.
type stubGateway struct {
	mu       sync.Mutex
	routes   map[string]stubAnswer
	requests []*http.Request
	bodies   []string
}

type stubAnswer struct {
	status int
	body   string
}

func newStubGateway(t *testing.T) (*stubGateway, *gatewayClient) {
	t.Helper()
	s := &stubGateway{routes: map[string]stubAnswer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, r)
		s.bodies = append(s.bodies, string(body))
		a, ok := s.routes[r.Method+" "+r.URL.EscapedPath()]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	auth := "Bearer sk-test"
	return s, &gatewayClient{baseURL: srv.URL, authHeader: &auth, http: llm.NoRedirectClient(), log: slog.Default()}
}

func (s *stubGateway) respond(method, path string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = stubAnswer{status, body}
}

func (s *stubGateway) seen() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

const twoProfiles = `{"object":"list","has_more":false,"errors":[],"data":[
  {"id":"tg_1","source":"telegram","title":"Groceries","started_at":1790000000.5,
   "last_active":1790000100.0,"message_count":4,"profile":"mac-mcp","is_active":true,
   "estimated_cost_usd":0.002},
  {"id":"cron_1","source":"cron","title":"","preview":"daily brief","started_at":1789990000.0,
   "profile":"default","is_active":false}
]}`

func TestGatewaySessionsFromEveryProfileKeepProfileAndSource(t *testing.T) {
	s, client := newStubGateway(t)
	s.respond("GET", "/api/profiles/sessions", 200, twoProfiles)

	page, err := client.sessions(context.Background(), "byo", nil, nil, 50)
	require.NoError(t, err)
	require.True(t, page.profileAware)
	require.Len(t, page.sessions, 2)
	tg := page.sessions[0]
	require.Equal(t, "tg_1", tg.ID)
	require.Equal(t, "mac-mcp", *tg.Profile)
	require.Equal(t, "telegram", tg.Source)
	require.True(t, tg.IsActive)
	require.Equal(t, 4, *tg.MessageCount)
	require.InDelta(t, 0.002, float64(*tg.CostUSD), 1e-12)
	require.Equal(t, time.Unix(1_790_000_000, 500_000_000), tg.StartedAt.Time)
	// An empty title falls back to the preview.
	require.Equal(t, "daily brief", *page.sessions[1].Title)
	// The gateway key goes on every call, and the limit on the query.
	require.Equal(t, "Bearer sk-test", s.seen()[0].Header.Get("Authorization"))
	require.Equal(t, "limit=50", s.seen()[0].URL.RawQuery)
}

func TestGatewayOlderHermesFallsBackToItsOwnProfile(t *testing.T) {
	s, client := newStubGateway(t)
	// No /api/profiles/sessions route: the stub's plain 404.
	s.respond("GET", "/api/sessions", 200, `{"data":[{"id":"s1","source":"api_server","started_at":1}]}`)

	page, err := client.sessions(context.Background(), "byo", nil, ptr("a b+c&d=e"), 50)
	require.NoError(t, err)
	require.False(t, page.profileAware)
	require.Len(t, page.sessions, 1)
	require.Equal(t, "s1", page.sessions[0].ID)
	require.Nil(t, page.sessions[0].Profile)
	// URLComponents.queryItems: "+" stays, "&" and "=" are escaped.
	require.Equal(t, "limit=50&source=a%20b+c%26d%3De", s.seen()[1].URL.RawQuery)
}

func TestGatewayUnknownProfileIsARealNotFound(t *testing.T) {
	s, client := newStubGateway(t)
	s.respond("GET", "/api/profiles/sessions", 404, `{"error":{"message":"Profile 'x' does not exist.","code":"profile_not_found"}}`)

	_, err := client.sessions(context.Background(), "byo", ptr("x"), nil, 50)
	require.True(t, isFailure(err, failHTTP))
	var g *gatewayError
	require.ErrorAs(t, err, &g)
	require.Equal(t, 404, g.status)
	for _, r := range s.seen() {
		require.NotContains(t, r.URL.Path, "/api/sessions")
	}
	require.Equal(t, "limit=50&profile=x", s.seen()[0].URL.RawQuery)
}

func TestGatewayMessagesFromTheNamedProfileToolCallsAsJSON(t *testing.T) {
	s, client := newStubGateway(t)
	s.respond("GET", "/api/profiles/mac-mcp/sessions/tg_1/messages", 200, `{"session_id":"tg_1","data":[
	  {"role":"user","content":"add milk","timestamp":1790000000.0},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"reminder_create"}}]},
	  {"role":"tool","content":"{\"status\":\"ok\"}","tool_name":"reminder_create"},
	  {"content":"no role","tool_calls":"raw text"}
	]}`)

	id, messages, err := client.messages(context.Background(), ptr("mac-mcp"), "tg_1")
	require.NoError(t, err)
	require.Equal(t, "tg_1", id)
	roles := []string{}
	for _, m := range messages {
		roles = append(roles, m.Role)
	}
	require.Equal(t, []string{"user", "assistant", "tool", "unknown"}, roles)
	require.Contains(t, *messages[1].ToolCalls, "reminder_create")
	require.Nil(t, messages[1].Content)
	require.Equal(t, "reminder_create", *messages[2].ToolName)
	require.Equal(t, "raw text", *messages[3].ToolCalls)
}

func TestGatewayIDsThatCouldLeaveTheRouteAreRefused(t *testing.T) {
	s, client := newStubGateway(t)
	for _, bad := range []string{"../secrets", "a/b", "..", "", "x\n", ".", `a\b`, "a\u200eb", strings.Repeat("x", 257)} {
		_, _, err := client.messages(context.Background(), nil, bad)
		require.True(t, isFailure(err, failInvalidID), "%q", bad)
		_, _, err = client.messages(context.Background(), &bad, "ok")
		require.True(t, isFailure(err, failInvalidID), "%q", bad)
		_, _, err = client.chat(context.Background(), bad, "t", "s", "m")
		require.True(t, isFailure(err, failInvalidID), "%q", bad)
	}
	require.Empty(t, s.seen())

	// What passes is percent-encoded as one segment: "%", "?" and "#" too.
	seg, err := pathSegment("a%2F?#é;=")
	require.NoError(t, err)
	require.Equal(t, "a%252F%3F%23%C3%A9;=", seg)
}

func TestGatewayInstanceReportsProfilesAndAnOlderHermesSaysSo(t *testing.T) {
	s, client := newStubGateway(t)
	s.respond("GET", "/api/instance", 200, `{"hostname":"hermes-vps-2","version":"0.18.2","profiles":["default","mac-mcp"]}`)
	info, err := client.instance(context.Background())
	require.NoError(t, err)
	require.Equal(t, "hermes-vps-2", *info.hostname)
	require.Equal(t, "0.18.2", *info.version)
	require.Equal(t, []string{"default", "mac-mcp"}, info.profiles)
	require.Nil(t, info.activeProfile)

	_, older := newStubGateway(t)
	_, err = older.instance(context.Background())
	require.True(t, isFailure(err, failNotSupported))
}

func TestGatewayOnlyConnectedPlatformsAreListed(t *testing.T) {
	s, client := newStubGateway(t)
	s.respond("GET", "/health/detailed", 200, `{"platforms":{"telegram":{"state":"connected"},"discord":{"state":"error"},"whatsapp":{},"sms":"x"}}`)
	require.Equal(t, []string{"sms", "telegram", "whatsapp"}, client.connectedPlatforms(context.Background()))
}

func TestGatewayUnreachableIsReportedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	client := &gatewayClient{baseURL: srv.URL, http: llm.NoRedirectClient(), log: slog.Default()}
	_, err := client.instance(context.Background())
	require.True(t, isFailure(err, failUnreachable))
	require.Equal(t, "Your Hermes did not answer.", describe(err))
}

// Beyond the Swift file: redirects are answered as they are, an oversized body is
// unreachable, and chat creates its session first, accepting a 409.
func TestGatewayTransportEdges(t *testing.T) {
	s, client := newStubGateway(t)
	s.respond("GET", "/api/instance", 302, ``)
	_, err := client.instance(context.Background())
	require.Equal(t, "Your Hermes answered HTTP 302.", describe(err))

	s.respond("GET", "/api/instance", 200, `{"hostname":"`+strings.Repeat("x", gatewayBodyCap)+`"}`)
	_, err = client.instance(context.Background())
	require.True(t, isFailure(err, failUnreachable))

	s.respond("GET", "/api/instance", 401, `{}`)
	_, err = client.instance(context.Background())
	require.Equal(t, "Your Hermes rejected the saved key.", describe(err))

	s.respond("POST", "/api/sessions", 409, `{}`)
	s.respond("POST", "/api/sessions/room-1/chat", 200, `{"message":{"content":"hi"},"usage":{"input_tokens":3,"output_tokens":4}}`)
	text, tokens, err := client.chat(context.Background(), "room-1", "Room: x", "sys", "msg")
	require.NoError(t, err)
	require.Equal(t, "hi", text)
	require.Equal(t, 7, *tokens)
	require.JSONEq(t, `{"id":"room-1","title":"Room: x"}`, s.bodies[len(s.bodies)-2])
	require.JSONEq(t, `{"message":"msg","system_message":"sys"}`, s.bodies[len(s.bodies)-1])

	s.respond("POST", "/api/sessions", 500, `{}`)
	_, _, err = client.chat(context.Background(), "room-1", "Room: x", "sys", "msg")
	require.Equal(t, "it answered HTTP 500", describeTurnError(err))

	s.respond("POST", "/api/sessions", 201, `{}`)
	s.respond("POST", "/api/sessions/room-1/chat", 200, `{"message":{"content":3}}`)
	_, _, err = client.chat(context.Background(), "room-1", "Room: x", "sys", "msg")
	require.True(t, isFailure(err, failInvalidResponse))
	require.Equal(t, "the call failed", describeTurnError(err))
}

// MARK: - Agents/AgentRoomTurnPolicyTests.swift (6)

var (
	research = uuid.New()
	ops      = uuid.New()
	opsbot   = uuid.New()
	members  = []policyMember{
		{id: research, handle: "research", respondMode: modeEveryMessage},
		{id: ops, handle: "ops", respondMode: modeMention},
		{id: opsbot, handle: "opsbot", respondMode: modeMention},
	}
)

func TestMentionsComeBackInWrittenOrder(t *testing.T) {
	require.Equal(t, []uuid.UUID{ops, research}, mentions("@ops look at this, then @research", members))
}

func TestHandleDoesNotMatchInsideALongerOne(t *testing.T) {
	require.Equal(t, []uuid.UUID{opsbot}, mentions("ask @opsbot", members))
	require.Equal(t, []uuid.UUID{opsbot, ops}, mentions("ask @opsbot and @ops.", members))
	require.Empty(t, mentions("email ops@example.com", members))
	require.Equal(t, []uuid.UUID{ops}, mentions("@OPS", members))
	require.Empty(t, mentions("@opsé", members))
	// A needle ending inside a Character is no match there.
	require.Empty(t, mentions("@opś", members))
}

func TestUnaddressedMessageGoesToMembersSetToAnswerEverything(t *testing.T) {
	require.Equal(t, []uuid.UUID{research}, firstResponders("morning all", members))
	// Naming someone overrides the standing responders.
	require.Equal(t, []uuid.UUID{ops}, firstResponders("@ops only you", members))
}

func TestReplyNeverQueuesItsSpeakerOrAMemberTwice(t *testing.T) {
	queue := enqueueHandovers("@research here, over to @ops and @opsbot, @research again", research, members, []uuid.UUID{ops})
	require.Equal(t, []uuid.UUID{ops, opsbot}, queue)
}

func TestHandlesAreDerivedAndValidated(t *testing.T) {
	require.Equal(t, "mac-mcp", handleFrom("Mac MCP"))
	require.Equal(t, "stocks-news", handleFrom("  Stocks & News!! "))
	require.Equal(t, "agent", handleFrom("✨"))
	require.Equal(t, "a.b", strings.ReplaceAll(handleFrom("a..b"), "-", "."))
	require.Equal(t, "x_", handleFrom("_-x_-"))
	require.Equal(t, strings.Repeat("a", 31), handleFrom(strings.Repeat("A", 40)))
	require.True(t, isValidHandle("mac-mcp"))
	require.False(t, isValidHandle("Mac"))
	require.False(t, isValidHandle("-ops"))
	require.False(t, isValidHandle(""))
	require.False(t, isValidHandle(strings.Repeat("a", 32)))
	require.True(t, isValidHandle(strings.Repeat("a", 31)))
}

func TestMembersMustBeSeatableWithDistinctHandles(t *testing.T) {
	available := []candidate{
		{InstanceID: "central", Profile: ptr("default"), DisplayName: "Lumina", SuggestedHandle: "default"},
		{InstanceID: "central", Profile: ptr("stocks"), DisplayName: "Stocks", SuggestedHandle: "stocks"},
		{InstanceID: "byo", DisplayName: "Hermes · mac-mcp", SuggestedHandle: "mac-mcp"},
	}
	every := modeEveryMessage
	seated, err := resolveMembers([]memberRequest{
		{InstanceID: "central", Profile: ptr("stocks"), RespondMode: &every},
		{InstanceID: "byo", Handle: ptr("vps"), DisplayName: ptr("My VPS")},
	}, available)
	require.NoError(t, err)
	require.Equal(t, "stocks", seated[0].handle)
	require.Equal(t, "vps", seated[1].handle)
	require.Equal(t, "Stocks", seated[0].displayName)
	require.Equal(t, "My VPS", seated[1].displayName)
	require.Equal(t, modeEveryMessage, seated[0].respondMode)
	require.Equal(t, modeMention, seated[1].respondMode)

	for name, req := range map[string][]memberRequest{
		"unknown_member":   {{InstanceID: "central", Profile: ptr("someone-elses")}},
		"duplicate_handle": {{InstanceID: "central", Profile: ptr("default"), Handle: ptr("x")}, {InstanceID: "central", Profile: ptr("stocks"), Handle: ptr("x")}},
		"duplicate_member": {{InstanceID: "byo", Handle: ptr("a")}, {InstanceID: "byo", Handle: ptr("b")}},
		"invalid_handle":   {{InstanceID: "central", Profile: ptr("default"), Handle: ptr("Bad Handle")}},
	} {
		_, err := resolveMembers(req, available)
		require.EqualError(t, err, name)
	}
}

// MARK: - Agents/AgentRoomOrchestratorTests.swift (6)

// scriptedSpeaker replies from a script keyed by handle and records who was asked.
type scriptedSpeaker struct {
	mu     sync.Mutex
	asked  []string
	last   map[string]string
	tokens *int
	onAsk  func(room db.AgentRoom)
	script func(handle string) (string, error)
}

func (s *scriptedSpeaker) reply(_ context.Context, _ uuid.UUID, room db.AgentRoom, member db.AgentRoomMember, _, message string) (string, *int, error) {
	s.mu.Lock()
	s.asked = append(s.asked, member.Handle)
	if s.last == nil {
		s.last = map[string]string{}
	}
	s.last[member.Handle] = message
	s.mu.Unlock()
	if s.onAsk != nil {
		s.onAsk(room)
	}
	text, err := s.script(member.Handle)
	return text, s.tokens, err
}

type roomSeat struct {
	handle string
	mode   respondMode
}

func makeRoom(t *testing.T, pool *pgxpool.Pool, budget int64, seats ...roomSeat) db.AgentRoom {
	t.Helper()
	ctx := context.Background()
	q := db.New(pool)
	now := time.Now()
	room := db.AgentRoom{ID: uuid.New(), TenantID: uuid.New(), Title: "Planning", TokenBudget: budget, CreatedAt: &now, UpdatedAt: &now}
	require.NoError(t, q.InsertRoom(ctx, db.InsertRoomParams{ID: room.ID, TenantID: room.TenantID, Title: room.Title, TokenBudget: budget, CreatedAt: &now, UpdatedAt: &now}))
	for i, s := range seats {
		at := now.Add(time.Duration(i+1) * time.Millisecond)
		require.NoError(t, q.InsertMember(ctx, db.InsertMemberParams{
			ID: uuid.New(), RoomID: room.ID, InstanceID: centralID, Profile: ptr(s.handle), Handle: s.handle,
			DisplayName: strings.ToUpper(s.handle[:1]) + s.handle[1:], RespondMode: string(s.mode), CreatedAt: &at,
		}))
	}
	return room
}

func roomMessages(t *testing.T, pool *pgxpool.Pool, room uuid.UUID) []db.AgentRoomMessage {
	t.Helper()
	rows, err := db.New(pool).LatestMessages(context.Background(), room)
	require.NoError(t, err)
	out := make([]db.AgentRoomMessage, len(rows))
	for i := range rows {
		out[len(rows)-1-i] = rows[i]
	}
	return out
}

func newOrchestrator(pool *pgxpool.Pool, s speaker) *orchestrator {
	return &orchestrator{q: db.New(pool), speaker: s, registry: newRunRegistry(), log: slog.Default(), now: time.Now}
}

type collected struct {
	mu     sync.Mutex
	events []AgentRoomStreamEvent
}

func (c *collected) add(e AgentRoomStreamEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func TestOrchestrator(t *testing.T) {
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	ctx := context.Background()

	t.Run("two agents handing over forever stop at the turn cap", func(t *testing.T) {
		room := makeRoom(t, pool, 200_000, roomSeat{"alpha", modeMention}, roomSeat{"beta", modeMention})
		ten := 10
		s := &scriptedSpeaker{tokens: &ten, script: func(h string) (string, error) {
			if h == "alpha" {
				return "over to @beta", nil
			}
			return "back to @alpha", nil
		}}
		events := &collected{}
		reason, err := newOrchestrator(pool, s).post(ctx, room.TenantID, room, "@alpha start", events.add)
		require.NoError(t, err)
		require.Equal(t, TurnCap, reason)
		require.Equal(t, []string{"alpha", "beta", "alpha", "beta", "alpha", "beta"}, s.asked)
		rows := roomMessages(t, pool, room.ID)
		require.Equal(t, authorHuman, rows[0].AuthorKind)
		agents := 0
		for _, r := range rows {
			if r.AuthorKind == authorAgent {
				agents++
			}
		}
		require.Equal(t, maxAgentTurns, agents)
		require.Equal(t, authorSystem, rows[len(rows)-1].AuthorKind)
		require.Contains(t, rows[len(rows)-1].Body, "Paused after 6")
		thinking := 0
		for _, e := range events.events {
			if e.Kind == Thinking {
				thinking++
			}
		}
		require.Equal(t, 6, thinking)
		stored, err := db.New(pool).FindRoom(ctx, room.ID)
		require.NoError(t, err)
		require.Equal(t, int64(60), stored.SpentTokens)
	})

	t.Run("nobody answers twice in a row and an unaddressed reply ends the chain", func(t *testing.T) {
		room := makeRoom(t, pool, 200_000, roomSeat{"alpha", modeEveryMessage}, roomSeat{"beta", modeMention})
		ten := 10
		s := &scriptedSpeaker{tokens: &ten, script: func(h string) (string, error) {
			if h == "alpha" {
				return "I, @alpha, ask @beta", nil
			}
			return "done", nil
		}}
		reason, err := newOrchestrator(pool, s).post(ctx, room.TenantID, room, "hello", func(AgentRoomStreamEvent) {})
		require.NoError(t, err)
		require.Equal(t, Idle, reason)
		require.Equal(t, []string{"alpha", "beta"}, s.asked)
		// beta's first turn sees the user's message and alpha's reply.
		require.Equal(t, "User: hello\n\n@alpha: I, @alpha, ask @beta", s.last["beta"])
	})

	t.Run("a spent budget stops the chain before the next call", func(t *testing.T) {
		room := makeRoom(t, pool, 100, roomSeat{"alpha", modeMention}, roomSeat{"beta", modeMention})
		n := 150
		s := &scriptedSpeaker{tokens: &n, script: func(h string) (string, error) {
			if h == "alpha" {
				return "@beta", nil
			}
			return "@alpha", nil
		}}
		reason, err := newOrchestrator(pool, s).post(ctx, room.TenantID, room, "@alpha go", func(AgentRoomStreamEvent) {})
		require.NoError(t, err)
		require.Equal(t, Budget, reason)
		require.Equal(t, []string{"alpha"}, s.asked)
		stored, err := db.New(pool).FindRoom(ctx, room.ID)
		require.NoError(t, err)
		require.Equal(t, int64(150), stored.SpentTokens)
	})

	t.Run("stop ends the chain after the turn in flight", func(t *testing.T) {
		room := makeRoom(t, pool, 200_000, roomSeat{"alpha", modeMention}, roomSeat{"beta", modeMention})
		s := &scriptedSpeaker{
			// The user presses Stop while the first agent is thinking.
			onAsk: func(r db.AgentRoom) {
				now := time.Now()
				require.NoError(t, db.New(pool).SetStopRequested(ctx, db.SetStopRequestedParams{ID: r.ID, StopRequestedAt: &now, UpdatedAt: &now}))
			},
			script: func(h string) (string, error) {
				if h == "alpha" {
					return "@beta", nil
				}
				return "@alpha", nil
			},
		}
		reason, err := newOrchestrator(pool, s).post(ctx, room.TenantID, room, "@alpha go", func(AgentRoomStreamEvent) {})
		require.NoError(t, err)
		require.Equal(t, Stopped, reason)
		require.Equal(t, []string{"alpha"}, s.asked)
		rows := roomMessages(t, pool, room.ID)
		require.Equal(t, "Stopped.", rows[len(rows)-1].Body)
		// No token count from the agent: a quarter of the characters, at least 1.
		stored, err := db.New(pool).FindRoom(ctx, room.ID)
		require.NoError(t, err)
		require.Equal(t, int64(max(1, (len("User: @alpha go")+len("@beta"))/4)), stored.SpentTokens)
	})

	t.Run("a failing agent is reported and the others still answer", func(t *testing.T) {
		room := makeRoom(t, pool, 200_000, roomSeat{"alpha", modeMention}, roomSeat{"beta", modeMention})
		s := &scriptedSpeaker{script: func(h string) (string, error) {
			if h == "alpha" {
				return "", errors.New("boom")
			}
			return "beta here", nil
		}}
		events := &collected{}
		reason, err := newOrchestrator(pool, s).post(ctx, room.TenantID, room, "@alpha and @beta", events.add)
		require.NoError(t, err)
		require.Equal(t, Idle, reason)
		require.Equal(t, []string{"alpha", "beta"}, s.asked)
		rows := roomMessages(t, pool, room.ID)
		require.Equal(t, "@alpha could not answer: the call failed", rows[1].Body)
		require.Equal(t, authorSystem, rows[1].AuthorKind)
		require.NotNil(t, rows[1].MemberID)
		require.Equal(t, "beta here", rows[len(rows)-1].Body)
		// The note names the member on the frame too.
		require.NotNil(t, events.events[2].MemberID)
	})

	t.Run("a second chain in the same room is refused while one runs", func(t *testing.T) {
		room := makeRoom(t, pool, 200_000, roomSeat{"alpha", modeMention})
		o := newOrchestrator(pool, &scriptedSpeaker{script: func(string) (string, error) { return "hi", nil }})
		require.True(t, o.registry.begin(room.ID))
		_, err := o.post(ctx, room.TenantID, room, "@alpha", func(AgentRoomStreamEvent) {})
		require.ErrorIs(t, err, errAlreadyRunning)
		_, err = o.post(ctx, room.TenantID, room, "   ", func(AgentRoomStreamEvent) {})
		require.ErrorIs(t, err, errEmptyMessage)
	})

	// Beyond the Swift file: a cancelled chain stops before the next turn with no note.
	t.Run("leaving the stream stops the chain before the next turn", func(t *testing.T) {
		room := makeRoom(t, pool, 200_000, roomSeat{"alpha", modeMention}, roomSeat{"beta", modeMention})
		cctx, cancel := context.WithCancel(ctx)
		s := &scriptedSpeaker{onAsk: func(db.AgentRoom) { cancel() }, script: func(h string) (string, error) { return "@beta", nil }}
		reason, err := newOrchestrator(pool, s).post(cctx, room.TenantID, room, "@alpha go", func(AgentRoomStreamEvent) {})
		require.NoError(t, err)
		require.Equal(t, Stopped, reason)
		require.Equal(t, []string{"alpha"}, s.asked)
		rows := roomMessages(t, pool, room.ID)
		require.Equal(t, authorAgent, rows[len(rows)-1].AuthorKind, "the reply in flight is still saved")
	})

	// LiveAgentRoomSpeaker's central branch: the session key, the session id and the
	// two messages sent to the routed chat service.
	t.Run("the central agent speaks through the routed chat service", func(t *testing.T) {
		q := db.New(pool)
		user := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, created_at, updated_at)
			VALUES ($1, 'live@test', 'x', true, 'live', 'pro', 'none', now(), now())`, user)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status) VALUES ($1, $2, 'hp-1', 'ready')`, uuid.New(), user)
		require.NoError(t, err)
		chat := &fakeChat{reply: "hello", total: ptr(42)}
		sp := liveSpeaker{q: q, llm: chat, agents: &service{log: slog.Default()}}
		room := db.AgentRoom{ID: uuid.MustParse("AAAAAAAA-0000-4000-8000-000000000001"), Title: "T"}
		member := db.AgentRoomMember{ID: uuid.MustParse("BBBBBBBB-0000-4000-8000-000000000002"), InstanceID: centralID, Profile: ptr("stocks")}
		text, tokens, err := sp.reply(ctx, user, room, member, "sys", "msg")
		require.NoError(t, err)
		require.Equal(t, "hello", text)
		require.Equal(t, 42, *tokens)
		require.Equal(t, "hp-1:stocks", chat.key)
		require.Equal(t, "room-aaaaaaaa-0000-4000-8000-000000000001-bbbbbbbb-0000-4000-8000-000000000002", chat.session)
		require.Equal(t, []llm.CompletionMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "msg"}}, chat.req.Messages)
		require.Nil(t, chat.req.Temperature)

		other := uuid.MustParse("CCCCCCCC-0000-4000-8000-000000000003")
		member.Profile = nil
		_, _, err = sp.reply(ctx, other, room, member, "sys", "msg")
		require.NoError(t, err)
		require.Equal(t, "pending-CCCCCCCC-0000-4000-8000-000000000003:default", chat.key)

		member.InstanceID = byoID
		_, _, err = sp.reply(ctx, other, room, member, "sys", "msg")
		require.ErrorIs(t, err, errNoGateway)
		require.Equal(t, "your Hermes is no longer linked", describeTurnError(err))
		member.InstanceID = "elsewhere"
		_, _, err = sp.reply(ctx, other, room, member, "sys", "msg")
		require.Equal(t, "the call failed", describeTurnError(err))
	})
}

type fakeChat struct {
	key, session string
	req          llm.CompletionRequest
	reply        string
	total        *int
}

func (f *fakeChat) Chat(_ context.Context, key, session string, req llm.CompletionRequest) (*llm.ChatResponse, error) {
	f.key, f.session, f.req = key, session, req
	return &llm.ChatResponse{Message: llm.CompletionMessage{Role: "assistant", Content: f.reply}, Raw: llm.UpstreamResponse{Usage: &llm.UpstreamUsage{TotalTokens: f.total}}}, nil
}

func TestPreamble(t *testing.T) {
	room := db.AgentRoom{Title: "Trip"}
	a := db.AgentRoomMember{ID: uuid.New(), Handle: "alpha", DisplayName: "Alpha"}
	b := db.AgentRoomMember{ID: uuid.New(), Handle: "beta", DisplayName: "Beta B"}
	require.Equal(t, `You are Alpha, @alpha, in a LuminaVault room called "Trip" with the user and these agents: @beta (Beta B). `+
		`The message below is what was said in the room since you last spoke. Reply as yourself only, and keep it short. `+
		`Do not write lines for the user or the other agents. To hand the conversation to another agent, write their @handle; `+
		`otherwise do not mention them.`, preamble(room, a, []db.AgentRoomMember{a, b}))
	require.Contains(t, preamble(room, a, []db.AgentRoomMember{a}), `called "Trip" with the user. The message`)
}
