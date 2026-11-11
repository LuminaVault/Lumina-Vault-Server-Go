package quickassistant

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/chunkindex"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/modules/quickassistant/db"
	"lumina/internal/soul"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MCPLimits, Swift's verbatim. The same numbers gate the HTTP layer.
const (
	minLimit           = 1
	maxSearchLimit     = 50
	defaultSearchLimit = 10
	maxBrowseLimit     = 500
	defaultBrowseLimit = 50
	maxRecentLimit     = 100
	defaultRecentLimit = 10
	maxReadChars       = 100_000
	defaultReadChars   = 20000
	maxQueryLength     = 1000
	maxMemoryLength    = 4000
	maxMemoryTags      = 10
	maxContextMemories = 30
)

// memoryKinds is MemoryKindDTO.allCases, in Shared order.
var memoryKinds = []string{
	memorystore.KindPreference, memorystore.KindDecision, memorystore.KindFact, memorystore.KindNote,
}

// vaultTool runs one read-only vault tool on vault, which the caller has
// already resolved for this actor. These are the bodies of MCPService's
// runSearch, runBrowse, runRead, runRecent and runMemoryContext (Decision 1);
// when MCP is ported both should call one implementation.
func (r *Runner) vaultTool(ctx context.Context, name string, args map[string]any, vault uuid.UUID) (any, error) {
	switch name {
	case "search":
		return r.runSearch(ctx, args, vault)
	case "browse":
		return r.runBrowse(ctx, args, vault)
	case "read":
		return r.runRead(ctx, args, vault)
	case "recent":
		return r.runRecent(ctx, args, vault)
	case "memory_context":
		return r.runMemoryContext(ctx, args, vault)
	default:
		// The policy offers nothing else that reaches here; fail closed.
		return nil, fmt.Errorf("unknown vault tool %q", name)
	}
}

// validateLimit is MCPLimits.validate: an explicit error rather than a clamp.
func validateLimit(args map[string]any, name string, def, maximum int) (int, error) {
	value, ok := intArg(args, name)
	if !ok {
		value = def
	}
	if value < minLimit || value > maximum {
		return 0, refuse(fmt.Sprintf("%s must be between %d and %d; got %d", name, minLimit, maximum, value))
	}
	return value, nil
}

func (r *Runner) queries() *db.Queries { return db.New(r.Pool) }

func (r *Runner) runSearch(ctx context.Context, args map[string]any, vault uuid.UUID) (any, error) {
	query, ok := args["query"].(string)
	if !ok || swiftstr.TrimWhitespaces(query) == "" {
		return nil, refuse("query is required and must be a non-empty string")
	}
	if swiftstr.Count(query) > maxQueryLength {
		return nil, refuse(fmt.Sprintf("query must be at most %d characters", maxQueryLength))
	}
	limit, err := validateLimit(args, "limit", defaultSearchLimit, maxSearchLimit)
	if err != nil {
		return nil, err
	}
	spaceID, err := r.resolveSpace(ctx, args, vault)
	if err != nil {
		return nil, err
	}
	vec, err := embedding.EmbedForQuery(ctx, r.Embeddings, query, vault, "mcp.search")
	if err != nil {
		return nil, err
	}
	hits, err := r.Hybrid.Search(ctx, vault, query, vec, limit, memorystore.SearchOptions{SpaceID: spaceID})
	if err != nil {
		return nil, err
	}
	results := make([]any, 0, len(hits))
	for _, hit := range hits {
		row := map[string]any{"memoryID": ids.Upper(hit.ID), "text": hit.Content}
		if hit.Snippet != nil {
			row["snippet"] = *hit.Snippet
		}
		if c := hit.Citation; c != nil {
			// The locator is the point of this tool, flattened so a model
			// cannot miss it.
			row["path"] = c.Path
			heading := c.HeadingPath
			if heading == nil {
				heading = []string{}
			}
			row["headingPath"] = heading
			row["startLine"] = c.StartLine
			row["endLine"] = c.EndLine
			row["cite"] = c.DisplayTrail()
		} else {
			// Explicitly absent, so "no citation" reads as a fact.
			row["path"] = nil
			row["cite"] = nil
		}
		results = append(results, row)
	}
	return map[string]any{"query": query, "total": len(hits), "results": results}, nil
}

func (r *Runner) runBrowse(ctx context.Context, args map[string]any, vault uuid.UUID) (any, error) {
	limit, err := validateLimit(args, "limit", defaultBrowseLimit, maxBrowseLimit)
	if err != nil {
		return nil, err
	}
	params := db.BrowseFilesParams{TenantID: vault, RowLimit: int32(limit)} //nolint:gosec // validated 1..500
	if prefix := stringArg(args, "pathPrefix"); prefix != "" {
		high := prefix + "￿"
		params.Low, params.High = &prefix, &high
	}
	files, err := r.queries().BrowseFiles(ctx, params)
	if err != nil {
		return nil, err
	}
	documents := make([]any, 0, len(files))
	for _, f := range files {
		documents = append(documents, map[string]any{
			"path": f.Path, "sizeBytes": f.SizeBytes, "vaultFileID": ids.Upper(f.ID),
		})
	}
	return map[string]any{"count": len(files), "documents": documents}, nil
}

func (r *Runner) runRead(ctx context.Context, args map[string]any, vault uuid.UUID) (any, error) {
	path, ok := args["path"].(string)
	if !ok || swiftstr.TrimWhitespaces(path) == "" {
		return nil, refuse("path is required and must be a non-empty string")
	}
	maxChars, err := validateLimit(args, "maxChars", defaultReadChars, maxReadChars)
	if err != nil {
		return nil, err
	}
	stored, err := r.queries().FileAtPath(ctx, db.FileAtPathParams{TenantID: vault, Path: chunkindex.NormalizePath(path)})
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]any{
			"isError": true,
			"path":    path,
			"message": "no document at that path — use `browse` or `search` to find the exact path",
		}, nil
	}
	if err != nil {
		return nil, err
	}
	// The source file, not the reassembled chunks: line numbers in a citation
	// refer to the file a user would open.
	text, readable := r.readVaultFile(vault, stored)
	if !readable {
		return map[string]any{
			"isError": true,
			"path":    stored,
			"message": "document is indexed but its file could not be read",
		}, nil
	}
	total := swiftstr.Count(text)
	truncated := total > maxChars
	if truncated {
		text = swiftstr.Prefix(text, maxChars)
	}
	return map[string]any{"path": stored, "content": text, "truncated": truncated, "totalChars": total}, nil
}

func (r *Runner) readVaultFile(vault uuid.UUID, path string) (string, bool) {
	full, err := vaultfiles.ResolveInside(r.Paths.Raw(vault), path)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(full) //nolint:gosec // resolved inside the vault's raw root
	if err != nil {
		return "", false
	}
	return vaultfiles.DecodeUTF8(data)
}

func (r *Runner) runRecent(ctx context.Context, args map[string]any, vault uuid.UUID) (any, error) {
	limit, err := validateLimit(args, "limit", defaultRecentLimit, maxRecentLimit)
	if err != nil {
		return nil, err
	}
	files, err := r.queries().RecentFiles(ctx, db.RecentFilesParams{TenantID: vault, Limit: int32(limit)}) //nolint:gosec // validated 1..100
	if err != nil {
		return nil, err
	}
	documents := make([]any, 0, len(files))
	for _, f := range files {
		row := map[string]any{"path": f.Path}
		if f.UpdatedAt != nil {
			row["updatedAt"] = isoString(*f.UpdatedAt)
		}
		documents = append(documents, row)
	}
	return map[string]any{"count": len(files), "documents": documents}, nil
}

// runMemoryContext is the user's SOUL.md, read-only, and their most recent
// decisions and preferences, optionally for one Space.
func (r *Runner) runMemoryContext(ctx context.Context, args map[string]any, vault uuid.UUID) (any, error) {
	spaceID, err := r.resolveSpace(ctx, args, vault)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries().ContextMemories(ctx, db.ContextMemoriesParams{
		TenantID: vault, SpaceID: spaceID, RowLimit: maxContextMemories,
	})
	if err != nil {
		return nil, err
	}
	var soulText any
	if r.Soul != nil {
		// The caller's own SOUL, as Swift reads it for caller.userID.
		text := swiftstr.TrimWhitespacesAndNewlines(soul.Strip(r.Soul.Read(r.Actor, r.Username, r.now())))
		if text != "" {
			soulText = text
		}
	}
	memories := make([]any, 0, len(rows))
	for _, m := range rows {
		var savedAt any
		if m.CreatedAt != nil {
			savedAt = isoString(*m.CreatedAt)
		}
		memories = append(memories, map[string]any{
			"memoryID": ids.Upper(m.ID), "kind": m.Kind, "text": m.Content, "savedAt": savedAt,
		})
	}
	return map[string]any{"soul": soulText, "soulEditable": false, "memories": memories}, nil
}

// runMemorySave saves one durable fact, preference or decision. An exact
// repeat returns the existing memory instead of a second copy.
func (r *Runner) runMemorySave(ctx context.Context, args map[string]any, vault uuid.UUID) (out map[string]any, duplicate bool, err error) {
	content := swiftstr.TrimWhitespacesAndNewlines(stringArg(args, "content"))
	if content == "" {
		return nil, false, refuse("content is required and must be a non-empty string")
	}
	if swiftstr.Count(content) > maxMemoryLength {
		return nil, false, refuse(fmt.Sprintf("content must be at most %d characters", maxMemoryLength))
	}
	spaceID, err := r.resolveSpace(ctx, args, vault)
	if err != nil {
		return nil, false, err
	}
	tags, err := memoryTags(args)
	if err != nil {
		return nil, false, err
	}
	kind, err := memoryKind(args)
	if err != nil {
		return nil, false, err
	}

	existing, err := r.queries().ExistingMemory(ctx, db.ExistingMemoryParams{TenantID: vault, Content: content})
	switch {
	case err == nil:
		return map[string]any{
			"memoryID":  ids.Upper(existing),
			"duplicate": true,
			"message":   "An identical memory already exists; nothing was added.",
		}, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, err
	}

	vec, err := embedding.EmbedForWrite(ctx, r.Embeddings, content, vault, "mcp.memorySave")
	if err != nil {
		return nil, false, err
	}
	// MCPCaller.session's contribution: provider "mcp", model "session".
	provider, model := "mcp", "session"
	contribution := memorystore.ModelContribution(memorystore.OperationCreate, memorystore.SourceManual, &provider, &model, nil)
	memory, err := r.Memories.Create(ctx, memorystore.CreateParams{
		Tenant: vault, Content: content, Embedding: vec, Tags: tags, SpaceID: spaceID,
		Contribution: &contribution, Kind: kind,
	})
	if err != nil {
		return nil, false, err
	}
	if err := r.Memories.StampCreator(ctx, memory, r.Actor); err != nil {
		return nil, false, err
	}
	if r.Indexer != nil {
		r.Indexer.IndexBestEffort(ctx, chunkindex.IndexParams{
			Tenant: vault, MemoryID: memory.ID, SpaceID: spaceID, Content: content,
		})
	}
	r.logger().InfoContext(ctx, "mcp.memory.save tenant="+vault.String()+" memory="+memory.ID.String()+" connection=session")
	return map[string]any{"memoryID": ids.Upper(memory.ID), "duplicate": false}, false, nil
}

func memoryKind(args map[string]any) (*string, error) {
	raw, ok := args["kind"].(string)
	if !ok {
		return nil, nil //nolint:nilnil // no kind is a valid answer
	}
	if !slices.Contains(memoryKinds, raw) {
		return nil, refuse("kind must be one of " + strings.Join(memoryKinds, ", "))
	}
	return &raw, nil
}

func memoryTags(args map[string]any) ([]string, error) {
	value, present := args["tags"]
	if !present {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, refuse("tags must be an array of strings")
	}
	tags := make([]string, 0, len(items))
	for _, item := range items {
		s, isString := item.(string)
		tag := swiftstr.TrimWhitespaces(s)
		if !isString || tag == "" {
			return nil, refuse("tags must be non-empty strings")
		}
		tags = append(tags, tag)
	}
	if len(tags) > maxMemoryTags {
		return nil, refuse(fmt.Sprintf("at most %d tags", maxMemoryTags))
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

// resolveSpace is MCPService.resolveSpace: a Space slug from the "space"
// argument. "inbox", or nothing, is unfiled; an unknown slug is refused with
// the slugs that exist.
func (r *Runner) resolveSpace(ctx context.Context, args map[string]any, vault uuid.UUID) (*uuid.UUID, error) {
	slug := swiftstr.TrimWhitespaces(stringArg(args, "space"))
	if slug == "" || slug == "inbox" {
		return nil, nil //nolint:nilnil // unfiled
	}
	spaces, err := r.queries().SpacesForVault(ctx, vault)
	if err != nil {
		return nil, err
	}
	known := make([]string, 0, len(spaces))
	for _, s := range spaces {
		if s.Slug == slug {
			id := s.ID
			return &id, nil
		}
		known = append(known, s.Slug)
	}
	slices.Sort(known)
	list := strings.Join(known, ", ")
	if list == "" {
		list = "none"
	}
	return nil, refuse(fmt.Sprintf("unknown space '%s'. Known spaces: %s, or inbox", slug, list))
}

// isoString is ISO8601DateFormatter with .withInternetDateTime: UTC, whole
// seconds, "Z".
func isoString(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z07:00") }
