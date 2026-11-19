package vault

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// Backlog E8.3: saved views (spec 2026-11-16-e8-properties-and-views, D6).
// A view is a stored POST /v1/vault/query plus its columns, validated as a
// query would be so a saved view always runs.

const (
	viewsPath       = "/v1/vault/views"
	maxViews        = vaultfiles.MaxViews
	maxViewColumns  = vaultfiles.MaxViewColumns
	maxViewName     = 200
	minColumnWidth  = 40
	maxColumnWidth  = 2000
	viewColumnsList = `id, name, scope, columns, filter, sort, group_by, created_at, updated_at`
)

func scanView(row pgx.Row) (VaultView, error) {
	var v VaultView
	var id uuid.UUID
	var scope, columns, filter, sort []byte
	var created, updated time.Time
	if err := row.Scan(&id, &v.Name, &scope, &columns, &filter, &sort, &v.Group, &created, &updated); err != nil {
		return v, err
	}
	v.Id = wire.UUID{UUID: id}
	v.CreatedAt, v.UpdatedAt = wire.Time{Time: created}, wire.Time{Time: updated}
	for _, part := range []struct {
		raw []byte
		to  any
	}{{scope, &v.Scope}, {columns, &v.Columns}, {filter, &v.Filter}, {sort, &v.Sort}} {
		if err := json.Unmarshal(part.raw, part.to); err != nil {
			return v, err
		}
	}
	if v.Columns == nil {
		v.Columns = []VaultViewColumn{}
	}
	if v.Filter == nil {
		v.Filter = []VaultQueryFilter{}
	}
	if v.Sort == nil {
		v.Sort = []VaultQuerySort{}
	}
	return v, nil
}

// viewRow is a validated input, ready to store.
type viewRow struct {
	name                         string
	scope, columns, filter, sort []byte
	group                        *string
}

// validateView checks a view the way /v1/vault/query checks a request.
func (m *Module) validateView(ctx context.Context, tenant uuid.UUID, in VaultViewInput) (viewRow, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > maxViewName || strings.ContainsAny(name, "\x00\n\r") {
		return viewRow{}, badQuery("a view name is 1 to %d characters on one line", maxViewName)
	}
	typeOf, err := m.propertyTypeOf(ctx, tenant)
	if err != nil {
		return viewRow{}, err
	}
	scratch := &sqlBuilder{}
	if _, err := scopeClauses(scratch, in.Scope); err != nil {
		return viewRow{}, err
	}
	filter := []VaultQueryFilter{}
	if in.Filter != nil {
		filter = *in.Filter
	}
	if len(filter) > queryMaxFilters {
		return viewRow{}, badQuery("at most %d filters", queryMaxFilters)
	}
	for _, f := range filter {
		if _, err := filterClause(scratch, f, typeOf); err != nil {
			return viewRow{}, err
		}
	}
	sort := []VaultQuerySort{}
	if in.Sort != nil {
		sort = *in.Sort
	}
	if len(sort) > queryMaxSorts {
		return viewRow{}, badQuery("at most %d sort keys", queryMaxSorts)
	}
	for _, s := range sort {
		if _, err := sortExpr(scratch, s.Property); err != nil {
			return viewRow{}, err
		}
		if s.Dir != nil && *s.Dir != "asc" && *s.Dir != "desc" {
			return viewRow{}, badQuery("sort dir is asc or desc")
		}
	}
	var group *string
	if in.Group != nil && *in.Group != "" {
		if err := checkName(*in.Group); err != nil {
			return viewRow{}, err
		}
		group = in.Group
	}
	columns := []VaultViewColumn{}
	if in.Columns != nil {
		columns = *in.Columns
	}
	if len(columns) > maxViewColumns {
		return viewRow{}, badQuery("at most %d columns", maxViewColumns)
	}
	for _, c := range columns {
		if err := checkName(c.Property); err != nil {
			return viewRow{}, err
		}
		if c.Width != nil && (*c.Width < minColumnWidth || *c.Width > maxColumnWidth) {
			return viewRow{}, badQuery("a column width is %d to %d", minColumnWidth, maxColumnWidth)
		}
	}
	out := viewRow{name: name, group: group}
	for _, part := range []struct {
		v  any
		to *[]byte
	}{{in.Scope, &out.scope}, {columns, &out.columns}, {filter, &out.filter}, {sort, &out.sort}} {
		if *part.to, err = json.Marshal(part.v); err != nil {
			return viewRow{}, err
		}
	}
	return out, nil
}

// viewAccess resolves the vault for a views route; 404 while properties are off.
func (m *Module) viewAccess(c *echo.Context, level vaultaccess.Permission) (actor, tenant uuid.UUID, err error) {
	if !vaultfiles.PropertiesEnabled() {
		return uuid.Nil, uuid.Nil, errPropertiesOff()
	}
	if actor, err = requireIdentity(c); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	a, err := m.access.Resolve(c, level)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return actor, a.VaultID, nil
}

func viewID(c *echo.Context) (uuid.UUID, error) {
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, echo.ErrNotFound
	}
	return id, nil
}

func (m *Module) listViews(c *echo.Context) error {
	_, tenant, err := m.viewAccess(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	rows, err := m.pool.Query(c.Request().Context(),
		`SELECT `+viewColumnsList+` FROM vault_views WHERE tenant_id = $1 ORDER BY lower(name), created_at, id`, tenant)
	if err != nil {
		return err
	}
	views, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (VaultView, error) { return scanView(r) })
	if err != nil {
		return err
	}
	if views == nil {
		views = []VaultView{}
	}
	return wire.JSON(c, http.StatusOK, VaultViewsResponse{Views: views})
}

func (m *Module) createView(c *echo.Context) error {
	actor, tenant, err := m.viewAccess(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	in, err := wire.Bind[VaultViewInput](c)
	if err != nil {
		return err
	}
	v, err := m.validateView(ctx, tenant, *in)
	if err != nil {
		return err
	}
	// The count check and the insert are one statement, so two creates can't
	// both pass a full vault.
	saved, err := scanView(m.pool.QueryRow(ctx, `
		INSERT INTO vault_views (tenant_id, name, scope, columns, filter, sort, group_by, created_by)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8
		WHERE (SELECT count(*) FROM vault_views WHERE tenant_id = $1) < $9
		RETURNING `+viewColumnsList,
		tenant, v.name, v.scope, v.columns, v.filter, v.sort, v.group, actor, maxViews))
	if errors.Is(err, pgx.ErrNoRows) {
		return wire.NewError(http.StatusConflict, "this vault already has the most views it can keep; delete one first")
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusCreated, saved)
}

func (m *Module) getView(c *echo.Context) error {
	_, tenant, err := m.viewAccess(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	id, err := viewID(c)
	if err != nil {
		return err
	}
	v, err := scanView(m.pool.QueryRow(c.Request().Context(),
		`SELECT `+viewColumnsList+` FROM vault_views WHERE tenant_id = $1 AND id = $2`, tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, v)
}

func (m *Module) putView(c *echo.Context) error {
	_, tenant, err := m.viewAccess(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	id, err := viewID(c)
	if err != nil {
		return err
	}
	// Another vault's view is a 404 whatever the body says.
	var exists bool
	if err := m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM vault_views WHERE tenant_id = $1 AND id = $2)`,
		tenant, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return echo.ErrNotFound
	}
	in, err := wire.Bind[VaultViewInput](c)
	if err != nil {
		return err
	}
	v, err := m.validateView(ctx, tenant, *in)
	if err != nil {
		return err
	}
	saved, err := scanView(m.pool.QueryRow(ctx, `
		UPDATE vault_views
		SET name = $3, scope = $4, columns = $5, filter = $6, sort = $7, group_by = $8, updated_at = NOW()
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+viewColumnsList,
		tenant, id, v.name, v.scope, v.columns, v.filter, v.sort, v.group))
	if errors.Is(err, pgx.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, saved)
}

func (m *Module) deleteView(c *echo.Context) error {
	_, tenant, err := m.viewAccess(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	id, err := viewID(c)
	if err != nil {
		return err
	}
	tag, err := m.pool.Exec(c.Request().Context(), `DELETE FROM vault_views WHERE tenant_id = $1 AND id = $2`, tenant, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return echo.ErrNotFound
	}
	return c.NoContent(http.StatusNoContent)
}
