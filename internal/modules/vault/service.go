package vault

import (
	"context"
	"log/slog"
	"lumina/internal/achievementsink"
	"lumina/internal/chunkindex"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/modules/achievements"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Files writes, moves and deletes a vault's files the way the /v1/vault/files
// routes do, for callers that have no HTTP request (the assistant's tools,
// GitHub sync). The caller owns the access check.
//
// Its path locks live in this process. Two replicas that save one path at the
// same moment are not serialized by them: the If-Match check of each still
// runs against the file on the shared disk, and the write is an atomic
// rename, so the result is one whole version and never a mix, but the later
// rename wins whatever either check saw. GitHub sync's pull relies on the
// same locks and has the same limit (accepted in the E4.9 review, 2026-11-26):
// a save on another replica that lands between a pull's look at a file and
// its write can be replaced by the remote version.
type Files struct {
	store      *vaultfiles.Store
	memories   *memorystore.Repository
	paths      vaultpaths.Paths
	now        func() time.Time
	embeddings embedding.Service
	indexer    *chunkindex.Indexer
	guard      *diskguard.Guard
	locks      *pathLocks
	sink       *achievementsink.Sink
	latches    *onboardlatch.Latches
	analytics  *posthog.Client
	filing     func(tenant, file uuid.UUID)
}

// FilesOptions are what Write needs beyond the store. Every field may be left
// out: no Embeddings means no note memory, no Indexer no chunks or links, no
// Guard no disk check, and Sink, Latches, Analytics and Filing do nothing when
// nil.
type FilesOptions struct {
	Embeddings embedding.Service
	Indexer    *chunkindex.Indexer
	Guard      *diskguard.Guard
	Sink       *achievementsink.Sink
	Latches    *onboardlatch.Latches
	Analytics  *posthog.Client
	Filing     func(tenant, file uuid.UUID)
}

// NewFiles builds a Files over the same store and paths the vault module uses.
// Its path locks are its own: a caller that must not race the routes' saves
// uses the module's Files (Module.Files) instead.
func NewFiles(pool *pgxpool.Pool, paths vaultpaths.Paths, opts ...FilesOptions) *Files {
	var o FilesOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return newFiles(vaultfiles.NewStore(pool, paths), memorystore.New(pool, nil), paths, &pathLocks{}, o)
}

func newFiles(store *vaultfiles.Store, memories *memorystore.Repository, paths vaultpaths.Paths, locks *pathLocks, o FilesOptions) *Files {
	return &Files{
		store:      store,
		memories:   memories,
		paths:      paths,
		now:        time.Now,
		embeddings: o.Embeddings,
		indexer:    o.Indexer,
		guard:      o.Guard,
		locks:      locks,
		sink:       o.Sink,
		latches:    o.Latches,
		analytics:  o.Analytics,
		filing:     o.Filing,
	}
}

// WriteInput is one file write (spec 2026-11-19-e4-github-sync D7).
type WriteInput struct {
	Vault, Actor uuid.UUID
	// Path must already be what SanitizePath returns; anything else is a 400.
	Path string
	// Data may be empty: the HTTP handlers refuse an empty body themselves.
	Data []byte
	// ContentType "" is the extension's first allowed type. Either way it must
	// be allowed for the extension (415 otherwise).
	ContentType string
	// SpaceID nil keeps the row's Space, or, for a new row, files it by the
	// path's first folder (SpaceIDForPath).
	SpaceID *uuid.UUID
	// KeepNilSpace is upload's own rule for a nil SpaceID: the row keeps its
	// Space in the database, but this write's memory, chunks, analytics and
	// auto-filing treat the file as unfiled, without looking the row up.
	KeepNilSpace bool
	Metadata     *vaultfiles.Metadata
	// Note nil is isNotePath(Path). A note gets a recall memory, chunks and links.
	Note      *bool
	Processed bool
	// IfMatch is an If-Match value: sha256 values the file on disk must still
	// hash to, or "*". "" checks nothing.
	IfMatch string
	// IfAbsent is If-None-Match: *, create only.
	IfAbsent bool
	// Reason and Source are passed to vaultgit.Touch. Reason "" is "upload".
	Reason, Source string
	// DeferReresolve leaves the vault's pending links to the caller, which runs
	// ReresolvePendingBestEffort once per batch.
	DeferReresolve bool
	// Quiet skips the achievement, the onboarding latch, analytics and
	// auto-filing: the write was not a user's upload.
	Quiet bool
}

// WriteResult is what a write saved. MemoryID is nil when the file got no
// note memory (not a note, not UTF-8, no embeddings, or the memory failed).
type WriteResult struct {
	ID          uuid.UUID
	Path        string
	ContentType string
	Size        int
	SHA256      string
	SpaceID     *uuid.UUID
	MemoryID    *uuid.UUID
}

// Write is the vault's one write pipeline, what upload and replace do once the
// request is read: disk guard, tenant directories, ResolveInside, the path lock
// around the If-Match / If-None-Match check and the write, the history touch,
// the row and its actors, the upload side effects unless Quiet, the note
// memory, the chunk index and the pending-link re-resolve unless deferred. The
// file is written before the row and isn't undone if the row fails, as in
// Swift.
func (f *Files) Write(ctx context.Context, in WriteInput) (*WriteResult, error) {
	safe, err := vaultfiles.SanitizePath(in.Path)
	if err != nil {
		return nil, err
	}
	if safe != in.Path {
		return nil, wire.NewError(http.StatusBadRequest, "path is not sanitized")
	}
	ext := vaultfiles.Lowercased(vaultfiles.PathExtension(safe))
	contentType := in.ContentType
	if contentType == "" {
		contentType, _ = vaultfiles.ContentTypeForExtension(ext)
	}
	if err := vaultfiles.ValidateContentType(contentType, ext); err != nil {
		return nil, err
	}
	isNote := isNotePath(safe)
	if in.Note != nil {
		isNote = *in.Note
	}
	meta := in.Metadata
	// A markdown note without a sidecar title is titled from its first H1, else its
	// filename, the same rule bulk import uses.
	if isNote && isMarkdownPath(safe) && (meta == nil || meta.Title == nil) {
		if content, ok := vaultfiles.DecodeUTF8(in.Data); ok {
			title := vaultfiles.MarkdownTitle(content, vaultfiles.LastPathComponent(safe))
			next := vaultfiles.Metadata{}
			if meta != nil {
				next = *meta
			}
			next.Title = &title
			meta = &next
		}
	}
	if f.guard != nil {
		if err := f.guard.Check(int64(len(in.Data))); err != nil {
			return nil, err
		}
	}
	tenant := in.Vault
	if err := f.paths.EnsureTenantDirectories(tenant); err != nil {
		return nil, err
	}
	target, err := vaultfiles.ResolveInside(f.paths.Raw(tenant), safe)
	if err != nil {
		return nil, err
	}
	if err := f.heldGuard(ctx, tenant, safe); err != nil {
		return nil, err
	}
	// E5.1: the check and the write hold one lock, so two saves made from the
	// same version can't both pass.
	unlock := f.locks.lock(tenant, safe)
	if in.IfMatch != "" {
		if err := checkIfMatch(in.IfMatch, target); err != nil {
			unlock()
			return nil, err
		}
	}
	// Create-only: an import never replaces a note that already has the name,
	// so the client can pick another one.
	if in.IfAbsent {
		if err := checkAbsent(target); err != nil {
			unlock()
			return nil, err
		}
	}
	err = vaultfiles.ReplaceFile(target, in.Data)
	unlock()
	if err != nil {
		return nil, err
	}
	reason := in.Reason
	if reason == "" {
		reason = "upload"
	}
	vaultgit.Touch(tenant, reason, ids.Upper(in.Actor), in.Source)

	spaceID, err := f.spaceFor(ctx, tenant, safe, in.SpaceID, in.KeepNilSpace)
	if err != nil {
		return nil, err
	}
	digest := sha256Hex(in.Data)
	savedID, err := f.store.Upsert(ctx, vaultfiles.UpsertParams{
		TenantID:    tenant,
		SpaceID:     spaceID,
		Path:        safe,
		ContentType: contentType,
		SizeBytes:   int64(len(in.Data)),
		SHA256:      digest,
		Processed:   in.Processed,
		Metadata:    meta,
	})
	if err != nil {
		return nil, err
	}
	if err := f.store.StampActors(ctx, savedID, in.Actor); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "vault upload", "tenant", ids.Upper(tenant), "path", safe, "bytes", len(in.Data), "reason", reason)

	if !in.Quiet {
		f.sink.Enqueue(tenant, achievements.EventVaultUploaded)
		f.latches.Latch(ctx, onboardlatch.FirstCapture, tenant)
		f.analytics.Capture("vault_file_uploaded", ids.Upper(in.Actor), map[string]any{
			"content_type": contentType,
			"is_note":      isNote,
			"has_space":    spaceID != nil,
		})
	}
	var memoryID *uuid.UUID
	if isNote {
		memoryID = f.upsertNoteMemory(ctx, tenant, savedID, spaceID, safe, in.Data, meta)
	}
	// UpdateContent drops a note's chunks, so every note write re-indexes: chunks for
	// retrieval and [[wikilinks]] for backlinks and the graph. Then the vault's
	// pending links re-resolve, since this note may be the one they point at.
	if memoryID != nil && f.indexer != nil {
		if content, ok := vaultfiles.DecodeUTF8(in.Data); ok {
			f.indexer.IndexBestEffort(ctx, chunkindex.IndexParams{
				Tenant: tenant, MemoryID: *memoryID, VaultFileID: &savedID, SpaceID: spaceID,
				SourcePath: &safe, Content: content,
			})
			// Links elsewhere that were waiting for this note now find it.
			if !in.DeferReresolve {
				f.indexer.ReresolvePendingBestEffort(ctx, tenant)
			}
		}
	}
	// An unfiled note gets a Space suggestion in the background.
	if !in.Quiet && isNote && spaceID == nil && f.filing != nil {
		f.filing(tenant, savedID)
	}
	return &WriteResult{
		ID: savedID, Path: safe, ContentType: contentType, Size: len(in.Data), SHA256: digest,
		SpaceID: spaceID, MemoryID: memoryID,
	}, nil
}

// ReresolvePending re-resolves the vault's pending [[wikilinks]]: what a
// caller that wrote a batch with DeferReresolve runs once at its end.
func (f *Files) ReresolvePending(ctx context.Context, vault uuid.UUID) {
	if f.indexer != nil {
		f.indexer.ReresolvePendingBestEffort(ctx, vault)
	}
}

// spaceFor is WriteInput.SpaceID's rule: a given Space wins, else (unless
// keepNil) the row's, else the path's first folder.
func (f *Files) spaceFor(ctx context.Context, tenant uuid.UUID, path string, given *uuid.UUID, keepNil bool) (*uuid.UUID, error) {
	if given != nil || keepNil {
		return given, nil
	}
	row, err := f.store.FindByPath(ctx, tenant, path)
	if err != nil {
		return nil, err
	}
	if row != nil {
		return row.SpaceID, nil
	}
	return f.store.SpaceIDForPath(ctx, tenant, path)
}

// upsertNoteMemory gives a written note its recall memory, keyed on the vault
// file. The file write already succeeded, so a failure is logged and the result
// has no memory. Text that isn't UTF-8 has no memory at all. Indexing (chunks and
// wikilinks) runs after it in Write, since UpdateContent deletes the note's chunks.
func (f *Files) upsertNoteMemory(ctx context.Context, tenant, fileID uuid.UUID, spaceID *uuid.UUID, path string, data []byte, sidecar *vaultfiles.Metadata) *uuid.UUID {
	content, ok := vaultfiles.DecodeUTF8(data)
	if f.embeddings == nil || !ok || content == "" {
		return nil
	}
	id, err := f.writeNoteMemory(ctx, tenant, fileID, spaceID, content, sidecar)
	if err != nil {
		slog.ErrorContext(ctx, "note memory upsert failed", "tenant", ids.Upper(tenant), "path", path, "error", err)
		return nil
	}
	return &id
}

func (f *Files) writeNoteMemory(ctx context.Context, tenant, fileID uuid.UUID, spaceID *uuid.UUID, content string, sidecar *vaultfiles.Metadata) (uuid.UUID, error) {
	// An embedding outage still writes the memory, without a vector.
	vec, err := embedding.EmbedForWrite(ctx, f.embeddings, content, tenant, "vault.note")
	if err != nil {
		return uuid.Nil, err
	}
	existing, err := f.memories.IDBySourceVaultFileID(ctx, tenant, fileID)
	if err != nil {
		return uuid.Nil, err
	}
	if existing != nil {
		if _, err := f.memories.UpdateContent(ctx, tenant, *existing, content, vec, nil); err != nil {
			return uuid.Nil, err
		}
		return *existing, nil
	}
	var tags []string
	if sidecar != nil {
		tags = sidecar.Tags
	}
	created, err := f.memories.Create(ctx, memorystore.CreateParams{
		Tenant:            tenant,
		Content:           content,
		Embedding:         vec,
		Tags:              tags,
		SourceVaultFileID: &fileID,
		SpaceID:           spaceID,
		ReviewState:       memorystore.ReviewAuto,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return created.ID, nil
}

// Find returns the row of the file at path, or the route's 404.
func (f *Files) Find(ctx context.Context, vault uuid.UUID, path string) (*vaultfiles.VaultFile, error) {
	safe, err := vaultfiles.SanitizePath(path)
	if err != nil {
		return nil, err
	}
	row, err := f.store.FindByPath(ctx, vault, safe)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errFileNotFound
	}
	return row, nil
}

// Move renames the file at path to newPath, both relative to the vault's raw
// root, and returns its row.
func (f *Files) Move(ctx context.Context, vault uuid.UUID, path, newPath string) (*vaultfiles.VaultFile, error) {
	return f.MoveIfMatch(ctx, vault, path, newPath, "")
}

// MoveIfMatch is Move for a caller that planned the move from a known version
// of the file (GitHub sync's pull): with a non-empty ifMatch (the If-Match
// syntax Write takes: sha256 values, or "*") the move only happens while the
// bytes at path still hash to one of them, else ErrPreconditionFailed. The
// check and the move hold the path locks of both paths, the ones every save
// takes, so no save of either path lands in between.
func (f *Files) MoveIfMatch(ctx context.Context, vault uuid.UUID, path, newPath, ifMatch string) (*vaultfiles.VaultFile, error) {
	from, err := vaultfiles.SanitizePath(path)
	if err != nil {
		return nil, err
	}
	to, err := vaultfiles.SanitizePath(newPath)
	if err != nil {
		return nil, err
	}
	if from == to {
		return nil, wire.NewError(http.StatusBadRequest, "path and newPath are identical")
	}
	if err := f.heldGuard(ctx, vault, to); err != nil {
		return nil, err
	}
	unlock := f.locks.lock2(vault, from, to)
	defer unlock()
	row, err := f.store.FindByPath(ctx, vault, from)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errFileNotFound
	}
	if ifMatch != "" {
		target, err := vaultfiles.ResolveEntryInside(f.paths.Raw(vault), from)
		if err != nil {
			return nil, err
		}
		if err := checkIfMatch(ifMatch, target); err != nil {
			return nil, err
		}
	}
	if err := f.store.MoveFile(ctx, row, to, vault); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "vault move", "tenant", ids.Upper(vault), "from", from, "to", to)
	return row, nil
}

// Delete removes the file at path for actor: the file moves to the trash
// (vault_trash records it, so it can be restored), and its row and the note
// memory made from it are removed. A failed move is logged and doesn't stop
// the delete, since the row is what the user sees.
func (f *Files) Delete(ctx context.Context, vault, actor uuid.UUID, path string) error {
	return f.DeleteIfMatch(ctx, vault, actor, path, "")
}

// DeleteIfMatch is Delete with MoveIfMatch's precondition: with a non-empty
// ifMatch the file is only trashed while its bytes still hash to one of the
// listed sha256 values, else ErrPreconditionFailed. The check and the delete
// hold the path's lock.
func (f *Files) DeleteIfMatch(ctx context.Context, vault, actor uuid.UUID, path, ifMatch string) error {
	safe, err := vaultfiles.SanitizePath(path)
	if err != nil {
		return err
	}
	unlock := f.locks.lock(vault, safe)
	defer unlock()
	row, err := f.store.FindByPath(ctx, vault, safe)
	if err != nil {
		return err
	}
	if row == nil {
		return errFileNotFound
	}
	rawRoot := f.paths.Raw(vault)
	target, err := vaultfiles.ResolveEntryInside(rawRoot, safe)
	if err != nil {
		return err
	}
	if ifMatch != "" {
		if err := checkIfMatch(ifMatch, target); err != nil {
			return err
		}
	}
	if _, err := f.store.Trash(ctx, rawRoot, target, row, &actor, f.now()); err != nil {
		slog.ErrorContext(ctx, "vault delete: move to trash failed", "tenant", ids.Upper(vault), "path", safe, "error", err)
	}
	// Before the row: the foreign key would only set the memory's source to NULL.
	// Swift ignores a failure here (try?).
	if _, err := f.memories.DeleteBySourceVaultFileID(ctx, vault, row.ID); err != nil {
		slog.WarnContext(ctx, "vault delete: note memory not removed", "tenant", ids.Upper(vault), "error", err)
	}
	if err := f.store.DeleteRow(ctx, row.ID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "vault delete", "tenant", ids.Upper(vault), "path", safe)
	return nil
}
