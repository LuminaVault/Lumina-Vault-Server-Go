package workers

import (
	"context"
	"errors"
	"lumina/internal/embedding"
	"lumina/internal/keepjudge"
	"lumina/internal/memorystore"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type stubEmbedder struct {
	err   error
	texts []string
}

func (s *stubEmbedder) Embed(_ context.Context, text string, _ uuid.UUID) ([]float32, error) {
	s.texts = append(s.texts, text)
	if s.err != nil {
		return nil, s.err
	}
	v := make([]float32, embedding.Dimensions)
	v[0] = 1
	return v, nil
}

type stubJudge struct {
	d   keepjudge.Decision
	err error
}

func (s stubJudge) Judge(context.Context, string, uuid.UUID) (keepjudge.Decision, error) {
	return s.d, s.err
}

func newMemoryIndex(pool *pgxpool.Pool, emb embedding.Service, judge KeepJudge) *MemoryIndex {
	return &MemoryIndex{Pool: pool, Embeddings: emb, Memories: memorystore.New(pool, nil), Judge: judge, Log: quiet()}
}

func enqueueOutput(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, sourceID, content string) {
	t.Helper()
	provider, model := "anthropic", "claude-haiku-5-5"
	require.NoError(t, memorystore.New(pool, nil).EnqueueOutput(t.Context(), memorystore.EnqueueOutputParams{
		Tenant: tenant, Source: memorystore.SourceChat, SourceID: sourceID, Content: content,
		Provider: &provider, Model: &model,
	}))
}

func jobStatus(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, sourceID string) (status string, attempts int, lastError *string) {
	t.Helper()
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT status, attempts, last_error FROM memory_index_jobs WHERE tenant_id = $1 AND source_id = $2`,
		tenant, sourceID).Scan(&status, &attempts, &lastError))
	return status, attempts, lastError
}

// A queued answer becomes one model-output memory with the model's provenance,
// cut to 12000 characters, and the job completes. An empty queue is a no-op tick.
func testMemoryIndexStoresAnswer(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	emb := &stubEmbedder{}
	w := newMemoryIndex(pool, emb, nil)
	enqueueOutput(t, pool, tenant, "msg-1", strings.Repeat("é", 12005))

	n, err := w.Tick(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	status, attempts, _ := jobStatus(t, pool, tenant, "msg-1")
	require.Equal(t, "completed", status)
	require.Equal(t, 1, attempts)

	var content, kind, model string
	var tags []string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT content, origin_kind, origin_model, tags FROM memories WHERE tenant_id = $1 AND origin_source_id = 'msg-1'`,
		tenant).Scan(&content, &kind, &model, &tags))
	require.Equal(t, 12000, len([]rune(content)))
	require.Equal(t, "chat", kind)
	require.Equal(t, "claude-haiku-5-5", model)
	require.Equal(t, []string{"model-output"}, tags)
	require.Len(t, emb.texts, 1)
	require.Equal(t, 1, countRows(t, pool,
		`SELECT count(*) FROM memory_contributions WHERE tenant_id = $1 AND actor_kind = 'model'`, tenant))

	n, err = w.Tick(t.Context())
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

// A memory already saved for the same source isn't saved twice: the job completes
// without an embed.
func testMemoryIndexSkipsExistingMemory(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	emb := &stubEmbedder{}
	w := newMemoryIndex(pool, emb, nil)
	enqueueOutput(t, pool, tenant, "msg-dup", "first")
	_, err := w.Tick(t.Context())
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(),
		`UPDATE memory_index_jobs SET status = 'pending' WHERE tenant_id = $1 AND source_id = 'msg-dup'`, tenant)
	require.NoError(t, err)

	_, err = w.Tick(t.Context())
	require.NoError(t, err)
	require.Len(t, emb.texts, 1)
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM memories WHERE tenant_id = $1`, tenant))
}

// An embed failure parks the job in retry with the error and a 2^attempts back-off,
// and the tick itself succeeds.
func testMemoryIndexRetriesOnFailure(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	w := newMemoryIndex(pool, &stubEmbedder{err: errors.New("provider down")}, nil)
	enqueueOutput(t, pool, tenant, "msg-fail", "answer")

	n, err := w.Tick(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	status, attempts, lastError := jobStatus(t, pool, tenant, "msg-fail")
	require.Equal(t, "retry", status)
	require.Equal(t, 1, attempts)
	require.NotNil(t, lastError)
	require.Contains(t, *lastError, "provider down")
	require.Equal(t, 1, countRows(t, pool, `
		SELECT count(*) FROM memory_index_jobs WHERE tenant_id = $1
		AND next_attempt_at BETWEEN NOW() + INTERVAL '1 second' AND NOW() + INTERVAL '3 seconds'`, tenant))
	require.Equal(t, 0, countRows(t, pool, `SELECT count(*) FROM memories WHERE tenant_id = $1`, tenant))

	// Not due yet: the next tick finds nothing.
	n, err = w.Tick(t.Context())
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

// The keep gate: a "not durable" verdict skips the job; a kind is stored on the
// memory; a judge error fails open and keeps the reply unclassified.
func testMemoryIndexKeepGate(t *testing.T, pool *pgxpool.Pool) {
	tenant := newTenant(t, pool)
	enqueueOutput(t, pool, tenant, "msg-skip", "hello there")
	_, err := newMemoryIndex(pool, &stubEmbedder{}, stubJudge{d: keepjudge.Decision{Keep: false}}).Tick(t.Context())
	require.NoError(t, err)
	status, _, _ := jobStatus(t, pool, tenant, "msg-skip")
	require.Equal(t, "skipped", status)

	enqueueOutput(t, pool, tenant, "msg-fact", "I live in Lisbon")
	_, err = newMemoryIndex(pool, &stubEmbedder{}, stubJudge{d: keepjudge.Decision{Keep: true, Kind: "fact"}}).Tick(t.Context())
	require.NoError(t, err)
	var kind *string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT kind FROM memories WHERE tenant_id = $1 AND origin_source_id = 'msg-fact'`, tenant).Scan(&kind))
	require.NotNil(t, kind)
	require.Equal(t, "fact", *kind)

	enqueueOutput(t, pool, tenant, "msg-err", "judge breaks")
	_, err = newMemoryIndex(pool, &stubEmbedder{}, stubJudge{err: errors.New("judge down")}).Tick(t.Context())
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT kind FROM memories WHERE tenant_id = $1 AND origin_source_id = 'msg-err'`, tenant).Scan(&kind))
	require.Nil(t, kind)
}
