package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"lumina/internal/ids"
	"lumina/internal/modules/vault/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strconv"
	"strings"

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

// fileDTO is LuminaVaultShared's VaultFileDTO and matches the spec's schema of
// that name. It stays hand-written to keep the wire form Swift sent: sha256 is
// always present (the generated type omits an empty one) and metadata uses
// noteMetadataDTO.
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
	// Backlog E8.2: absent when the note has none.
	Properties      map[string]any `json:"properties,omitempty"`
	PropertiesError *string        `json:"propertiesError,omitempty"`
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
		PropertiesError: f.PropertiesError,
	}
	if len(f.Properties) > 0 {
		out.Properties = f.Properties
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
	return m.writeFile(c, actor, tenant, safe, spaceID)
}

// replace is PUT /v1/vault/files/{path}: new bytes for a file that already
// exists, written at its own path and kept in its own Space. Upload files by
// base name under the Space's folder, so a note deeper than that, such as a
// bulk-imported <space>/lore/people/ada.md, could not be saved in place. A path
// with no row is 404: new files go through upload, which picks their folder.
func (m *Module) replace(c *echo.Context) error {
	actor, err := requireIdentity(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	row, safe, err := m.rowAtCatchAll(c, a)
	if err != nil {
		return err
	}
	return m.writeFile(c, actor, a.VaultID, safe, row.SpaceID)
}

// writeFile is the shared half of upload and replace: everything after the path
// and Space are decided. Query `note` and `processed`, the X-Vault-Metadata
// sidecar, If-Match and If-None-Match apply to both. Files.Write does the rest.
func (m *Module) writeFile(c *echo.Context, actor, tenant uuid.UUID, safe string, spaceID *uuid.UUID) error {
	r := c.Request()
	contentType := "application/octet-stream"
	if values, ok := r.Header["Content-Type"]; ok {
		contentType = strings.Join(values, ", ")
	}
	// Write checks it again; checking here first keeps a wrong type a 415
	// before the body is read, so a large body of the wrong type is not a 413.
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
	isNote := wire.Query(c, "note") == "true"
	saved, err := m.files.Write(r.Context(), WriteInput{
		Vault: tenant, Actor: actor, Path: safe, Data: data,
		ContentType: contentType, SpaceID: spaceID, KeepNilSpace: true, Metadata: sidecar,
		Note: &isNote, Processed: wire.Query(c, "processed") == "true",
		IfMatch: r.Header.Get("If-Match"), IfAbsent: r.Header.Get("If-None-Match") == "*",
		Reason: "upload",
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, VaultUploadResponse{
		Path: saved.Path, Size: saved.Size, ContentType: saved.ContentType, Sha256: saved.SHA256,
		MemoryId: wireUUID(saved.MemoryID),
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

// delete is VaultController.delete. Files.Delete moves the file to the trash.
func (m *Module) delete(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	raw := catchAll(c)
	if raw == "" {
		return errMissingPath
	}
	actor, err := requireIdentity(c)
	if err != nil {
		return err
	}
	if err := m.files.Delete(c.Request().Context(), a.VaultID, actor, raw); err != nil {
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
	body, err := wire.Bind[moveRequest](c)
	if err != nil {
		return err
	}
	row, err := m.files.Move(c.Request().Context(), a.VaultID, body.Path, body.NewPath)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(row))
}

func isMarkdownPath(path string) bool {
	switch vaultfiles.Lowercased(vaultfiles.PathExtension(path)) {
	case "md", "markdown":
		return true
	}
	return false
}
