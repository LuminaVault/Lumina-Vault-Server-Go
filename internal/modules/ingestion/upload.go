package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"lumina/internal/ids"
	"lumina/internal/modules/ingestion/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v5"
)

const (
	// Both servers run as UID 999 (decisions.md D3), so the worker can read these.
	dirPerm  = 0o755
	filePerm = 0o644
)

// uploadChunk is the controller's uploadChunk and the service's. The body is read
// after the ids and before their rows are looked up, as Swift collects it there.
func (m *Module) uploadChunk(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	batchID, itemID, err := itemParams(c)
	if err != nil {
		return err
	}
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil {
		return echo.ErrBadRequest // HTTPError(.badRequest): no message, no body
	}
	data, err := io.ReadAll(io.LimitReader(c.Request().Body, chunkSize+1))
	if err != nil {
		return err
	}
	if len(data) > chunkSize {
		return wire.ErrBodyTooLarge
	}
	if index < 0 || len(data) == 0 {
		return wire.NewError(http.StatusBadRequest, "invalid upload chunk")
	}
	item, err := m.requireItem(ctx, acc.VaultID, batchID, itemID)
	if err != nil {
		return err
	}
	if item.State != stateAwaitingUpload {
		return wire.NewError(http.StatusConflict, "item is not awaiting upload")
	}
	if err := m.disk.Check(int64(len(data))); err != nil {
		return err
	}
	dir := m.chunkDir(acc.VaultID, itemID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	target := filepath.Join(dir, strconv.Itoa(index))
	existing, err := os.ReadFile(target) //nolint:gosec // an item UUID and an integer under the tenant root.
	switch {
	case err == nil:
		if !slices.Equal(existing, data) {
			return wire.NewError(http.StatusConflict, "chunk already exists with different bytes")
		}
		return c.NoContent(http.StatusNoContent)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := writeChunk(target, data); err != nil {
		return err
	}
	// Swift's order: the chunk is already on disk when this refuses it, and the
	// row never counts it, so its retry answers 204 and the upload can't complete.
	// The count is a read then a write, so concurrent chunks can lose an update.
	uploaded := item.UploadedBytes + int64(len(data))
	limit := maxFileBytes
	if item.SizeBytes != nil {
		limit = *item.SizeBytes
	}
	if uploaded > limit {
		return wire.NewError(http.StatusRequestEntityTooLarge, "uploaded bytes exceed declared size")
	}
	if err := m.q.SetUploadedBytes(ctx, db.SetUploadedBytesParams{ID: itemID, UploadedBytes: uploaded, UpdatedAt: m.now()}); err != nil {
		return err
	}
	state := item.State
	if err := m.recordEvent(ctx, item, eventProgress, &state, uploaded); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// writeChunk is Data.write(to:options:.atomic). The temp file sits beside the item's
// directory, not in it: complete assembles every file that directory holds.
func writeChunk(target string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(filepath.Dir(target)), ".chunk-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// complete is MultimodalIngestionService.complete. A second call answers the batch
// as it stands. It doesn't refresh the batch state, and queues the item for the
// worker.
func (m *Module) complete(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	batchID, itemID, err := itemParams(c)
	if err != nil {
		return err
	}
	item, err := m.requireItem(ctx, acc.VaultID, batchID, itemID)
	if err != nil {
		return err
	}
	if item.State != stateAwaitingUpload {
		return m.respondDetail(c, acc.VaultID, batchID)
	}
	if item.SizeBytes == nil || item.UploadedBytes != *item.SizeBytes {
		return wire.NewError(http.StatusBadRequest, "upload is incomplete")
	}
	// Assembly writes a second full copy before the chunks are removed.
	if err := m.disk.Check(item.UploadedBytes); err != nil {
		return err
	}
	batch, err := m.requireBatch(ctx, acc.VaultID, batchID)
	if err != nil {
		return err
	}
	fileID, sha, err := m.assembleAndStore(ctx, item, batch)
	if err != nil {
		return err
	}
	if err := m.q.QueueCompletedItem(ctx, db.QueueCompletedItemParams{
		ID: itemID, VaultFileID: pgtype.UUID{Bytes: fileID, Valid: true}, ContentSha256: &sha, UpdatedAt: m.now(),
	}); err != nil {
		return err
	}
	state := stateQueued
	if err := m.recordEvent(ctx, item, eventStateChanged, &state, item.UploadedBytes); err != nil {
		return err
	}
	_ = os.RemoveAll(m.chunkDir(acc.VaultID, itemID))
	return m.respondDetail(c, acc.VaultID, batchID)
}

// assembleAndStore joins the chunks into raw/<folder>/<item id>-<name> and inserts
// the vault_files row. The folder is the space's slug, or inbox.
func (m *Module) assembleAndStore(ctx context.Context, item db.IngestionItem, batch db.IngestionBatch) (uuid.UUID, string, error) {
	if item.FileName == nil || item.ContentType == nil {
		return uuid.Nil, "", wire.NewError(http.StatusBadRequest, "missing file metadata")
	}
	safeName := vaultfiles.SafeFileName(vaultfiles.LastPathComponent(*item.FileName))
	folder := "inbox"
	var spaceID *uuid.UUID
	if batch.SpaceID.Valid {
		id := uuid.UUID(batch.SpaceID.Bytes)
		spaceID = &id
		slug, err := m.q.SpaceSlug(ctx, db.SpaceSlugParams{TenantID: item.TenantID, ID: id})
		switch {
		case err == nil:
			folder = slug
		case notFound(err, nil) != nil:
			return uuid.Nil, "", err
		}
	}
	relative, err := vaultfiles.SanitizePath(folder + "/" + item.ID.String() + "-" + safeName)
	if err != nil {
		return uuid.Nil, "", err
	}
	if err := vaultfiles.ValidateContentType(*item.ContentType, vaultfiles.Lowercased(vaultfiles.PathExtension(safeName))); err != nil {
		return uuid.Nil, "", err
	}
	if err := m.paths.EnsureTenantDirectories(item.TenantID); err != nil {
		return uuid.Nil, "", err
	}
	target, err := vaultfiles.ResolveInside(m.paths.Raw(item.TenantID), relative)
	if err != nil {
		return uuid.Nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
		return uuid.Nil, "", err
	}
	temp := target + ".assembling"
	sha, err := assemble(temp, m.chunkDir(item.TenantID, item.ID))
	if err != nil {
		_ = os.Remove(temp)
		return uuid.Nil, "", err
	}
	if item.ExpectedSha256 != nil && *item.ExpectedSha256 != sha {
		_ = os.Remove(temp)
		return uuid.Nil, "", wire.NewError(http.StatusBadRequest, "SHA-256 mismatch")
	}
	if _, err := os.Stat(target); err == nil {
		if err := os.Remove(target); err != nil {
			return uuid.Nil, "", err
		}
	}
	if err := os.Rename(temp, target); err != nil {
		return uuid.Nil, "", err
	}
	id, err := m.files.Insert(ctx, vaultfiles.UpsertParams{
		TenantID: item.TenantID, SpaceID: spaceID, Path: relative, ContentType: *item.ContentType,
		SizeBytes: *item.SizeBytes, SHA256: sha,
	})
	return id, sha, err
}

// assemble writes the chunks to temp in numeric order and returns their SHA-256. A
// name that isn't a number sorts as 0, as in Swift's comparator.
func assemble(temp, dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	number := func(e fs.DirEntry) int {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			return 0
		}
		return n
	}
	slices.SortStableFunc(entries, func(a, b fs.DirEntry) int { return number(a) - number(b) })
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm) //nolint:gosec // beside a path ResolveInside passed.
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	w := io.MultiWriter(out, digest)
	for _, e := range entries {
		if err := appendChunk(w, filepath.Join(dir, e.Name())); err != nil {
			_ = out.Close()
			return "", err
		}
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func appendChunk(w io.Writer, path string) error {
	in, err := os.Open(path) //nolint:gosec // a chunk in the item directory.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	_, err = io.Copy(w, in)
	return err
}

// retry is MultimodalIngestionService.retry. Attempts aren't reset.
func (m *Module) retry(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	batchID, itemID, err := itemParams(c)
	if err != nil {
		return err
	}
	if _, err := m.requireBatch(ctx, acc.VaultID, batchID); err != nil {
		return err
	}
	item, err := m.requireItem(ctx, acc.VaultID, batchID, itemID)
	if err != nil {
		return err
	}
	if item.State != stateFailed && item.State != stateBlockedCapability {
		return wire.NewError(http.StatusConflict, "only failed or capability-blocked items can retry")
	}
	if err := m.q.RetryItem(ctx, db.RetryItemParams{ID: itemID, UpdatedAt: m.now()}); err != nil {
		return err
	}
	if err := m.refreshBatch(ctx, acc.VaultID, batchID); err != nil {
		return err
	}
	return m.respondDetail(c, acc.VaultID, batchID)
}

// cancel is MultimodalIngestionService.cancel: idempotent on a finished item, and
// unlike retry it leaves terminal_notified_at alone.
func (m *Module) cancel(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	batchID, itemID, err := itemParams(c)
	if err != nil {
		return err
	}
	item, err := m.requireItem(ctx, acc.VaultID, batchID, itemID)
	if err != nil {
		return err
	}
	if item.State == stateCompleted || item.State == stateCancelled {
		return m.respondDetail(c, acc.VaultID, batchID)
	}
	if err := m.q.CancelItem(ctx, db.CancelItemParams{ID: itemID, UpdatedAt: m.now()}); err != nil {
		return err
	}
	_ = os.RemoveAll(m.chunkDir(acc.VaultID, itemID))
	if err := m.refreshBatch(ctx, acc.VaultID, batchID); err != nil {
		return err
	}
	return m.respondDetail(c, acc.VaultID, batchID)
}

// refreshBatch is MultimodalIngestionService.refreshBatch. It always writes:
// Fluent counts assigning the same state as a change, so updated_at moves.
func (m *Module) refreshBatch(ctx context.Context, tenant, batchID uuid.UUID) error {
	if _, err := m.requireBatch(ctx, tenant, batchID); err != nil {
		return err
	}
	states, err := m.q.ItemStates(ctx, db.ItemStatesParams{TenantID: tenant, BatchID: batchID})
	if err != nil {
		return err
	}
	return m.q.SetBatchState(ctx, db.SetBatchStateParams{TenantID: tenant, ID: batchID, State: batchState(states), UpdatedAt: m.now()})
}

// batchState is refreshBatch's rule.
func batchState(states []string) string {
	finished, completed := true, true
	for _, s := range states {
		completed = completed && s == stateCompleted
		finished = finished && (s == stateCompleted || s == stateFailed || s == stateBlockedCapability || s == stateCancelled)
	}
	switch {
	case completed:
		return "completed"
	case finished:
		return "attention"
	default:
		return "active"
	}
}

func (m *Module) recordEvent(ctx context.Context, item db.IngestionItem, kind string, state *string, uploaded int64) error {
	return m.q.InsertEvent(ctx, db.InsertEventParams{
		TenantID: item.TenantID, BatchID: item.BatchID, ItemID: pgtype.UUID{Bytes: item.ID, Valid: true},
		Type: kind, State: state, UploadedBytes: &uploaded,
	})
}

// chunkDir is chunkDirectory: <tenant root>/tmp/ingestion/<UPPERCASE item id>. Swift
// writes and reads the same path, so an upload may span a flip.
func (m *Module) chunkDir(tenant, itemID uuid.UUID) string {
	return filepath.Join(m.paths.TenantRoot(tenant), "tmp", "ingestion", ids.Upper(itemID))
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}
