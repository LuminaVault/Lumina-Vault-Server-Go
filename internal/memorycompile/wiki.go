package memorycompile

import (
	"context"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func utf8Valid(b []byte) bool { return utf8.Valid(b) }

const wikiFileMode = 0o644

// rebuildWiki is rebuildWiki: a deterministic rebuild of wiki/ from the current vault
// files and non-rejected memories. Best-effort: a failure is logged and swallowed.
func (s *Service) rebuildWiki(ctx context.Context, tenant uuid.UUID, at time.Time) {
	if err := s.writeWiki(ctx, tenant, at); err != nil {
		slog.WarnContext(ctx, "wiki rebuild failed", "tenant", ids.Upper(tenant), "error", err)
	}
}

type wikiEntry struct{ name, page string }

func (s *Service) writeWiki(ctx context.Context, tenant uuid.UUID, at time.Time) error {
	wikiRoot := s.opt.Paths.Wiki(tenant)
	rawRoot := s.opt.Paths.Raw(tenant)
	if err := os.MkdirAll(wikiRoot, 0o755); err != nil { //nolint:gosec // as Foundation's createDirectory
		return err
	}
	slugByID := map[uuid.UUID]string{}
	nameBySlug := map[string]string{"inbox": "Inbox"}
	rows, err := s.pool.Query(ctx, `SELECT id, slug, name FROM spaces WHERE tenant_id = $1`, tenant)
	if err != nil {
		return err
	}
	var id uuid.UUID
	var slug, name string
	if _, err := pgx.ForEachRow(rows, []any{&id, &slug, &name}, func() error {
		slugByID[id] = slug
		nameBySlug[slug] = name
		return nil
	}); err != nil {
		return err
	}

	fileRows, err := s.pool.Query(ctx, `SELECT `+vaultfiles.Columns+` FROM vault_files WHERE tenant_id = $1 ORDER BY created_at DESC`, tenant)
	if err != nil {
		return err
	}
	files, err := pgx.CollectRows(fileRows, func(row pgx.CollectableRow) (*vaultfiles.VaultFile, error) { return vaultfiles.Scan(row) })
	if err != nil {
		return err
	}
	memRows, err := s.pool.Query(ctx, `SELECT content FROM memories WHERE tenant_id = $1 AND review_state <> $2 ORDER BY created_at DESC`, tenant, memorystore.ReviewRejected)
	if err != nil {
		return err
	}
	mems, err := pgx.CollectRows(memRows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	sourcesDir := filepath.Join(wikiRoot, "sources")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil { //nolint:gosec // as Foundation's createDirectory
		return err
	}
	bySlug := map[string][]wikiEntry{}
	for _, f := range files {
		slug := "inbox"
		if f.SpaceID != nil {
			if known, ok := slugByID[*f.SpaceID]; ok {
				slug = known
			}
		}
		pageSlug := WikiSlug(f.Path)
		base := vaultfiles.LastPathComponent(f.Path)
		body := "_(binary asset: " + f.ContentType + ")_"
		if isTextLike(f.ContentType) {
			// Swift appends the row's path to the root unchecked, as here.
			if data, err := os.ReadFile(filepath.Join(rawRoot, f.Path)); err == nil && utf8.Valid(data) { //nolint:gosec // the row's own path, as Swift
				body = string(data)
			}
		}
		spaceName := nameBySlug[slug]
		if spaceName == "" {
			spaceName = slug
		}
		page := "---\nsource_path: \"" + f.Path + "\"\nspace: \"" + spaceName + "\"\ncontent_type: \"" + f.ContentType + "\"\n---\n\n# " + base + "\n\n" + body
		_ = os.WriteFile(filepath.Join(sourcesDir, pageSlug+".md"), []byte(page), wikiFileMode) //nolint:gosec // best-effort, shared PVC
		bySlug[slug] = append(bySlug[slug], wikiEntry{base, pageSlug})
	}
	for slug, entries := range bySlug {
		lines := make([]string, len(entries))
		for i, e := range entries {
			lines[i] = "- [" + e.name + "](sources/" + e.page + ".md)"
		}
		title := nameBySlug[slug]
		if title == "" {
			title = slug
		}
		plural := "s"
		if len(entries) == 1 {
			plural = ""
		}
		page := "# " + title + "\n\n" + strconv.Itoa(len(entries)) + " source" + plural + ".\n\n" + strings.Join(lines, "\n")
		_ = os.WriteFile(filepath.Join(wikiRoot, slug+".md"), []byte(page), wikiFileMode) //nolint:gosec // best-effort, shared PVC
	}
	shown := mems[:min(len(mems), 1000)]
	lines := make([]string, len(shown))
	for i, m := range shown {
		lines[i] = "- " + m
	}
	memPage := "# What Lumina knows\n\n" + strconv.Itoa(len(mems)) + " memories (rejected excluded).\n\n" + strings.Join(lines, "\n")
	_ = os.WriteFile(filepath.Join(wikiRoot, "memories.md"), []byte(memPage), wikiFileMode) //nolint:gosec // best-effort, shared PVC

	stamp, _ := wire.Time{Time: at}.MarshalJSON()
	var idx strings.Builder
	idx.WriteString("# Vault wiki\n\nLast compiled: " + strings.Trim(string(stamp), `"`) + "\n\n## Spaces\n")
	for _, slug := range slices.Sorted(maps.Keys(bySlug)) {
		title := nameBySlug[slug]
		if title == "" {
			title = slug
		}
		idx.WriteString("- [" + title + "](" + slug + ".md) — " + strconv.Itoa(len(bySlug[slug])) + " sources\n")
	}
	idx.WriteString("\n[What Lumina knows](memories.md) — " + strconv.Itoa(len(mems)) + " memories\n")
	_ = os.WriteFile(filepath.Join(wikiRoot, "index.md"), []byte(idx.String()), wikiFileMode) //nolint:gosec // best-effort, shared PVC
	return nil
}

// WikiSlug is wikiSlug: a vault path's basename, lowercased, with non-alphanumerics
// collapsed to "-".
func WikiSlug(path string) string {
	base := strings.ToLower(vaultfiles.LastPathComponent(path))
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	var out strings.Builder
	lastDash := false
	for _, r := range base {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			out.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	trimmed := strings.Trim(out.String(), "-")
	if trimmed == "" {
		return "untitled"
	}
	return trimmed
}
