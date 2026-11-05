package memoryagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	defaultMemoIterations = 6
	defaultMaxMemoBytes   = 32 * 1024
	maxTopicChars         = 256
	maxSlugChars          = 64
)

var (
	errBadTopic         = wire.NewError(http.StatusBadRequest, "topic empty or too long")
	errMemoNotConverged = wire.NewError(http.StatusBadGateway, "memo generator did not converge")
)

// MemoOptions are MemoGeneratorService's dependencies.
type MemoOptions struct {
	Transport     llm.ChatTransport
	Memories      *memorystore.Repository
	Embeddings    embedding.Service
	Paths         vaultpaths.Paths
	Store         *vaultfiles.Store
	DefaultModel  string
	MaxIterations int // 0 means 6
	MaxMemoBytes  int // 0 means 32 KiB
	Now           func() time.Time
}

// MemoService is MemoGeneratorService: an agent loop with read-only tools that writes
// a synthesis memo to the vault. It never calls memory_upsert.
type MemoService struct{ opt MemoOptions }

// NewMemoService builds the service.
func NewMemoService(o MemoOptions) *MemoService {
	if o.MaxIterations == 0 {
		o.MaxIterations = defaultMemoIterations
	}
	if o.MaxMemoBytes == 0 {
		o.MaxMemoBytes = defaultMaxMemoBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &MemoService{opt: o}
}

// MemoResult is MemoGenerationResult.
type MemoResult struct {
	Memo      string
	Path      *string
	SourceIDs []uuid.UUID
	Summary   string
}

// memoSystemPrompt is runAgent's system prompt.
func memoSystemPrompt(hint *string) string {
	suffix := ""
	if hint != nil {
		suffix = "User hint: " + *hint
	}
	return `You are Hermes writing a synthesis memo for the user. Your job:
1. Search their memories for the supplied topic using ` + "`session_search`" + `
   until you have enough to write the memo.
2. Write a structured markdown memo with these sections:
   ## Summary
   ## Key Points
   ## Connections
   ## Open Questions
3. Cite sources inline with ` + "`[[memory:<uuid>]]`" + ` for any claim that
   comes from a stored memory. Do NOT invent quotes or facts.
4. Match the user's voice from their existing notes — do not
   default to corporate tone.

If there are no relevant memories, write a short "Hermes hasn't
seen anything about <topic> yet" memo and stop.
` + suffix
}

var memoSearchTool = tool("session_search",
	"Semantic-search the user's memories. Read-only. Returns up to `limit` memories.",
	map[string]propertySchema{
		"query": {Type: "string", Description: "Natural-language search query."},
		"limit": {Type: "integer", Description: "Maximum memories (1-20, default 5)."},
	}, "query")

// Generate is generate.
func (s *MemoService) Generate(ctx context.Context, tenant uuid.UUID, sessionKey, sessionID, topic string, hint *string, save bool) (MemoResult, error) {
	if count := swiftstr.Count(topic); count == 0 || count > maxTopicChars {
		return MemoResult{}, errBadTopic
	}
	summary, sources, err := s.runAgent(ctx, tenant, sessionKey, sessionID, topic, hint)
	if err != nil {
		return MemoResult{}, err
	}
	if len(summary) > s.opt.MaxMemoBytes {
		return MemoResult{}, wire.NewError(http.StatusRequestEntityTooLarge, "memo exceeds "+strconv.Itoa(s.opt.MaxMemoBytes)+" bytes")
	}
	memo := s.renderMarkdown(topic, sources, summary)
	result := MemoResult{Memo: memo, SourceIDs: sources, Summary: summary}
	if save {
		saved, err := s.persist(ctx, tenant, topic, memo)
		if err != nil {
			return MemoResult{}, err
		}
		result.Path = &saved
	}
	return result, nil
}

func (s *MemoService) runAgent(ctx context.Context, tenant uuid.UUID, sessionKey, sessionID, topic string, hint *string) (string, []uuid.UUID, error) {
	conversation := []agentMessage{
		textMessage("system", memoSystemPrompt(hint)),
		textMessage("user", "Topic: "+topic),
	}
	seen := map[uuid.UUID]bool{}
	consulted := []uuid.UUID{}
	for range s.opt.MaxIterations {
		payload, err := json.Marshal(chatRequestBody{
			Model: s.opt.DefaultModel, Messages: conversation, Tools: []toolDefinition{memoSearchTool}, ToolChoice: "auto", Temperature: 0.3,
		})
		if err != nil {
			return "", nil, err
		}
		raw, err := s.opt.Transport.ChatCompletions(ctx, payload, sessionKey, sessionID)
		if err != nil {
			return "", nil, err
		}
		assistant, err := decodeChatResponse(raw.Data)
		if err != nil {
			return "", nil, err
		}
		conversation = append(conversation, *assistant)
		if len(assistant.ToolCalls) > 0 {
			for _, call := range assistant.ToolCalls {
				result := s.dispatch(ctx, tenant, call, seen, &consulted)
				id, name := call.ID, call.Function.Name
				conversation = append(conversation, agentMessage{Role: "tool", Content: &result, ToolCallID: &id, Name: &name})
			}
			continue
		}
		summary := ""
		if assistant.Content != nil {
			summary = *assistant.Content
		}
		return summary, consulted, nil
	}
	slog.WarnContext(ctx, "memo agent loop hit max iterations", "iterations", s.opt.MaxIterations)
	return "", nil, errMemoNotConverged
}

func (s *MemoService) dispatch(ctx context.Context, tenant uuid.UUID, call toolCall, seen map[uuid.UUID]bool, consulted *[]uuid.UUID) string {
	if call.Function.Name != "session_search" {
		return toolError("unknown tool " + call.Function.Name)
	}
	args, err := wire.Decode[struct {
		Query string `json:"query"`
		Limit *int   `json:"limit,omitempty"`
	}]([]byte(call.Function.Arguments))
	if err != nil {
		return toolError("session_search failed: " + err.Error())
	}
	vec, err := embedding.EmbedForQuery(ctx, s.opt.Embeddings, args.Query, tenant, "memo.sessionSearch")
	if err != nil {
		return toolError("session_search failed: " + err.Error())
	}
	limit := 5
	if args.Limit != nil {
		limit = *args.Limit
	}
	hits, err := s.opt.Memories.SearchDocuments(ctx, tenant, args.Query, vec, max(1, min(limit, 20)), memorystore.SearchOptions{})
	if err != nil {
		return toolError("session_search failed: " + err.Error())
	}
	rows := make([]map[string]string, len(hits))
	for i, hit := range hits {
		if !seen[hit.ID] {
			seen[hit.ID] = true
			*consulted = append(*consulted, hit.ID)
		}
		rows[i] = map[string]string{"id": ids.Upper(hit.ID), "content": hit.Content, "distance": strconv.FormatFloat(float64(hit.Distance), 'g', -1, 32)}
	}
	return encodeTool(map[string]any{"status": "ok", "results": rows})
}

func (s *MemoService) renderMarkdown(topic string, sources []uuid.UUID, body string) string {
	var f strings.Builder
	f.WriteString("---\n")
	f.WriteString(`topic: "` + strings.ReplaceAll(topic, `"`, `\"`) + "\"\n")
	f.WriteString("generated_at: " + s.opt.Now().UTC().Format("2006-01-02T15:04:05Z") + "\n")
	f.WriteString("hermes_model: " + s.opt.DefaultModel + "\n")
	if len(sources) > 0 {
		f.WriteString("source_memory_ids:\n")
		for _, id := range sources {
			f.WriteString("  - " + ids.Upper(id) + "\n")
		}
	}
	f.WriteString("---\n\n")
	return f.String() + body
}

func (s *MemoService) persist(ctx context.Context, tenant uuid.UUID, topic, memo string) (string, error) {
	if err := s.opt.Paths.EnsureTenantDirectories(tenant); err != nil {
		return "", err
	}
	stamp := s.opt.Now().UTC().Format("2006-01-02")
	slug := MemoSlug(topic)
	relativeDir := "memos/" + stamp
	dir := filepath.Join(s.opt.Paths.Raw(tenant), relativeDir)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // as Foundation's createDirectory
		return "", err
	}
	target := filepath.Join(dir, slug+".md")
	finalRelative := relativeDir + "/" + slug + ".md"
	// Don't clobber a memo on this topic from earlier today.
	if vaultfiles.Exists(target) {
		alt := slug + "-" + strings.ToLower(uuid.NewString()[:8]) + ".md"
		target = filepath.Join(dir, alt)
		finalRelative = relativeDir + "/" + alt
	}
	data := []byte(memo)
	if err := vaultfiles.WriteAtomic(target, data); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if _, err := s.opt.Store.Insert(ctx, vaultfiles.UpsertParams{
		TenantID: tenant, Path: finalRelative, ContentType: "text/markdown",
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
	}); err != nil {
		return "", err
	}
	return finalRelative, nil
}

// MemoSlug is slug(_:): lowercase, alphanumerics and hyphens, at most 64 characters.
func MemoSlug(topic string) string {
	var out []string
	for _, ch := range swiftstr.Characters(strings.ToLower(topic)) {
		r := []rune(ch)[0]
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			out = append(out, ch)
		case ch == " " || ch == "-" || ch == "_":
			if len(out) > 0 && out[len(out)-1] != "-" {
				out = append(out, "-")
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == "-" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return "memo"
	}
	if len(out) > maxSlugChars {
		out = out[:maxSlugChars]
	}
	return strings.Join(out, "")
}

// TitleFromPath is titleFromPath: "memos/2026-05-17/sleep-patterns.md" is "Sleep Patterns".
func TitleFromPath(path string) string {
	stem := vaultfiles.LastPathComponent(path)
	if i := strings.LastIndexByte(stem, '.'); i > 0 {
		stem = stem[:i]
	}
	parts := strings.FieldsFunc(stem, func(r rune) bool { return r == '-' })
	for i, p := range parts {
		chars := swiftstr.Characters(p)
		if len(chars) > 0 {
			parts[i] = strings.ToUpper(chars[0]) + strings.Join(chars[1:], "")
		}
	}
	return strings.Join(parts, " ")
}
