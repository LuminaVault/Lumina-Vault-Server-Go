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
	"lumina/internal/vaultpaths"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FileInput is one file of a bulk import.
type FileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
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
	Imported  int
	Skipped   int
	Failed    int
}

// IngestBatch is ingestBatch. The disk check comes first, so a refused import leaves
// nothing half-written.
func (i *Ingest) IngestBatch(ctx context.Context, tenant uuid.UUID, spaceName string, files []FileInput) (IngestResult, error) {
	if i.guard != nil {
		var total int64
		for _, f := range files {
			total += int64(len(f.Content))
		}
		if err := i.guard.Check(total); err != nil {
			return IngestResult{}, err
		}
	}
	spaceID, slug, err := i.ensureSpace(ctx, tenant, spaceName)
	if err != nil {
		return IngestResult{}, err
	}
	if err := i.paths.EnsureTenantDirectories(tenant); err != nil {
		return IngestResult{}, err
	}
	result := IngestResult{SpaceID: spaceID, SpaceSlug: slug}
	for _, file := range files {
		ingested, err := i.ingestOne(ctx, tenant, spaceID, slug, file)
		switch {
		case err != nil:
			result.Failed++
			slog.ErrorContext(ctx, "vault ingest failed", "tenant", tenant, "path", file.Path, "error", err)
		case ingested:
			result.Imported++
		default:
			result.Skipped++
		}
	}
	return result, nil
}

func (i *Ingest) ensureSpace(ctx context.Context, tenant uuid.UUID, name string) (uuid.UUID, string, error) {
	slug := Slugify(name)
	id, err := i.q.SpaceIDBySlug(ctx, db.SpaceIDBySlugParams{TenantID: tenant, Slug: slug})
	if err == nil {
		return id, slug, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", err
	}
	description, icon := "Imported from your Hermes vault.", "tray.and.arrow.down"
	return i.imports.spaces.CreateSpace(ctx, tenant, name, slug, &description, &icon)
}

// ingestOne is ingestOne. It reports whether the file was (re)ingested: an empty or
// unchanged one is not.
//
// The path goes through SafePath and ResolveInside, not SanitizePath, which would
// refuse the spaces and emoji Obsidian filenames carry. Same as Swift.
func (i *Ingest) ingestOne(ctx context.Context, tenant, spaceID uuid.UUID, slug string, input FileInput) (bool, error) {
	content := input.Content
	if swiftstr.TrimWhitespacesAndNewlines(content) == "" {
		return false, nil
	}
	safe := SafePath(slug, input.Path)
	data := []byte(content)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	existing, err := i.store.FindByPath(ctx, tenant, safe)
	if err != nil {
		return false, err
	}
	if existing != nil && existing.SHA256 == digest {
		return false, nil
	}
	target, err := vaultfiles.ResolveInside(i.paths.Raw(tenant), safe)
	if err != nil {
		return false, err
	}
	if err := vaultfiles.CreateParent(target); err != nil {
		return false, err
	}
	if err := vaultfiles.WriteAtomic(target, data); err != nil {
		return false, err
	}
	title := Title(content, vaultfiles.LastPathComponent(input.Path))
	metadata := &vaultfiles.Metadata{Title: &title}
	var saved uuid.UUID
	if existing != nil {
		saved = existing.ID
		if err := i.store.UpdateIngested(ctx, saved, spaceID, int64(len(data)), digest, metadata); err != nil {
			return false, err
		}
	} else {
		saved, err = i.store.Insert(ctx, vaultfiles.UpsertParams{
			TenantID: tenant, SpaceID: &spaceID, Path: safe, ContentType: "text/markdown",
			SizeBytes: int64(len(data)), SHA256: digest, Processed: true, Metadata: metadata,
		})
		if err != nil {
			return false, err
		}
	}
	// The file is already written, so an embedding outage stores the memory without a
	// vector rather than half-import it.
	vec, err := embedding.EmbedForWrite(ctx, i.embeddings, content, tenant, "vault.ingest")
	if err != nil {
		return false, err
	}
	memoryID, err := i.memories.IDBySourceVaultFileID(ctx, tenant, saved)
	if err != nil {
		return false, err
	}
	var id uuid.UUID
	if memoryID != nil {
		id = *memoryID
		if _, err := i.memories.UpdateContent(ctx, tenant, id, content, vec, nil); err != nil {
			return false, err
		}
	} else {
		created, err := i.memories.Create(ctx, memorystore.CreateParams{
			Tenant: tenant, Content: content, Embedding: vec, SourceVaultFileID: &saved,
			SpaceID: &spaceID, ReviewState: "auto",
		})
		if err != nil {
			return false, err
		}
		id = created.ID
	}
	if i.indexer != nil {
		i.indexer.IndexBestEffort(ctx, chunkindex.IndexParams{
			Tenant: tenant, MemoryID: id, VaultFileID: &saved, SpaceID: &spaceID,
			SourcePath: &safe, Content: content,
		})
	}
	return true, nil
}

// Title is title(from:fallback:): the first H1 among the file's first 21 nonempty lines,
// else the filename without its extension. Splitting on the Character "\n" leaves CRLF
// lines joined, and the last piece holds the rest of the file, as `maxSplits: 20` does.
func Title(content, fallback string) string {
	rest := content
	for pieces := 0; pieces < 21 && rest != ""; pieces++ {
		at := -1
		for start := 0; pieces < 20; {
			next := strings.IndexByte(rest[start:], '\n')
			if next < 0 {
				break
			}
			if next+start == 0 || rest[next+start-1] != '\r' {
				at = next + start
				break
			}
			start += next + 1
		}
		line := rest
		if at >= 0 {
			line, rest = rest[:at], rest[at+1:]
		} else {
			rest = ""
		}
		if line == "" {
			pieces--
			continue
		}
		t := swiftstr.TrimWhitespaces(line)
		if swiftstr.HasASCIIPrefix(t, "# ") {
			return swiftstr.TrimWhitespaces(t[2:])
		}
	}
	return deleteExtension(fallback)
}

// deleteExtension is `(name as NSString).deletingPathExtension` on Linux, for a single
// path component: "/" is "", and a trailing dot or a leading one is no extension.
func deleteExtension(name string) string {
	if name == "/" {
		return ""
	}
	ext := vaultfiles.PathExtension(name)
	if ext == "" {
		return name
	}
	return strings.TrimSuffix(name, "."+ext)
}

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

func slugifySegment(s string) string {
	var out []string
	lastDash := false
	for _, ch := range swiftstr.Characters(vaultfiles.Lowercased(s)) {
		if swiftstr.IsLetterOrNumber(ch) || ch == "." || ch == "_" {
			out = append(out, ch)
			lastDash = false
		} else if !lastDash {
			out = append(out, "-")
			lastDash = true
		}
	}
	trimmed := strings.Trim(strings.Join(out, ""), "-")
	if trimmed == "" {
		return "x"
	}
	return swiftstr.Prefix(trimmed, 80)
}

func isBlank(s string) bool { return swiftstr.TrimWhitespacesAndNewlines(s) == "" }
