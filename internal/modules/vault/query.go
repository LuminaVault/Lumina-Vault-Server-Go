package vault

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultprops"
	"lumina/internal/wire"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// Backlog E8.2: POST /v1/vault/query (spec 2026-11-16-e8-properties-and-views, D6).
//
// Every clause is a fixed SQL fragment; property names, values and patterns
// are bound parameters. A property's registry type picks the fragment.

const (
	queryPath         = "/v1/vault/query"
	queryDefaultLimit = 100
	queryMaxLimit     = 500
	queryMaxFilters   = 20
	queryMaxSorts     = 3
	queryMaxColumns   = 200
	queryMaxGroups    = 200
	maxPropertyName   = 200
)

// markdownOnly keeps queries to notes, the files that have properties.
const markdownOnly = `(lower(path) LIKE '%.md' OR lower(path) LIKE '%.markdown')`

// isoDatePattern guards text comparisons of dates: values that aren't ISO
// dates are left out rather than compared as text.
const isoDatePattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}([T ][0-9:.]+(Z|[+-][0-9:]+)?)?$`

// builtins are the row columns usable as properties.
var builtins = map[string]struct {
	expr string
	typ  vaultprops.Type
}{
	"path":      {"path", vaultprops.Text},
	"name":      {`regexp_replace(path, '^.*/', '')`, vaultprops.Text},
	"createdAt": {"created_at", vaultprops.DateTime},
	"updatedAt": {"updated_at", vaultprops.DateTime},
}

type queryRow struct {
	File       fileDTO        `json:"file"`
	Properties map[string]any `json:"properties"`
}

type queryResponse struct {
	Rows       []queryRow          `json:"rows"`
	Columns    []VaultPropertyType `json:"columns"`
	Groups     []VaultQueryGroup   `json:"groups,omitempty"`
	NextCursor *string             `json:"nextCursor,omitempty"`
}

// sqlBuilder collects bound arguments.
type sqlBuilder struct{ args []any }

func (b *sqlBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func badQuery(format string, a ...any) error {
	return wire.NewError(http.StatusBadRequest, fmt.Sprintf(format, a...))
}

func (m *Module) query(c *echo.Context) error {
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
	tenant := a.VaultID
	ctx := c.Request().Context()
	req, err := wire.Bind[VaultQueryRequest](c)
	if err != nil {
		return err
	}

	types := map[string]vaultprops.Type{}
	registry, err := m.store.PropertyTypes(ctx, tenant)
	if err != nil {
		return err
	}
	for _, t := range registry {
		types[strings.ToLower(t.Name)] = t.Type
	}
	typeOf := func(name string) vaultprops.Type {
		if t, ok := types[strings.ToLower(name)]; ok {
			return t
		}
		if t, ok := vaultprops.Reserved[strings.ToLower(name)]; ok {
			return t
		}
		return vaultprops.Text
	}

	b := &sqlBuilder{}
	where := []string{"tenant_id = " + b.arg(tenant), markdownOnly}
	scope, err := scopeClauses(b, req.Scope)
	if err != nil {
		return err
	}
	where = append(where, scope...)
	// The scope alone, for the columns list.
	scopeWhere := strings.Join(where, " AND ")
	scopeArgs := append([]any{}, b.args...)

	if req.Filter != nil {
		if len(*req.Filter) > queryMaxFilters {
			return badQuery("at most %d filters", queryMaxFilters)
		}
		for _, f := range *req.Filter {
			clause, err := filterClause(b, f, typeOf)
			if err != nil {
				return err
			}
			where = append(where, clause)
		}
	}
	filtered := strings.Join(where, " AND ")
	filteredArgs := append([]any{}, b.args...)

	var order []string
	if req.Group != nil && *req.Group != "" {
		expr, err := sortExpr(b, *req.Group)
		if err != nil {
			return err
		}
		order = append(order, expr+" ASC NULLS LAST")
	}
	if req.Sort != nil {
		if len(*req.Sort) > queryMaxSorts {
			return badQuery("at most %d sort keys", queryMaxSorts)
		}
		for _, s := range *req.Sort {
			expr, err := sortExpr(b, s.Property)
			if err != nil {
				return err
			}
			dir := "ASC"
			if s.Dir != nil && *s.Dir == "desc" {
				dir = "DESC"
			} else if s.Dir != nil && *s.Dir != "asc" {
				return badQuery("sort dir is asc or desc")
			}
			order = append(order, expr+" "+dir+" NULLS LAST")
		}
	}
	order = append(order, "path ASC", "id ASC")

	limit := queryDefaultLimit
	if req.Limit != nil {
		limit = max(1, min(*req.Limit, queryMaxLimit))
	}
	offset, err := decodeCursor(req.Cursor)
	if err != nil {
		return err
	}

	sql := `SELECT ` + vaultfiles.Columns + ` FROM vault_files WHERE ` + filtered +
		` ORDER BY ` + strings.Join(order, ", ") +
		` LIMIT ` + b.arg(limit+1) + ` OFFSET ` + b.arg(offset)
	rows, err := m.pool.Query(ctx, sql, b.args...)
	if err != nil {
		return err
	}
	files, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*vaultfiles.VaultFile, error) {
		return vaultfiles.Scan(r)
	})
	if err != nil {
		return err
	}
	out := queryResponse{Rows: []queryRow{}, Columns: []VaultPropertyType{}}
	if len(files) > limit {
		files = files[:limit]
		next := encodeCursor(offset + limit)
		out.NextCursor = &next
	}
	for _, f := range files {
		props := map[string]any(f.Properties)
		if props == nil {
			props = map[string]any{}
		}
		out.Rows = append(out.Rows, queryRow{File: toDTO(f), Properties: props})
	}

	if out.Columns, err = m.queryColumns(c, scopeWhere, scopeArgs, typeOf); err != nil {
		return err
	}
	if req.Group != nil && *req.Group != "" {
		if out.Groups, err = m.queryGroups(c, filtered, filteredArgs, *req.Group, typeOf); err != nil {
			return err
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

func scopeClauses(b *sqlBuilder, s VaultQueryScope) ([]string, error) {
	var out []string
	if s.SpaceId != nil {
		out = append(out, "space_id = "+b.arg(s.SpaceId.UUID))
	}
	if s.Folder != nil && strings.Trim(*s.Folder, "/ ") != "" {
		folder, err := vaultfiles.SanitizePath(strings.Trim(*s.Folder, "/"))
		if err != nil {
			return nil, badQuery("invalid folder")
		}
		out = append(out, `path LIKE `+b.arg(likeEscaper.Replace(folder)+"/%"))
	}
	if s.Tag != nil && strings.TrimPrefix(strings.TrimSpace(*s.Tag), "#") != "" {
		tag := strings.TrimPrefix(strings.TrimSpace(*s.Tag), "#")
		out = append(out, `COALESCE(metadata->'tags', '[]'::jsonb) ? `+b.arg(tag))
	}
	if len(out) == 0 {
		return nil, badQuery("scope needs spaceId, folder or tag")
	}
	return out, nil
}

func checkName(name string) error {
	if name == "" || len(name) > maxPropertyName {
		return badQuery("invalid property name")
	}
	return nil
}

// valueExpr is the property's jsonb value, or a built-in column.
func valueExpr(b *sqlBuilder, name string) (expr string, builtin bool, err error) {
	if err := checkName(name); err != nil {
		return "", false, err
	}
	if bi, ok := builtins[name]; ok {
		return bi.expr, true, nil
	}
	return "properties -> " + b.arg(name), false, nil
}

// sortExpr orders by the jsonb value: values of one type sort naturally
// (numbers numerically, ISO dates as text), and a missing value is NULL.
func sortExpr(b *sqlBuilder, name string) (string, error) {
	expr, _, err := valueExpr(b, name)
	return expr, err
}

func filterClause(b *sqlBuilder, f VaultQueryFilter, typeOf func(string) vaultprops.Type) (string, error) {
	if err := checkName(f.Property); err != nil {
		return "", err
	}
	if bi, ok := builtins[f.Property]; ok {
		return builtinFilter(b, bi.expr, bi.typ, f)
	}
	t := typeOf(f.Property)
	key := b.arg(f.Property)
	val := "properties -> " + key
	text := "properties ->> " + key
	empty := `COALESCE(` + val + `, 'null'::jsonb) IN ('null'::jsonb, '""'::jsonb, '[]'::jsonb)`

	switch f.Op {
	case "isEmpty":
		return empty, nil
	case "isNotEmpty":
		return "NOT " + empty, nil
	}
	if f.Value == nil {
		return "", badQuery("%s needs a value", f.Op)
	}
	switch f.Op {
	case "eq", "neq":
		v, err := normalizeValue(t, f.Value)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			return "", badQuery("bad value")
		}
		p := b.arg(string(encoded)) + "::jsonb"
		// A list matches when it holds the value (multiselect).
		match := `(` + val + ` = ` + p + ` OR (jsonb_typeof(` + val + `) = 'array' AND ` + val + ` @> jsonb_build_array(` + p + `)))`
		if f.Op == "eq" {
			return match, nil
		}
		return `NOT COALESCE(` + match + `, false)`, nil
	case "contains":
		s, ok := f.Value.(string)
		if !ok {
			return "", badQuery("contains takes text")
		}
		if t == vaultprops.MultiSelect {
			return `(jsonb_typeof(` + val + `) = 'array' AND ` + val + ` @> jsonb_build_array(` + b.arg(s) + `::text))`, nil
		}
		return `(jsonb_typeof(` + val + `) = 'string' AND ` + text + ` ILIKE ` + b.arg("%"+likeEscaper.Replace(s)+"%") + `)`, nil
	case "gt", "gte", "lt", "lte":
		op := map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[string(f.Op)]
		switch t {
		case vaultprops.Number:
			n, ok := asNumber(f.Value)
			if !ok {
				return "", badQuery("%s on a number property takes a number", f.Op)
			}
			return `(jsonb_typeof(` + val + `) = 'number' AND (` + text + `)::numeric ` + op + ` ` + b.arg(n) + `::numeric)`, nil
		case vaultprops.Date, vaultprops.DateTime:
			d, err := normalizeDate(f.Value)
			if err != nil {
				return "", err
			}
			return `(jsonb_typeof(` + val + `) = 'string' AND ` + text + ` ~ '` + isoDatePattern + `' AND ` + text + ` ` + op + ` ` + b.arg(d) + `)`, nil
		}
		return "", badQuery("%s needs a number or date property; %q is %s", f.Op, f.Property, t)
	}
	return "", badQuery("unknown op %q", f.Op)
}

func builtinFilter(b *sqlBuilder, expr string, t vaultprops.Type, f VaultQueryFilter) (string, error) {
	switch f.Op {
	case "isEmpty":
		return "false", nil
	case "isNotEmpty":
		return "true", nil
	}
	if t == vaultprops.DateTime {
		op, ok := map[string]string{"eq": "=", "neq": "<>", "gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[string(f.Op)]
		if !ok {
			return "", badQuery("%s doesn't apply to %s", f.Op, f.Property)
		}
		d, err := normalizeDate(f.Value)
		if err != nil {
			return "", err
		}
		return expr + " " + op + " " + b.arg(d) + "::timestamptz", nil
	}
	s, ok := f.Value.(string)
	if !ok {
		return "", badQuery("%s takes text", f.Property)
	}
	switch f.Op {
	case "eq":
		return expr + " = " + b.arg(s), nil
	case "neq":
		return expr + " <> " + b.arg(s), nil
	case "contains":
		return expr + " ILIKE " + b.arg("%"+likeEscaper.Replace(s)+"%"), nil
	}
	return "", badQuery("%s doesn't apply to %s", f.Op, f.Property)
}

// normalizeValue shapes a filter value like the index stores it.
func normalizeValue(t vaultprops.Type, v any) (any, error) {
	switch t {
	case vaultprops.Date, vaultprops.DateTime:
		if s, ok := v.(string); ok {
			if d, err := normalizeDate(s); err == nil {
				return d, nil
			}
		}
	case vaultprops.Number:
		if s, ok := v.(string); ok {
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				return n, nil
			}
		}
	}
	switch v.(type) {
	case string, float64, bool:
		return v, nil
	}
	return nil, badQuery("a filter value is text, a number or a boolean")
}

// normalizeDate returns a date as the index writes it: 2006-01-02, or an
// RFC 3339 time in UTC.
func normalizeDate(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", badQuery("a date value is an ISO 8601 string")
	}
	s = strings.TrimSpace(s)
	if d, err := time.Parse(time.DateOnly, s); err == nil {
		return d.Format(time.DateOnly), nil
	}
	if d, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return d.UTC().Format(time.RFC3339), nil
	}
	return "", badQuery("%q is not an ISO 8601 date", s)
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset)))
}

func decodeCursor(c *string) (int, error) {
	if c == nil || *c == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(*c)
	if err != nil || !strings.HasPrefix(string(raw), "o:") {
		return 0, badQuery("invalid cursor")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "o:"))
	if err != nil || n < 0 {
		return 0, badQuery("invalid cursor")
	}
	return n, nil
}

// queryColumns lists the property names present in scope, with types.
func (m *Module) queryColumns(c *echo.Context, where string, args []any, typeOf func(string) vaultprops.Type) ([]VaultPropertyType, error) {
	rows, err := m.pool.Query(c.Request().Context(),
		`SELECT DISTINCT k FROM vault_files, jsonb_object_keys(properties) AS k WHERE `+where+
			` ORDER BY k LIMIT `+strconv.Itoa(queryMaxColumns), args...)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]VaultPropertyType, 0, len(names))
	for _, n := range names {
		out = append(out, VaultPropertyType{Name: n, Type: VaultPropertyTypeType(typeOf(n)), Options: []string{}})
	}
	return out, nil
}

// queryGroups counts the filtered notes per value of one property. A list
// counts once per item; a date groups by day; notes without it form one
// group with no value.
func (m *Module) queryGroups(c *echo.Context, where string, args []any, name string, typeOf func(string) vaultprops.Type) ([]VaultQueryGroup, error) {
	b := &sqlBuilder{args: append([]any{}, args...)}
	var g string
	if bi, ok := builtins[name]; ok {
		g = "to_jsonb(" + bi.expr + ")"
		if bi.typ == vaultprops.DateTime {
			g = "to_jsonb(to_char(" + bi.expr + " AT TIME ZONE 'UTC', 'YYYY-MM-DD'))"
		}
	} else {
		val := "properties -> " + b.arg(name)
		switch typeOf(name) {
		case vaultprops.Date, vaultprops.DateTime:
			g = `CASE WHEN jsonb_typeof(` + val + `) = 'string' THEN to_jsonb(left(` + val + ` #>> '{}', 10)) END`
		default:
			// One row per list item; a scalar is its own row, and a missing
			// value or an empty list is one row with no value.
			isList := `jsonb_typeof(` + val + `) = 'array'`
			sql := `SELECT x.v, count(*) FROM vault_files,
				LATERAL (
					SELECT e AS v FROM jsonb_array_elements(CASE WHEN ` + isList + ` THEN ` + val + ` ELSE '[]'::jsonb END) AS e
					UNION ALL
					SELECT CASE WHEN ` + isList + ` THEN NULL ELSE ` + val + ` END
					WHERE NOT COALESCE(` + isList + `, false) OR ` + val + ` = '[]'::jsonb
				) AS x
				WHERE ` + where + ` GROUP BY x.v ORDER BY x.v ASC NULLS LAST LIMIT ` + strconv.Itoa(queryMaxGroups)
			return m.collectGroups(c, sql, b.args)
		}
	}
	sql := `SELECT ` + g + ` AS v, count(*) FROM vault_files WHERE ` + where +
		` GROUP BY v ORDER BY v ASC NULLS LAST LIMIT ` + strconv.Itoa(queryMaxGroups)
	return m.collectGroups(c, sql, b.args)
}

func (m *Module) collectGroups(c *echo.Context, sql string, args []any) ([]VaultQueryGroup, error) {
	rows, err := m.pool.Query(c.Request().Context(), sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (VaultQueryGroup, error) {
		var raw []byte
		var count int64
		if err := r.Scan(&raw, &count); err != nil {
			return VaultQueryGroup{}, err
		}
		g := VaultQueryGroup{Count: int(count)}
		if len(raw) > 0 && string(raw) != "null" {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return g, err
			}
			g.Value = v
		}
		return g, nil
	})
}
