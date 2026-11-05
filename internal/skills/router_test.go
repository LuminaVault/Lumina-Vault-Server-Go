package skills

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubSelector struct {
	mu        sync.Mutex
	calls     int
	message   string
	manifests []Manifest
	canned    *Manifest
}

func (s *stubSelector) SelectSkill(_ context.Context, message string, manifests []Manifest, _ time.Duration) *Manifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.message, s.manifests = message, manifests
	return s.canned
}

func routerFor(selector Selector, manifests []Manifest, entitled func(RoutingUser) bool) *ContextRouter {
	return &ContextRouter{
		Manifests:   func(context.Context, uuid.UUID) ([]Manifest, error) { return manifests, nil },
		NewSelector: func(uuid.UUID, string) Selector { return selector },
		Entitled:    entitled,
	}
}

func chatBody(t *testing.T, pairs ...string) []byte {
	t.Helper()
	body := ChatRoutingBody{}
	for i := 0; i+1 < len(pairs); i += 2 {
		body.Messages = append(body.Messages, RoutingMessage{Role: pairs[i], Content: pairs[i+1]})
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return raw
}

func decodeBody(t *testing.T, raw []byte) ChatRoutingBody {
	t.Helper()
	body, ok := DecodeChatRoutingBody(raw)
	require.True(t, ok, string(raw))
	return body
}

// Swift ContextRouterTests, all 7. The middleware's four run against Rewrite.
func TestChatRoutingBody(t *testing.T) {
	t.Run("prepends system when no system present", func(t *testing.T) {
		result := ChatRoutingBody{Messages: []RoutingMessage{{Role: "user", Content: "hello"}}}.PrependingSystem("INJECTED")
		require.Len(t, result.Messages, 2)
		assert.Equal(t, RoutingMessage{Role: "system", Content: "INJECTED"}, result.Messages[0])
		assert.Equal(t, "user", result.Messages[1].Role)
	})
	t.Run("merges into existing system", func(t *testing.T) {
		original := ChatRoutingBody{Messages: []RoutingMessage{{Role: "system", Content: "BASE"}, {Role: "user", Content: "hi"}}}
		result := original.PrependingSystem("INJECTED")
		require.Len(t, result.Messages, 2)
		assert.Equal(t, RoutingMessage{Role: "system", Content: "INJECTED\n\nBASE"}, result.Messages[0])
		assert.Equal(t, "BASE", original.Messages[0].Content, "a copy")
	})
	t.Run("latest user content walks backwards", func(t *testing.T) {
		content, ok := ChatRoutingBody{Messages: []RoutingMessage{
			{Role: "user", Content: "old"}, {Role: "assistant", Content: "..."}, {Role: "user", Content: "newest"},
		}}.LatestUserContent()
		require.True(t, ok)
		assert.Equal(t, "newest", content)
	})
}

func TestContextRouter(t *testing.T) {
	ctx := context.Background()
	user := RoutingUser{Tenant: uuid.New(), Username: "u", ContextRouting: true}
	dailyBrief := Manifest{Source: SourceBuiltin, Name: "daily-brief", Description: "x", Capability: CapabilityLow, Body: "## daily-brief SKILL BODY"}

	t.Run("disabled user is no op", func(t *testing.T) {
		selector := &stubSelector{}
		body := chatBody(t, "user", "anything")
		off := user
		off.ContextRouting = false
		assert.Equal(t, body, routerFor(selector, nil, nil).Rewrite(ctx, off, body))
		assert.Zero(t, selector.calls, "disabled users must not consume a selector hit")
	})
	t.Run("selector returning nil leaves body untouched", func(t *testing.T) {
		selector := &stubSelector{}
		body := chatBody(t, "user", "what's blocking me?")
		assert.Equal(t, body, routerFor(selector, nil, nil).Rewrite(ctx, user, body))
		assert.Zero(t, selector.calls, "empty catalog must short-circuit before the selector")
	})
	t.Run("selector match prepends system prompt", func(t *testing.T) {
		selector := &stubSelector{canned: &dailyBrief}
		body := chatBody(t, "system", "you are hermes", "user", "what's blocking me from yesterday?")
		echoed := decodeBody(t, routerFor(selector, []Manifest{dailyBrief}, nil).Rewrite(ctx, user, body))
		require.Len(t, echoed.Messages, 2)
		assert.Equal(t, "system", echoed.Messages[0].Role)
		assert.Contains(t, echoed.Messages[0].Content, "daily-brief SKILL BODY")
		assert.Contains(t, echoed.Messages[0].Content, "you are hermes")
		assert.Equal(t, 1, selector.calls)
		assert.Equal(t, "what's blocking me from yesterday?", selector.message)
	})
	t.Run("entitlement gate blocks when false", func(t *testing.T) {
		selector := &stubSelector{canned: &dailyBrief}
		body := chatBody(t, "user", "hi")
		refuse := func(RoutingUser) bool { return false }
		assert.Equal(t, body, routerFor(selector, []Manifest{dailyBrief}, refuse).Rewrite(ctx, user, body))
		assert.Zero(t, selector.calls)
	})

	t.Run("no match leaves the bytes alone, extra fields included", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
		assert.Equal(t, body, routerFor(&stubSelector{}, []Manifest{dailyBrief}, nil).Rewrite(ctx, user, body))
	})
	t.Run("a match keeps only messages, model and temperature", func(t *testing.T) {
		selector := &stubSelector{canned: &dailyBrief}
		got := routerFor(selector, []Manifest{dailyBrief}, nil).Rewrite(ctx, user,
			[]byte(`{"messages":[{"role":"user","content":"hi","name":"n"}],"model":"m","temperature":0.5,"stream":true,"tools":[]}`))
		assert.JSONEq(t, `{"messages":[{"role":"system","content":"## daily-brief SKILL BODY"},{"role":"user","content":"hi"}],"model":"m","temperature":0.5}`, string(got))
	})
	for name, body := range map[string]string{
		"not json":              `nope`,
		"no messages":           `{"model":"m"}`,
		"null messages":         `{"messages":null}`,
		"key in another case":   `{"Messages":[{"role":"user","content":"hi"}]}`,
		"array content":         `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		"null content":          `{"messages":[{"role":"user","content":null}]}`,
		"no role":               `{"messages":[{"content":"hi"}]}`,
		"model of another type": `{"messages":[{"role":"user","content":"hi"}],"model":1}`,
		"no user message":       `{"messages":[{"role":"system","content":"hi"}]}`,
	} {
		t.Run("no op: "+name, func(t *testing.T) {
			selector := &stubSelector{canned: &dailyBrief}
			assert.Equal(t, []byte(body), routerFor(selector, []Manifest{dailyBrief}, nil).Rewrite(ctx, user, []byte(body)))
			assert.Zero(t, selector.calls)
		})
	}
	t.Run("a null model and temperature are absent", func(t *testing.T) {
		got := decodeBody(t, []byte(`{"messages":[],"model":null,"temperature":null}`))
		assert.Nil(t, got.Model)
		assert.Nil(t, got.Temperature)
	})
	t.Run("a catalog failure is no op", func(t *testing.T) {
		selector := &stubSelector{canned: &dailyBrief}
		r := routerFor(selector, nil, nil)
		r.Manifests = func(context.Context, uuid.UUID) ([]Manifest, error) { return nil, errors.New("boom") }
		body := chatBody(t, "user", "hi")
		assert.Equal(t, body, r.Rewrite(ctx, user, body))
	})
	t.Run("NewContextRouter reads the catalog and passes the user to the factory", func(t *testing.T) {
		tenant := uuid.New()
		selector := &stubSelector{}
		var gotTenant uuid.UUID
		var gotUsername string
		r := NewContextRouter(NewCatalog(newPaths(t)), func(id uuid.UUID, username string) Selector {
			gotTenant, gotUsername = id, username
			return selector
		}, nil)
		r.Rewrite(ctx, RoutingUser{Tenant: tenant, Username: "me", ContextRouting: true}, chatBody(t, "user", "hi"))
		assert.Equal(t, tenant, gotTenant)
		assert.Equal(t, "me", gotUsername)
		assert.Len(t, selector.manifests, 20)
	})
}

type stubTransport struct {
	mu         sync.Mutex
	calls      int
	payload    []byte
	sessionKey string
	sessionID  string
	reply      string
	err        error
	block      bool
}

func (s *stubTransport) ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) ([]byte, error) {
	s.mu.Lock()
	s.calls++
	s.payload, s.sessionKey, s.sessionID = payload, sessionKey, sessionID
	s.mu.Unlock()
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []byte(s.reply), s.err
}

func replyWith(content string) string {
	raw, err := json.Marshal(content)
	if err != nil {
		panic(err)
	}
	return `{"choices":[{"message":{"role":"assistant","content":` + string(raw) + `}}]}`
}

func TestDefaultSelector(t *testing.T) {
	ctx := context.Background()
	catalog := []Manifest{{Name: "daily-brief", Description: "themes & nudges"}, {Name: "weekly-memo", Description: "a memo"}}
	selection := func(reply string) *Manifest {
		transport := &stubTransport{reply: reply}
		return DefaultSelector{Transport: transport, Model: "m", SessionKey: "K"}.SelectSkill(ctx, "hi", catalog, time.Second)
	}

	t.Run("the request", func(t *testing.T) {
		transport := &stubTransport{reply: replyWith("NONE")}
		DefaultSelector{Transport: transport, Model: "hermes-3", SessionKey: "TENANT"}.SelectSkill(ctx, "what now?", catalog, time.Second)
		require.Equal(t, 1, transport.calls)
		assert.Equal(t, "TENANT", transport.sessionKey)
		assert.Empty(t, transport.sessionID)
		assert.JSONEq(t, `{"model":"hermes-3","temperature":0,"stream":false,"messages":[
			{"role":"system","content":"You route an incoming chat message to ONE of the user's skills,\nor NONE. Respond with EXACTLY one token: the skill name from the\ncatalog below, or the literal word NONE. No prose, no\npunctuation, no explanation."},
			{"role":"user","content":"Catalog:\ndaily-brief — themes & nudges\nweekly-memo — a memo\n\nMessage:\nwhat now?\n\nReply with one of: daily-brief, weekly-memo, NONE."}
		]}`, string(transport.payload))
	})
	t.Run("no call for an empty catalog or message", func(t *testing.T) {
		transport := &stubTransport{reply: replyWith("daily-brief")}
		s := DefaultSelector{Transport: transport, Model: "m", SessionKey: "K"}
		assert.Nil(t, s.SelectSkill(ctx, "hi", nil, time.Second))
		assert.Nil(t, s.SelectSkill(ctx, "", catalog, time.Second))
		assert.Zero(t, transport.calls)
	})
	t.Run("the first word names the skill", func(t *testing.T) {
		for _, reply := range []string{"daily-brief", "  daily-brief\n", "daily-brief because it fits", "daily-brief\tx", "daily-brief\u00a0x"} {
			got := selection(replyWith(reply))
			require.NotNil(t, got, "%q", reply)
			assert.Equal(t, "daily-brief", got.Name)
		}
	})
	t.Run("anything else is no skill", func(t *testing.T) {
		for _, reply := range []string{
			replyWith("NONE"), replyWith("none"), replyWith("None."), replyWith(""), replyWith("   "),
			replyWith("Daily-Brief"), replyWith("daily-brief."), replyWith("nope"),
			`{"choices":[]}`, `{"choices":[{"message":{"role":"assistant","content":null}}]}`,
			`{"choices":[{"message":{"role":"assistant"}}]}`, `{"choices":[{"message":{"content":"daily-brief"}}]}`,
			`{"choices":[{"message":{"role":"assistant","content":"daily-brief"}},{"message":{}}]}`,
			`{"Choices":[{"message":{"role":"assistant","content":"daily-brief"}}]}`,
			`{}`, `not json`, ``,
		} {
			assert.Nil(t, selection(reply), "%s", reply)
		}
	})
	t.Run("an upstream failure is no skill", func(t *testing.T) {
		transport := &stubTransport{err: errors.New("boom")}
		assert.Nil(t, DefaultSelector{Transport: transport}.SelectSkill(ctx, "hi", catalog, time.Second))
	})
	t.Run("the timeout cancels the call", func(t *testing.T) {
		transport := &stubTransport{block: true}
		start := time.Now()
		assert.Nil(t, DefaultSelector{Transport: transport}.SelectSkill(ctx, "hi", catalog, 10*time.Millisecond))
		assert.Less(t, time.Since(start), 5*time.Second)
	})
}
