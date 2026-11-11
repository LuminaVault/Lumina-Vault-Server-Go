package llmchat

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/ratelimit"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports Tests/AppTests/LLM/LLMControllerPushSideEffectTests.swift.

var testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type stubChat struct {
	mu   sync.Mutex
	reqs []llm.CompletionRequest
	fail error
}

func (s *stubChat) Chat(_ context.Context, _, _ string, req llm.CompletionRequest) (*llm.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	if s.fail != nil {
		return nil, s.fail
	}
	return &llm.ChatResponse{ID: "r1", Model: "m", Message: llm.CompletionMessage{Role: "assistant", Content: "pong"}}, nil
}

type recordingPusher struct {
	mu    sync.Mutex
	sent  []string
	err   error
	calls chan struct{}
}

func (p *recordingPusher) NotifyLLMReply(_ context.Context, _ uuid.UUID, username, content string) error {
	p.mu.Lock()
	p.sent = append(p.sent, username+":"+content)
	p.mu.Unlock()
	if p.calls != nil {
		p.calls <- struct{}{}
	}
	return p.err
}

func newModule(t *testing.T, chat llm.ChatService, push Pusher) (*Module, *echo.Echo, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	users := auth.PGUsers(pool)
	m := New(pool, Options{Identify: auth.Identify(testKeys, users), Limits: ratelimit.NewStore(), MaxStreams: 3, Chat: chat, Push: push})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(testKeys, users), Modules: []httpserver.Module{m}})
	return m, e, pool
}

func tenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, 'pushy', 'stub', true, 'trial', now(), now())`, id, id.String()+"@test.luminavault")
	require.NoError(t, err)
	return id
}

func post(t *testing.T, e *echo.Echo, user uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/llm/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	token, err := scenario.Mint(testKeys, user)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestPushSideEffectInvokesTheSenderOnSuccess(t *testing.T) {
	push := &recordingPusher{calls: make(chan struct{}, 1)}
	_, e, pool := newModule(t, &stubChat{}, push)
	user := tenant(t, pool)
	rec := post(t, e, user, `{"messages":[{"role":"user","content":"ping"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	select {
	case <-push.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("push never sent")
	}
	assert.Equal(t, []string{"pushy:pong"}, push.sent)
}

func TestNoPushWhenTheChatFails(t *testing.T) {
	push := &recordingPusher{}
	_, e, pool := newModule(t, &stubChat{fail: &llm.ChatError{Status: http.StatusBadGateway, Message: "upstream failure"}}, push)
	user := tenant(t, pool)
	rec := post(t, e, user, `{"messages":[{"role":"user","content":"ping"}]}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Empty(t, push.sent)
}

func captureLogs(m *Module) *bytes.Buffer {
	var buf bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&buf, nil))
	return &buf
}

func TestPushErrorIsLoggedNotSwallowed(t *testing.T) {
	push := &recordingPusher{err: errors.New("apns down")}
	m, _, _ := newModule(t, &stubChat{}, push)
	logs := captureLogs(m)
	<-m.dispatchPush(uuid.New(), "u", "c")
	assert.Contains(t, logs.String(), "apns down")
}

func TestPushCancellationIsSilent(t *testing.T) {
	push := &recordingPusher{err: context.Canceled}
	m, _, _ := newModule(t, &stubChat{}, push)
	logs := captureLogs(m)
	<-m.dispatchPush(uuid.New(), "u", "c")
	assert.Empty(t, logs.String())
}

func TestPushDispatchIsAwaitable(t *testing.T) {
	push := &recordingPusher{}
	m, _, _ := newModule(t, &stubChat{}, push)
	<-m.dispatchPush(uuid.New(), "u", "c")
	assert.Equal(t, []string{"u:c"}, push.sent)
}

func TestEmptyMessagesIs400(t *testing.T) {
	_, e, pool := newModule(t, &stubChat{}, nil)
	user := tenant(t, pool)
	rec := post(t, e, user, `{"messages":[]}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "messages required")
}
