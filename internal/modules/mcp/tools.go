package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/chunkindex"
	"lumina/internal/devicetools"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/soul"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultindex"
	"lumina/internal/vaultpaths"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// caller is MCPCaller: the account, and for an agent key, which key.
type caller struct {
	userID         uuid.UUID
	connectionID   *uuid.UUID
	connectionName *string
	clientKind     *string
}

// contribution is MCPCaller.contribution: provider "mcp:<kind>" (or "mcp"),
// model the key's name (or "session"), and a reference naming the key, unique
// per create because it becomes memories.origin_source_id.
func (c caller) contribution(operation string) *memorystore.Contribution {
	provider := "mcp"
	if c.clientKind != nil {
		provider = "mcp:" + *c.clientKind
	}
	model := "session"
	if c.connectionName != nil {
		model = *c.connectionName
	}
	var reference *string
	if c.connectionID != nil {
		ref := "agent_connection:" + ids.Upper(*c.connectionID)
		if operation == memorystore.OperationCreate {
			ref += "#" + ids.Upper(uuid.New())
		}
		reference = &ref
	}
	contribution := memorystore.ModelContribution(operation, memorystore.SourceManual, &provider, &model, reference)
	return &contribution
}

// Service is MCPService.
type Service struct {
	pool       *pgxpool.Pool
	paths      vaultpaths.Paths
	status     *vaultindex.Service
	links      *chunkindex.LinkRepository
	hybrid     *chunkindex.Hybrid
	embeddings embedding.Service
	backfill   *chunkindex.Backfiller
	indexer    *chunkindex.Indexer
	memories   *memorystore.Repository
	personal   *devicetools.Tools
	// soul nil leaves SOUL.md out of memory_context, as Swift's optional does.
	soul *soul.Service
	now  func() time.Time
}

// ServiceDeps are the Service's collaborators.
type ServiceDeps struct {
	Pool       *pgxpool.Pool
	Paths      vaultpaths.Paths
	Status     *vaultindex.Service
	Links      *chunkindex.LinkRepository
	Hybrid     *chunkindex.Hybrid
	Embeddings embedding.Service
	Backfill   *chunkindex.Backfiller
	Indexer    *chunkindex.Indexer
	Memories   *memorystore.Repository
	Personal   *devicetools.Tools
	Soul       *soul.Service
}

// NewService returns the tool runner.
func NewService(d ServiceDeps) *Service {
	return &Service{
		pool: d.Pool, paths: d.Paths, status: d.Status, links: d.Links, hybrid: d.Hybrid,
		embeddings: d.Embeddings, backfill: d.Backfill, indexer: d.Indexer, memories: d.Memories,
		personal: d.Personal, soul: d.Soul, now: time.Now,
	}
}

// call is MCPService.call for the vault tools.
func (s *Service) call(ctx context.Context, name string, args map[string]any, tenant uuid.UUID, who caller) (map[string]any, error) {
	switch name {
	case "status":
		st, err := s.status.Status(ctx, tenant)
		if err != nil {
			return nil, err
		}
		return asObject(st)
	case "search":
		return s.search(ctx, args, tenant)
	case "browse":
		return s.browse(ctx, args, tenant)
	case "read":
		return s.read(ctx, args, tenant)
	case "recent":
		return s.recent(ctx, args, tenant)
	case "links":
		return s.linksTool(ctx, args, tenant)
	case "context":
		return s.contextTool(ctx, args, tenant)
	case "index":
		return s.index(ctx, args, tenant)
	case "memory_save":
		return s.memorySave(ctx, args, tenant, who)
	case "memory_update":
		return s.memoryUpdate(ctx, args, tenant, who)
	case "memory_forget":
		return s.memoryForget(ctx, args, tenant, who)
	case "memory_context":
		return s.memoryContext(ctx, args, tenant, who)
	}
	return nil, methodNotFound("unknown tool '" + name + "'")
}

// callPersonal is MCPService.callPersonal: user is the caller's own account.
func (s *Service) callPersonal(ctx context.Context, name string, args map[string]any, user uuid.UUID) (map[string]any, error) {
	var raw string
	switch name {
	case "health_query":
		raw = s.personal.HealthQuery(ctx, user, argStringPtr(args, "metric"), argIntPtr(args, "days"))
	case "calendar_query":
		raw = s.personal.CalendarQuery(ctx, user, argIntPtr(args, "days"))
	case "reminders_list":
		raw = s.personal.RemindersList(ctx, user)
	case "calendar_create":
		title, err := requireString(args, "title")
		if err != nil {
			return nil, err
		}
		start, err := requireString(args, "start")
		if err != nil {
			return nil, err
		}
		raw = s.personal.CalendarCreate(ctx, user, title, start, argStringPtr(args, "end"), argStringPtr(args, "location"))
	case "reminder_create":
		title, err := requireString(args, "title")
		if err != nil {
			return nil, err
		}
		raw = s.personal.ReminderCreate(ctx, user, title, argStringPtr(args, "notes"), argStringPtr(args, "due"))
	default:
		return nil, methodNotFound("unknown tool '" + name + "'")
	}
	slog.InfoContext(ctx, fmt.Sprintf("mcp.personal tool=%s user=%s", name, ids.Upper(user)))
	return personalResult(raw), nil
}

// personalResult is MCPService.toolResult(fromPersonalJSON:).
func personalResult(raw string) map[string]any {
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return map[string]any{"isError": true, "message": "tool returned no result"}
	}
	if object["status"] == "error" {
		object["isError"] = true
		if reason, ok := object["reason"]; ok {
			object["message"] = reason
		} else {
			object["message"] = "tool failed"
		}
	}
	return object
}

// asObject is JSONValue.encoding: the value's JSON as a tree.
func asObject(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

func asValue(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	return out, json.Unmarshal(b, &out)
}

func (s *Service) search(ctx context.Context, args map[string]any, tenant uuid.UUID) (map[string]any, error) {
	query, ok := argString(args, "query")
	if !ok || swiftstr.TrimWhitespaces(query) == "" {
		return nil, invalidParams("query is required and must be a non-empty string")
	}
	if swiftstr.Count(query) > maxQueryLength {
		return nil, invalidParams(fmt.Sprintf("query must be at most %d characters", maxQueryLength))
	}
	limit, err := validate(intOr(args, "limit", defaultSearchLimit), "limit", maxSearchLimit)
	if err != nil {
		return nil, err
	}
	spaceID, err := s.resolveSpace(ctx, argStringPtr(args, "space"), tenant)
	if err != nil {
		return nil, err
	}
	vec, err := embedding.EmbedForQuery(ctx, s.embeddings, query, tenant, "mcp.search")
	if err != nil {
		return nil, err
	}
	hits, err := s.hybrid.Search(ctx, tenant, query, vec, limit, memorystore.SearchOptions{SpaceID: spaceID})
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
			if c.Path != nil {
				row["path"] = *c.Path
			} else {
				row["path"] = nil
			}
			headings := c.HeadingPath
			if headings == nil {
				headings = []string{}
			}
			row["headingPath"] = headings
			row["startLine"] = c.StartLine
			row["endLine"] = c.EndLine
			row["cite"] = c.DisplayTrail()
		} else {
			row["path"] = nil
			row["cite"] = nil
		}
		results = append(results, row)
	}
	return map[string]any{"query": query, "total": len(hits), "results": results}, nil
}

func (s *Service) memorySave(ctx context.Context, args map[string]any, tenant uuid.UUID, who caller) (map[string]any, error) {
	content, err := memoryContent(args)
	if err != nil {
		return nil, err
	}
	spaceID, err := s.resolveSpace(ctx, argStringPtr(args, "space"), tenant)
	if err != nil {
		return nil, err
	}
	tags, err := memoryTags(args)
	if err != nil {
		return nil, err
	}
	kind, err := memoryKind(args)
	if err != nil {
		return nil, err
	}
	var existing uuid.UUID
	err = s.pool.QueryRow(ctx, `SELECT id FROM memories WHERE tenant_id = $1 AND content = $2 AND review_state <> 'rejected' LIMIT 1`,
		tenant, content).Scan(&existing)
	if err == nil {
		return map[string]any{
			"memoryID":  ids.Upper(existing),
			"duplicate": true,
			"message":   "An identical memory already exists; nothing was added.",
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	vec, err := embedding.EmbedForWrite(ctx, s.embeddings, content, tenant, "mcp.memorySave")
	if err != nil {
		return nil, err
	}
	m, err := s.memories.Create(ctx, memorystore.CreateParams{
		Tenant: tenant, Content: content, Embedding: vec, Tags: tags, SpaceID: spaceID,
		Contribution: who.contribution(memorystore.OperationCreate), Kind: kind,
	})
	if err != nil {
		return nil, err
	}
	if err := s.memories.StampCreator(ctx, m, who.userID); err != nil {
		return nil, err
	}
	s.indexer.IndexBestEffort(ctx, chunkindex.IndexParams{Tenant: tenant, MemoryID: m.ID, SpaceID: spaceID, Content: content})
	slog.InfoContext(ctx, fmt.Sprintf("mcp.memory.save tenant=%s memory=%s connection=%s", ids.Upper(tenant), ids.Upper(m.ID), who.connectionLabel()))
	return map[string]any{"memoryID": ids.Upper(m.ID), "duplicate": false}, nil
}

func (c caller) connectionLabel() string {
	if c.connectionID == nil {
		return "session"
	}
	return ids.Upper(*c.connectionID)
}

func (s *Service) memoryUpdate(ctx context.Context, args map[string]any, tenant uuid.UUID, who caller) (map[string]any, error) {
	id, err := memoryID(args)
	if err != nil {
		return nil, err
	}
	content, err := memoryContent(args)
	if err != nil {
		return nil, err
	}
	vec, err := embedding.EmbedForWrite(ctx, s.embeddings, content, tenant, "mcp.memoryUpdate")
	if err != nil {
		return nil, err
	}
	updated, err := s.memories.UpdateContent(ctx, tenant, id, content, vec, who.contribution(memorystore.OperationUpdate))
	if err != nil {
		return nil, err
	}
	if !updated {
		return memoryNotFound(id), nil
	}
	row, err := s.memories.Find(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return memoryNotFound(id), nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE memories SET updated_by_user_id = $2, updated_at = NOW() WHERE id = $1`, id, who.userID); err != nil {
		return nil, err
	}
	s.indexer.IndexBestEffort(ctx, chunkindex.IndexParams{Tenant: tenant, MemoryID: id, SpaceID: row.SpaceID, Content: content})
	slog.InfoContext(ctx, fmt.Sprintf("mcp.memory.update tenant=%s memory=%s connection=%s", ids.Upper(tenant), ids.Upper(id), who.connectionLabel()))
	return map[string]any{"memoryID": ids.Upper(id), "updated": true}, nil
}

func (s *Service) memoryForget(ctx context.Context, args map[string]any, tenant uuid.UUID, who caller) (map[string]any, error) {
	id, err := memoryID(args)
	if err != nil {
		return nil, err
	}
	forgotten, err := s.memories.Forget(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if !forgotten {
		return memoryNotFound(id), nil
	}
	slog.InfoContext(ctx, fmt.Sprintf("mcp.memory.forget tenant=%s memory=%s connection=%s", ids.Upper(tenant), ids.Upper(id), who.connectionLabel()))
	return map[string]any{"memoryID": ids.Upper(id), "forgotten": true}, nil
}

func (s *Service) memoryContext(ctx context.Context, args map[string]any, tenant uuid.UUID, who caller) (map[string]any, error) {
	spaceID, err := s.resolveSpace(ctx, argStringPtr(args, "space"), tenant)
	if err != nil {
		return nil, err
	}
	query := `SELECT id, kind, content, created_at FROM memories
		WHERE tenant_id = $1 AND kind = ANY($2) AND review_state <> 'rejected'`
	params := []any{tenant, []string{"decision", "preference"}}
	if spaceID != nil {
		query += ` AND space_id = $3`
		params = append(params, *spaceID)
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, maxContextMemories)
	rows, err := s.pool.Query(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	memories := []any{}
	for rows.Next() {
		var (
			id        uuid.UUID
			kind      *string
			content   string
			createdAt *time.Time
		)
		if err := rows.Scan(&id, &kind, &content, &createdAt); err != nil {
			return nil, err
		}
		row := map[string]any{"memoryID": ids.Upper(id), "kind": nil, "text": content, "savedAt": nil}
		if kind != nil {
			row["kind"] = *kind
		}
		if createdAt != nil {
			row["savedAt"] = createdAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		memories = append(memories, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var soulText any
	if s.soul != nil {
		var username string
		err := s.pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, who.userID).Scan(&username)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			if text := swiftstr.TrimWhitespacesAndNewlines(soul.Strip(s.soul.Read(who.userID, username, s.now()))); text != "" {
				soulText = text
			}
		}
	}
	return map[string]any{"soul": soulText, "soulEditable": false, "memories": memories}, nil
}

func memoryContent(args map[string]any) (string, error) {
	raw, _ := argString(args, "content")
	content := swiftstr.TrimWhitespacesAndNewlines(raw)
	if content == "" {
		return "", invalidParams("content is required and must be a non-empty string")
	}
	if swiftstr.Count(content) > maxMemoryLength {
		return "", invalidParams(fmt.Sprintf("content must be at most %d characters", maxMemoryLength))
	}
	return content, nil
}

func memoryID(args map[string]any) (uuid.UUID, error) {
	raw, ok := argString(args, "memoryID")
	if ok {
		if id, err := ids.Parse(raw); err == nil {
			return id, nil
		}
	}
	return uuid.Nil, invalidParams("memoryID is required and must be a UUID from search or memory_save")
}

func memoryKind(args map[string]any) (*string, error) {
	raw, ok := argString(args, "kind")
	if !ok {
		return nil, nil
	}
	if !slices.Contains(memoryKinds, raw) {
		return nil, invalidParams("kind must be one of " + strings.Join(memoryKinds, ", "))
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
		return nil, invalidParams("tags must be an array of strings")
	}
	tags := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		tag := swiftstr.TrimWhitespaces(s)
		if !ok || tag == "" {
			return nil, invalidParams("tags must be non-empty strings")
		}
		tags = append(tags, tag)
	}
	if len(tags) > maxMemoryTags {
		return nil, invalidParams(fmt.Sprintf("at most %d tags", maxMemoryTags))
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

// resolveSpace maps a Space slug to its id; "inbox" or nothing is unfiled.
func (s *Service) resolveSpace(ctx context.Context, slug *string, tenant uuid.UUID) (*uuid.UUID, error) {
	if slug == nil {
		return nil, nil
	}
	trimmed := swiftstr.TrimWhitespaces(*slug)
	if trimmed == "" || trimmed == "inbox" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, slug FROM spaces WHERE tenant_id = $1`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var known []string
	var found *uuid.UUID
	for rows.Next() {
		var (
			id uuid.UUID
			sl string
		)
		if err := rows.Scan(&id, &sl); err != nil {
			return nil, err
		}
		if found == nil && sl == trimmed {
			found = &id
		}
		known = append(known, sl)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if found != nil {
		return found, nil
	}
	slices.Sort(known)
	list := strings.Join(known, ", ")
	if list == "" {
		list = "none"
	}
	return nil, invalidParams(fmt.Sprintf("unknown space '%s'. Known spaces: %s, or inbox", trimmed, list))
}

func memoryNotFound(id uuid.UUID) map[string]any {
	return map[string]any{"isError": true, "message": "no memory " + ids.Upper(id) + " in this vault"}
}

func (s *Service) browse(ctx context.Context, args map[string]any, tenant uuid.UUID) (map[string]any, error) {
	limit, err := validate(intOr(args, "limit", defaultBrowseLimit), "limit", maxBrowseLimit)
	if err != nil {
		return nil, err
	}
	query := `SELECT id, path, size_bytes FROM vault_files WHERE tenant_id = $1`
	params := []any{tenant}
	if prefix, ok := argString(args, "pathPrefix"); ok && prefix != "" {
		query += ` AND path >= $2 AND path < $3`
		params = append(params, prefix, prefix+"￿")
	}
	query += fmt.Sprintf(` ORDER BY path LIMIT %d`, limit)
	rows, err := s.pool.Query(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs := []any{}
	for rows.Next() {
		var (
			id   uuid.UUID
			path string
			size int64
		)
		if err := rows.Scan(&id, &path, &size); err != nil {
			return nil, err
		}
		docs = append(docs, map[string]any{"path": path, "sizeBytes": size, "vaultFileID": ids.Upper(id)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"count": len(docs), "documents": docs}, nil
}

// fileID finds the vault file at the normalized path.
func (s *Service) file(ctx context.Context, tenant uuid.UUID, path string) (uuid.UUID, string, bool, error) {
	var (
		id     uuid.UUID
		stored string
	)
	err := s.pool.QueryRow(ctx, `SELECT id, path FROM vault_files WHERE tenant_id = $1 AND path = $2 LIMIT 1`,
		tenant, chunkindex.NormalizePath(path)).Scan(&id, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false, nil
	}
	return id, stored, err == nil, err
}

func pathNotFound(path string) map[string]any {
	return map[string]any{
		"isError": true,
		"path":    path,
		"message": "no document at that path — use `browse` or `search` to find the exact path",
	}
}

func (s *Service) read(ctx context.Context, args map[string]any, tenant uuid.UUID) (map[string]any, error) {
	path, err := requirePath(args)
	if err != nil {
		return nil, err
	}
	maxChars, err := validate(intOr(args, "maxChars", defaultReadChars), "maxChars", maxReadChars)
	if err != nil {
		return nil, err
	}
	_, stored, ok, err := s.file(ctx, tenant, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return pathNotFound(path), nil
	}
	text, readable := s.readFile(tenant, stored)
	if !readable {
		return map[string]any{
			"isError": true,
			"path":    stored,
			"message": "document is indexed but its file could not be read",
		}, nil
	}
	total := swiftstr.Count(text)
	truncated := total > maxChars
	content := text
	if truncated {
		content = swiftstr.Prefix(text, maxChars)
	}
	return map[string]any{"path": stored, "content": content, "truncated": truncated, "totalChars": total}, nil
}

// readFile is VaultController.resolveInside plus String(contentsOf:encoding:.utf8).
func (s *Service) readFile(tenant uuid.UUID, relative string) (string, bool) {
	target, err := vaultfiles.ResolveInside(s.paths.Raw(tenant), relative)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(target) //nolint:gosec // resolved inside the tenant's raw root
	if err != nil {
		return "", false
	}
	return vaultfiles.DecodeUTF8(data)
}

func (s *Service) recent(ctx context.Context, args map[string]any, tenant uuid.UUID) (map[string]any, error) {
	limit, err := validate(intOr(args, "limit", defaultRecentLimit), "limit", maxRecentLimit)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT path, updated_at FROM vault_files WHERE tenant_id = $1
		ORDER BY updated_at DESC LIMIT %d`, limit), tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs := []any{}
	for rows.Next() {
		var (
			path    string
			updated *time.Time
		)
		if err := rows.Scan(&path, &updated); err != nil {
			return nil, err
		}
		row := map[string]any{"path": path}
		if updated != nil {
			row["updatedAt"] = updated.UTC().Format("2006-01-02T15:04:05Z")
		}
		docs = append(docs, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"count": len(docs), "documents": docs}, nil
}

func (s *Service) linksTool(ctx context.Context, args map[string]any, tenant uuid.UUID) (map[string]any, error) {
	path, err := requirePath(args)
	if err != nil {
		return nil, err
	}
	id, stored, ok, err := s.file(ctx, tenant, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return pathNotFound(path), nil
	}
	outgoing, err := s.links.Outgoing(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	incoming, err := s.links.Incoming(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	out, err := asValue(outgoing)
	if err != nil {
		return nil, err
	}
	in, err := asValue(incoming)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []any{}
	}
	if in == nil {
		in = []any{}
	}
	return map[string]any{"path": stored, "outgoing": out, "incoming": in}, nil
}

func (s *Service) contextTool(ctx context.Context, args map[string]any, tenant uuid.UUID) (map[string]any, error) {
	path, err := requirePath(args)
	if err != nil {
		return nil, err
	}
	siblingLimit, err := validate(intOr(args, "siblingLimit", vaultindex.DefaultSiblingLimit), "siblingLimit", vaultindex.MaxSiblingLimit)
	if err != nil {
		return nil, err
	}
	id, _, ok, err := s.file(ctx, tenant, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return pathNotFound(path), nil
	}
	packet, err := s.status.Context(ctx, tenant, id, siblingLimit)
	if err != nil {
		return nil, err
	}
	return asObject(packet)
}

func (s *Service) index(ctx context.Context, args map[string]any, tenant uuid.UUID) (map[string]any, error) {
	batch, err := validate(intOr(args, "batchSize", chunkindex.DefaultBackfillBatchSize), "batchSize", maxIndexBatch)
	if err != nil {
		return nil, err
	}
	result, err := s.backfill.Backfill(ctx, tenant, batch)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, fmt.Sprintf("mcp.index tenant=%s scanned=%d indexed=%d", ids.Upper(tenant), result.Scanned, result.Indexed))
	return asObject(result)
}

func requireString(args map[string]any, name string) (string, error) {
	v, ok := argString(args, name)
	if !ok || swiftstr.TrimWhitespaces(v) == "" {
		return "", invalidParams(name + " is required and must be a non-empty string")
	}
	return v, nil
}

func requirePath(args map[string]any) (string, error) {
	v, ok := argString(args, "path")
	if !ok || swiftstr.TrimWhitespaces(v) == "" {
		return "", invalidParams("path is required and must be a non-empty string")
	}
	return v, nil
}
