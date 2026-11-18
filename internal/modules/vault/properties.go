package vault

import (
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultprops"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// Backlog E8.2: note properties (spec 2026-11-16-e8-properties-and-views, D5).

const (
	propertiesPath    = "/v1/vault/properties"
	propertyTypesPath = "/v1/vault/property-types"
	// maxPropertyOptions bounds a select's option list.
	maxPropertyOptions = 500
)

func errPropertiesOff() error { return echo.ErrNotFound }

// patchProperties edits a note's frontmatter and saves it like an upload.
func (m *Module) patchProperties(c *echo.Context) error {
	if !vaultfiles.PropertiesEnabled() {
		return errPropertiesOff()
	}
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
	if !vaultfiles.IsMarkdownPath(path) {
		return wire.NewError(http.StatusBadRequest, "only Markdown notes have properties")
	}
	body, err := wire.Bind[VaultPropertiesPatchRequest](c)
	if err != nil {
		return err
	}
	set := map[string]any{}
	if body.Set != nil {
		set = *body.Set
	}
	var unset []string
	if body.Unset != nil {
		unset = *body.Unset
	}
	if len(set) == 0 && len(unset) == 0 {
		return wire.NewError(http.StatusBadRequest, "nothing to change: send `set` or `unset`")
	}
	if err := m.checkPropertyTypes(c, tenant, set); err != nil {
		return err
	}

	target, err := vaultfiles.ResolveInside(m.paths.Raw(tenant), path)
	if err != nil {
		return err
	}
	unlock := m.locks.lock(tenant, path)
	current, err := os.ReadFile(target) //nolint:gosec // target comes from ResolveInside
	if errors.Is(err, os.ErrNotExist) {
		unlock()
		return echo.ErrNotFound
	}
	if err != nil {
		unlock()
		return err
	}
	if ifMatch := c.Request().Header.Get("If-Match"); ifMatch != "" {
		if err := checkIfMatch(ifMatch, target); err != nil {
			unlock()
			return err
		}
	}
	next, err := vaultprops.Apply(current, set, unset)
	if err != nil {
		unlock()
		return propertiesError(err)
	}
	changed := string(next) != string(current)
	if changed {
		if m.guard != nil {
			if err := m.guard.Check(int64(len(next))); err != nil {
				unlock()
				return err
			}
		}
		err = vaultfiles.ReplaceFile(target, next)
	}
	unlock()
	if err != nil {
		return err
	}

	row, err := m.store.FindByPath(ctx, tenant, path)
	if err != nil {
		return err
	}
	if !changed && row != nil {
		return wire.JSON(c, http.StatusOK, toDTO(row))
	}
	var spaceID *uuid.UUID
	if row != nil {
		spaceID = row.SpaceID
	} else if spaceID, err = m.store.SpaceIDForPath(ctx, tenant, path); err != nil {
		return err
	}
	id, err := m.store.Upsert(ctx, vaultfiles.UpsertParams{
		TenantID: tenant, SpaceID: spaceID, Path: path, ContentType: contentTypeFor(row, path),
		SizeBytes: int64(len(next)), SHA256: sha256Hex(next),
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
	m.upsertNoteMemory(ctx, tenant, id, spaceID, path, next, saved.Metadata)
	vaultgit.Touch(tenant, "properties", ids.Upper(actor), "")
	slog.InfoContext(ctx, "vault properties patch", "tenant", ids.Upper(tenant), "path", path,
		"set", len(set), "unset", len(unset))
	m.analytics.Capture("vault_properties_patched", ids.Upper(actor), nil)
	return wire.JSON(c, http.StatusOK, toDTO(saved))
}

// checkPropertyTypes refuses a value that doesn't fit its declared type,
// or a reserved key's fixed one.
func (m *Module) checkPropertyTypes(c *echo.Context, tenant uuid.UUID, set map[string]any) error {
	if len(set) == 0 {
		return nil
	}
	declared := map[string]vaultprops.Type{}
	types, err := m.store.PropertyTypes(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	for _, t := range types {
		declared[strings.ToLower(t.Name)] = t.Type
	}
	for name, v := range set {
		key := strings.ToLower(name)
		t, ok := declared[key]
		if !ok {
			t, ok = vaultprops.Reserved[key]
		}
		if ok && t == vaultprops.Date && vaultprops.Reserved[key] == vaultprops.Date {
			t = vaultprops.DateTime // `due` takes a date or a datetime (spec D3)
		}
		if ok && !vaultprops.Fits(t, v) {
			return wire.NewError(http.StatusUnprocessableEntity,
				fmt.Sprintf("%q is a %s property; %v doesn't fit", name, t, v))
		}
	}
	return nil
}

// propertiesError maps vaultprops errors to statuses.
func propertiesError(err error) error {
	switch {
	case errors.Is(err, vaultprops.ErrBadKey), errors.Is(err, vaultprops.ErrBadValue):
		return wire.NewError(http.StatusBadRequest, err.Error())
	case errors.Is(err, vaultprops.ErrInvalid):
		return wire.NewError(http.StatusUnprocessableEntity,
			err.Error()+"; fix it in the editor, then set properties again")
	case errors.Is(err, vaultprops.ErrUneditable):
		return wire.NewError(http.StatusUnprocessableEntity, err.Error()+"; edit it in the editor")
	}
	return err
}

func propertyTypeDTO(t vaultfiles.PropertyType) VaultPropertyType {
	return VaultPropertyType{Name: t.Name, Type: VaultPropertyTypeType(t.Type), Options: t.Options}
}

func (m *Module) propertyTypes(c *echo.Context) error {
	if !vaultfiles.PropertiesEnabled() {
		return errPropertiesOff()
	}
	if _, err := requireIdentity(c); err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	types, err := m.store.PropertyTypes(c.Request().Context(), a.VaultID)
	if err != nil {
		return err
	}
	out := VaultPropertyTypesResponse{Types: make([]VaultPropertyType, 0, len(types))}
	for _, t := range types {
		out.Types = append(out.Types, propertyTypeDTO(t))
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) putPropertyType(c *echo.Context) error {
	if !vaultfiles.PropertiesEnabled() {
		return errPropertiesOff()
	}
	if _, err := requireIdentity(c); err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(c.Param("name"))
	if name == "" || len(name) > 200 || strings.ContainsAny(name, "\n\r\x00") {
		return wire.NewError(http.StatusBadRequest, "invalid property name")
	}
	body, err := wire.Bind[VaultPropertyTypePutRequest](c)
	if err != nil {
		return err
	}
	t := vaultprops.Type(body.Type)
	if !t.Valid() {
		return wire.NewError(http.StatusBadRequest, fmt.Sprintf("unknown type %q", body.Type))
	}
	if fixed, ok := vaultprops.Reserved[strings.ToLower(name)]; ok {
		allowed := t == fixed || (fixed == vaultprops.Date && t == vaultprops.DateTime)
		if !allowed {
			return wire.NewError(http.StatusBadRequest, fmt.Sprintf("%q is always a %s property", name, fixed))
		}
	}
	var options []string
	if body.Options != nil {
		if len(*body.Options) > maxPropertyOptions {
			return wire.NewError(http.StatusBadRequest, fmt.Sprintf("at most %d options", maxPropertyOptions))
		}
		options = *body.Options
	}
	saved, err := m.store.SetPropertyType(c.Request().Context(), a.VaultID, name, t, options)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, propertyTypeDTO(saved))
}
