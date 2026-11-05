// Package memorycompile is MemoryCompileService from LuminaVaultServer Sources/App/KB:
// read a batch of vault files, ask the model once for structured JSON, persist each
// extracted memory as pending, stamp the files and rebuild the wiki export. POST
// /v1/memory-compile (internal/modules/kbcompile) and the import approve route
// (internal/modules/imports) are its callers.
package memorycompile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// MaxBlockChars is maxBlockChars: the lead of a file fed to the model.
	MaxBlockChars = 8000
	// maxMemoryChars is the cap on one extracted memory.
	maxMemoryChars = 2000

	defaultMaxFileSize   = 10 * 1024 * 1024
	defaultMaxBatchBytes = 32 * 1024 * 1024
)

// ErrNoFiles is MemoryCompileError.noFiles.
var ErrNoFiles = errors.New("memorycompile: no files")

// FileTooLargeError is MemoryCompileError.fileTooLarge. It isn't an HTTPError in Swift,
// so the route answers a bare 500.
type FileTooLargeError struct {
	Path  string
	Limit int
}

func (e *FileTooLargeError) Error() string {
	return fmt.Sprintf("fileTooLarge(path: %q, limit: %d)", e.Path, e.Limit)
}

// WrittenFile is InternalKBCompileWrittenFile.
type WrittenFile struct {
	Path        string
	Size        int
	ContentType string
	SHA256      string
}

// MemoryRef is InternalKBCompileMemoryRef.
type MemoryRef struct {
	ID      uuid.UUID
	Content string
}

// Result is InternalMemoryCompileResult.
type Result struct {
	WrittenFiles []WrittenFile
	Memories     []MemoryRef
	Summary      string
}

// Options are the service's dependencies. Progress nil means NoopPublisher.
type Options struct {
	Paths         vaultpaths.Paths
	Transport     llm.ChatTransport
	Memories      *memorystore.Repository
	Embeddings    embedding.Service
	DefaultModel  string
	Progress      Publisher
	MaxFileSize   int
	MaxBatchBytes int
	Now           func() time.Time
}

// Service is MemoryCompileService.
type Service struct {
	pool *pgxpool.Pool
	opt  Options
}

// New builds the service.
func New(pool *pgxpool.Pool, o Options) *Service {
	if o.Progress == nil {
		o.Progress = NoopPublisher{}
	}
	if o.MaxFileSize == 0 {
		o.MaxFileSize = defaultMaxFileSize
	}
	if o.MaxBatchBytes == 0 {
		o.MaxBatchBytes = defaultMaxBatchBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{pool: pool, opt: o}
}

// Progress is the publisher the service was built with, which the route shares.
func (s *Service) Progress() Publisher { return s.opt.Progress }

type textBlock struct {
	path, content, contentType string
}

// CompileExistingVaultFiles is compileExistingVaultFiles.
func (s *Service) CompileExistingVaultFiles(ctx context.Context, tenant uuid.UUID, sessionKey string, rows []*vaultfiles.VaultFile, hint *string, runID uuid.UUID) (Result, error) {
	if len(rows) == 0 {
		return Result{}, ErrNoFiles
	}
	rawRoot := s.opt.Paths.Raw(tenant)
	var (
		written    []WrittenFile
		blocks     []textBlock
		totalBytes int
		rowIDs     []uuid.UUID
		missing    []uuid.UUID
		deferred   int
	)
	for _, row := range rows {
		// A link still being enriched is only the "# [Pending Enrichment]" skeleton:
		// skip it unstamped, so the next compile picks up the real content.
		if row.Metadata != nil && row.Metadata.EnrichmentStatus != nil && *row.Metadata.EnrichmentStatus == "pending" {
			deferred++
			continue
		}
		safe, err := vaultfiles.SanitizePath(row.Path)
		if err != nil {
			return Result{}, err
		}
		target, err := vaultfiles.ResolveInside(rawRoot, safe)
		if err != nil {
			return Result{}, err
		}
		payload, err := os.ReadFile(target) //nolint:gosec // path checked by SanitizePath and ResolveInside
		if err != nil {
			// A row whose file is gone: skip it, stamp it at the end.
			slog.WarnContext(ctx, "kb-compile skipping missing vault file", "path", safe, "tenant", ids.Upper(tenant), "error", err)
			missing = append(missing, row.ID)
			continue
		}
		if len(payload) > s.opt.MaxFileSize {
			return Result{}, &FileTooLargeError{Path: safe, Limit: s.opt.MaxFileSize}
		}
		totalBytes += len(payload)
		if totalBytes > s.opt.MaxBatchBytes {
			return Result{}, wire.NewError(http.StatusRequestEntityTooLarge, "batch exceeds "+strconv.Itoa(s.opt.MaxBatchBytes)+" bytes")
		}
		written = append(written, WrittenFile{Path: safe, Size: len(payload), ContentType: row.ContentType, SHA256: row.SHA256})
		rowIDs = append(rowIDs, row.ID)
		if isTextLike(row.ContentType) && utf8Valid(payload) {
			blocks = append(blocks, textBlock{safe, swiftstr.Prefix(string(payload), MaxBlockChars), row.ContentType})
		}
	}

	// Every row was an orphan: clear them so the pending count drains. No LLM call.
	if len(written) == 0 {
		if len(missing) > 0 {
			if err := s.markProcessed(ctx, missing, s.opt.Now()); err != nil {
				return Result{}, err
			}
		}
		slog.WarnContext(ctx, "kb-compile: no readable vault files", "missing", len(missing), "enriching", deferred, "tenant", ids.Upper(tenant))
		return Result{Summary: "No readable files to learn from."}, nil
	}

	rejected, err := s.loadRejectedHashes(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	s.opt.Progress.Publish(ctx, PreparingEvent(runID), tenant)
	text, memories, err := s.runCompile(ctx, tenant, sessionKey, blocks, hint, runID, rejected)
	if err != nil {
		return Result{}, err
	}
	completedAt := s.opt.Now()
	if err := s.markProcessed(ctx, append(rowIDs, missing...), completedAt); err != nil {
		return Result{}, err
	}
	spaceIDs := map[uuid.UUID]bool{}
	for _, row := range rows {
		if row.SpaceID != nil {
			spaceIDs[*row.SpaceID] = true
		}
	}
	if err := s.RefreshSpaceCounters(ctx, tenant, spaceIDs, completedAt); err != nil {
		return Result{}, err
	}
	// A pure side effect: memories are the source of truth.
	s.rebuildWiki(ctx, tenant, completedAt)
	return Result{WrittenFiles: written, Memories: memories, Summary: text}, nil
}

type extractionPayload struct {
	Model          string            `json:"model"`
	Messages       []llm.ChatMessage `json:"messages"`
	ResponseFormat map[string]string `json:"response_format"`
	Temperature    float64           `json:"temperature"`
	Stream         bool              `json:"stream"`
}

// SystemPrompt is runCompileLoop's system prompt.
func SystemPrompt(hint *string, now time.Time) string {
	suffix := ""
	if hint != nil {
		suffix = "User hint: " + *hint
	}
	return `You are Hermes' kb-compile agent. Extract durable, high-signal memories from the SUBSTANCE of the user's files:
  - For personal notes: the user's preferences, decisions, plans, TODOs, and recurring patterns ("Prefers espresso over filter coffee.").
  - For saved articles / documents / links: the key facts, claims, definitions, and takeaways stated in the material itself ("Stoicism teaches that virtue is the only true good.").
Write each memory as one concise, self-contained statement of fact in the third person. Never invent — only extract what is actually present. Do NOT write meta-observations about the user saving, capturing, or having a file (e.g. avoid "User captured a Wikipedia article" or "User maintains a knowledge base"). Skip navigation/boilerplate. Return ONLY a JSON object of the form {"memories": [{"text": "<memory>", "kind": "<kind>", "source": "<file path>"}, ...], "commitments": [{"text": "<commitment>", "due": "<YYYY-MM-DD or null>", "source": "<file path>"}, ...]}, with empty arrays when there is nothing to store. ` + "`kind`" + ` is "preference" (how the user likes things), "decision" (something the user settled), "fact" (about the user, their work, or the material), or "note". ` + "`source`" + ` is the FILE path the item came from, exactly as given below. ` + "`commitments`" + ` are only promises the user made in the first person in their own notes ("I'll…", "I need to…", "I promised…", "I have to…"), never claims from saved articles. Write each as a short action in the user's voice ("Send Ana the deck"). ` + "`due`" + ` is a calendar date only when the note states or clearly implies one (today is ` + now.UTC().Format("2006-01-02") + ` UTC), otherwise null.
` + suffix
}

func (s *Service) runCompile(ctx context.Context, tenant uuid.UUID, sessionKey string, blocks []textBlock, hint *string, runID uuid.UUID, rejected map[string]bool) (string, []MemoryRef, error) {
	bundled := "(No text-shaped files in this batch — only binary assets were written.)"
	if len(blocks) > 0 {
		parts := make([]string, len(blocks))
		for i, b := range blocks {
			parts[i] = "----- FILE: " + b.path + " (" + b.contentType + ") -----\n" + b.content
		}
		bundled = strings.Join(parts, "\n\n")
	}
	s.opt.Progress.Publish(ctx, ThinkingEvent(runID, 1), tenant)

	payload, err := json.Marshal(extractionPayload{
		Model: s.opt.DefaultModel,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: SystemPrompt(hint, s.opt.Now())},
			{Role: "user", Content: bundled},
		},
		ResponseFormat: map[string]string{"type": "json_object"},
		Temperature:    0.2,
	})
	if err != nil {
		return "", nil, err
	}
	raw, err := s.opt.Transport.ChatCompletions(ctx, payload, sessionKey, "")
	if err != nil {
		return "", nil, err
	}
	content, err := firstContent(raw.Data)
	if err != nil {
		return "", nil, err
	}
	parsed := ParseExtractedItems(content)
	slog.InfoContext(ctx, "kb-compile extracted candidate memories", "count", len(parsed), "tenant", ids.Upper(tenant))

	// A memory names the file it came from, trusted only when it's in this batch.
	fileIDs := map[string]uuid.UUID{}
	for _, b := range blocks {
		if _, seen := fileIDs[b.path]; seen {
			continue
		}
		var id uuid.UUID
		err := s.pool.QueryRow(ctx, `SELECT id FROM vault_files WHERE tenant_id = $1 AND path = $2 LIMIT 1`, tenant, b.path).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		fileIDs[b.path] = id
	}

	provider, model := "hermes", s.opt.DefaultModel
	var memories []MemoryRef
	for _, item := range parsed {
		trimmed := swiftstr.TrimWhitespacesAndNewlines(item.Text)
		if trimmed == "" {
			continue
		}
		capped := swiftstr.Prefix(trimmed, maxMemoryChars)
		if rejected[ContentHash(capped)] {
			continue
		}
		vec, err := embedding.EmbedForWrite(ctx, s.opt.Embeddings, capped, tenant, "kb.compile")
		if err != nil {
			return "", nil, err
		}
		var source *uuid.UUID
		if item.Source != nil {
			if id, ok := fileIDs[*item.Source]; ok {
				source = &id
			}
		}
		saved, err := s.opt.Memories.Create(ctx, memorystore.CreateParams{
			Tenant: tenant, Content: capped, Embedding: vec, SourceVaultFileID: source,
			ReviewState: memorystore.ReviewPending, Kind: item.Kind,
			Contribution: &memorystore.Contribution{
				Operation: memorystore.OperationCreate, Actor: memorystore.ActorModel,
				Source: memorystore.SourceKnowledgeCompile, Provider: &provider, Model: &model,
			},
		})
		if err != nil {
			return "", nil, err
		}
		memories = append(memories, MemoryRef{ID: saved.ID, Content: saved.Content})
		s.opt.Progress.Publish(ctx, MemorySavedEvent(runID, saved, item.Kind), tenant)
	}

	s.recordCommitments(ctx, content, tenant, fileIDs, memories)
	noun := "memories"
	if len(memories) == 1 {
		noun = "memory"
	}
	return "Learned " + strconv.Itoa(len(memories)) + " " + noun + ".", memories, nil
}

// firstContent decodes ChatResponseBody, whose id, model and choices are required: an
// answer missing them is a decoding error, a 500. No choice or no content is "".
func firstContent(data []byte) (string, error) {
	var body struct {
		ID      *string `json:"id"`
		Model   *string `json:"model"`
		Choices *[]struct {
			Message *struct {
				Role    *string `json:"role"`
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", fmt.Errorf("memorycompile: decode response: %w", err)
	}
	if body.ID == nil || body.Model == nil || body.Choices == nil {
		return "", errors.New("memorycompile: response is missing id, model or choices")
	}
	if len(*body.Choices) == 0 {
		return "", nil
	}
	m := (*body.Choices)[0].Message
	if m == nil || m.Role == nil {
		return "", errors.New("memorycompile: choice has no message role")
	}
	if m.Content == nil {
		return "", nil
	}
	return *m.Content, nil
}

// recordCommitments never fails the compile. Lineage: the vault file when source is
// in this batch, and the memory saved in this batch that says the same thing.
func (s *Service) recordCommitments(ctx context.Context, content string, tenant uuid.UUID, fileIDs map[string]uuid.UUID, saved []MemoryRef) {
	extracted := ParseExtractedCommitments(content)
	if len(extracted) == 0 {
		return
	}
	memoryByText := make(map[string]uuid.UUID, len(saved))
	for _, m := range saved {
		key := NormalizeCommitment(m.Content)
		if _, ok := memoryByText[key]; !ok {
			memoryByText[key] = m.ID
		}
	}
	items := make([]NewCommitment, len(extracted))
	for i, e := range extracted {
		items[i] = NewCommitment{Text: e.Text, Due: e.Due}
		if id, ok := memoryByText[NormalizeCommitment(e.Text)]; ok {
			items[i].MemoryID = &id
		}
		if e.Source != nil {
			if id, ok := fileIDs[*e.Source]; ok {
				items[i].SourceVaultFileID = &id
			}
		}
	}
	added, err := RecordCommitments(ctx, s.pool, tenant, items, s.opt.Now())
	if err != nil {
		slog.WarnContext(ctx, "kb-compile commitments not recorded", "tenant", ids.Upper(tenant), "error", err)
		return
	}
	slog.InfoContext(ctx, "kb-compile recorded commitments", "added", added, "extracted", len(extracted), "tenant", ids.Upper(tenant))
}

// ContentHash is contentHash: the SHA256 hex digest kb_compile_reject_list stores.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (s *Service) loadRejectedHashes(ctx context.Context, tenant uuid.UUID) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT content_hash FROM kb_compile_reject_list WHERE tenant_id = $1`, tenant)
	if err != nil {
		return nil, err
	}
	hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		out[h] = true
	}
	return out, nil
}

func isTextLike(contentType string) bool {
	mime, _, _ := strings.Cut(contentType, ";")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "text/")
}

func (s *Service) markProcessed(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE vault_files SET processed_at = $1, updated_at = updated_at WHERE id = ANY($2::uuid[])`, at, ids)
	return err
}

// RefreshSpaceCounters is refreshSpaceCounters: note_count recounted live and
// last_compiled_at stamped, for the Spaces this compile touched.
func (s *Service) RefreshSpaceCounters(ctx context.Context, tenant uuid.UUID, spaceIDs map[uuid.UUID]bool, at time.Time) error {
	if len(spaceIDs) == 0 {
		return nil
	}
	list := make([]uuid.UUID, 0, len(spaceIDs))
	for id := range spaceIDs {
		list = append(list, id)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE spaces s
		SET note_count = (
			SELECT COUNT(*)::int FROM vault_files vf
			WHERE vf.tenant_id = s.tenant_id AND vf.space_id = s.id
		), last_compiled_at = $1
		WHERE s.tenant_id = $2 AND s.id = ANY($3::uuid[])`, at, tenant, list)
	return err
}
