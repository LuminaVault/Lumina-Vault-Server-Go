package vaultfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/vaultprops"
	"os"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Backlog E8.2: note properties (spec 2026-11-16-e8-properties-and-views, D1–D3).
//
// While enabled, every content write of a Markdown file (Upsert, Insert,
// UpdateContent, UpdateIngested) re-reads the file's frontmatter into
// vault_files.properties, records types for names seen the first time, and
// projects title, tags, due and done into the sidecar metadata. Off, nothing
// here runs and the columns keep their defaults.
//
// UpdateMetadata does not re-index: it is how todos set `done` in the sidecar,
// and re-projecting the file right after would undo that for a note whose
// frontmatter has `done`. Those writers move to the frontmatter in part 3.

var propertiesOn atomic.Bool

// EnableProperties turns indexing on or off for the process
// (VAULT_PROPERTIES_ENABLED).
func EnableProperties(on bool) { propertiesOn.Store(on) }

// PropertiesEnabled reports the flag.
func PropertiesEnabled() bool { return propertiesOn.Load() }

// IsMarkdownPath: the files that carry properties.
func IsMarkdownPath(path string) bool {
	p := strings.ToLower(path)
	return strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".markdown")
}

// frontmatterReadLimit reads enough of a file for any frontmatter Parse
// accepts, plus the fences.
const frontmatterReadLimit = vaultprops.MaxFrontmatterBytes + 4096

// readFrontmatter returns the start of the file at path: all of it, or the
// first frontmatterReadLimit bytes with truncated set.
func readFrontmatter(rawRoot, path string) (data []byte, truncated bool, err error) {
	target, err := ResolveInside(rawRoot, path)
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open(target) //nolint:gosec // target comes from ResolveInside
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, frontmatterReadLimit+1))
	if err != nil {
		return nil, false, err
	}
	if len(buf) > frontmatterReadLimit {
		return buf[:frontmatterReadLimit], true, nil
	}
	return buf, false, nil
}

// ParseFileProperties reads a note's properties from disk. A file whose
// opening `---` is not closed within the read limit is too large: invalid.
func ParseFileProperties(rawRoot, path string) (vaultprops.Props, error) {
	data, truncated, err := readFrontmatter(rawRoot, path)
	if err != nil {
		return nil, err
	}
	if truncated && !vaultprops.HasFrontmatter(data) && strings.HasPrefix(string(data), "---") {
		return vaultprops.Props{}, fmt.Errorf("%w: larger than %d bytes", vaultprops.ErrInvalid, vaultprops.MaxFrontmatterBytes)
	}
	return vaultprops.Parse(data)
}

// reindexAfterWrite runs Reindex when enabled. A failure is logged, never
// returned: the write it follows has already succeeded, and the backfill
// (part 3) or the next write repairs the index.
func (s *Store) reindexAfterWrite(ctx context.Context, id uuid.UUID, path string) {
	if !PropertiesEnabled() || id == uuid.Nil || (path != "" && !IsMarkdownPath(path)) {
		return
	}
	if err := s.Reindex(ctx, id); err != nil {
		slog.WarnContext(ctx, "vault properties reindex", "id", id, "err", err)
	}
}

// Reindex re-reads one row's file and stores its properties, any parse
// error, new property types and the sidecar projection. A non-Markdown row,
// a gone row or a gone file is a no-op.
func (s *Store) Reindex(ctx context.Context, id uuid.UUID) error {
	row, err := s.find(ctx, `id = $1`, id)
	if err != nil || row == nil || !IsMarkdownPath(row.Path) {
		return err
	}
	props, perr := ParseFileProperties(s.paths.Raw(row.TenantID), row.Path)
	if errors.Is(perr, os.ErrNotExist) {
		// Nothing to read; mark it so the backfill moves on.
		_, err := s.pool.Exec(ctx, `UPDATE vault_files SET properties_indexed_at = NOW() WHERE id = $1`, id)
		return err
	}
	var errText *string
	if perr != nil {
		if !errors.Is(perr, vaultprops.ErrInvalid) {
			return perr
		}
		msg := perr.Error()
		errText = &msg
		props = vaultprops.Props{}
	}
	encoded, err := json.Marshal(props)
	if err != nil {
		return err
	}

	projected := project(row.Metadata, vaultprops.Project(props))
	var metadata *string
	if projected != nil {
		if metadata, err = encodeColumn(projected); err != nil {
			return err
		}
	}
	// No updated_at bump: the write that triggered this already moved it.
	if _, err := s.pool.Exec(ctx, `
		UPDATE vault_files
		SET properties = $2::jsonb, properties_error = $3, metadata = COALESCE($4::jsonb, metadata),
			properties_indexed_at = NOW()
		WHERE id = $1`, id, string(encoded), errText, metadata); err != nil {
		return err
	}
	return s.recordTypes(ctx, row.TenantID, props)
}

// project applies the frontmatter's title, tags, due and done to the
// sidecar. It returns nil when nothing changes (so the column isn't written).
func project(current *Metadata, p vaultprops.Projection) *Metadata {
	if p.Title == nil && p.Tags == nil && p.DueAt == nil && p.Done == nil {
		return nil
	}
	var next Metadata
	if current != nil {
		next = *current
	}
	if p.Title != nil {
		next.Title = p.Title
	}
	if p.Tags != nil {
		next.Tags = append([]string{}, (*p.Tags)...)
	}
	if p.DueAt != nil {
		next.DueAt = p.DueAt
	}
	if p.Done != nil {
		next.Done = p.Done
	}
	if current != nil && reflect.DeepEqual(next, *current) {
		return nil
	}
	return &next
}

// recordTypes adds a registry row for each name seen the first time, with
// the type its value suggests. Existing rows (any case) are left alone; an
// empty value doesn't decide a type.
func (s *Store) recordTypes(ctx context.Context, tenant uuid.UUID, props vaultprops.Props) error {
	names := make([]string, 0, len(props))
	types := make([]string, 0, len(props))
	seen := map[string]bool{}
	for name, v := range props {
		key := strings.ToLower(name)
		if v == nil || seen[key] {
			continue
		}
		seen[key] = true
		names = append(names, name)
		types = append(types, string(vaultprops.Infer(name, v)))
	}
	if len(names) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO vault_property_types (tenant_id, name, type)
		SELECT $1, n, t FROM unnest($2::text[], $3::text[]) AS x(n, t)
		ON CONFLICT (tenant_id, lower(name)) DO NOTHING`, tenant, names, types)
	return err
}

// DeclarePropertyTypes adds a registry row for each name the vault doesn't
// have yet, with the given type and options: the Notion import's column types,
// written before its notes so the first note doesn't infer something else. A
// name the registry already has (any case) keeps its type, and reserved names
// are skipped: their types are fixed.
func (s *Store) DeclarePropertyTypes(ctx context.Context, tenant uuid.UUID, types []PropertyType) error {
	var names, kinds, options []string
	seen := map[string]bool{}
	for _, t := range types {
		key := strings.ToLower(t.Name)
		if _, reserved := vaultprops.Reserved[key]; reserved || seen[key] || t.Name == "" || !t.Type.Valid() {
			continue
		}
		seen[key] = true
		opts := t.Options
		if opts == nil {
			opts = []string{}
		}
		encoded, err := json.Marshal(opts)
		if err != nil {
			return err
		}
		names, kinds, options = append(names, t.Name), append(kinds, string(t.Type)), append(options, string(encoded))
	}
	if len(names) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO vault_property_types (tenant_id, name, type, options)
		SELECT $1, n, t, o::jsonb FROM unnest($2::text[], $3::text[], $4::text[]) AS x(n, t, o)
		ON CONFLICT (tenant_id, lower(name)) DO NOTHING`, tenant, names, kinds, options)
	return err
}

// PropertyType is one vault_property_types row.
type PropertyType struct {
	Name    string
	Type    vaultprops.Type
	Options []string
}

// PropertyTypes returns the vault's registry, by name.
func (s *Store) PropertyTypes(ctx context.Context, tenant uuid.UUID) ([]PropertyType, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, type, options FROM vault_property_types WHERE tenant_id = $1 ORDER BY lower(name)`, tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PropertyType, error) {
		var pt PropertyType
		var typ string
		var options []byte
		if err := row.Scan(&pt.Name, &typ, &options); err != nil {
			return pt, err
		}
		pt.Type = vaultprops.Type(typ)
		pt.Options = []string{}
		if len(options) > 0 {
			if err := json.Unmarshal(options, &pt.Options); err != nil {
				return pt, fmt.Errorf("vault_property_types.options: %w", err)
			}
		}
		return pt, nil
	})
}

// SetPropertyType declares a property's type (E8.4's "change type"). It only
// changes the registry; no file is rewritten. options nil keeps the old ones.
func (s *Store) SetPropertyType(ctx context.Context, tenant uuid.UUID, name string, t vaultprops.Type, options []string) (PropertyType, error) {
	var opts *string
	if options != nil {
		b, err := json.Marshal(options)
		if err != nil {
			return PropertyType{}, err
		}
		o := string(b)
		opts = &o
	}
	var out PropertyType
	var typ string
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		INSERT INTO vault_property_types (tenant_id, name, type, options)
		VALUES ($1, $2, $3, COALESCE($4::jsonb, '[]'))
		ON CONFLICT (tenant_id, lower(name)) DO UPDATE
		SET type = EXCLUDED.type,
			options = COALESCE($4::jsonb, vault_property_types.options),
			updated_at = NOW()
		RETURNING name, type, options`, tenant, name, string(t), opts).Scan(&out.Name, &typ, &raw)
	if err != nil {
		return PropertyType{}, err
	}
	out.Type = vaultprops.Type(typ)
	out.Options = []string{}
	if err := json.Unmarshal(raw, &out.Options); err != nil {
		return PropertyType{}, err
	}
	return out, nil
}

// UnindexedNotes returns up to limit Markdown rows never indexed, by id after
// afterID (uuid.Nil to start), for the backfill.
func (s *Store) UnindexedNotes(ctx context.Context, afterID uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM vault_files
		WHERE properties_indexed_at IS NULL AND id > $1
			AND (lower(path) LIKE '%.md' OR lower(path) LIKE '%.markdown')
		ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// WriteProperties sets keys in a Markdown note's frontmatter and saves the
// row with metadata m, so the projection (D1) reads back what the caller set
// and the file and the sidecar can't drift. It reports false, writing
// nothing, when properties are off, the row isn't a note, the file is gone,
// or its frontmatter can't be edited (invalid YAML, anchors, ...): the caller
// then keeps the sidecar-only write.
func (s *Store) WriteProperties(ctx context.Context, row *VaultFile, set map[string]any, m *Metadata) (bool, error) {
	if !PropertiesEnabled() || row == nil || !IsMarkdownPath(row.Path) || len(set) == 0 {
		return false, nil
	}
	target, err := ResolveInside(s.paths.Raw(row.TenantID), row.Path)
	if err != nil {
		return false, nil //nolint:nilerr // an unreachable file keeps the sidecar write
	}
	current, err := os.ReadFile(target) //nolint:gosec // target comes from ResolveInside
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	next, err := vaultprops.Apply(current, set, nil)
	if errors.Is(err, vaultprops.ErrInvalid) || errors.Is(err, vaultprops.ErrUneditable) {
		slog.InfoContext(ctx, "vault properties: frontmatter not editable, sidecar only", "id", row.ID, "err", err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(next) != string(current) {
		if err := ReplaceFile(target, next); err != nil {
			return false, err
		}
	}
	sum := sha256.Sum256(next)
	if err := s.UpdateContent(ctx, row.ID, int64(len(next)), hex.EncodeToString(sum[:]), m); err != nil {
		return false, err
	}
	return true, nil
}
