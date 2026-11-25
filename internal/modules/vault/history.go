package vault

import (
	"errors"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultgit"
	"lumina/internal/wire"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// History routes (backlog E4.3, spec D6). Versions come from the tenant's
// vault repository (internal/vaultgit). With history off there is no
// repository, so the list is empty and a version is 404.
const (
	historyPath  = "/v1/vault/history"
	versionsPath = "/v1/vault/versions"

	defaultHistoryLimit = 50
	maxHistoryLimit     = 200
)

var errNoVersion = wire.NewError(http.StatusNotFound, "no such version of that file")

// historyFilePath is the sanitized `path` query parameter.
func historyFilePath(c *echo.Context) (string, error) {
	raw := wire.Query(c, "path")
	if raw == "" {
		return "", wire.NewError(http.StatusBadRequest, "missing required query parameter `path`")
	}
	return vaultfiles.SanitizePath(raw)
}

func (m *Module) history(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	path, err := historyFilePath(c)
	if err != nil {
		return err
	}
	limit := defaultHistoryLimit
	if raw := wire.Query(c, "limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return wire.NewError(http.StatusBadRequest, "`limit` must be a positive integer")
		}
		limit = min(n, maxHistoryLimit)
	}
	out := VaultHistoryResponse{Versions: []VaultHistoryVersion{}}
	repo, err := vaultgit.Open(m.paths.Raw(a.VaultID))
	if errors.Is(err, vaultgit.ErrNoRepo) {
		return wire.JSON(c, http.StatusOK, out)
	}
	if err != nil {
		return err
	}
	versions, err := repo.Log(path, limit, wire.Query(c, "before"))
	if err != nil {
		return err
	}
	for _, v := range versions {
		dto := VaultHistoryVersion{
			Sha: v.SHA, CommittedAt: *wireTime(&v.CommittedAt), Message: v.Message,
			Reason: v.Reason, SizeBytes: v.SizeBytes, Deleted: v.Deleted,
		}
		if id, err := uuid.Parse(v.ActorID); err == nil {
			dto.ActorId = &wire.UUID{UUID: id}
		}
		out.Versions = append(out.Versions, dto)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// versionBytes reads path at the :sha in the URL from tenant's repository.
func (m *Module) versionBytes(c *echo.Context, tenant uuid.UUID, path string) ([]byte, error) {
	repo, err := vaultgit.Open(m.paths.Raw(tenant))
	if errors.Is(err, vaultgit.ErrNoRepo) {
		return nil, errNoVersion
	}
	if err != nil {
		return nil, err
	}
	data, err := repo.Blob(c.Param("sha"), path)
	if errors.Is(err, vaultgit.ErrNotFound) {
		return nil, errNoVersion
	}
	return data, err
}

// contentTypeFor is the file's current type when it still has a row, or one
// from its extension.
func contentTypeFor(row *vaultfiles.VaultFile, path string) string {
	if row != nil && row.ContentType != "" {
		return row.ContentType
	}
	ext := strings.ToLower(vaultfiles.PathExtension(path))
	switch ext {
	case "md", "markdown":
		return "text/markdown"
	case "":
		return "application/octet-stream"
	}
	if t := mime.TypeByExtension("." + ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (m *Module) version(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	path, err := historyFilePath(c)
	if err != nil {
		return err
	}
	data, err := m.versionBytes(c, a.VaultID, path)
	if err != nil {
		return err
	}
	row, err := m.store.FindByPath(c.Request().Context(), a.VaultID, path)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Length", strconv.Itoa(len(data)))
	return c.Blob(http.StatusOK, contentTypeFor(row, path), data)
}

// restoreVersion writes path as it was at :sha, at the same path, through the
// same steps as an upload: disk check, If-Match under the path lock, row,
// recall memory, history. The restore is a new version; nothing is rewritten.
func (m *Module) restoreVersion(c *echo.Context) error {
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
	path, err := historyFilePath(c)
	if err != nil {
		return err
	}
	data, err := m.versionBytes(c, tenant, path)
	if err != nil {
		return err
	}
	if m.guard != nil {
		if err := m.guard.Check(int64(len(data))); err != nil {
			return err
		}
	}
	if err := m.paths.EnsureTenantDirectories(tenant); err != nil {
		return err
	}
	target, err := vaultfiles.ResolveInside(m.paths.Raw(tenant), path)
	if err != nil {
		return err
	}
	if err := vaultfiles.CreateParent(target); err != nil {
		return err
	}
	unlock := m.locks.lock(tenant, path)
	if ifMatch := c.Request().Header.Get("If-Match"); ifMatch != "" {
		if err := checkIfMatch(ifMatch, target); err != nil {
			unlock()
			return err
		}
	}
	err = vaultfiles.ReplaceFile(target, data)
	unlock()
	if err != nil {
		return err
	}

	row, err := m.store.FindByPath(ctx, tenant, path)
	if err != nil {
		return err
	}
	var spaceID *uuid.UUID
	if row != nil {
		spaceID = row.SpaceID
	} else if spaceID, err = m.store.SpaceIDForPath(ctx, tenant, path); err != nil {
		return err
	}
	id, err := m.store.Upsert(ctx, vaultfiles.UpsertParams{
		TenantID: tenant, SpaceID: spaceID, Path: path, ContentType: contentTypeFor(row, path),
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data),
	})
	if err != nil {
		return err
	}
	if err := m.store.StampActors(ctx, id, actor); err != nil {
		return err
	}
	saved, err := m.store.FindByID(ctx, tenant, id)
	if err != nil {
		return err
	}
	if isNotePath(path) {
		m.files.upsertNoteMemory(ctx, tenant, id, spaceID, path, data, saved.Metadata)
	}
	vaultgit.Touch(tenant, "restore", ids.Upper(actor), "")
	slog.InfoContext(ctx, "vault version restore", "tenant", ids.Upper(tenant), "path", path, "sha", c.Param("sha"))
	m.analytics.Capture("vault_version_restored", ids.Upper(actor), nil)
	return wire.JSON(c, http.StatusOK, toDTO(saved))
}
