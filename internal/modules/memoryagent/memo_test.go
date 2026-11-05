package memoryagent

// Ports Tests/AppTests/Memory/MemoGeneratorTests.swift
// Ports Tests/AppTests/Memory/MemoControllerTitleTests.swift

import (
	"context"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func memoService(pool *pgxpool.Pool, root string, transport *scriptedTransport) *MemoService {
	paths := vaultpaths.Paths{Root: filepath.Join(root, "vault")}
	return NewMemoService(MemoOptions{
		Transport: transport, Memories: memorystore.New(pool, nil), Embeddings: embedding.Deterministic{},
		Paths: paths, Store: vaultfiles.NewStore(pool, paths), DefaultModel: "test-model",
	})
}

func vaultFileRows(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT path, content_type FROM vault_files WHERE tenant_id = $1`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, contentType string
		require.NoError(t, rows.Scan(&path, &contentType))
		out[path] = contentType
	}
	return out
}

func TestMemoLoopTerminatesAndPersistsToVault(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	tenant := createTenant(t, pool)
	root := t.TempDir()
	repo := memorystore.New(pool, nil)
	for _, content := range []string{"morning drink: jasmine tea, never coffee", "evening drink: chamomile tea before bed"} {
		vec, err := embedding.Deterministic{}.Embed(ctx, content, tenant)
		require.NoError(t, err)
		_, err = repo.CreateEmbedded(ctx, tenant, content, vec)
		require.NoError(t, err)
	}
	transport := &scriptedTransport{steps: []step{
		toolStep("session_search", `{"query":"drinks","limit":5}`),
		plainStep(`## Summary
The user prefers tea over coffee, by time of day.

## Key Points
- Mornings: jasmine tea [[memory:abc]]
- Evenings: chamomile [[memory:def]]

## Connections
Drinks correlate with energy intent.

## Open Questions
Does this hold on weekends?`),
	}}
	result, err := memoService(pool, root, transport).Generate(ctx, tenant, "memo-test", "", "drinks", nil, true)
	require.NoError(t, err)
	assert.Contains(t, result.Summary, "## Summary")
	assert.True(t, strings.HasPrefix(result.Memo, "---\n"))
	assert.Contains(t, result.Memo, `topic: "drinks"`)
	assert.Contains(t, result.Memo, "## Key Points")
	require.NotNil(t, result.Path)
	assert.True(t, strings.HasPrefix(*result.Path, "memos/"))
	assert.True(t, strings.HasSuffix(*result.Path, "/drinks.md"))
	assert.GreaterOrEqual(t, len(result.SourceIDs), 2)

	onDisk := filepath.Join(root, "vault", "tenants", ids.Upper(tenant), "raw", *result.Path)
	_, err = os.Stat(onDisk)
	require.NoError(t, err)
	assert.Equal(t, "text/markdown", vaultFileRows(t, pool, tenant)[*result.Path])
}

func TestMemoDryRunDoesNotPersist(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	transport := &scriptedTransport{steps: []step{plainStep("## Summary\nNothing here yet.")}}
	result, err := memoService(pool, t.TempDir(), transport).Generate(context.Background(), tenant, "memo-test", "", "anything", nil, false)
	require.NoError(t, err)
	assert.Nil(t, result.Path)
	assert.Empty(t, vaultFileRows(t, pool, tenant))
}

func TestMemoLoopMaxIterationsFails(t *testing.T) {
	pool := migratedPool(t)
	tenant := createTenant(t, pool)
	loop := toolStep("session_search", `{"query":"anything","limit":3}`)
	transport := &scriptedTransport{repeating: &loop}
	_, err := memoService(pool, t.TempDir(), transport).Generate(context.Background(), tenant, "memo-test", "", "loop", nil, false)
	require.ErrorIs(t, err, errMemoNotConverged)
}

func TestMemoSlugSanitizesTopic(t *testing.T) {
	assert.Equal(t, "hello-world", MemoSlug("Hello World!"))
	assert.Equal(t, "my-drinks-home", MemoSlug("My drinks @ home"))
	assert.Equal(t, "memo", MemoSlug("---"))
	assert.Equal(t, "memo", MemoSlug(""))
	assert.Len(t, MemoSlug(strings.Repeat("x", 200)), 64)
}

func TestTitleFromPath(t *testing.T) {
	assert.Equal(t, "Sleep Patterns", TitleFromPath("memos/2026-05-17/sleep-patterns.md"))
	assert.Equal(t, "Quick Thoughts", TitleFromPath("memos/2026-05-17/quick-thoughts.md"))
	assert.Equal(t, "Sleep", TitleFromPath("memos/2026-05-17/sleep.md"))
	assert.Equal(t, "Foo Bar Baz", TitleFromPath("memos/foo-bar-baz.md"))
}
