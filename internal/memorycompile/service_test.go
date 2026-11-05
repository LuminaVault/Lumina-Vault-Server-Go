package memorycompile

// Ports Tests/AppTests/KB/MemoryCompileHappyPathTests.swift
// Ports Tests/AppTests/KB/MemoryCompileMemoryUpsertTests.swift
// Ports Tests/AppTests/KB/MemoryCompileProgressPublisherTests.swift
// Ports Tests/AppTests/KB/MemoryCompileProgressServiceTests.swift
// Ports Tests/AppTests/KB/MemoryCompileSpaceCountersTests.swift

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTransport answers every completion with the same assistant content, or fails.
type stubTransport struct {
	content string
	fail    error
	mu      sync.Mutex
	calls   int
}

func (s *stubTransport) ChatCompletions(context.Context, []byte, string, string) (llm.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail != nil {
		return llm.Metadata{}, s.fail
	}
	data, err := json.Marshal(map[string]any{
		"id": "stub-extract", "model": "stub-model",
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": s.content}}},
	})
	return llm.Metadata{Data: data}, err
}

func (s *stubTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{}, errors.New("no stream")) }
}

// recordingPublisher is RecordingProgressPublisher.
type recordingPublisher struct {
	mu     sync.Mutex
	events []Event
}

func (p *recordingPublisher) Publish(_ context.Context, event Event, _ uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
}

type fixture struct {
	pool *pgxpool.Pool
	root string
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return fixture{pool: pool, root: t.TempDir()}
}

func (f fixture) tenant(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	slug := "kbp" + id.String()[:8]
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'trial', now(), now())`, id, slug+"@test.luminavault", slug)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'test', now(), now())`, id)
	require.NoError(t, err)
	return id
}

func (f fixture) service(transport llm.ChatTransport, progress Publisher) *Service {
	return New(f.pool, Options{
		Paths: vaultpaths.Paths{Root: f.root}, Transport: transport, Memories: memorystore.New(f.pool, nil),
		Embeddings: embedding.Deterministic{}, DefaultModel: "test-model", Progress: progress,
	})
}

func (f fixture) file(t *testing.T, tenant uuid.UUID, path, content string) *vaultfiles.VaultFile {
	t.Helper()
	target := filepath.Join(f.root, "tenants", ids.Upper(tenant), "raw", path)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(t, os.WriteFile(target, []byte(content), 0o600))
	id := uuid.New()
	_, err := f.pool.Exec(context.Background(), `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		VALUES ($1, $2, $3, 'text/markdown', $4, $5, now(), now())`, id, tenant, path, len(content), strings.Repeat("d", 64))
	require.NoError(t, err)
	return &vaultfiles.VaultFile{ID: id, TenantID: tenant, Path: path, ContentType: "text/markdown", SizeBytes: int64(len(content))}
}

func runIDOf(t *testing.T, event Event) string {
	t.Helper()
	data, err := json.Marshal(event.Payload)
	require.NoError(t, err)
	var payload struct {
		RunID string `json:"runId"`
	}
	require.NoError(t, json.Unmarshal(data, &payload))
	return payload.RunID
}

func labels(events []Event) []string {
	out := make([]string, len(events))
	for i, event := range events {
		out[i] = event.Type
	}
	return out
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

func TestHappyPathEmitsPreparingThenThinkingThenMemorySaved(t *testing.T) {
	f := setup(t)
	tenant := f.tenant(t)
	row := f.file(t, tenant, "notes/progress.md", "# Progress\n\nUser prefers chunky markdown sections.")
	recorder := &recordingPublisher{}
	transport := &stubTransport{content: `{"memories":["User prefers chunky markdown sections."]}`}
	runID := uuid.New()
	_, err := f.service(transport, recorder).CompileExistingVaultFiles(context.Background(), tenant, "test-user", []*vaultfiles.VaultFile{row}, nil, runID)
	require.NoError(t, err)

	types := labels(recorder.events)
	require.NotEmpty(t, types)
	assert.Equal(t, "preparing", types[0])
	assert.Contains(t, types, "thinking")
	assert.Contains(t, types, "memorySaved")
	assert.NotContains(t, types, "started")
	assert.NotContains(t, types, "completed")
	assert.NotContains(t, types, "error")
	assert.Less(t, indexOf(types, "preparing"), indexOf(types, "thinking"))
	assert.Less(t, indexOf(types, "thinking"), indexOf(types, "memorySaved"))
	for _, event := range recorder.events {
		assert.Equal(t, ids.Upper(runID), runIDOf(t, event))
	}
}

func TestExtractedMemoriesKeepTheirKindAndTheFileTheyCameFrom(t *testing.T) {
	f := setup(t)
	tenant := f.tenant(t)
	row := f.file(t, tenant, "notes/prefs.md", "Prefers Python for scripts. We ship on Tuesdays.")
	transport := &stubTransport{content: `{"memories":[
		{"text":"Prefers Python for scripts.","kind":"preference","source":"notes/prefs.md"},
		{"text":"Ships on Tuesdays.","kind":"decision","source":"elsewhere/other.md"},
		{"text":"Likes long walks.","kind":"hunch"}]}`}
	_, err := f.service(transport, &recordingPublisher{}).CompileExistingVaultFiles(context.Background(), tenant, "test-user", []*vaultfiles.VaultFile{row}, nil, uuid.New())
	require.NoError(t, err)

	saved, err := memorystore.New(f.pool, nil).List(context.Background(), tenant)
	require.NoError(t, err)
	byText := map[string]memorystore.Memory{}
	for _, m := range saved {
		byText[m.Content] = m
	}
	python, ok := byText["Prefers Python for scripts."]
	require.True(t, ok)
	require.NotNil(t, python.Kind)
	assert.Equal(t, "preference", *python.Kind)
	require.NotNil(t, python.SourceVaultFileID)
	assert.Equal(t, row.ID, *python.SourceVaultFileID)
	assert.Equal(t, memorystore.SourceKnowledgeCompile, python.OriginKind)
	tuesday, ok := byText["Ships on Tuesdays."]
	require.True(t, ok)
	require.NotNil(t, tuesday.Kind)
	assert.Equal(t, "decision", *tuesday.Kind)
	assert.Nil(t, tuesday.SourceVaultFileID, "a path not in the batch is not lineage")
	walks, ok := byText["Likes long walks."]
	require.True(t, ok)
	assert.Nil(t, walks.Kind, "an unknown kind is stored unclassified")
}

func TestErrorPathBubblesUpAndServiceEmitsNoCompletion(t *testing.T) {
	f := setup(t)
	tenant := f.tenant(t)
	row := f.file(t, tenant, "notes/will-fail.md", "# Will fail\n\nTransport will throw on the first turn.")
	recorder := &recordingPublisher{}
	transport := &stubTransport{fail: errors.New("scripted transport failure")}
	_, err := f.service(transport, recorder).CompileExistingVaultFiles(context.Background(), tenant, "test-user", []*vaultfiles.VaultFile{row}, nil, uuid.New())
	require.Error(t, err)
	types := labels(recorder.events)
	assert.NotContains(t, types, "completed")
	assert.NotContains(t, types, "error")
	assert.NotContains(t, types, "started")
	assert.Contains(t, types, "preparing")
}

func TestPublisherEncodesEnvelopeAndBroadcasts(t *testing.T) {
	run := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	var got, tenantSeen string
	publisher := BroadcastPublisher{Broadcast: func(_ context.Context, tenant, message string) {
		tenantSeen, got = tenant, message
	}}
	publisher.Publish(context.Background(), StartedEvent(run, 7), run)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", tenantSeen)
	assert.JSONEq(t, `{"type":"started","payload":{"runId":"22222222-2222-2222-2222-222222222222","totalFiles":7}}`, got)
}

func TestEncodedShapeIsTaggedEnvelope(t *testing.T) {
	data, err := json.Marshal(PreparingEvent(uuid.New()))
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "preparing", decoded["type"])
	payload, ok := decoded["payload"].(map[string]any)
	require.True(t, ok)
	assert.IsType(t, "", payload["runId"])
}

func (f fixture) space(t *testing.T, tenant uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := f.pool.Exec(context.Background(), `INSERT INTO spaces (id, tenant_id, name, slug, created_at, updated_at) VALUES ($1, $2, $3, $4, now(), now())`, id, tenant, "Space "+slug, slug)
	require.NoError(t, err)
	return id
}

func (f fixture) spaceFile(t *testing.T, tenant, space uuid.UUID, index int) {
	t.Helper()
	path := ids.Upper(space) + "/file-" + string(rune('0'+index)) + ".md"
	_, err := f.pool.Exec(context.Background(), `INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'text/markdown', 16, $5, now(), now())`, uuid.New(), tenant, space, path, strings.Repeat(string(rune('0'+index)), 64))
	require.NoError(t, err)
}

func (f fixture) counters(t *testing.T, space uuid.UUID) (int, *time.Time) {
	t.Helper()
	var count int
	var compiled *time.Time
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT note_count, last_compiled_at FROM spaces WHERE id = $1`, space).Scan(&count, &compiled))
	return count, compiled
}

func TestRefreshSpaceCountersWritesCountAndCompiledAtPerSpace(t *testing.T) {
	f := setup(t)
	tenant := f.tenant(t)
	a, b := f.space(t, tenant, "a1"), f.space(t, tenant, "b1")
	for i := range 3 {
		f.spaceFile(t, tenant, a, i)
		f.spaceFile(t, tenant, b, i)
	}
	at := time.Now()
	require.NoError(t, f.service(&stubTransport{}, nil).RefreshSpaceCounters(context.Background(), tenant, map[uuid.UUID]bool{a: true, b: true}, at))
	for _, space := range []uuid.UUID{a, b} {
		count, compiled := f.counters(t, space)
		assert.Equal(t, 3, count)
		require.NotNil(t, compiled)
		assert.WithinDuration(t, at, *compiled, time.Second)
	}
}

func TestRefreshSpaceCountersIsTenantScoped(t *testing.T) {
	f := setup(t)
	ta, tb := f.tenant(t), f.tenant(t)
	a, b := f.space(t, ta, "ta"), f.space(t, tb, "tb")
	f.spaceFile(t, ta, a, 0)
	f.spaceFile(t, tb, b, 0)
	require.NoError(t, f.service(&stubTransport{}, nil).RefreshSpaceCounters(context.Background(), ta, map[uuid.UUID]bool{a: true, b: true}, time.Now()))
	count, compiled := f.counters(t, a)
	assert.Equal(t, 1, count)
	assert.NotNil(t, compiled)
	count, compiled = f.counters(t, b)
	assert.Equal(t, 0, count)
	assert.Nil(t, compiled)
}

func TestRefreshSpaceCountersResetsNoteCountToLiveCount(t *testing.T) {
	f := setup(t)
	tenant := f.tenant(t)
	space := f.space(t, tenant, "h1")
	_, err := f.pool.Exec(context.Background(), `UPDATE spaces SET note_count = 999 WHERE id = $1`, space)
	require.NoError(t, err)
	f.spaceFile(t, tenant, space, 0)
	f.spaceFile(t, tenant, space, 1)
	require.NoError(t, f.service(&stubTransport{}, nil).RefreshSpaceCounters(context.Background(), tenant, map[uuid.UUID]bool{space: true}, time.Now()))
	count, _ := f.counters(t, space)
	assert.Equal(t, 2, count)
}

func TestRefreshSpaceCountersIsNoOpForEmptySpaceIDs(t *testing.T) {
	f := setup(t)
	tenant := f.tenant(t)
	space := f.space(t, tenant, "e1")
	f.spaceFile(t, tenant, space, 0)
	require.NoError(t, f.service(&stubTransport{}, nil).RefreshSpaceCounters(context.Background(), tenant, map[uuid.UUID]bool{}, time.Now()))
	count, compiled := f.counters(t, space)
	assert.Equal(t, 0, count)
	assert.Nil(t, compiled)
}

type commitmentRow struct {
	text     string
	state    string
	surfaced int
	due      *time.Time
	file     *uuid.UUID
	memory   *uuid.UUID
}

func (f fixture) commitments(t *testing.T, tenant uuid.UUID) map[string]commitmentRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT text, state, surfaced_count, due_at, source_vault_file_id, memory_id FROM commitments WHERE tenant_id = $1`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]commitmentRow{}
	for rows.Next() {
		var r commitmentRow
		require.NoError(t, rows.Scan(&r.text, &r.state, &r.surfaced, &r.due, &r.file, &r.memory))
		out[r.text] = r
	}
	return out
}

func TestCompileStoresCommitmentsWithTheirFileAndRecompileAddsNoDuplicate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tenant := f.tenant(t)
	row := f.file(t, tenant, "notes/todo.md", "I'll send Ana the deck by Friday. I need to renew my passport.")
	reply := `{"memories": [{"text": "Renew passport", "kind": "note", "source": "notes/todo.md"}], "commitments": [{"text": "Send Ana the deck", "due": "2026-10-23", "source": "notes/todo.md"}, {"text": "Renew passport", "due": "someday", "source": "elsewhere.md"}]}`
	service := f.service(&stubTransport{content: reply}, nil)
	for range 2 {
		_, err := service.CompileExistingVaultFiles(ctx, tenant, "slug", []*vaultfiles.VaultFile{row}, nil, uuid.New())
		require.NoError(t, err)
	}
	open := f.commitments(t, tenant)
	assert.Len(t, open, 2, "the second compile must not add the same promises again")
	var total int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM commitments WHERE tenant_id = $1`, tenant).Scan(&total))
	assert.Equal(t, 2, total)
	deck := open["Send Ana the deck"]
	require.NotNil(t, deck.file)
	assert.Equal(t, row.ID, *deck.file)
	require.NotNil(t, deck.due)
	assert.True(t, deck.due.Equal(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)))
	assert.Equal(t, "open", deck.state)
	assert.Equal(t, 0, deck.surfaced)
	passport := open["Renew passport"]
	assert.Nil(t, passport.due)
	assert.Nil(t, passport.file, "a path outside the batch is not trusted")
	require.NotNil(t, passport.memory)
	var linked int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE tenant_id = $1 AND content = 'Renew passport' AND id = $2`, tenant, *passport.memory).Scan(&linked))
	assert.Equal(t, 1, linked, "linked to the memory saying the same thing")
}

func TestCompileReplyWithoutCommitmentsStillSavesItsMemories(t *testing.T) {
	f := setup(t)
	tenant := f.tenant(t)
	row := f.file(t, tenant, "notes/x.md", "Prefers tea.")
	service := f.service(&stubTransport{content: `{"memories": ["Prefers tea."], "commitments": [{"text": 42}]}`}, nil)
	result, err := service.CompileExistingVaultFiles(context.Background(), tenant, "slug", []*vaultfiles.VaultFile{row}, nil, uuid.New())
	require.NoError(t, err)
	assert.Len(t, result.Memories, 1)
	assert.Empty(t, f.commitments(t, tenant))
}
