package chat

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/connectedapps"
	"lumina/internal/llm"
	"lumina/internal/modules/quickassistant"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Connected-app tools on a Quick Assistant turn, through the real chat route,
// transport and database. The model and the connected-apps service are stubs:
// no OAuth, no network, no keys.

// MARK: - Fakes

// ownEndpointTransport is the real transport, except that it says an Anthropic
// primary goes to Anthropic's own endpoint. Only the real Anthropic adapter
// can answer that (internal/llm tests it); the scripted adapter cannot.
type ownEndpointTransport struct{ *llm.Transport }

func (t ownEndpointTransport) DispatchesToProviderEndpoint(_ context.Context, d llm.Decision) bool {
	return d.Primary.Provider == llm.Anthropic
}

type appCall struct {
	user uuid.UUID
	name string
}

// stubApps is the connected-apps service: each user's tools, one canned result.
type stubApps struct {
	mu       sync.Mutex
	tools    map[uuid.UUID][]connectedapps.AppTool
	toolsErr error
	result   string
	callErr  error
	asked    []uuid.UUID
	calls    []appCall
}

func (s *stubApps) Tools(_ context.Context, user uuid.UUID, _ []string) ([]connectedapps.AppTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, user)
	if s.toolsErr != nil {
		return nil, s.toolsErr
	}
	return s.tools[user], nil
}

func (s *stubApps) Call(_ context.Context, user uuid.UUID, name string, _ map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, appCall{user, name})
	if s.callErr != nil {
		return "", s.callErr
	}
	return s.result, nil
}

func (s *stubApps) seen() (asked []uuid.UUID, calls []appCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.asked...), append([]appCall(nil), s.calls...)
}

var aaruApp = connectedapps.App{ID: "aaru", Name: "Aaru"}

func aaruTool(name, schema string) connectedapps.AppTool {
	tool := connectedapps.AppTool{App: aaruApp, Name: "aaru__" + name, Remote: connectedapps.RemoteTool{Name: name, Description: "Reads " + name}}
	if schema != "" {
		tool.Remote.InputSchema = json.RawMessage(schema)
	}
	return tool
}

const (
	objectSchema  = `{"type":"object","properties":{"status":{"type":"string"}}}`
	libraryResult = "LIBRARY-ROW Severance season 2"
)

// toolNames is the names of the tools a model request offers.
func toolNames(payload []byte) []string {
	tools, _ := decodeBody(payload)["tools"].([]any)
	var out []string
	for _, t := range tools {
		fn, _ := t.(map[string]any)["function"].(map[string]any)
		name, _ := fn["name"].(string)
		out = append(out, name)
	}
	return out
}

func appToolNames(payloads [][]byte) []string {
	var out []string
	for _, p := range payloads {
		for _, name := range toolNames(p) {
			if strings.Contains(name, "__") {
				out = append(out, name)
			}
		}
	}
	return out
}

// origins is each stored row's "role:origin", oldest first.
func (q *qa) origins(conversation uuid.UUID) []string {
	q.t.Helper()
	rows, err := q.pool.Query(context.Background(),
		`SELECT role || ':' || COALESCE(origin, '') FROM conversation_messages WHERE conversation_id = $1 ORDER BY created_at, role DESC`, conversation)
	require.NoError(q.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(q.t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(q.t, rows.Err())
	return out
}

// connectedQA is a pro user connected to Aaru, on a scripted Anthropic route.
func connectedQA(t *testing.T, steps ...qaStep) (*qa, *stubApps, *scriptedAdapter, *fixedRouter, uuid.UUID, uuid.UUID) {
	adapter := newAdapter(llm.Anthropic, steps...)
	router := &fixedRouter{decision: decision(haikuRoute)}
	apps := &stubApps{result: libraryResult}
	q := newQA(t, qaPool(t), qaOptions{router: router, adapters: []llm.Adapter{adapter}, connectedApps: apps})
	user, conversation := q.seed("pro")
	apps.tools = map[uuid.UUID][]connectedapps.AppTool{user: {aaruTool("read_library", objectSchema)}}
	return q, apps, adapter, router, user, conversation
}

// MARK: - The flag

func TestQuickTurnWithConnectedAppFlagsBothRows(t *testing.T) {
	q, apps, adapter, _, user, conversation := connectedQA(t,
		call("aaru__read_library", `{"status":"watching"}`), say("You are watching Severance."))

	fs := q.send(user, conversation, body(qaBody{content: "what am I watching"}))

	assert.Equal(t, "You are watching Severance.", tokens(fs))
	// A connected app can never put a chip in Lumina.
	assert.False(t, hasFrame(fs, "assistant_action"))
	asked, calls := apps.seen()
	assert.Equal(t, []uuid.UUID{user}, asked)
	assert.Equal(t, []appCall{{user, "aaru__read_library"}}, calls)
	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 2)
	assert.Contains(t, toolNames(payloads[0]), "aaru__read_library")
	_, lastTool := parts(t, payloads[1])
	require.NotNil(t, lastTool)
	assert.Equal(t, libraryResult, *lastTool)

	assert.Equal(t, []string{"user:connected_app", "assistant:connected_app"}, q.origins(conversation))
	assert.Equal(t, []string{"what am I watching", "You are watching Severance."}, contents(q.stored(conversation)))
	// Another app's data never becomes a memory.
	assert.Equal(t, 0, q.indexJobs(user))

	// The user still reads the turn in their own history, under an origin the
	// apps already know.
	rec := q.do(user, http.MethodGet, "/v1/conversations/"+conversation.String(), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var detail struct {
		Messages []struct{ Content, Origin string }
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	require.Len(t, detail.Messages, 2)
	assert.Equal(t, "You are watching Severance.", detail.Messages[1].Content)
	assert.Equal(t, "reply", detail.Messages[1].Origin)
}

// A turn that offered the tools but ran none is an ordinary turn.
func TestQuickTurnThatRunsNoAppToolIsNotFlagged(t *testing.T) {
	q, apps, adapter, _, user, conversation := connectedQA(t, say("Just an answer."))

	q.send(user, conversation, body(qaBody{content: "a plain question"}))

	_, calls := apps.seen()
	assert.Empty(t, calls)
	assert.Equal(t, []string{"aaru__read_library"}, appToolNames(adapter.completionPayloads()))
	assert.Equal(t, []string{"user:", "assistant:"}, q.origins(conversation))
	assert.Equal(t, 1, q.indexJobs(user))
}

// An app's failure reaches the model as the service's own fixed text, and
// nothing else: no chip, no error frame.
func TestConnectedAppErrorReachesTheModelAsContent(t *testing.T) {
	q, apps, adapter, _, user, conversation := connectedQA(t, call("aaru__read_library", "{}"), say("Aaru needs reconnecting."))
	apps.callErr = &connectedapps.ReconnectError{AppName: "Aaru"}

	fs := q.send(user, conversation, body(qaBody{content: "what am I watching"}))

	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.False(t, hasFrame(fs, "error"))
	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 2)
	_, lastTool := parts(t, payloads[1])
	require.NotNil(t, lastTool)
	assert.Equal(t, "Reconnect Aaru in Lumina Settings.", *lastTool)
}

// MARK: - Replay

func TestHermesHistoryExcludesConnectedAppTurns(t *testing.T) {
	q, _, _, router, user, conversation := connectedQA(t,
		say("Plain earlier answer."),
		call("aaru__read_library", "{}"), say("Flagged answer about Severance."))
	q.send(user, conversation, body(qaBody{content: "earlier plain question"}))
	q.send(user, conversation, body(qaBody{content: "flagged library question"}))
	require.Equal(t, []string{"user:", "assistant:", "user:connected_app", "assistant:connected_app"}, q.origins(conversation))

	// The next turn is classic chat on the Hermes gateway.
	router.decision = decision(gatewayRoute)
	q.send(user, conversation, `{"content":"classic follow up"}`)

	requests := q.gateway.all()
	require.Len(t, requests, 1)
	var history []string
	for _, m := range requests[0].Messages[1:] {
		history = append(history, m.Role+":"+m.Content)
	}
	assert.Equal(t, []string{"user:earlier plain question", "assistant:Plain earlier answer.", "user:classic follow up"}, history)
	sent, err := json.Marshal(requests[0])
	require.NoError(t, err)
	for _, flagged := range []string{"flagged library question", "Flagged answer about Severance.", "LIBRARY-ROW"} {
		assert.NotContains(t, string(sent), flagged)
	}
}

func TestHistoryMessagesSkipsConnectedAppRows(t *testing.T) {
	flagged, proactive := OriginConnectedApp, "proactive"
	got := historyMessages([]messageRow{
		{Role: "user", Content: "one"},
		{Role: "user", Content: "app question", Origin: &flagged},
		{Role: "assistant", Content: "app answer", Origin: &flagged},
		{Role: "assistant", Content: "nudge", Origin: &proactive},
	})
	assert.Equal(t, []llm.ChatMessage{{Role: "user", Content: "one"}, {Role: "assistant", Content: "nudge"}}, got)
	assert.Empty(t, historyMessages(nil))
}

// MARK: - Who gets the tools

func TestQuickTurnWithoutConnectionsOffersNoAppTools(t *testing.T) {
	adapter := newAdapter(llm.Anthropic, say("Hi."))
	apps := &stubApps{}
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}, connectedApps: apps})
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, body(qaBody{content: "hello there"}))

	assert.Equal(t, "Hi.", tokens(fs))
	asked, calls := apps.seen()
	assert.Equal(t, []uuid.UUID{user}, asked)
	assert.Empty(t, calls)
	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 1)
	assert.NotEmpty(t, toolNames(payloads[0]))
	assert.Empty(t, appToolNames(payloads))
	assert.Equal(t, []string{"user:", "assistant:"}, q.origins(conversation))
}

// A user with nothing connected keeps their fallbacks: the turn is not pinned.
func TestQuickTurnWithoutConnectionsIsNotPinnedToAnthropic(t *testing.T) {
	groq := newAdapter(llm.Groq, say("From the first route."))
	anthropic := newAdapter(llm.Anthropic, say("From Anthropic."))
	q := newQA(t, qaPool(t), qaOptions{
		router:   &fixedRouter{decision: decision(llm.Route{Provider: llm.Groq, ModelID: "llama"}, haikuRoute)},
		adapters: []llm.Adapter{groq, anthropic}, connectedApps: &stubApps{},
	})
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, body(qaBody{content: "hello there"}))

	assert.Equal(t, "From the first route.", tokens(fs))
	assert.Empty(t, anthropic.completionPayloads())
}

// The tools failing to load costs the turn its app tools, never its answer.
func TestConnectedAppToolLoadFailureRunsTheTurnWithoutThem(t *testing.T) {
	q, apps, adapter, _, user, conversation := connectedQA(t, say("Still answered."))
	apps.toolsErr = errors.New("store down: secret detail")

	fs := q.send(user, conversation, body(qaBody{content: "hello there"}))

	assert.Equal(t, "Still answered.", tokens(fs))
	assert.False(t, hasFrame(fs, "error"))
	assert.Empty(t, appToolNames(adapter.completionPayloads()))
	assert.Equal(t, []string{"user:", "assistant:"}, q.origins(conversation))
}

// The tools belong to whoever is asking, never to the vault the turn reads.
func TestSharedVaultMemberGetsNoOwnersTools(t *testing.T) {
	// The model asks for the owner's tool anyway.
	adapter := newAdapter(llm.Anthropic, call("aaru__read_library", "{}"), say("Nothing to show."))
	apps := &stubApps{result: libraryResult}
	q := newQA(t, qaPool(t), qaOptions{router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}, connectedApps: apps})
	alice, _ := q.seed("pro")
	bob, conversation := q.seed("free")
	apps.tools = map[uuid.UUID][]connectedapps.AppTool{alice: {aaruTool("read_library", objectSchema)}}
	team, vault := uuid.New(), uuid.New()
	q.exec(`INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at) VALUES ($1, 'qa-team', $2, $2, now(), now())`, team, alice)
	q.exec(`INSERT INTO vaults (id, team_id, name, created_at, updated_at) VALUES ($1, $2, 'shared', now(), now())`, vault, team)
	q.exec(`INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
		VALUES ($1, $2, $3, 'viewer', true, $4, now(), now())`, uuid.New(), vault, bob, alice)

	q.send(bob, conversation, body(qaBody{content: "what is alice watching"}), "X-Vault-ID", strings.ToUpper(vault.String()))

	asked, calls := apps.seen()
	// Asked as Bob, and only as Bob: not as Alice, not as the vault.
	assert.Equal(t, []uuid.UUID{bob}, asked)
	assert.Empty(t, calls, "the app saw a request")
	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 2)
	assert.Empty(t, appToolNames(payloads))
	_, lastTool := parts(t, payloads[1])
	require.NotNil(t, lastTool)
	assert.NotContains(t, *lastTool, "LIBRARY-ROW")
	assert.Equal(t, []string{"user:", "assistant:"}, q.origins(conversation))
}

func TestConnectedAppTurnNeverReachesNonAnthropicAdapter(t *testing.T) {
	groq := newAdapter(llm.Groq, say("From Groq."))
	anthropic := newAdapter(llm.Anthropic, call("aaru__read_library", "{}"), say("From Anthropic."))
	apps := &stubApps{result: libraryResult}
	q := newQA(t, qaPool(t), qaOptions{
		router:   &fixedRouter{decision: decision(llm.Route{Provider: llm.Groq, ModelID: "llama"}, haikuRoute)},
		adapters: []llm.Adapter{groq, anthropic}, connectedApps: apps,
	})
	user, conversation := q.seed("pro")
	apps.tools = map[uuid.UUID][]connectedapps.AppTool{user: {aaruTool("read_library", objectSchema)}}

	fs := q.send(user, conversation, body(qaBody{content: "what am I watching"}))

	assert.Equal(t, "From Anthropic.", tokens(fs))
	assert.Empty(t, groq.completionPayloads())
	assert.Empty(t, groq.streamPayloads())
	assert.Len(t, anthropic.completionPayloads(), 2)
	_, calls := apps.seen()
	assert.Len(t, calls, 1)
	assert.Equal(t, []string{"user:connected_app", "assistant:connected_app"}, q.origins(conversation))
}

// MARK: - Mapping

// Anthropic answers 400 to a tool whose schema is not an object, which would
// fail every turn of a connected user.
func TestConnectedToolsSkipsToolsWithoutAnObjectSchema(t *testing.T) {
	got := connectedTools([]connectedapps.AppTool{
		aaruTool("read_library", objectSchema),
		aaruTool("no_schema", ""),
		aaruTool("null_schema", `null`),
		aaruTool("array_schema", `[]`),
		aaruTool("string_type", `{"type":"string"}`),
		aaruTool("no_type", `{"properties":{}}`),
		aaruTool("type_list", `{"type":["object","null"]}`),
		aaruTool("broken", `{"type":"object"`),
		aaruTool("list_airs", `{"type":"object"}`),
	})

	assert.Equal(t, []quickassistant.Tool{
		{Name: "aaru__read_library", Title: "Aaru", Description: "Reads read_library", InputSchema: json.RawMessage(objectSchema), Personal: true},
		{Name: "aaru__list_airs", Title: "Aaru", Description: "Reads list_airs", InputSchema: json.RawMessage(`{"type":"object"}`), Personal: true},
	}, got)
	assert.Empty(t, connectedTools(nil))
}
