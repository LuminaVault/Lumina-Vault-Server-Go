package imports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/diskguard"
	"lumina/internal/modules/imports/db"
	"lumina/internal/notionimport"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultprops"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Backlog E7.1 and E7.2: the Notion import (spec 2026-11-18-e7-notion-import).
//
// The export is never held in memory. The caller spools the upload to disk;
// notionimport.Unpack reads only the ZIP's directory; and each entry is then
// opened on its own: a page or CSV is read whole (16 MiB at most) because it
// is rewritten, an attachment is streamed to its place in the vault. Notes go
// to bulk ingest in batches of a few MiB.

const (
	// MaxNotionNotes is the most notes one import writes; a larger export is
	// refused and the user splits it (spec, owner question 2). The import is one
	// synchronous request and each note costs two embedding round trips, so the
	// cap keeps it within minutes: 2,000 at notionIngestConcurrency.
	MaxNotionNotes = 2000
	// notionIngestConcurrency is how many notes are written and embedded at once.
	notionIngestConcurrency = 8
	// maxNotionReportFiles and maxNotionUnresolved are the wire contract's caps.
	maxNotionReportFiles = 5000
	maxNotionUnresolved  = 50
	maxNotionWarnings    = 200
	// maxNotionAttachment is spec D5's cap on one binary file.
	maxNotionAttachment = 25 << 20
	// maxNotionText caps a page or a CSV, which is read into memory.
	maxNotionText = 16 << 20
	// maxNotionCSVTotal caps the CSVs of one import together: their tables
	// stay in memory until the rows are written.
	maxNotionCSVTotal = 48 << 20
	// A batch of notes is flushed to bulk ingest at either bound.
	notionBatchBytes = 4 << 20
	notionBatchFiles = 200

	// DefaultNotionSpace is the Space an import goes to when none is named.
	DefaultNotionSpace = "Notion"
	notionProvenance   = "notion"
	notionSpaceAbout   = "Imported from Notion."
	untitledNote       = "Untitled"
)

// NotionOptions are the import's query parameters.
type NotionOptions struct {
	// Space is the target Space's name; blank is DefaultNotionSpace.
	Space      string
	DryRun     bool
	OnConflict OnConflict
}

// notionColumn is one database column that becomes a property.
type notionColumn struct {
	name string
	at   int
	typ  vaultprops.Type
	// warned is set once the column has been reported for a value that
	// doesn't fit its type, so the report says it once.
	warned bool
}

// notionDB is one database, read and typed.
type notionDB struct {
	notionimport.Database
	table   notionimport.Table
	columns []*notionColumn
	// names are the CSV's column names: the lines RowPage strips.
	names  []string
	status VaultNotionImportFileStatus
	// pageless are the rows no page matched, with the note path each gets.
	pageless []notionRow
}

type notionRow struct {
	db    *notionDB
	cells []string
	title string
	saved string
}

// notionItem is one line of the report: an entry of the ZIP, or a database
// row that has no page of its own.
type notionItem struct {
	entry     *notionimport.Entry
	entryPath string
	kind      VaultNotionImportFileKind
	saved     string
	status    VaultNotionImportFileStatus
	row       *notionRow
}

type notionRun struct {
	i      *Ingest
	ctx    context.Context //nolint:containedctx // one request's import; the run does not outlive it
	tenant uuid.UUID
	opts   NotionOptions
	space  string
	slug   string

	m       *notionimport.Mapping
	entries map[string]*notionimport.Entry
	dbs     []*notionDB
	rowOf   map[string]*notionRow
	declare []vaultfiles.PropertyType

	spaceID uuid.UUID
	report  VaultNotionImportReport
	wrote   bool

	batch      []FileInput
	batchItems []*notionItem
	batchBytes int
}

// notionError turns an unusable ZIP into the 400 the contract names.
func notionError(err error) error {
	var limit *notionimport.LimitError
	if errors.Is(err, notionimport.ErrNotZip) || errors.Is(err, notionimport.ErrEmpty) || errors.As(err, &limit) {
		return wire.NewError(http.StatusBadRequest, upperFirst(err.Error()))
	}
	return err
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ImportNotion imports the Notion export in zip, or with DryRun reports what
// an import would do and writes nothing. tmpDir is where nested ZIPs are
// extracted; it must be on a volume the disk guard watches.
func (i *Ingest) ImportNotion(ctx context.Context, tenant uuid.UUID, zip io.ReaderAt, size int64, tmpDir string, opts NotionOptions) (VaultNotionImportReport, error) {
	archive, err := notionimport.Unpack(zip, size, tmpDir)
	if err != nil {
		return VaultNotionImportReport{}, notionError(err)
	}
	defer archive.Close()

	space := opts.Space
	if isBlank(space) {
		space = DefaultNotionSpace
	}
	run := &notionRun{
		i: i, ctx: ctx, tenant: tenant, opts: opts, space: space, slug: Slugify(space),
		entries: make(map[string]*notionimport.Entry, len(archive.Entries)),
		rowOf:   map[string]*notionRow{},
		report: VaultNotionImportReport{
			DryRun:     opts.DryRun,
			Links:      VaultNotionImportLinks{UnresolvedList: []VaultNotionImportUnresolvedLink{}},
			Columns:    []VaultNotionImportColumn{},
			Views:      []VaultNotionImportView{},
			Collisions: []VaultNotionImportCollision{},
			Files:      []VaultNotionImportFile{},
			Warnings:   []string{},
		},
	}
	run.report.SpaceSlug = run.slug
	for at := range archive.Entries {
		run.entries[archive.Entries[at].Path] = &archive.Entries[at]
	}
	run.m = notionimport.MapPaths(archive.Entries, run.slug)
	for _, c := range run.m.Collisions {
		run.report.Collisions = append(run.report.Collisions, VaultNotionImportCollision{From: c.From, To: c.To})
	}

	if err := run.planDatabases(archive.Entries); err != nil {
		return VaultNotionImportReport{}, err
	}
	items := run.plan(archive.Entries)
	notes, incoming := 0, int64(0)
	for _, item := range items {
		if item.status != "" {
			continue
		}
		if item.kind == Page || item.kind == Row {
			notes++
		}
		if item.entry != nil && item.kind != Database {
			incoming += item.entry.Size
		}
	}
	if notes > MaxNotionNotes {
		return VaultNotionImportReport{}, wire.NewError(http.StatusBadRequest, fmt.Sprintf(
			"This export has %d notes, and one import takes at most %d. Split the export in Notion (export a few top-level pages at a time) and import each part.",
			notes, MaxNotionNotes))
	}

	if err := run.prepare(incoming); err != nil {
		return VaultNotionImportReport{}, err
	}
	for _, item := range items {
		if err := run.process(item); err != nil {
			return VaultNotionImportReport{}, err
		}
	}
	if err := run.flush(); err != nil {
		return VaultNotionImportReport{}, err
	}
	run.views()
	if run.wrote {
		i.indexer.ReresolvePendingBestEffort(ctx, tenant)
	}
	run.tally(items)
	return run.report, nil
}

func (r *notionRun) warn(format string, args ...any) {
	if len(r.report.Warnings) < maxNotionWarnings {
		r.report.Warnings = append(r.report.Warnings, fmt.Sprintf(format, args...))
	}
}

// planDatabases reads each database's CSV, types its columns and matches its
// rows to their pages. While properties are off it does nothing but say so:
// the row pages then import as ordinary pages.
func (r *notionRun) planDatabases(entries []notionimport.Entry) error {
	if len(r.m.Databases) == 0 {
		return nil
	}
	r.report.Databases = len(r.m.Databases)
	if !vaultfiles.PropertiesEnabled() {
		r.warn("Databases were imported as plain pages, with their properties kept as text, because properties are turned off on this server.")
		return nil
	}
	registry, err := r.i.store.PropertyTypes(r.ctx, r.tenant)
	if err != nil {
		return err
	}
	known := make(map[string]vaultprops.Type, len(registry))
	for _, t := range registry {
		known[strings.ToLower(t.Name)] = t.Type
	}
	pagesIn := map[string][]*notionimport.Entry{}
	for at := range entries {
		e := &entries[at]
		if to, ok := r.m.New[e.Path]; ok && notionimport.KindOf(e.Path) == notionimport.KindPage {
			pagesIn[path.Dir(to)] = append(pagesIn[path.Dir(to)], e)
		}
	}
	csvBytes := int64(0)
	for _, found := range r.m.Databases {
		d := &notionDB{Database: found}
		r.dbs = append(r.dbs, d)
		csvEntry := r.entries[found.CSV]
		data, err := csvEntry.Read(maxNotionText)
		if errors.Is(err, notionimport.ErrEntryTooLarge) || csvBytes+csvEntry.Size > maxNotionCSVTotal {
			d.status = VaultNotionImportFileStatusTooLarge
			r.warn("%s: the CSV is too large (%d MB each, %d MB for all databases); its rows were imported as plain pages",
				d.Name, maxNotionText>>20, maxNotionCSVTotal>>20)
			continue
		}
		csvBytes += csvEntry.Size
		if err == nil {
			d.table, err = notionimport.ParseCSV(data)
		}
		if err != nil {
			d.status = VaultNotionImportFileStatusFailed
			r.warn("%s: the CSV could not be read; its rows were imported as plain pages", d.Name)
			slog.WarnContext(r.ctx, "notion import: csv", "tenant", r.tenant, "entry", found.CSV, "error", err)
			continue
		}
		r.typeColumns(d, known)
		r.matchRows(d, pagesIn[d.Folder])
		r.report.Rows += len(d.table.Rows)
	}
	return nil
}

// typeColumns infers each column's type (spec D6). The first column is the
// title and no property. A name the registry has keeps its type, a reserved
// name its fixed one, and a name two databases share the first one's.
func (r *notionRun) typeColumns(d *notionDB, known map[string]vaultprops.Type) {
	d.names = d.table.Columns
	seen := map[string]bool{}
	for at := 1; at < len(d.table.Columns); at++ {
		name := d.table.Columns[at]
		key := strings.ToLower(name)
		switch {
		case name == "":
			continue
		case len(name) > 200 || strings.ContainsAny(name, "\n\r\x00"):
			r.warn("%s: a column name is too long or spans lines; the column was left out", d.Name)
			continue
		case seen[key]:
			r.warn("%s: column %s appears twice; only the first was imported", d.Name, name)
			continue
		}
		seen[key] = true
		inferred := notionimport.InferColumn(d.table.Column(at))
		typ := inferred.Type
		if fixed, reserved := vaultprops.Reserved[key]; reserved {
			// `due` takes a date or a datetime; the others have one type.
			if key != "due" || (typ != vaultprops.Date && typ != vaultprops.DateTime) {
				typ = fixed
			}
		} else if had, ok := known[key]; ok {
			if had != typ {
				r.warn("%s: column %s keeps the type it already has in this vault (%s, not %s)", d.Name, name, had, typ)
			}
			typ = had
		} else {
			known[key] = typ
			r.declare = append(r.declare, vaultfiles.PropertyType{Name: name, Type: typ, Options: inferred.Options})
		}
		if inferred.Ranges && (typ == vaultprops.Date || typ == vaultprops.DateTime) {
			r.warn("%s: column %s had date ranges; kept the start date", d.Name, name)
		}
		d.columns = append(d.columns, &notionColumn{name: name, at: at, typ: typ})
		r.report.Columns = append(r.report.Columns, VaultNotionImportColumn{
			Database: d.Name, Name: name, Type: VaultNotionImportColumnType(typ),
		})
	}
}

// matchRows pairs each CSV row with the page in the database's folder that has
// its title: by the page's heading first, then by its file name, since Notion
// shortens long names. A row without a page gets a path of its own; a page
// without a row stays an ordinary page.
func (r *notionRun) matchRows(d *notionDB, pages []*notionimport.Entry) {
	type candidate struct {
		entry *notionimport.Entry
		title string
		stem  string
		used  bool
	}
	candidates := make([]*candidate, 0, len(pages))
	for _, e := range pages {
		c := &candidate{entry: e, stem: strings.TrimSuffix(path.Base(r.m.New[e.Path]), ".md")}
		clean, _ := notionimport.StripID(vaultfiles.DeleteExtension(path.Base(e.Path)))
		c.title = clean
		if data, err := e.Read(maxNotionText); err == nil {
			c.title = swiftstr.TrimWhitespacesAndNewlines(Title(string(bytes.ToValidUTF8(data, []byte("�"))), clean))
		}
		candidates = append(candidates, c)
	}
	take := func(fits func(*candidate) bool) *candidate {
		for _, c := range candidates {
			if !c.used && fits(c) {
				c.used = true
				return c
			}
		}
		return nil
	}
	for _, cells := range d.table.Rows {
		title := strings.TrimSpace(cells[0])
		row := &notionRow{db: d, cells: cells, title: title}
		if title == "" {
			row.title = untitledNote
		}
		match := take(func(c *candidate) bool { return c.title == row.title })
		if match == nil {
			slug := vaultfiles.SlugifySegment(row.title)
			match = take(func(c *candidate) bool { return c.stem == slug })
		}
		if match != nil {
			row.saved = r.m.New[match.entry.Path]
			r.rowOf[match.entry.Path] = row
			continue
		}
		row.saved = r.m.Allocate(d.Folder, row.title)
		d.pageless = append(d.pageless, *row)
	}
	orphans := 0
	for _, c := range candidates {
		if !c.used {
			orphans++
		}
	}
	if orphans > 0 {
		r.warn("%s: %d page(s) in the database's folder have no row in the CSV; imported as plain pages", d.Name, orphans)
	}
}

// plan lists the report's items in ZIP order, with the status already known
// for anything that will not be written.
func (r *notionRun) plan(entries []notionimport.Entry) []*notionItem {
	byCSV := map[string]*notionDB{}
	for _, d := range r.dbs {
		byCSV[d.CSV] = d
		for _, other := range d.Others {
			byCSV[other] = d
		}
	}
	items := make([]*notionItem, 0, len(entries))
	for at := range entries {
		e := &entries[at]
		item := &notionItem{entry: e, entryPath: e.Path, saved: r.m.New[e.Path]}
		items = append(items, item)
		switch {
		case item.saved == "":
			item.kind, item.status = Other, VaultNotionImportFileStatusHidden
		case notionimport.KindOf(e.Path) == notionimport.KindPage:
			item.kind = Page
			if row, ok := r.rowOf[e.Path]; ok {
				item.kind, item.row = Row, row
			}
		case notionimport.KindOf(e.Path) == notionimport.KindCSV:
			item.kind = Database
			d := byCSV[e.Path]
			if d == nil {
				// Properties are off: the CSV itself is not imported.
				item.status = VaultNotionImportFileStatusUnsupported
				break
			}
			item.status = d.status
			if e.Path == d.CSV && d.status == "" {
				for at := range d.pageless {
					row := &d.pageless[at]
					items = append(items, &notionItem{entryPath: e.Path, kind: Row, saved: row.saved, row: row})
				}
			}
		default:
			item.kind = Attachment
			if _, ok := vaultfiles.ContentTypeForExtension(vaultfiles.PathExtension(path.Base(item.saved))); !ok {
				item.status = VaultNotionImportFileStatusUnsupported
			} else if e.Size > maxNotionAttachment {
				item.status = VaultNotionImportFileStatusTooLarge
			}
		}
	}
	return items
}

// prepare is everything before the first file: the disk check, the Space and
// the registry for an import; only a look at the Space for a dry run.
func (r *notionRun) prepare(incoming int64) error {
	if r.opts.DryRun {
		id, err := r.i.q.SpaceIDBySlug(r.ctx, db.SpaceIDBySlugParams{TenantID: r.tenant, Slug: r.slug})
		if err == nil {
			r.report.SpaceId = &wire.UUID{UUID: id}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return nil
	}
	if r.i.guard != nil {
		if err := r.i.guard.Check(incoming); err != nil {
			return err
		}
	}
	id, slug, err := r.i.ensureSpace(r.ctx, r.tenant, r.space, notionSpaceAbout)
	if err != nil {
		return err
	}
	r.spaceID, r.slug = id, slug
	r.report.SpaceId, r.report.SpaceSlug = &wire.UUID{UUID: id}, slug
	if err := r.i.paths.EnsureTenantDirectories(r.tenant); err != nil {
		return err
	}
	// Types go in before any note, so the first note's indexing finds them
	// instead of inferring from one value (E8 spec, D8).
	return r.i.store.DeclarePropertyTypes(r.ctx, r.tenant, r.declare)
}

func (r *notionRun) process(item *notionItem) error {
	if item.status != "" {
		return nil
	}
	switch item.kind {
	case Page, Row:
		content, ok := r.noteContent(item)
		if !ok {
			return nil
		}
		if r.opts.DryRun {
			item.status = r.dryNote(item.saved, content)
			return nil
		}
		return r.enqueue(item, content)
	case Attachment:
		if r.opts.DryRun {
			item.status = r.dryAttachment(item)
			return nil
		}
		status, err := r.attachment(item)
		var full *diskguard.InsufficientStorageError
		if errors.As(err, &full) {
			return err
		}
		if err != nil {
			status = VaultNotionImportFileStatusFailed
			slog.ErrorContext(r.ctx, "notion import: attachment", "tenant", r.tenant, "entry", item.entryPath, "error", err)
		}
		item.status = status
	case Database:
		item.status = r.done()
	case Other:
	}
	return nil
}

// done is the status of something written: planned on a dry run.
func (r *notionRun) done() VaultNotionImportFileStatus {
	if r.opts.DryRun {
		return VaultNotionImportFileStatusPlanned
	}
	return VaultNotionImportFileStatusImported
}

// noteContent builds a note: the page with its links rewritten, and for a
// database row the property block replaced by frontmatter from the CSV.
func (r *notionRun) noteContent(item *notionItem) (string, bool) {
	body := ""
	if item.entry != nil {
		data, err := item.entry.Read(maxNotionText)
		if errors.Is(err, notionimport.ErrEntryTooLarge) {
			item.status = VaultNotionImportFileStatusTooLarge
			return "", false
		}
		if err != nil {
			item.status = VaultNotionImportFileStatusFailed
			slog.ErrorContext(r.ctx, "notion import: read", "tenant", r.tenant, "entry", item.entryPath, "error", err)
			return "", false
		}
		body = string(bytes.ToValidUTF8(data, []byte("�")))
		if item.row != nil {
			_, body = notionimport.RowPage(body, item.row.db.names)
		}
		var stats notionimport.LinkStats
		body, stats = notionimport.RewriteLinks(body, item.entry.Path, r.m)
		links := &r.report.Links
		links.Resolved += stats.Resolved
		links.External += stats.External
		links.Unresolved += stats.Unresolved
		for _, target := range stats.UnresolvedTargets {
			if len(links.UnresolvedList) < maxNotionUnresolved {
				links.UnresolvedList = append(links.UnresolvedList, VaultNotionImportUnresolvedLink{Page: item.saved, Target: target})
			}
		}
	} else if item.row != nil {
		body = "# " + item.row.title + "\n"
	}
	if item.row != nil {
		body = r.frontmatter(body, item.row)
	}
	return body, true
}

// frontmatter writes the row's cells as the note's properties, in CSV order.
// A cell that doesn't fit its column's type is written as text (spec D6).
func (r *notionRun) frontmatter(body string, row *notionRow) string {
	doc := []byte(body)
	for _, col := range row.db.columns {
		value, ok := notionimport.Convert(col.typ, row.cells[col.at])
		if !ok {
			value = strings.TrimSpace(row.cells[col.at])
			if !col.warned {
				col.warned = true
				r.warn("%s: column %s has values that are not a %s; kept them as text", row.db.Name, col.name, col.typ)
			}
		}
		if value == nil {
			continue
		}
		next, err := vaultprops.Apply(doc, map[string]any{col.name: value}, nil)
		if err != nil {
			if !col.warned {
				col.warned = true
				r.warn("%s: column %s could not be written as a property", row.db.Name, col.name)
			}
			slog.WarnContext(r.ctx, "notion import: property", "tenant", r.tenant, "column", col.name, "error", err)
			continue
		}
		doc = next
	}
	return string(doc)
}

// dryNote is what bulk ingest would do with the note, without doing it.
func (r *notionRun) dryNote(saved, content string) VaultNotionImportFileStatus {
	if swiftstr.TrimWhitespacesAndNewlines(content) == "" {
		return VaultNotionImportFileStatusEmpty
	}
	existing, err := r.i.store.FindByPath(r.ctx, r.tenant, saved)
	if err != nil {
		slog.ErrorContext(r.ctx, "notion import: dry run lookup", "tenant", r.tenant, "path", saved, "error", err)
		return VaultNotionImportFileStatusFailed
	}
	sum := sha256.Sum256([]byte(content))
	switch {
	case existing == nil:
	case existing.SHA256 == hex.EncodeToString(sum[:]):
		return VaultNotionImportFileStatusUnchanged
	case r.opts.OnConflict == OnConflictSkip:
		return VaultNotionImportFileStatusExists
	}
	return VaultNotionImportFileStatusPlanned
}

// enqueue adds a note to the batch, and flushes a full one.
func (r *notionRun) enqueue(item *notionItem, content string) error {
	relative := r.m.Relative(item.saved)
	// Bulk ingest slugifies the path again; the map already did, so it must
	// come back the same or links would point at the wrong note.
	if SafePath(r.slug, relative) != item.saved {
		item.status = VaultNotionImportFileStatusFailed
		slog.ErrorContext(r.ctx, "notion import: path not stable", "tenant", r.tenant, "entry", item.entryPath, "path", item.saved)
		return nil
	}
	in := FileInput{Path: relative, Content: content}
	if item.row != nil {
		in.Title = item.row.title
	}
	r.batch, r.batchItems = append(r.batch, in), append(r.batchItems, item)
	r.batchBytes += len(content)
	if r.batchBytes >= notionBatchBytes || len(r.batch) >= notionBatchFiles {
		return r.flush()
	}
	return nil
}

func (r *notionRun) flush() error {
	if len(r.batch) == 0 {
		return nil
	}
	provenance := notionProvenance
	result, err := r.i.IngestBatchWith(r.ctx, r.tenant, r.space, r.batch, IngestOptions{
		Provenance: &provenance, OnConflict: r.opts.OnConflict, SpaceDescription: notionSpaceAbout, DeferReresolve: true,
		Concurrency: notionIngestConcurrency,
	})
	if err != nil {
		return err
	}
	for at, f := range result.Files {
		switch f.Status {
		case FileImported, FileUpdated:
			r.wrote = true
			r.batchItems[at].status = VaultNotionImportFileStatus(f.Status)
		case FileUnchanged, FileEmpty, FileExists, FileHidden, FileUnsupported:
			r.batchItems[at].status = VaultNotionImportFileStatus(f.Status)
		case FileCollision, FileFailed:
			// A collision can't happen: the map gives every note its own path.
			r.batchItems[at].status = VaultNotionImportFileStatusFailed
		}
	}
	r.batch, r.batchItems, r.batchBytes = r.batch[:0], r.batchItems[:0], 0
	return nil
}

// dryAttachment is attachment without the write. Bytes are compared only
// when a file of the same size is already there.
func (r *notionRun) dryAttachment(item *notionItem) VaultNotionImportFileStatus {
	existing, err := r.i.store.FindByPath(r.ctx, r.tenant, item.saved)
	if err != nil {
		return VaultNotionImportFileStatusFailed
	}
	if existing == nil {
		return VaultNotionImportFileStatusPlanned
	}
	if existing.SizeBytes == item.entry.Size {
		if rc, err := item.entry.Open(); err == nil {
			sum := sha256.New()
			_, err = io.Copy(sum, rc)
			_ = rc.Close()
			if err == nil && hex.EncodeToString(sum.Sum(nil)) == existing.SHA256 {
				return VaultNotionImportFileStatusUnchanged
			}
		}
	}
	if r.opts.OnConflict == OnConflictSkip {
		return VaultNotionImportFileStatusExists
	}
	return VaultNotionImportFileStatusPlanned
}

// attachment streams one binary entry into the vault (spec D5): to a hidden
// file beside its target, then renamed over it, so it is never in memory and
// never half-written. The row is unprocessed, as an upload's is, so the
// ingestion worker picks up images and PDFs.
func (r *notionRun) attachment(item *notionItem) (VaultNotionImportFileStatus, error) {
	contentType, _ := vaultfiles.ContentTypeForExtension(vaultfiles.PathExtension(path.Base(item.saved)))
	if r.i.guard != nil {
		if err := r.i.guard.Check(item.entry.Size); err != nil {
			return "", err
		}
	}
	existing, err := r.i.store.FindByPath(r.ctx, r.tenant, item.saved)
	if err != nil {
		return "", err
	}
	target, err := vaultfiles.ResolveInside(r.i.paths.Raw(r.tenant), item.saved)
	if err != nil {
		return "", err
	}
	if err := vaultfiles.CreateParent(target); err != nil {
		return "", err
	}
	src, err := item.entry.Open()
	if err != nil {
		return "", err
	}
	aux, size, digest, err := vaultfiles.SpoolBeside(target, io.LimitReader(src, maxNotionAttachment+1))
	_ = src.Close()
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(aux)
		}
	}()
	switch {
	case size > maxNotionAttachment:
		return VaultNotionImportFileStatusTooLarge, nil
	case size == 0:
		return VaultNotionImportFileStatusEmpty, nil
	case existing != nil && existing.SHA256 == digest:
		return VaultNotionImportFileStatusUnchanged, nil
	case existing != nil && r.opts.OnConflict == OnConflictSkip:
		return VaultNotionImportFileStatusExists, nil
	}
	if err := os.Rename(aux, target); err != nil {
		return "", err
	}
	keep = true
	vaultgit.Touch(r.tenant, "import", "", "")
	provenance := notionProvenance
	if _, err := r.i.store.Upsert(r.ctx, vaultfiles.UpsertParams{
		TenantID: r.tenant, SpaceID: &r.spaceID, Path: item.saved, ContentType: contentType,
		SizeBytes: size, SHA256: digest, Metadata: &vaultfiles.Metadata{Provenance: &provenance},
	}); err != nil {
		return "", err
	}
	if existing != nil {
		return VaultNotionImportFileStatusUpdated, nil
	}
	return VaultNotionImportFileStatusImported, nil
}

// views saves one view per database: its folder, the CSV's columns in order
// (spec D6). A re-import finds the view it made before.
func (r *notionRun) views() {
	for _, d := range r.dbs {
		if d.status != "" {
			continue
		}
		name := strings.TrimSpace(d.Name)
		if name == "" {
			name = untitledNote
		}
		if len(name) > 200 {
			name = swiftstr.Prefix(name, 100)
		}
		columns := make([]string, 0, len(d.columns))
		for _, col := range d.columns {
			columns = append(columns, col.name)
		}
		if len(columns) > vaultfiles.MaxViewColumns {
			columns = columns[:vaultfiles.MaxViewColumns]
			r.warn("%s: the view shows the first %d columns", d.Name, vaultfiles.MaxViewColumns)
		}
		view := VaultNotionImportView{Name: name, Folder: d.Folder}
		if !r.opts.DryRun {
			id, _, err := r.i.store.EnsureFolderView(r.ctx, r.tenant, r.tenant, name, d.Folder, columns)
			if err != nil {
				slog.ErrorContext(r.ctx, "notion import: view", "tenant", r.tenant, "database", d.Name, "error", err)
				r.warn("%s: the view could not be saved", d.Name)
				continue
			}
			if id == uuid.Nil {
				r.warn("%s: no view was saved, because this vault already has %d views", d.Name, vaultfiles.MaxViews)
				continue
			}
			view.Id = &wire.UUID{UUID: id}
		}
		r.report.Views = append(r.report.Views, view)
	}
}

// tally counts the items and lists them, up to the contract's cap.
func (r *notionRun) tally(items []*notionItem) {
	for _, item := range items {
		switch item.kind {
		case Page:
			r.report.Pages++
		case Attachment:
			r.report.Attachments++
		case Row, Database, Other:
		}
		switch item.status {
		case VaultNotionImportFileStatusImported, VaultNotionImportFileStatusUpdated:
			if item.kind != Database {
				r.report.Written++
			}
		case VaultNotionImportFileStatusFailed:
			r.report.Failed++
		case VaultNotionImportFileStatusPlanned:
		case VaultNotionImportFileStatusUnchanged, VaultNotionImportFileStatusExists, VaultNotionImportFileStatusEmpty,
			VaultNotionImportFileStatusTooLarge, VaultNotionImportFileStatusHidden, VaultNotionImportFileStatusUnsupported:
			if item.kind != Database {
				r.report.Skipped++
			}
		}
		if len(r.report.Files) == maxNotionReportFiles {
			continue
		}
		file := VaultNotionImportFile{Entry: item.entryPath, Kind: item.kind, Status: item.status}
		if item.saved != "" {
			saved := item.saved
			file.SavedPath = &saved
		}
		r.report.Files = append(r.report.Files, file)
	}
	if len(items) > maxNotionReportFiles {
		r.warn("The export has %d files; the report lists the first %d.", len(items), maxNotionReportFiles)
	}
}
