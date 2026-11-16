package chat

import (
	"lumina/internal/llm"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// App actions on the main chat stream: a plain chat turn that asks for an app
// change runs the tool loop, for a client that declared chat.app_actions, when
// CHAT_APP_ACTIONS_ENABLED is on. Everything else is the chat it was.

const bothCaps = "chat.hermes_run, chat.app_actions"

// appQA is a chat with app actions on (or off), Hermes runs available, and a
// model that plays steps.
func appQA(t *testing.T, on bool, steps ...qaStep) (*qa, *scriptedAdapter, *fakeRuns) {
	t.Helper()
	runs := &fakeRuns{}
	adapter := newAdapter(llm.Anthropic, steps...)
	q := newQA(t, qaPool(t), qaOptions{
		router: &fixedRouter{decision: decision(haikuRoute)}, adapters: []llm.Adapter{adapter}, runs: runs, appActions: on,
	})
	return q, adapter, runs
}

func createHealth() []qaStep {
	return []qaStep{call("space_create", `{"name":"Health"}`), say("I created the Health space.")}
}

// escalate posts one message that is expected to become a Hermes run: such a
// turn ends at the run pointer and never reaches the stream task.
func (q *qa) escalate(user, conversation uuid.UUID, raw string, headers ...string) []map[string]any {
	q.t.Helper()
	rec := q.do(user, http.MethodPost, "/v1/conversations/"+conversation.String()+"/messages/stream", raw, headers...)
	require.Equal(q.t, http.StatusOK, rec.Code, rec.Body.String())
	return frames(q.t, rec.Body.String())
}

func (q *qa) spaceCount(tenant uuid.UUID) int {
	return q.count(`SELECT count(*) FROM spaces WHERE tenant_id = $1`, tenant)
}

// offered is the tool names the model was given on a request.
func offered(payload []byte) []string {
	var out []string
	tools, _ := decodeBody(payload)["tools"].([]any)
	for _, tool := range tools {
		fn, _ := tool.(map[string]any)["function"].(map[string]any)
		name, _ := fn["name"].(string)
		out = append(out, name)
	}
	return out
}

func actionFrames(fs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, f := range fs {
		if f["type"] == "assistant_action" {
			payload, _ := f["payload"].(map[string]any)
			out = append(out, payload)
		}
	}
	return out
}

func TestAChatTurnThatAsksForASpaceMakesIt(t *testing.T) {
	q, adapter, runs := appQA(t, true, createHealth()...)
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, `{"content":"create a space called Health"}`, "x-lv-client-caps", bothCaps)

	assert.Equal(t, "I created the Health space.", tokens(fs))
	assert.Equal(t, "done", fs[len(fs)-1]["type"])
	assert.False(t, hasFrame(fs, "error"))
	assert.False(t, hasFrame(fs, "hermes_run"), "Hermes cannot reach spaces")
	assert.Empty(t, runs.starts)

	actions := actionFrames(fs)
	require.Len(t, actions, 1)
	assert.Equal(t, "done", actions[0]["kind"])
	resource, _ := actions[0]["resource"].(map[string]any)
	require.NotNil(t, resource, "the client refreshes from the resource")
	assert.Equal(t, "space", resource["kind"])
	assert.Equal(t, "created", resource["change"])

	var id uuid.UUID
	var slug string
	var by uuid.UUID
	require.NoError(t, q.pool.QueryRow(t.Context(),
		`SELECT id, slug, created_by_user_id FROM spaces WHERE tenant_id = $1 AND name = 'Health'`, user).Scan(&id, &slug, &by))
	assert.Equal(t, "health", slug)
	assert.Equal(t, user, by, "the change belongs to the user who asked")
	assert.Equal(t, strings.ToUpper(id.String()), strings.ToUpper(resource["id"].(string)))

	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 2)
	system, _ := parts(t, payloads[0])
	assert.Contains(t, system, "main chat")
	assert.NotContains(t, system, "small card")
	assert.Contains(t, system, "The user is on the AI tab")
	assert.Contains(t, system, "call space_list first")
	assert.Contains(t, offered(payloads[0]), "space_create")
	assert.Contains(t, offered(payloads[0]), "file_delete")

	rows := q.stored(conversation)
	assert.Equal(t, []string{"create a space called Health", "I created the Health space."}, contents(rows))
	assert.Equal(t, 1, rows[1].ToolCalls)
	assert.Equal(t, 0, q.indexJobs(user), "a report of a change is not a memory")
}

func TestWithoutTheCapabilityTheTurnIsWhatItWas(t *testing.T) {
	q, adapter, runs := appQA(t, true, createHealth()...)
	user, conversation := q.seed("pro")

	fs := q.escalate(user, conversation, `{"content":"create a space called Health"}`, "x-lv-client-caps", "chat.hermes_run")

	assert.True(t, hasFrame(fs, "hermes_run"))
	assert.Len(t, runs.starts, 1)
	assert.False(t, hasFrame(fs, "assistant_action"), "an old client dies on a frame it does not know")
	assert.Empty(t, adapter.completionPayloads())
	assert.Zero(t, q.spaceCount(user))
}

func TestWithNoCapabilitiesAtAllTheTurnIsClassicChat(t *testing.T) {
	q, adapter, runs := appQA(t, true, createHealth()...)
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, `{"content":"create a space called Health"}`)

	assert.False(t, hasFrame(fs, "hermes_run"))
	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.True(t, hasFrame(fs, "follow_ups"))
	assert.Empty(t, runs.starts)
	assert.Empty(t, adapter.completionPayloads())
	assert.Zero(t, q.spaceCount(user))
}

func TestWithTheFlagOffTheCapabilityChangesNothing(t *testing.T) {
	q, adapter, runs := appQA(t, false, createHealth()...)
	user, conversation := q.seed("pro")

	fs := q.escalate(user, conversation, `{"content":"create a space called Health"}`, "x-lv-client-caps", bothCaps)

	assert.True(t, hasFrame(fs, "hermes_run"))
	assert.Len(t, runs.starts, 1)
	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.Empty(t, adapter.completionPayloads())
	assert.Zero(t, q.spaceCount(user))
}

func TestForcedAgentModeStillGoesToHermes(t *testing.T) {
	q, adapter, runs := appQA(t, true, createHealth()...)
	user, conversation := q.seed("pro")

	fs := q.escalate(user, conversation, `{"content":"create a space called Health","agentMode":"force"}`, "x-lv-client-caps", bothCaps)

	assert.True(t, hasFrame(fs, "hermes_run"))
	assert.Len(t, runs.starts, 1)
	assert.Empty(t, adapter.completionPayloads())
	assert.Zero(t, q.spaceCount(user))
}

func TestAgentModeOffStillAllowsAnAppChange(t *testing.T) {
	q, _, runs := appQA(t, true, createHealth()...)
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, `{"content":"create a space called Health","agentMode":"off"}`, "x-lv-client-caps", bothCaps)

	assert.Len(t, actionFrames(fs), 1)
	assert.Empty(t, runs.starts)
	assert.Equal(t, 1, q.spaceCount(user))
}

func TestOrdinaryChatKeepsStreamingWithTheCapability(t *testing.T) {
	q, adapter, runs := appQA(t, true, createHealth()...)
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, `{"content":"what did I save about sleep?"}`, "x-lv-client-caps", bothCaps)

	assert.Empty(t, adapter.completionPayloads(), "the loop answers in one piece, so ordinary chat does not use it")
	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.True(t, hasFrame(fs, "follow_ups"))
	assert.Empty(t, runs.starts)
	assert.NotEmpty(t, tokens(fs))
}

func TestACodingRequestStillEscalates(t *testing.T) {
	q, adapter, runs := appQA(t, true, createHealth()...)
	user, conversation := q.seed("pro")

	fs := q.escalate(user, conversation, `{"content":"create a python script that sorts a list"}`, "x-lv-client-caps", bothCaps)

	assert.True(t, hasFrame(fs, "hermes_run"))
	assert.Len(t, runs.starts, 1)
	assert.Empty(t, adapter.completionPayloads())
}

func TestADeleteOnAChatTurnWaitsForTheUser(t *testing.T) {
	q, _, _ := appQA(t, true, call("space_delete", `{"space":"recipes"}`), say("I've asked you to confirm."))
	user, conversation := q.seed("pro")
	q.exec(`INSERT INTO spaces (id, tenant_id, name, slug) VALUES ($1, $2, 'Recipes', 'recipes')`, uuid.New(), user)

	fs := q.send(user, conversation, `{"content":"delete the recipes space"}`, "x-lv-client-caps", bothCaps)

	actions := actionFrames(fs)
	require.Len(t, actions, 1)
	assert.Equal(t, "needsConfirm", actions[0]["kind"])
	assert.Equal(t, 1, q.spaceCount(user), "nothing is deleted until the card is confirmed")
	assert.Equal(t, 1, q.count(`SELECT count(*) FROM assistant_pending_actions WHERE tenant_id = $1`, user))
}

func TestTheCardGetsTheAppToolsOnlyFromAClientThatDeclaresThem(t *testing.T) {
	q, adapter, _ := appQA(t, true, say("Here."))
	user, conversation := q.seed("pro")

	q.send(user, conversation, body(qaBody{}))
	q.send(user, conversation, body(qaBody{}), "x-lv-client-caps", bothCaps)

	payloads := adapter.completionPayloads()
	require.Len(t, payloads, 2)
	assert.NotContains(t, offered(payloads[0]), "space_create")
	assert.Contains(t, offered(payloads[1]), "space_create")
	system, _ := parts(t, payloads[1])
	assert.Contains(t, system, "small card", "the card keeps its own opening")
	assert.Contains(t, system, "call space_list first")
}

func TestAnAppChangeWhoseLoopCannotRunAnswersClassicAndSaysItCannot(t *testing.T) {
	q, _, runs := appQA(t, true, fail())
	user, conversation := q.seed("pro")

	fs := q.send(user, conversation, `{"content":"create a space called Health"}`, "x-lv-client-caps", bothCaps)

	assert.False(t, hasFrame(fs, "assistant_action"))
	assert.False(t, hasFrame(fs, "hermes_run"))
	assert.Empty(t, runs.starts)
	assert.NotEmpty(t, tokens(fs))
	assert.Zero(t, q.spaceCount(user))
}
