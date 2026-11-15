package vault

import (
	"context"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Files moves and deletes a vault's files the way the /v1/vault/files routes
// do, for callers that have no HTTP request (the assistant's tools). The
// caller owns the access check.
type Files struct {
	store    *vaultfiles.Store
	memories *memorystore.Repository
	paths    vaultpaths.Paths
	now      func() time.Time
}

// NewFiles builds a Files over the same store and paths the vault module uses.
func NewFiles(pool *pgxpool.Pool, paths vaultpaths.Paths) *Files {
	return &Files{
		store:    vaultfiles.NewStore(pool, paths),
		memories: memorystore.New(pool, nil),
		paths:    paths,
		now:      time.Now,
	}
}

// Find returns the row of the file at path, or the route's 404.
func (f *Files) Find(ctx context.Context, vault uuid.UUID, path string) (*vaultfiles.VaultFile, error) {
	safe, err := vaultfiles.SanitizePath(path)
	if err != nil {
		return nil, err
	}
	row, err := f.store.FindByPath(ctx, vault, safe)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errFileNotFound
	}
	return row, nil
}

// Move renames the file at path to newPath, both relative to the vault's raw
// root, and returns its row.
func (f *Files) Move(ctx context.Context, vault uuid.UUID, path, newPath string) (*vaultfiles.VaultFile, error) {
	from, err := vaultfiles.SanitizePath(path)
	if err != nil {
		return nil, err
	}
	to, err := vaultfiles.SanitizePath(newPath)
	if err != nil {
		return nil, err
	}
	if from == to {
		return nil, wire.NewError(http.StatusBadRequest, "path and newPath are identical")
	}
	row, err := f.store.FindByPath(ctx, vault, from)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errFileNotFound
	}
	if err := f.store.MoveFile(ctx, row, to, vault); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "vault move", "tenant", ids.Upper(vault), "from", from, "to", to)
	return row, nil
}

// Delete removes the file at path for actor: the file moves to the trash
// (vault_trash records it, so it can be restored), and its row and the note
// memory made from it are removed. A failed move is logged and doesn't stop
// the delete, since the row is what the user sees.
func (f *Files) Delete(ctx context.Context, vault, actor uuid.UUID, path string) error {
	safe, err := vaultfiles.SanitizePath(path)
	if err != nil {
		return err
	}
	row, err := f.store.FindByPath(ctx, vault, safe)
	if err != nil {
		return err
	}
	if row == nil {
		return errFileNotFound
	}
	rawRoot := f.paths.Raw(vault)
	target, err := vaultfiles.ResolveInside(rawRoot, safe)
	if err != nil {
		return err
	}
	if _, err := f.store.Trash(ctx, rawRoot, target, row, &actor, f.now()); err != nil {
		slog.ErrorContext(ctx, "vault delete: move to trash failed", "tenant", ids.Upper(vault), "path", safe, "error", err)
	}
	// Before the row: the foreign key would only set the memory's source to NULL.
	// Swift ignores a failure here (try?).
	if _, err := f.memories.DeleteBySourceVaultFileID(ctx, vault, row.ID); err != nil {
		slog.WarnContext(ctx, "vault delete: note memory not removed", "tenant", ids.Upper(vault), "error", err)
	}
	if err := f.store.DeleteRow(ctx, row.ID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "vault delete", "tenant", ids.Upper(vault), "path", safe)
	return nil
}
