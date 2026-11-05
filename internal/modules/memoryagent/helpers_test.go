package memoryagent

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// step is one scripted model turn: a tool call when name is set, plain content otherwise.
type step struct {
	name, arguments, content string
}

func toolStep(name, arguments string) step { return step{name: name, arguments: arguments} }
func plainStep(content string) step        { return step{content: content} }

type recordedCall struct {
	payload             []byte
	sessionKey, session string
}

// scriptedTransport plays back canned chat completions, as ScriptedTransport does.
type scriptedTransport struct {
	mu        sync.Mutex
	steps     []step
	repeating *step
	fail      error
	calls     []recordedCall
}

func (s *scriptedTransport) ChatCompletions(_ context.Context, payload []byte, sessionKey, sessionID string) (llm.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedCall{payload, sessionKey, sessionID})
	if s.fail != nil {
		return llm.Metadata{}, s.fail
	}
	var next step
	switch {
	case s.repeating != nil:
		next = *s.repeating
	case len(s.calls) <= len(s.steps):
		next = s.steps[len(s.calls)-1]
	default:
		return llm.Metadata{}, errors.New("script exhausted")
	}
	return llm.Metadata{Data: encodeStep(next)}, nil
}

func (s *scriptedTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{}, errors.New("no stream scripted")) }
}

func encodeStep(st step) []byte {
	message := map[string]any{"role": "assistant", "content": st.content}
	reason := "stop"
	if st.name != "" {
		reason = "tool_calls"
		message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "call_" + uuid.NewString()[:8], "type": "function",
			"function": map[string]string{"name": st.name, "arguments": st.arguments},
		}}}
	}
	data, err := json.Marshal(map[string]any{
		"id": "test-resp", "model": "test",
		"choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}},
	})
	if err != nil {
		panic(err)
	}
	return data
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return pool
}

func createTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	short := strings.ToLower(id.String()[:8])
	_, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'trial', now(), now())`, id, "mem-"+short+"@test.luminavault", "mem-"+short)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `
		INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
		VALUES ($1, $1, 'test', now(), now())`, id)
	require.NoError(t, err)
	return id
}
