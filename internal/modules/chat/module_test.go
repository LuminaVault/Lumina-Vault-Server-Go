package chat

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/ratelimit"
	"lumina/internal/wire"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports Tests/AppTests/Chat/ConversationE2ETests.swift, ConversationTests.swift,
// ConversationEscalationE2ETests.swift, Trace/AgentTurnTraceRoundTripTests.swift and
// the chat half of Usability/UsabilityEndpointsTests.swift.

var testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// stubStream yields scripted tokens, or fails; it records the last request.
type stubStream struct {
	mu     sync.Mutex
	tokens []string
	fail   error
	last   llm.ChatRequest
}

func (s *stubStream) ChatStream(_ context.Context, _, _ string, req llm.ChatRequest) llm.Stream {
	s.mu.Lock()
	s.last = req
	s.mu.Unlock()
	return func(yield func(llm.Chunk, error) bool) {
		if s.fail != nil {
			yield(llm.Chunk{}, s.fail)
			return
		}
		for _, token := range s.tokens {
			if !yield(llm.Chunk{Delta: token}, nil) {
				return
			}
		}
	}
}

// noTransport fails every call, so follow-ups come back empty.
type noTransport struct{}

func (noTransport) ChatCompletions(context.Context, []byte, string, string) (llm.Metadata, error) {
	return llm.Metadata{}, errors.New("no transport in tests")
}

func (noTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{}, errors.New("no transport in tests")) }
}

// fakeRuns records what an escalation asked for.
type fakeRuns struct {
	mu     sync.Mutex
	starts []RunStart
	fail   error
}

func (f *fakeRuns) Start(_ context.Context, req RunStart) (RunRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, req)
	if f.fail != nil {
		return RunRef{}, f.fail
	}
	return RunRef{ID: uuid.New(), StartedAt: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}, nil
}

type env struct {
	t      *testing.T
	e      *echo.Echo
	pool   *pgxpool.Pool
	stream *stubStream
}

func newEnv(t *testing.T, runs HermesRuns) env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	users := auth.PGUsers(pool)
	stream := &stubStream{tokens: []string{"Hello", " there"}}
	module := New(pool, Options{
		Identify: auth.Identify(testKeys, users), Limits: ratelimit.NewStore(),
		Stream:       stream,
		FollowUps:    &memoryagent.FollowUpGenerator{Transport: noTransport{}, DefaultModel: "test"},
		Memories:     memorystore.New(pool, nil),
		Embeddings:   embedding.Deterministic{},
		Preferences:  llm.NewPreferenceRepository(pool),
		Traces:       llm.PGMeters{Pool: pool}.TraceRecorder(),
		Runs:         runs,
		DefaultModel: "test", HybridExecutionEnabled: true, LocalExecutionToolBrokerEnabled: true,
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(testKeys, users), Modules: []httpserver.Module{module}})
	return env{t: t, e: e, pool: pool, stream: stream}
}

func (v env) tenant() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	short := strings.ToLower(id.String()[:8])
	ctx := context.Background()
	_, err := v.pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'trial', now(), now())`, id, "chat-"+short+"@test.luminavault", "chat-"+short)
	require.NoError(v.t, err)
	_, err = v.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'test', now(), now())`, id)
	require.NoError(v.t, err)
	return id
}

func (v env) do(user uuid.UUID, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	if user != uuid.Nil {
		token, err := scenario.Mint(testKeys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func (v env) create(user uuid.UUID, body string) ConversationDTO {
	v.t.Helper()
	rec := v.do(user, http.MethodPost, "/v1/conversations", body)
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var dto ConversationDTO
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &dto))
	return dto
}

// frames parses an SSE body into its JSON frames.
func frames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var frame map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &frame), line)
		out = append(out, frame)
	}
	return out
}

func frameTypes(fs []map[string]any) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i], _ = f["type"].(string)
	}
	return out
}

func TestCreateConversationIsOwnedByTheCaller(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"Plans"}`)
	assert.Equal(t, "Plans", dto.Title)
	var owner uuid.UUID
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT tenant_id FROM conversations WHERE id = $1`, dto.ID.UUID).Scan(&owner))
	assert.Equal(t, user, owner)
}

func TestCreateConversationDefaultsABlankTitle(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	assert.Equal(t, "New conversation", v.create(user, `{}`).Title)
}

func TestListConversationsNewestFirst(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	first := v.create(user, `{"title":"first"}`)
	second := v.create(user, `{"title":"second"}`)
	_, err := v.pool.Exec(context.Background(), `UPDATE conversations SET updated_at = now() - interval '1 hour' WHERE id = $1`, first.ID.UUID)
	require.NoError(t, err)
	rec := v.do(user, http.MethodGet, "/v1/conversations", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var list listResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Conversations, 2)
	assert.Equal(t, second.ID, list.Conversations[0].ID)
	assert.Equal(t, first.ID, list.Conversations[1].ID)
}

func TestGetConversationReturnsAnEmptyTranscript(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodGet, "/v1/conversations/"+dto.ID.String(), "")
	require.Equal(t, http.StatusOK, rec.Code)
	var detail detailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	assert.Equal(t, dto.ID, detail.Conversation.ID)
	assert.Empty(t, detail.Messages)
	assert.Contains(t, rec.Body.String(), `"messages":[]`)
}

func TestDeleteConversationCascadesItsMessages(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream", `{"content":"hello"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, http.StatusNoContent, v.do(user, http.MethodDelete, "/v1/conversations/"+dto.ID.String(), "").Code)
	assert.Equal(t, http.StatusNotFound, v.do(user, http.MethodGet, "/v1/conversations/"+dto.ID.String(), "").Code)
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM conversation_messages WHERE conversation_id = $1`, dto.ID.UUID).Scan(&n))
	assert.Zero(t, n)
}

func TestCrossTenantConversationIsNotFound(t *testing.T) {
	v := newEnv(t, nil)
	owner, other := v.tenant(), v.tenant()
	dto := v.create(owner, `{"title":"mine"}`)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/conversations/" + dto.ID.String(), ""},
		{http.MethodGet, "/v1/conversations/" + dto.ID.String() + "/traces", ""},
		{http.MethodDelete, "/v1/conversations/" + dto.ID.String(), ""},
		{http.MethodPost, "/v1/conversations/" + dto.ID.String() + "/messages/stream", `{"content":"hi"}`},
	} {
		assert.Equal(t, http.StatusNotFound, v.do(other, c.method, c.path, c.body).Code, c.method+" "+c.path)
	}
	var list listResponse
	require.NoError(t, json.Unmarshal(v.do(other, http.MethodGet, "/v1/conversations", "").Body.Bytes(), &list))
	assert.Empty(t, list.Conversations)
}

func TestConversationsRequireAuth(t *testing.T) {
	v := newEnv(t, nil)
	for _, path := range []string{"/v1/conversations", "/v1/chat/inbox", "/v1/me/chat-preferences", "/v1/me/preferences/hybrid-execution"} {
		assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, path, "").Code, path)
	}
}

func TestStreamWithEmptyContentIs400(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream", `{"content":"   "}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "content required")
}

func TestStreamOnAnUnknownIDIs404(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+uuid.NewString()+"/messages/stream", `{"content":"hi"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// A classic turn streams tokens, persists both sides and ends with done.
func TestStreamAnswersAndPersistsTheTurn(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream", `{"content":"hello"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	types := frameTypes(frames(t, rec.Body.String()))
	assert.Contains(t, types, "token")
	assert.Equal(t, "done", types[len(types)-1])

	rows, err := v.pool.Query(context.Background(), `SELECT role, content FROM conversation_messages WHERE conversation_id = $1 ORDER BY created_at`, dto.ID.UUID)
	require.NoError(t, err)
	var got []string
	for rows.Next() {
		var role, content string
		require.NoError(t, rows.Scan(&role, &content))
		got = append(got, role+":"+content)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"user:hello", "assistant:Hello there"}, got)

	// The prompt replays the transcript after the grounding system message.
	require.NotEmpty(t, v.stream.last.Messages)
	assert.Equal(t, "system", v.stream.last.Messages[0].Role)
	assert.Equal(t, "hello", v.stream.last.Messages[len(v.stream.last.Messages)-1].Content)
}

// A routing failure reaches the user as its own message, with its ways out.
func TestRoutingFailureReachesTheUserAsItsOwnMessage(t *testing.T) {
	v := newEnv(t, nil)
	v.stream.fail = &llm.FreeLaneUnavailableError{Actions: []string{"add_key", "switch_to_managed"}}
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream", `{"content":"hello"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var errFrame map[string]any
	for _, f := range frames(t, rec.Body.String()) {
		if f["type"] == "error" {
			errFrame = f
			break
		}
	}
	require.NotNil(t, errFrame, rec.Body.String())
	assert.NotEqual(t, "upstream failure", errFrame["payload"])
	assert.NotEmpty(t, errFrame["payload"])
	assert.Equal(t, "free_lane_unavailable", errFrame["code"])
	assert.Equal(t, []any{"add_key", "switch_to_managed"}, errFrame["cta"])
}

// MARK: - escalation

func TestEscalationFallsBackWhenHermesIsAbsent(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream",
		`{"content":"deploy the api","agentMode":"force"}`, "x-lv-client-caps", "chat.hermes_run")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "hermes_run")
	assert.Contains(t, rec.Body.String(), `"token"`)
}

func TestEscalationFallsBackWhenTheRunFailsToStart(t *testing.T) {
	runs := &fakeRuns{fail: errors.New("watcher budget full")}
	v := newEnv(t, runs)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream",
		`{"content":"deploy the api"}`, "x-lv-client-caps", "chat.hermes_run")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "hermes_run")
	assert.Len(t, runs.starts, 1)
}

func TestSilentClientNeverGetsARunPointer(t *testing.T) {
	runs := &fakeRuns{}
	v := newEnv(t, runs)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream", `{"content":"deploy the api","agentMode":"force"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "hermes_run")
	assert.Empty(t, runs.starts)
}

func TestEscalatedTurnIsARunPointerThenDone(t *testing.T) {
	runs := &fakeRuns{}
	v := newEnv(t, runs)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream",
		`{"content":"deploy the api"}`, "x-lv-client-caps", "chat.hermes_run")
	require.Equal(t, http.StatusOK, rec.Code)
	fs := frames(t, rec.Body.String())
	assert.Equal(t, []string{"hermes_run", "done"}, frameTypes(fs))
	require.Len(t, runs.starts, 1)
	assert.Equal(t, "deploy the api", runs.starts[0].Prompt)
	assert.Equal(t, dto.ID.UUID, runs.starts[0].ConversationID)
	// Swift's session key is the vault's UUID string.
	assert.Equal(t, strings.ToUpper(user.String()), runs.starts[0].SessionKey)
	assert.True(t, strings.HasPrefix(runs.starts[0].Authorization, "Bearer "))
}

func TestRequestWithoutNewFieldsStillStreams(t *testing.T) {
	v := newEnv(t, &fakeRuns{})
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	assert.Equal(t, http.StatusOK, v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream", `{"content":"what did I note yesterday?"}`).Code)
}

func TestAgentModeIsCamelCaseOnTheWire(t *testing.T) {
	runs := &fakeRuns{}
	v := newEnv(t, runs)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	// camelCase "off" keeps an action on the classic stream.
	rec := v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream",
		`{"content":"deploy it","agentMode":"off"}`, "x-lv-client-caps", "chat.hermes_run")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, runs.starts)
	// snake_case is an unknown key, so the mode stays auto and the action escalates.
	rec = v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream",
		`{"content":"deploy it","agent_mode":"off"}`, "x-lv-client-caps", "chat.hermes_run")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, runs.starts, 1)
}

// MARK: - ConversationTests

func TestBuildPromptReplaysHistoryWithSystemContextPrepended(t *testing.T) {
	hits := []memorystore.SearchResult{{ID: uuid.New(), Content: "likes tea", Source: "manual"}}
	history := []llm.ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}, {Role: "user", Content: "drink?"}}
	got := BuildPrompt(history, hits, nil, nil, false)
	require.Len(t, got, 4)
	assert.Equal(t, "system", got[0].Role)
	assert.Contains(t, got[0].Content, "[1] [manual] likes tea")
	assert.Equal(t, history, got[1:])
}

func TestBuildPromptWithNoHitsSurfacesFallbackContext(t *testing.T) {
	got := BuildPrompt(nil, nil, nil, nil, false)
	assert.Contains(t, got[0].Content, "(no relevant memories were found)")
}

func TestMessagesKeepRoleContentAndSourceIDs(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	source := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO conversation_messages (id, conversation_id, role, content, source_memory_ids, created_at)
		VALUES ($1, $2, 'assistant', 'from memory', $3, now())`, uuid.New(), dto.ID.UUID, []uuid.UUID{source})
	require.NoError(t, err)
	var detail detailResponse
	require.NoError(t, json.Unmarshal(v.do(user, http.MethodGet, "/v1/conversations/"+dto.ID.String(), "").Body.Bytes(), &detail))
	require.Len(t, detail.Messages, 1)
	assert.Equal(t, "assistant", detail.Messages[0].Role)
	assert.Equal(t, "from memory", detail.Messages[0].Content)
	require.Len(t, detail.Messages[0].SourceMemoryIDs, 1)
	assert.Equal(t, source, detail.Messages[0].SourceMemoryIDs[0].UUID)
}

// MARK: - AgentTurnTraceRoundTripTests

func (v env) message(conversation uuid.UUID) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO conversation_messages (id, conversation_id, role, content, created_at)
		VALUES ($1, $2, 'assistant', 'a', now())`, id, conversation)
	require.NoError(v.t, err)
	return id
}

func (v env) trace(tenant, message uuid.UUID, toolCalls *string) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO agent_turn_traces (tenant_id, conversation_message_id, provider, model, tool_calls, tokens_in, tokens_out)
		VALUES ($1, $2, 'openRouter', 'm', $3::jsonb, 3, 4)`, tenant, message, toolCalls)
	require.NoError(v.t, err)
}

func (v env) traces(user uuid.UUID, conversation wire.UUID) []TraceDTO {
	v.t.Helper()
	rec := v.do(user, http.MethodGet, "/v1/conversations/"+conversation.String()+"/traces", "")
	require.Equal(v.t, http.StatusOK, rec.Code, rec.Body.String())
	var out tracesResponse
	require.NoError(v.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Traces
}

func TestTracesAreScopedToTheirTenant(t *testing.T) {
	v := newEnv(t, nil)
	owner, other := v.tenant(), v.tenant()
	dto := v.create(owner, `{"title":"x"}`)
	msg := v.message(dto.ID.UUID)
	v.trace(owner, msg, nil)
	v.trace(other, msg, nil) // a row for another tenant on the same message stays hidden
	assert.Len(t, v.traces(owner, dto.ID), 1)
}

func TestTraceReportsNamesAndCountsAsRecorded(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	calls := `{"names":["memory_search","web_fetch"],"count":3}`
	v.trace(user, v.message(dto.ID.UUID), &calls)
	got := v.traces(user, dto.ID)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"memory_search", "web_fetch"}, got[0].ToolNames)
	assert.Equal(t, 3, got[0].ToolCallCount)
	assert.Equal(t, int64(3), got[0].TokensIn)
	assert.Equal(t, int64(4), got[0].TokensOut)
}

func TestTraceToolNamesWithoutACountCountThemselves(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	calls := `{"names":["a","b"]}`
	v.trace(user, v.message(dto.ID.UUID), &calls)
	got := v.traces(user, dto.ID)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"a", "b"}, got[0].ToolNames)
	assert.Equal(t, 2, got[0].ToolCallCount)
}

func TestTraceEmptyToolListIsDistinctFromAbsent(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	empty := `{"names":[],"count":0}`
	v.trace(user, v.message(dto.ID.UUID), &empty)
	v.trace(user, v.message(dto.ID.UUID), nil)
	got := v.traces(user, dto.ID)
	require.Len(t, got, 2)
	for _, tr := range got {
		assert.Equal(t, []string{}, tr.ToolNames)
		assert.Zero(t, tr.ToolCallCount)
	}
}

// MARK: - UsabilityEndpointsTests (chat half)

func TestInboxReturnsConversationSummaries(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"Inbox"}`)
	_, err := v.pool.Exec(context.Background(), `INSERT INTO conversation_messages (id, conversation_id, role, content, created_at)
		VALUES ($1, $2, 'user', $3, now())`, uuid.New(), dto.ID.UUID, strings.Repeat("é", 200))
	require.NoError(t, err)
	rec := v.do(user, http.MethodGet, "/v1/chat/inbox", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var inbox inboxResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &inbox))
	require.Len(t, inbox.Items, 1)
	assert.Equal(t, "Inbox", inbox.Items[0].Title)
	assert.Equal(t, 1, inbox.Items[0].MessageCount)
	assert.Equal(t, strings.Repeat("é", 180), inbox.Items[0].Preview)
}

func TestChatPreferencesDefaultAndPersist(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	rec := v.do(user, http.MethodGet, "/v1/me/chat-preferences", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"preferences":{"autoExpandThinking":true,"sendOnReturn":false}}`, rec.Body.String())
	rec = v.do(user, http.MethodPut, "/v1/me/chat-preferences", `{"preferences":{"autoExpandThinking":false,"sendOnReturn":true}}`)
	require.Equal(t, http.StatusOK, rec.Code)
	rec = v.do(user, http.MethodGet, "/v1/me/chat-preferences", "")
	assert.JSONEq(t, `{"preferences":{"autoExpandThinking":false,"sendOnReturn":true}}`, rec.Body.String())
}

func TestHybridPrivateCannotFallBackToCloud(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	rec := v.do(user, http.MethodPut, "/v1/me/preferences/hybrid-execution",
		`{"profile":"private","localFallbackEnabled":true,"cloudFallbackEnabled":true,"syncLocalConversations":true}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = v.do(user, http.MethodPut, "/v1/me/preferences/hybrid-execution",
		`{"profile":"private","localFallbackEnabled":true,"cloudFallbackEnabled":false,"syncLocalConversations":true}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"profile":"private","localFallbackEnabled":true,"cloudFallbackEnabled":false,"syncLocalConversations":false}`, rec.Body.String())
}

// MARK: - local execution

func TestLocalExecutionPrepareToolCommit(t *testing.T) {
	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"x"}`)
	base := "/v1/conversations/" + dto.ID.String()
	rec := v.do(user, http.MethodPost, base+"/messages/prepare", `{"content":"plan?"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var prep prepareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &prep))
	require.NotEmpty(t, prep.Messages)
	assert.Equal(t, "system", prep.Messages[0].Role)

	rec = v.do(user, http.MethodPost, base+"/local-executions/"+prep.ExecutionID.String()+"/tools", `{"name":"memory_search","query":"plan"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "No relevant memories found.")

	commit := `{"executionID":"` + prep.ExecutionID.String() + `","content":"done","location":"on_device","provider":"apple","model":"fm"}`
	rec = v.do(user, http.MethodPost, base+"/messages/commit", commit)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	first := rec.Body.String()
	// A repeated commit answers the message it already stored, as Swift does.
	again := v.do(user, http.MethodPost, base+"/messages/commit", commit)
	require.Equal(t, http.StatusOK, again.Code)
	var a, b commitResponse
	require.NoError(t, json.Unmarshal([]byte(first), &a))
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &b))
	assert.Equal(t, a.Message.ID, b.Message.ID)
	assert.Equal(t, http.StatusConflict, v.do(user, http.MethodDelete, base+"/local-executions/"+prep.ExecutionID.String(), "").Code)
}

// MARK: - spec

// Validates success bodies against api/openapi.yaml.
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := newEnv(t, nil)
	user := v.tenant()
	dto := v.create(user, `{"title":"Spec"}`)
	require.Equal(t, http.StatusOK, v.do(user, http.MethodPost, "/v1/conversations/"+dto.ID.String()+"/messages/stream", `{"content":"hello"}`).Code)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/conversations", ""},
		{http.MethodGet, "/v1/conversations/" + dto.ID.String(), ""},
		{http.MethodGet, "/v1/conversations/" + dto.ID.String() + "/traces", ""},
		{http.MethodGet, "/v1/chat/inbox", ""},
		{http.MethodGet, "/v1/me/chat-preferences", ""},
		{http.MethodGet, "/v1/me/preferences/hybrid-execution", ""},
	} {
		rec := v.do(user, c.method, c.path, c.body)
		require.Equal(t, http.StatusOK, rec.Code, c.path)
		req := httptest.NewRequest(c.method, c.path, nil)
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: rec.Code,
			Header: http.Header{"Content-Type": {rec.Header().Get("Content-Type")}},
		}
		out.SetBodyBytes(rec.Body.Bytes())
		assert.NoError(t, openapi3filter.ValidateResponse(ctx, out), c.path+": "+rec.Body.String())
	}
}
