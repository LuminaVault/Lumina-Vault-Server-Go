package imports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"lumina/internal/chunkindex"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/memorystore"
	"lumina/internal/modules/imports/db"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultpaths"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FileInput is one file of a bulk import.
type FileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Title, when set, is the note's title instead of its first H1 or its file
	// name. Server-side callers only: a note that opens with a long
	// frontmatter block has its heading past the lines Title reads.
	Title string `json:"-"`
}

// Ingest is VaultIngestService: each file becomes a blob, a row and an embedded memory
// in one Space. Re-importing unchanged files skips them.
type Ingest struct {
	q          *db.Queries
	imports    *Service
	paths      vaultpaths.Paths
	store      *vaultfiles.Store
	memories   *memorystore.Repository
	embeddings embedding.Service
	indexer    *chunkindex.Indexer
	guard      *diskguard.Guard
}

// NewIngest builds the service.
func NewIngest(pool *pgxpool.Pool, imports *Service, memories *memorystore.Repository, embeddings embedding.Service, indexer *chunkindex.Indexer, guard *diskguard.Guard) *Ingest {
	return &Ingest{
		q: db.New(pool), imports: imports, paths: imports.paths, store: imports.store,
		memories: memories, embeddings: embeddings, indexer: indexer, guard: guard,
	}
}

// IngestResult is Result.
type IngestResult struct {
	SpaceID   uuid.UUID
	SpaceSlug string
	// Imported counts FileImported and FileUpdated; Failed counts FileFailed;
	// Skipped counts every other status.
	Imported int
	Skipped  int
	Failed   int
	// VaultFileIDs are the rows written or updated by this batch (the Hermes
	// mirror feeds them straight into a memory compile).
	VaultFileIDs []uuid.UUID
	// Files is one entry per input, in input order.
	Files []FileResult
}

// FileStatus is what happened to one input file.
type FileStatus string

const (
	// FileImported is a new vault file.
	FileImported FileStatus = "imported"
	// FileUpdated replaced an existing file at the same path (OnConflictOverwrite).
	FileUpdated FileStatus = "updated"
	// FileUnchanged already exists with the same bytes.
	FileUnchanged FileStatus = "unchanged"
	// FileEmpty has no content but whitespace.
	FileEmpty FileStatus = "empty"
	// FileHidden sits under a dot-folder or is a dotfile (.obsidian/, .trash/).
	FileHidden FileStatus = "hidden"
	// FileUnsupported has an extension other than .md, .markdown or .txt.
	FileUnsupported FileStatus = "unsupported"
	// FileCollision slugifies to the same vault path as an earlier file of the
	// same request, which was kept.
	FileCollision FileStatus = "collision"
	// FileExists would replace a different existing file (OnConflictSkip).
	FileExists FileStatus = "exists"
	// FileFailed could not be written; the server log has why.
	FileFailed FileStatus = "failed"
)

// FileResult reports one input. SavedPath is the vault path the input maps to:
// where it was written, or for a collision or an existing file, the path that
// is already taken. It is nil for hidden and unsupported inputs.
type FileResult struct {
	Path      string
	SavedPath *string
	Status    FileStatus
}

// OnConflict says what a file does to a different one already at its path.
type OnConflict int

const (
	// OnConflictOverwrite replaces it: re-syncing a vault updates the notes.
	OnConflictOverwrite OnConflict = iota
	// OnConflictSkip leaves it and reports FileExists: a first import never
	// replaces a note the user already has.
	OnConflictSkip
)

// IngestOptions are the knobs of IngestBatchWith.
type IngestOptions struct {
	// Provenance is stamped on every written row's metadata.
	Provenance *string
	OnConflict OnConflict
	// SpaceDescription describes the Space when this batch creates it; empty
	// is the Hermes import's.
	SpaceDescription string
	// DeferReresolve skips the pending-wikilink pass after the writes: a caller
	// that ingests in several batches runs it once at the end.
	DeferReresolve bool
	// Concurrency is how many files are written and embedded at once. Each file
	// waits on two embedding round trips (the note, then its chunks), so a large
	// import is bound by provider latency, not by the database. 0 or 1 is one at
	// a time, as before.
	Concurrency int
}

// textExtensions are the inputs bulk import writes; anything else (images,
// .canvas, .pdf) would become a junk note, so it is reported unsupported.
var textExtensions = map[string]bool{"md": true, "markdown": true, "txt": true}

// IngestBatch is ingestBatch. The disk check comes first, so a refused import leaves
// nothing half-written.
func (i *Ingest) IngestBatch(ctx context.Context, tenant uuid.UUID, spaceName string, files []FileInput) (IngestResult, error) {
	return i.IngestBatchWithProvenance(ctx, tenant, spaceName, files, nil)
}

// IngestBatchWithProvenance is ingestBatch(..., provenance:): provenance is
// stamped on every written row's metadata (hermes-mirror, hermes-session).
func (i *Ingest) IngestBatchWithProvenance(ctx context.Context, tenant uuid.UUID, spaceName string, files []FileInput, provenance *string) (IngestResult, error) {
	return i.IngestBatchWith(ctx, tenant, spaceName, files, IngestOptions{Provenance: provenance})
}

// IngestBatchWith is the batch with every option. Hidden and unsupported inputs
// are filtered first, then inputs that slugify to a path an earlier input of the
// request already holds. After the writes, the tenant's pending wikilinks are
// re-resolved, so a link to a file later in the batch (or in an earlier batch)
// resolves.
func (i *Ingest) IngestBatchWith(ctx context.Context, tenant uuid.UUID, spaceName string, files []FileInput, opts IngestOptions) (IngestResult, error) {
	if i.guard != nil {
		var total int64
		for _, f := range files {
			total += int64(len(f.Content))
		}
		if err := i.guard.Check(total); err != nil {
			return IngestResult{}, err
		}
	}
	spaceID, slug, err := i.ensureSpace(ctx, tenant, spaceName, opts.SpaceDescription)
	if err != nil {
		return IngestResult{}, err
	}
	if err := i.paths.EnsureTenantDirectories(tenant); err != nil {
		return IngestResult{}, err
	}
	result := IngestResult{SpaceID: spaceID, SpaceSlug: slug, Files: make([]FileResult, 0, len(files))}
	entries := make([]FileResult, len(files))
	saved := make([]*uuid.UUID, len(files))
	// Classify in order first, so collisions are decided by input order whatever
	// the concurrency; then write the files that passed.
	var work []int
	claimed := make(map[string]bool, len(files))
	for n, file := range files {
		entries[n] = FileResult{Path: file.Path}
		switch {
		case isHiddenPath(file.Path):
			entries[n].Status = FileHidden
		case !isTextPath(file.Path):
			entries[n].Status = FileUnsupported
		default:
			safe := SafePath(slug, file.Path)
			entries[n].SavedPath = &safe
			if claimed[safe] {
				entries[n].Status = FileCollision
				break
			}
			claimed[safe] = true
			work = append(work, n)
		}
	}
	ingest := func(n int) {
		file := files[n]
		id, status, err := i.ingestOne(ctx, tenant, spaceID, *entries[n].SavedPath, file, opts)
		entries[n].Status = status
		if err != nil {
			entries[n].Status = FileFailed
			slog.ErrorContext(ctx, "vault ingest failed", "tenant", tenant, "path", file.Path, "error", err)
		} else {
			saved[n] = id
		}
	}
	if opts.Concurrency <= 1 {
		for _, n := range work {
			ingest(n)
		}
	} else {
		sem := make(chan struct{}, opts.Concurrency)
		var wg sync.WaitGroup
		for _, n := range work {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				ingest(n)
			}()
		}
		wg.Wait()
	}
	for n, entry := range entries {
		if saved[n] != nil {
			result.VaultFileIDs = append(result.VaultFileIDs, *saved[n])
		}
		switch entry.Status {
		case FileImported, FileUpdated:
			result.Imported++
		case FileFailed:
			result.Failed++
		default:
			result.Skipped++
		}
		result.Files = append(result.Files, entry)
	}
	if result.Imported > 0 && !opts.DeferReresolve {
		i.indexer.ReresolvePendingBestEffort(ctx, tenant)
	}
	return result, nil
}

// isHiddenPath is true when any segment of the source path is a dotfile or
// dot-folder: Obsidian's .obsidian/ and .trash/, git's .git/. "." and ".." are
// not hidden; SafePath drops them.
func isHiddenPath(rawPath string) bool {
	for _, p := range strings.Split(rawPath, "/") {
		if p != "." && p != ".." && strings.HasPrefix(p, ".") {
			return true
		}
	}
	return false
}

// isTextPath is true for .md, .markdown and .txt, and for a name with no
// extension at all, which bulk import has always read as markdown.
func isTextPath(rawPath string) bool {
	ext := vaultfiles.PathExtension(vaultfiles.LastPathComponent(rawPath))
	return ext == "" || textExtensions[vaultfiles.Lowercased(ext)]
}

func (i *Ingest) ensureSpace(ctx context.Context, tenant uuid.UUID, name, describe string) (uuid.UUID, string, error) {
	slug := Slugify(name)
	id, err := i.q.SpaceIDBySlug(ctx, db.SpaceIDBySlugParams{TenantID: tenant, Slug: slug})
	if err == nil {
		return id, slug, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", err
	}
	description, icon := "Imported from your Hermes vault.", "tray.and.arrow.down"
	if describe != "" {
		description = describe
	}
	return i.imports.spaces.CreateSpace(ctx, tenant, name, slug, &description, &icon)
}

// ingestOne is ingestOne. It returns the row id when the file was (re)ingested: an
// empty or unchanged one is not.
//
// The path goes through SafePath and ResolveInside, not SanitizePath, which would
// refuse the spaces and emoji Obsidian filenames carry. Same as Swift.
func (i *Ingest) ingestOne(ctx context.Context, tenant, spaceID uuid.UUID, safe string, input FileInput, opts IngestOptions) (*uuid.UUID, FileStatus, error) {
	content := input.Content
	if swiftstr.TrimWhitespacesAndNewlines(content) == "" {
		return nil, FileEmpty, nil
	}
	data := []byte(content)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	existing, err := i.store.FindByPath(ctx, tenant, safe)
	if err != nil {
		return nil, FileFailed, err
	}
	if existing != nil && existing.SHA256 == digest {
		return nil, FileUnchanged, nil
	}
	if existing != nil && opts.OnConflict == OnConflictSkip {
		return nil, FileExists, nil
	}
	target, err := vaultfiles.ResolveInside(i.paths.Raw(tenant), safe)
	if err != nil {
		return nil, FileFailed, err
	}
	if err := vaultfiles.CreateParent(target); err != nil {
		return nil, FileFailed, err
	}
	if err := vaultfiles.WriteAtomic(target, data); err != nil {
		return nil, FileFailed, err
	}
	vaultgit.Touch(tenant, "import", "", "")
	title := input.Title
	if title == "" {
		title = Title(content, vaultfiles.LastPathComponent(input.Path))
	}
	metadata := &vaultfiles.Metadata{Title: &title, Provenance: opts.Provenance}
	var saved uuid.UUID
	status := FileImported
	if existing != nil {
		saved = existing.ID
		status = FileUpdated
		if err := i.store.UpdateIngested(ctx, saved, spaceID, int64(len(data)), digest, metadata); err != nil {
			return nil, FileFailed, err
		}
	} else {
		saved, err = i.store.Insert(ctx, vaultfiles.UpsertParams{
			TenantID: tenant, SpaceID: &spaceID, Path: safe, ContentType: "text/markdown",
			SizeBytes: int64(len(data)), SHA256: digest, Processed: true, Metadata: metadata,
		})
		if err != nil {
			return nil, FileFailed, err
		}
	}
	// The file is already written, so an embedding outage stores the memory without a
	// vector rather than half-import it.
	vec, err := embedding.EmbedForWrite(ctx, i.embeddings, content, tenant, "vault.ingest")
	if err != nil {
		return nil, FileFailed, err
	}
	memoryID, err := i.memories.IDBySourceVaultFileID(ctx, tenant, saved)
	if err != nil {
		return nil, FileFailed, err
	}
	var id uuid.UUID
	if memoryID != nil {
		id = *memoryID
		if _, err := i.memories.UpdateContent(ctx, tenant, id, content, vec, nil); err != nil {
			return nil, FileFailed, err
		}
	} else {
		created, err := i.memories.Create(ctx, memorystore.CreateParams{
			Tenant: tenant, Content: content, Embedding: vec, SourceVaultFileID: &saved,
			SpaceID: &spaceID, ReviewState: "auto",
		})
		if err != nil {
			return nil, FileFailed, err
		}
		id = created.ID
	}
	if i.indexer != nil {
		i.indexer.IndexBestEffort(ctx, chunkindex.IndexParams{
			Tenant: tenant, MemoryID: id, VaultFileID: &saved, SpaceID: &spaceID,
			SourcePath: &safe, Content: content,
		})
	}
	return &saved, status, nil
}

// Title is vaultfiles.MarkdownTitle, kept here for bulk import's callers and tests.
func Title(content, fallback string) string { return vaultfiles.MarkdownTitle(content, fallback) }

func deleteExtension(name string) string { return vaultfiles.DeleteExtension(name) }

// SafePath is safePath: each segment slugified, subfolders kept, `.` and `..` dropped,
// always ending .md, under the Space's slug.
func SafePath(slug, rawPath string) string {
	parts := []string{}
	for _, p := range strings.Split(rawPath, "/") {
		if p != "" && p != "." && p != ".." {
			parts = append(parts, p)
		}
	}
	segments := make([]string, 0, len(parts)+1)
	segments = append(segments, slug)
	for at, part := range parts {
		if at == len(parts)-1 {
			segments = append(segments, slugifySegment(deleteExtension(part))+".md")
		} else {
			segments = append(segments, slugifySegment(part))
		}
	}
	if len(parts) == 0 {
		segments = append(segments, "note.md")
	}
	return strings.Join(segments, "/")
}

func slugifySegment(s string) string { return vaultfiles.SlugifySegment(s) }

func isBlank(s string) bool { return swiftstr.TrimWhitespacesAndNewlines(s) == "" }
