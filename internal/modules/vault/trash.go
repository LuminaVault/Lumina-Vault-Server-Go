package vault

import (
	"errors"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strings"

	"github.com/labstack/echo/v5"
)

// Trash routes (backlog E3.2). A delete moves the file to the trash and records
// it in vault_trash (vaultfiles.Store.Trash); these list, restore and remove
// those entries. VaultTrashPurge removes them after the retention period.
const trashPath = "/v1/vault/trash"

func toTrashDTO(e vaultfiles.TrashEntry) VaultTrashItem {
	dto := VaultTrashItem{
		Id: wire.UUID{UUID: e.ID}, Path: e.OriginalPath, ContentType: e.ContentType,
		SizeBytes: e.SizeBytes, SpaceId: wireUUID(e.SpaceID), DeletedAt: *wireTime(&e.DeletedAt),
	}
	if e.Metadata != nil {
		dto.Title = e.Metadata.Title
	}
	return dto
}

var errNoTrashEntry = wire.NewError(http.StatusNotFound, "no trash entry with that id")

func (m *Module) listTrash(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	entries, err := m.store.ListTrash(c.Request().Context(), a.VaultID)
	if err != nil {
		return err
	}
	out := VaultTrashListResponse{Items: make([]VaultTrashItem, 0, len(entries))}
	for _, e := range entries {
		out.Items = append(out.Items, toTrashDTO(e))
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) trashEntry(c *echo.Context, perm vaultaccess.Permission) (*vaultfiles.TrashEntry, vaultaccess.Access, error) {
	a, err := m.access.Resolve(c, perm)
	if err != nil {
		return nil, a, err
	}
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return nil, a, wire.NewError(http.StatusBadRequest, "`id` is not a valid UUID")
	}
	e, err := m.store.FindTrash(c.Request().Context(), a.VaultID, id)
	if err != nil {
		return nil, a, err
	}
	if e == nil {
		return nil, a, errNoTrashEntry
	}
	return e, a, nil
}

// restoreTrash puts the file back where it was (or beside it, when that path
// is taken), recreates its row and, for a note, its recall memory. The response
// is the restored file, as the list returns it.
func (m *Module) restoreTrash(c *echo.Context) error {
	actor, err := requireIdentity(c)
	if err != nil {
		return err
	}
	e, a, err := m.trashEntry(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	tenant := a.VaultID
	rawRoot := m.paths.Raw(tenant)
	row, err := m.store.Restore(ctx, rawRoot, e, actor)
	if errors.Is(err, vaultfiles.ErrTrashFileGone) {
		if err := m.store.DeleteTrashRow(ctx, e.ID); err != nil {
			slog.WarnContext(ctx, "vault trash: stale entry not removed", "tenant", ids.Upper(tenant), "error", err)
		}
		return wire.NewError(http.StatusGone, "the deleted file is no longer available")
	}
	if err != nil {
		return err
	}
	if row == nil {
		return errNoTrashEntry
	}
	if isNotePath(row.Path) {
		if target, err := vaultfiles.ResolveInside(rawRoot, row.Path); err == nil {
			if data, err := os.ReadFile(target); err == nil { //nolint:gosec // target comes from SanitizePath and ResolveInside
				m.upsertNoteMemory(ctx, tenant, row.ID, row.SpaceID, row.Path, data, row.Metadata)
			}
		}
	}
	slog.InfoContext(ctx, "vault trash restore", "tenant", ids.Upper(tenant), "path", row.Path)
	m.analytics.Capture("vault_file_restored", ids.Upper(actor), nil)
	return wire.JSON(c, http.StatusOK, toDTO(row))
}

// deleteTrash removes the trashed file and its entry for good.
func (m *Module) deleteTrash(c *echo.Context) error {
	e, a, err := m.trashEntry(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := vaultfiles.RemoveTrashedFile(m.paths.Raw(a.VaultID), e.TrashName); err != nil {
		return err
	}
	if err := m.store.DeleteTrashRow(ctx, e.ID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "vault trash delete", "tenant", ids.Upper(a.VaultID), "path", e.OriginalPath)
	return c.NoContent(http.StatusNoContent)
}

// isNotePath says whether a restored file gets a recall memory: Markdown and
// plain text, which is what the clients upload with note=true.
func isNotePath(path string) bool {
	switch strings.ToLower(vaultfiles.PathExtension(path)) {
	case "md", "markdown", "txt":
		return true
	}
	return false
}
