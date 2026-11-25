package vault

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/chunkindex"
	"lumina/internal/ids"
	"lumina/internal/posthog"
	"lumina/internal/vaultgit/vaultgittest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Files.Write is the write pipeline upload and replace share, and the one a
// caller with no request (GitHub sync's pull) uses. The route tests cover it
// through HTTP; these cover the inputs only a programmatic caller sets.

func indexedFiles(t *testing.T, v env, change ...func(*Options)) (env, *Files) {
	t.Helper()
	w := setupOn(t, v, func(o *Options) {
		o.Embeddings = stubEmbedder{}
		o.Indexer = chunkindex.NewIndexer(chunkindex.NewChunkRepository(v.pool), stubEmbedder{}, chunkindex.NewLinkRepository(v.pool))
		for _, f := range change {
			f(o)
		}
	})
	return w, w.module.Files()
}

func TestFilesWrite(t *testing.T) {
	v := setup(t)
	ctx := context.Background()

	t.Run("writes the file, row, memory, chunks and the history touch", func(t *testing.T) {
		w, files := indexedFiles(t, v)
		rec := vaultgittest.Record(t)
		u := v.user("trial")
		projects := v.space(u, "projects")
		data := []byte("# Plan\n\nSee [[Elsewhere]].\n")

		got, err := files.Write(ctx, WriteInput{
			Vault: u, Actor: u, Path: "projects/deep/plan.md", Data: data,
			Reason: "pull", Source: "github", Quiet: true,
		})
		require.NoError(t, err)
		require.Equal(t, "projects/deep/plan.md", got.Path)
		require.Equal(t, "text/markdown", got.ContentType, "an empty content type is the extension's")
		require.Equal(t, sha256Hex(data), got.SHA256)
		require.Equal(t, len(data), got.Size)
		require.NotNil(t, got.SpaceID, "a new row is filed by its first folder")
		require.Equal(t, projects, *got.SpaceID)
		require.NotNil(t, got.MemoryID, "a .md path is a note when Note is nil")

		onDisk, err := os.ReadFile(filepath.Join(w.module.paths.Raw(u), "projects/deep/plan.md"))
		require.NoError(t, err)
		require.Equal(t, data, onDisk)

		var title string
		var space, creator uuid.UUID
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT metadata->>'title', space_id, created_by_user_id FROM vault_files WHERE id = $1`, got.ID).
			Scan(&title, &space, &creator))
		require.Equal(t, "Plan", title, "titled from the H1")
		require.Equal(t, projects, space)
		require.Equal(t, u, creator)
		var memory string
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT content FROM memories WHERE id = $1 AND source_vault_file_id = $2`, *got.MemoryID, got.ID).Scan(&memory))
		require.Equal(t, string(data), memory)
		var chunks, links int
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM memory_chunks WHERE vault_file_id = $1`, got.ID).Scan(&chunks))
		require.Positive(t, chunks)
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM vault_links WHERE source_vault_file_id = $1`, got.ID).Scan(&links))
		require.Equal(t, 1, links)

		touches := rec.For(u)
		require.Len(t, touches, 1)
		require.Equal(t, "pull", touches[0].Reason)
		require.Equal(t, "github", touches[0].Source)
		require.Equal(t, ids.Upper(u), touches[0].Actor)
	})

	t.Run("a nil Space keeps the row's, and Note false skips the memory", func(t *testing.T) {
		_, files := indexedFiles(t, v)
		u := v.user("trial")
		lore := v.space(u, "lore")
		first, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/ada.md", Data: []byte("a"), SpaceID: &lore})
		require.NoError(t, err)
		require.Equal(t, lore, *first.SpaceID)

		notNote := false
		second, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/ada.md", Data: []byte("b"), Note: &notNote})
		require.NoError(t, err)
		require.Equal(t, first.ID, second.ID, "the same row")
		require.NotNil(t, second.SpaceID)
		require.Equal(t, lore, *second.SpaceID, "inbox/ alone would be unfiled; the row's Space wins")
		require.Nil(t, second.MemoryID)

		// Upload's rule: the write is unfiled, the row keeps its Space.
		third, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/ada.md", Data: []byte("c"), KeepNilSpace: true})
		require.NoError(t, err)
		require.Nil(t, third.SpaceID)
		var kept uuid.UUID
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT space_id FROM vault_files WHERE id = $1`, third.ID).Scan(&kept))
		require.Equal(t, lore, kept)
	})

	t.Run("IfMatch and IfAbsent are checked before the write", func(t *testing.T) {
		w, files := indexedFiles(t, v)
		u := v.user("trial")
		target := filepath.Join(w.module.paths.Raw(u), "inbox/c.md")
		_, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/c.md", Data: []byte("one")})
		require.NoError(t, err)

		_, err = files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/c.md", Data: []byte("two"), IfMatch: sha256Hex([]byte("stale"))})
		require.Equal(t, http.StatusPreconditionFailed, errStatus(t, err))
		_, err = files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/c.md", Data: []byte("two"), IfAbsent: true})
		require.Equal(t, http.StatusPreconditionFailed, errStatus(t, err))
		onDisk, err := os.ReadFile(target)
		require.NoError(t, err)
		require.Equal(t, "one", string(onDisk), "a failed check writes nothing")

		_, err = files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/c.md", Data: []byte("two"), IfMatch: sha256Hex([]byte("one"))})
		require.NoError(t, err)
		_, err = files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/new.md", Data: []byte("x"), IfAbsent: true})
		require.NoError(t, err)
		_, err = files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/gone.md", Data: []byte("x"), IfMatch: sha256Hex([]byte("x"))})
		require.Equal(t, http.StatusPreconditionFailed, errStatus(t, err), "a missing file fails every If-Match")
	})

	t.Run("refuses a path that is not sanitized and a wrong content type", func(t *testing.T) {
		_, files := indexedFiles(t, v)
		u := v.user("trial")
		for _, path := range []string{" inbox/a.md", "../escape.md", "inbox/My Note.md", "/abs.md"} {
			_, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: path, Data: []byte("x")})
			require.Equal(t, http.StatusBadRequest, errStatus(t, err), path)
		}
		_, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/a.md", Data: []byte("x"), ContentType: "image/png"})
		require.Equal(t, http.StatusUnsupportedMediaType, errStatus(t, err))
		var rows int
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM vault_files WHERE tenant_id = $1`, u).Scan(&rows))
		require.Zero(t, rows)
	})

	t.Run("DeferReresolve leaves waiting links to the caller", func(t *testing.T) {
		w, files := indexedFiles(t, v)
		u := v.user("trial")
		_, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/hub.md", Data: []byte("# Hub\n\n[[leaf]]\n")})
		require.NoError(t, err)
		state := func() string {
			var s string
			require.NoError(t, v.pool.QueryRow(ctx, `SELECT resolution_state FROM vault_links WHERE tenant_id = $1 AND raw_target = 'leaf'`, u).Scan(&s))
			return s
		}
		require.NotEqual(t, "resolved", state())

		_, err = files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/leaf.md", Data: []byte("# Leaf\n"), DeferReresolve: true})
		require.NoError(t, err)
		require.NotEqual(t, "resolved", state(), "deferred: nothing re-resolved yet")
		w.module.files.indexer.ReresolvePendingBestEffort(ctx, u)
		require.Equal(t, "resolved", state())
	})

	t.Run("Quiet skips analytics and auto-filing", func(t *testing.T) {
		var mu sync.Mutex
		var events []string
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			var body struct {
				Batch []struct {
					Event string `json:"event"`
				} `json:"batch"`
			}
			if b, err := io.ReadAll(r.Body); err == nil && json.Unmarshal(b, &body) == nil {
				mu.Lock()
				for _, e := range body.Batch {
					events = append(events, e.Event)
				}
				mu.Unlock()
			}
		}))
		t.Cleanup(srv.Close)
		client := posthog.New("phc_stub", srv.URL, posthog.Options{})
		var filed []uuid.UUID
		_, files := indexedFiles(t, v, func(o *Options) {
			o.Analytics = client
			o.CaptureFiling = func(_, file uuid.UUID) {
				mu.Lock()
				filed = append(filed, file)
				mu.Unlock()
			}
		})
		u := v.user("trial")

		_, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/quiet.md", Data: []byte("q"), Quiet: true})
		require.NoError(t, err)
		loud, err := files.Write(ctx, WriteInput{Vault: u, Actor: u, Path: "inbox/loud.md", Data: []byte("l")})
		require.NoError(t, err)
		client.Flush(ctx)

		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, []string{"vault_file_uploaded"}, events)
		require.Equal(t, []uuid.UUID{loud.ID}, filed)
	})
}
