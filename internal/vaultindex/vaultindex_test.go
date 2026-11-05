package vaultindex

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/chunkindex"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// The behaviour is pinned by modules/vault's TestIndexRoutes and parity's vault/07-index,
// which go through the routes. These cover what MCP will call directly.
func TestService(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	s := New(pool, vaultfiles.NewStore(pool, vaultpaths.Paths{Root: t.TempDir()}), chunkindex.NewLinkRepository(pool))

	t.Run("an empty vault is fresh and its lists are never null", func(t *testing.T) {
		status, err := s.Status(ctx, tenantWith(t, pool))
		require.NoError(t, err)
		out, err := json.Marshal(status)
		require.NoError(t, err)
		require.JSONEq(t, `{"ambiguousLinkCount":0,"chunkCount":0,"documentCount":0,"memoryCount":0,"resolvedLinkCount":0,
			"stale":false,"staleDocumentCount":0,"staleReasons":[],"staleSample":[],"unindexedDocumentCount":0,"unindexedSample":[],"unresolvedLinkCount":0}`, string(out))
	})

	t.Run("an unindexed document is drift, and the sample is capped but the count isn't", func(t *testing.T) {
		tenant := tenantWith(t, pool)
		for range SamplePathLimit + 1 {
			file(t, pool, tenant, uuid.NewString()+".md", nil)
		}
		status, err := s.Status(ctx, tenant)
		require.NoError(t, err)
		require.True(t, status.Stale)
		require.Equal(t, SamplePathLimit+1, status.UnindexedDocumentCount)
		require.Len(t, status.UnindexedSample, SamplePathLimit)
		require.Equal(t, []string{"21 document(s) have no chunks — run the chunk backfill"}, status.StaleReasons)
	})

	t.Run("a file the tenant doesn't have is the 404", func(t *testing.T) {
		tenant, other := tenantWith(t, pool), tenantWith(t, pool)
		id := file(t, pool, other, "theirs.md", nil)
		_, err := s.Context(ctx, tenant, id, DefaultSiblingLimit)
		require.ErrorIs(t, err, ErrNoFileWithID)
		var wireErr *wire.Error
		require.True(t, errors.As(err, &wireErr))
		require.Equal(t, http.StatusNotFound, wireErr.Status)
	})

	t.Run("the sibling limit is clamped", func(t *testing.T) {
		tenant := tenantWith(t, pool)
		space := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, note_count, created_at, updated_at) VALUES ($1, $2, 's', 's', 0, now(), now())`, space, tenant)
		require.NoError(t, err)
		id := file(t, pool, tenant, "self.md", &space)
		for _, name := range []string{"a.md", "b.md"} {
			file(t, pool, tenant, name, &space)
		}
		packet, err := s.Context(ctx, tenant, id, 0)
		require.NoError(t, err)
		require.Len(t, packet.Siblings, 1, "a limit under 1 is 1")
		require.Equal(t, "a.md", packet.Siblings[0].Path)

		packet, err = s.Context(ctx, tenant, id, MaxSiblingLimit+1)
		require.NoError(t, err)
		require.Len(t, packet.Siblings, 2)
	})

	t.Run("an unfiled document has no siblings and its lists are never null", func(t *testing.T) {
		tenant := tenantWith(t, pool)
		id := file(t, pool, tenant, "alone.md", nil)
		file(t, pool, tenant, "other.md", nil)
		packet, err := s.Context(ctx, tenant, id, DefaultSiblingLimit)
		require.NoError(t, err)
		out, err := json.Marshal(packet)
		require.NoError(t, err)
		require.JSONEq(t, `{"chunkCount":0,"danglingLinks":[],"headings":[],"linkedFrom":[],"linksTo":[],"path":"alone.md","siblings":[],
			"vaultFileID":"`+strings.ToUpper(id.String())+`"}`, string(out))
	})
}

func tenantWith(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash) VALUES ($1, $2, $3, 'x')`,
		id, id.String()+"@test.luminavault", "u"+strings.ReplaceAll(id.String(), "-", "")[:20])
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(t, err)
	return id
}

func file(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, path string, space *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, space_id)
		VALUES ($1, $2, $3, 'text/markdown', 0, '', $4)`, id, tenant, path, space)
	require.NoError(t, err)
	return id
}
