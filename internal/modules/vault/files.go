package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/vault/db"
	"lumina/internal/onboardlatch"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

var (
	errFileNotFound = wire.NewError(http.StatusNotFound, "vault file not found")
	errMissingPath  = wire.NewError(http.StatusBadRequest, "missing path")
)

// fileDTO is LuminaVaultShared's VaultFileDTO. The spec's schema of that name is
// stale (no spaceId, updatedAt or actor fields), so this isn't the generated type.
type fileDTO struct {
	ID              wire.UUID        `json:"id"`
	Path            string           `json:"path"`
	ContentType     string           `json:"contentType"`
	SizeBytes       int64            `json:"sizeBytes"`
	SHA256          string           `json:"sha256"`
	SpaceID         *wire.UUID       `json:"spaceId,omitempty"`
	CreatedAt       *wire.Time       `json:"createdAt,omitempty"`
	UpdatedAt       *wire.Time       `json:"updatedAt,omitempty"`
	Metadata        *noteMetadataDTO `json:"metadata,omitempty"`
	CreatedByUserID *wire.UUID       `json:"createdByUserId,omitempty"`
	UpdatedByUserID *wire.UUID       `json:"updatedByUserId,omitempty"`
}

// noteMetadataDTO is VaultNoteMetadataDTO. Tags is a pointer because Swift tells
// no key from an empty list.
type noteMetadataDTO struct {
	Title            *string    `json:"title,omitempty"`
	Tags             *[]string  `json:"tags,omitempty"`
	IsTodo           *bool      `json:"isTodo,omitempty"`
	Done             *bool      `json:"done,omitempty"`
	DueAt            *wire.Time `json:"dueAt,omitempty"`
	EnrichmentStatus *string    `json:"enrichmentStatus,omitempty"`
	SuggestedSpaceID *wire.UUID `json:"suggestedSpaceId,omitempty"`
}

type fileListResponse struct {
	Files      []fileDTO  `json:"files"`
	Limit      int        `json:"limit"`
	NextBefore *wire.Time `json:"nextBefore,omitempty"`
}

// moveRequest is what Swift decodes. The spec's VaultMoveRequest says {from, to},
// which the server has never accepted.
type moveRequest struct {
	Path    string `json:"path"`
	NewPath string `json:"newPath"`
}

// toDTO is VaultFileDTO.fromRow.
func toDTO(f *vaultfiles.VaultFile) fileDTO {
	out := fileDTO{
		ID:              wire.UUID{UUID: f.ID},
		Path:            f.Path,
		ContentType:     f.ContentType,
		SizeBytes:       f.SizeBytes,
		SHA256:          f.SHA256,
		SpaceID:         wireUUID(f.SpaceID),
		CreatedAt:       wireTime(f.CreatedAt),
		UpdatedAt:       wireTime(f.UpdatedAt),
		CreatedByUserID: wireUUID(f.CreatedByUserID),
		UpdatedByUserID: wireUUID(f.UpdatedByUserID),
	}
	if m := f.Metadata; m != nil {
		meta := noteMetadataDTO{
			Title:            m.Title,
			IsTodo:           m.IsTodo,
			Done:             m.Done,
			DueAt:            wireTime(m.DueAt),
			EnrichmentStatus: m.EnrichmentStatus,
		}
		if m.Tags != nil {
			meta.Tags = &m.Tags
		}
		// Only meaningful while the file is unfiled.
		if f.SpaceID == nil {
			meta.SuggestedSpaceID = wireUUID(m.SuggestedSpaceID)
		}
		out.Metadata = &meta
	}
	return out
}

// upload is VaultController.upload. The file is written before the row and isn't
// undone if the row fails, as in Swift.
func (m *Module) upload(c *echo.Context) error {
	actor, err := requireIdentity(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	ctx := c.Request().Context()

	rawPath := wire.Query(c, "path")
	if rawPath == "" {
		return wire.NewError(http.StatusBadRequest, "missing required query parameter `path`")
	}
	safe, err := vaultfiles.SanitizePath(rawPath)
	if err != nil {
		return err
	}
	spaceID, folder, err := m.uploadSpace(ctx, tenant, wire.Query(c, "space_id"))
	if err != nil {
		return err
	}
	// The client's prefix is ignored: the Space decides the folder, unfiled is
	// inbox, and only the basename is kept.
	safe, err = vaultfiles.SanitizePath(folder + "/" + vaultfiles.LastPathComponent(safe))
	if err != nil {
		return err
	}

	r := c.Request()
	contentType := "application/octet-stream"
	if values, ok := r.Header["Content-Type"]; ok {
		contentType = strings.Join(values, ", ")
	}
	if err := vaultfiles.ValidateContentType(contentType, vaultfiles.Lowercased(vaultfiles.PathExtension(safe))); err != nil {
		return err
	}
	var sidecar *vaultfiles.Metadata
	if values, ok := r.Header["X-Vault-Metadata"]; ok {
		sidecar = vaultfiles.ParseSidecar(strings.Join(values, ", "))
	}

	data, err := m.readFile(r)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return wire.NewError(http.StatusBadRequest, "empty body")
	}
	if m.guard != nil {
		if err := m.guard.Check(int64(len(data))); err != nil {
			return err
		}
	}
	if err := m.paths.EnsureTenantDirectories(tenant); err != nil {
		return err
	}
	target, err := vaultfiles.ResolveInside(m.paths.Raw(tenant), safe)
	if err != nil {
		return err
	}
	if err := vaultfiles.ReplaceFile(target, data); err != nil {
		return err
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	savedID, err := m.store.Upsert(ctx, vaultfiles.UpsertParams{
		TenantID:    tenant,
		SpaceID:     spaceID,
		Path:        safe,
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		SHA256:      digest,
		Processed:   wire.Query(c, "processed") == "true",
		Metadata:    sidecar,
	})
	if err != nil {
		return err
	}
	if err := m.store.StampActors(ctx, savedID, actor); err != nil {
		return err
	}
	slog.InfoContext(ctx, "vault upload", "tenant", ids.Upper(tenant), "path", safe, "bytes", len(data))

	m.sink.Enqueue(tenant, achievements.EventVaultUploaded)
	m.latches.Latch(ctx, onboardlatch.FirstCapture, tenant)

	isNote := wire.Query(c, "note") == "true"
	m.analytics.Capture("vault_file_uploaded", ids.Upper(actor), map[string]any{
		"content_type": contentType,
		"is_note":      isNote,
		"has_space":    spaceID != nil,
	})
	var memoryID *wire.UUID
	if isNote {
		memoryID = m.upsertNoteMemory(ctx, tenant, savedID, spaceID, safe, data, sidecar)
	}
	return wire.JSON(c, http.StatusOK, VaultUploadResponse{
		Path: safe, Size: len(data), ContentType: contentType, Sha256: digest, MemoryId: memoryID,
	})
}

// uploadSpace is resolveOptionalSpaceID plus the folder lookup: a malformed id and
// a foreign one are both 400, so the client sees the mistake rather than an
// orphaned file.
func (m *Module) uploadSpace(ctx context.Context, tenant uuid.UUID, raw string) (*uuid.UUID, string, error) {
	if raw == "" {
		return nil, "inbox", nil
	}
	id, err := ids.Parse(raw)
	if err != nil {
		return nil, "", wire.NewError(http.StatusBadRequest, "`space_id` is not a valid UUID")
	}
	slug, err := m.q.SpaceSlug(ctx, db.SpaceSlugParams{TenantID: tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", wire.NewError(http.StatusBadRequest, "`space_id` does not belong to the caller")
	}
	if err != nil {
		return nil, "", err
	}
	return &id, slug, nil
}

// readFile is `collectBody(upTo: maxFileSize)`: exactly the limit is allowed.
func (m *Module) readFile(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, m.maxFile+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > m.maxFile {
		return nil, wire.ErrBodyTooLarge
	}
	return data, nil
}

// upsertNoteMemory gives a written note its recall memory, keyed on the vault
// file. The file write already succeeded, so a failure is logged and the response
// has no memoryId. Text that isn't UTF-8 has no memory at all. This doesn't index:
// no vault route writes memory_chunks, and a re-upload's UpdateContent deletes the
// note's chunks until something rebuilds them.
func (m *Module) upsertNoteMemory(ctx context.Context, tenant, fileID uuid.UUID, spaceID *uuid.UUID, path string, data []byte, sidecar *vaultfiles.Metadata) *wire.UUID {
	content, ok := decodeUTF8(data)
	if m.embeddings == nil || !ok || content == "" {
		return nil
	}
	id, err := m.writeNoteMemory(ctx, tenant, fileID, spaceID, content, sidecar)
	if err != nil {
		slog.ErrorContext(ctx, "note memory upsert failed", "tenant", ids.Upper(tenant), "path", path, "error", err)
		return nil
	}
	return &wire.UUID{UUID: id}
}

// decodeUTF8 is `String(data:encoding: .utf8)` and `String(contentsOf:encoding:
// .utf8)`, which on Linux drop one leading byte order mark (parity: vault/05-notes,
// vault/07-index).
func decodeUTF8(data []byte) (string, bool) {
	if !utf8.Valid(data) {
		return "", false
	}
	return strings.TrimPrefix(string(data), "\ufeff"), true
}

func (m *Module) writeNoteMemory(ctx context.Context, tenant, fileID uuid.UUID, spaceID *uuid.UUID, content string, sidecar *vaultfiles.Metadata) (uuid.UUID, error) {
	// An embedding outage still writes the memory, without a vector.
	vec, err := embedding.EmbedForWrite(ctx, m.embeddings, content, tenant, "vault.note")
	if err != nil {
		return uuid.Nil, err
	}
	existing, err := m.memories.IDBySourceVaultFileID(ctx, tenant, fileID)
	if err != nil {
		return uuid.Nil, err
	}
	if existing != nil {
		if _, err := m.memories.UpdateContent(ctx, tenant, *existing, content, vec, nil); err != nil {
			return uuid.Nil, err
		}
		return *existing, nil
	}
	var tags []string
	if sidecar != nil {
		tags = sidecar.Tags
	}
	created, err := m.memories.Create(ctx, memorystore.CreateParams{
		Tenant:            tenant,
		Content:           content,
		Embedding:         vec,
		Tags:              tags,
		SourceVaultFileID: &fileID,
		SpaceID:           spaceID,
		ReviewState:       memorystore.ReviewAuto,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return created.ID, nil
}

// list is VaultController.list, newest first.
func (m *Module) list(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	ctx := c.Request().Context()

	limit := defaultLimit
	if v, ok := wire.QueryInt(c, "limit"); ok {
		limit = v
	}
	limit = max(1, min(limit, maxLimit))

	where := []string{"tenant_id = $1"}
	args := []any{tenant}
	add := func(clause string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if t := parseISODate(wire.Query(c, "before")); t != nil {
		add("created_at < $%d", *t)
	}
	if t := parseISODate(wire.Query(c, "after")); t != nil {
		add("created_at > $%d", *t)
	}

	slug := wire.Query(c, "space")
	switch {
	// "inbox" is a reserved word for unfiled: no Space row has that slug.
	case wire.Query(c, "unfiled") == "true" || slug == "inbox":
		where = append(where, "space_id IS NULL")
	case slug != "":
		spaceID, err := m.q.SpaceIDBySlug(ctx, db.SpaceIDBySlugParams{TenantID: tenant, Slug: slug})
		if errors.Is(err, pgx.ErrNoRows) {
			return wire.NewError(http.StatusNotFound, "unknown space `"+slug+"`")
		}
		if err != nil {
			return err
		}
		add("space_id = $%d", spaceID)
	}
	if q := wire.Query(c, "q"); q != "" {
		add("path ILIKE $%d", "%"+likeEscaper.Replace(q)+"%")
	}

	args = append(args, limit)
	// where holds only the fixed clauses above; every value is a parameter.
	query := `SELECT ` + vaultfiles.Columns + ` FROM vault_files WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := m.pool.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := fileListResponse{Files: []fileDTO{}, Limit: limit}
	var last *vaultfiles.VaultFile
	for rows.Next() {
		f, err := vaultfiles.Scan(rows)
		if err != nil {
			return err
		}
		out.Files = append(out.Files, toDTO(f))
		last = f
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out.Files) == limit && last != nil {
		out.NextBefore = wireTime(last.CreatedAt)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// likeEscaper stops "%" and "_" widening a filename search.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// read is VaultController.read: 404 when the row is gone, which covers a deleted
// file whose `_deleted_*` copy still exists, or when the file is.
func (m *Module) read(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	row, safe, err := m.rowAtCatchAll(c, a)
	if err != nil {
		return err
	}
	target, err := vaultfiles.ResolveInside(m.paths.Raw(a.VaultID), safe)
	if err != nil {
		return err
	}
	if !vaultfiles.Exists(target) {
		return wire.NewError(http.StatusNotFound, "vault file body missing")
	}
	data, err := os.ReadFile(target) //nolint:gosec // target comes from SanitizePath and ResolveInside
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Length", strconv.Itoa(len(data)))
	return c.Blob(http.StatusOK, row.ContentType, data)
}

// delete is VaultController.delete: the file is renamed and the row removed. A
// failed rename is logged and doesn't stop the delete, since the row is what the
// user sees.
func (m *Module) delete(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	ctx := c.Request().Context()
	row, safe, err := m.rowAtCatchAll(c, a)
	if err != nil {
		return err
	}
	rawRoot := m.paths.Raw(tenant)
	target, err := vaultfiles.ResolveInside(rawRoot, safe)
	if err != nil {
		return err
	}
	if err := vaultfiles.SoftDeleteFile(rawRoot, target, safe, m.now()); err != nil {
		slog.ErrorContext(ctx, "vault soft-delete rename failed", "tenant", ids.Upper(tenant), "path", safe, "error", err)
	}
	// Before the row: the foreign key would only set the memory's source to NULL.
	// Swift ignores a failure here (try?).
	if _, err := m.memories.DeleteBySourceVaultFileID(ctx, tenant, row.ID); err != nil {
		slog.WarnContext(ctx, "vault delete: note memory not removed", "tenant", ids.Upper(tenant), "error", err)
	}
	if err := m.store.DeleteRow(ctx, row.ID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "vault delete", "tenant", ids.Upper(tenant), "path", safe)
	actor, err := requireIdentity(c)
	if err != nil {
		return err
	}
	m.analytics.Capture("vault_file_deleted", ids.Upper(actor), nil)
	return c.NoContent(http.StatusNoContent)
}

// rowAtCatchAll is the start of read and delete: the raw catch-all, sanitized,
// and the tenant's row at that path.
func (m *Module) rowAtCatchAll(c *echo.Context, a vaultaccess.Access) (*vaultfiles.VaultFile, string, error) {
	raw := catchAll(c)
	if raw == "" {
		return nil, "", errMissingPath
	}
	safe, err := vaultfiles.SanitizePath(raw)
	if err != nil {
		return nil, "", err
	}
	row, err := m.store.FindByPath(c.Request().Context(), a.VaultID, safe)
	if err != nil {
		return nil, "", err
	}
	if row == nil {
		return nil, "", errFileNotFound
	}
	return row, safe, nil
}

// move is VaultController.move.
func (m *Module) move(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	body, err := wire.Bind[moveRequest](c)
	if err != nil {
		return err
	}
	from, err := vaultfiles.SanitizePath(body.Path)
	if err != nil {
		return err
	}
	to, err := vaultfiles.SanitizePath(body.NewPath)
	if err != nil {
		return err
	}
	if from == to {
		return wire.NewError(http.StatusBadRequest, "path and newPath are identical")
	}
	ctx := c.Request().Context()
	row, err := m.store.FindByPath(ctx, tenant, from)
	if err != nil {
		return err
	}
	if row == nil {
		return errFileNotFound
	}
	if err := m.store.MoveFile(ctx, row, to, tenant); err != nil {
		return err
	}
	slog.InfoContext(ctx, "vault move", "tenant", ids.Upper(tenant), "from", from, "to", to)
	return wire.JSON(c, http.StatusOK, toDTO(row))
}
