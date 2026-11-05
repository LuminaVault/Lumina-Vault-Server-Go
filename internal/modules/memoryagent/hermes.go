package memoryagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/chunkindex"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultMaxToolIterations = 5
	maxTags                  = 5
)

var (
	errNoChoices    = wire.NewError(http.StatusBadGateway, "hermes returned no choices")
	errNotConverged = wire.NewError(http.StatusBadGateway, "hermes agent did not converge")
	errNoUpsert     = wire.NewError(http.StatusBadGateway, "hermes did not call memory_upsert")
)

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

// agentMessage is AgentMessage. Content nil encodes as absent, as Swift's synthesized
// encoder does for an optional.
type agentMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID *string    `json:"tool_call_id,omitempty"`
	Name       *string    `json:"name,omitempty"`
}

func textMessage(role, content string) agentMessage {
	return agentMessage{Role: role, Content: &content}
}

type toolDefinition struct {
	Type     string          `json:"type"`
	Function toolDefinition2 `json:"function"`
}

type toolDefinition2 struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  parameterSchema `json:"parameters"`
}

type parameterSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]propertySchema `json:"properties"`
	Required   []string                  `json:"required"`
}

type itemsSchema struct {
	Type string `json:"type"`
}

type propertySchema struct {
	Type        string       `json:"type"`
	Description string       `json:"description,omitempty"`
	Items       *itemsSchema `json:"items,omitempty"`
}

type chatRequestBody struct {
	Model       string           `json:"model"`
	Messages    []agentMessage   `json:"messages"`
	Tools       []toolDefinition `json:"tools"`
	ToolChoice  string           `json:"tool_choice"`
	Temperature float64          `json:"temperature"`
	Stream      bool             `json:"stream"`
}

// decodeChatResponse decodes ChatResponseBody. Its id, model and choices are required,
// as are role and the tool call fields: an answer missing one is a decoding error, a 500.
func decodeChatResponse(data []byte) (*agentMessage, error) {
	var body struct {
		ID      *string `json:"id"`
		Model   *string `json:"model"`
		Choices *[]struct {
			Message *struct {
				Role      *string `json:"role"`
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       *string `json:"id"`
					Type     *string `json:"type"`
					Function *struct {
						Name      *string `json:"name"`
						Arguments *string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				ToolCallID *string `json:"tool_call_id"`
				Name       *string `json:"name"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("memoryagent: decode response: %w", err)
	}
	if body.ID == nil || body.Model == nil || body.Choices == nil {
		return nil, errors.New("memoryagent: response is missing id, model or choices")
	}
	if len(*body.Choices) == 0 {
		return nil, errNoChoices
	}
	m := (*body.Choices)[0].Message
	if m == nil || m.Role == nil {
		return nil, errors.New("memoryagent: choice has no message role")
	}
	out := &agentMessage{Role: *m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
	for _, c := range m.ToolCalls {
		if c.ID == nil || c.Type == nil || c.Function == nil || c.Function.Name == nil || c.Function.Arguments == nil {
			return nil, errors.New("memoryagent: incomplete tool call")
		}
		out.ToolCalls = append(out.ToolCalls, toolCall{*c.ID, *c.Type, toolFunction{*c.Function.Name, *c.Function.Arguments}})
	}
	return out, nil
}

// encodeTool is encodeJSON: what a tool answers the model, without HTML escaping.
func encodeTool(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return `{"status":"error","reason":"could not encode tool result"}`
	}
	return strings.TrimRight(buf.String(), "\n")
}

func toolError(reason string) string {
	return encodeTool(map[string]string{"status": "error", "reason": reason})
}

// Telemetry is what RetrievalTelemetryWorker would be handed. Go inserts directly:
// the table is insert-only (wave-4-plan.md). Nil means retrieval.telemetry.enabled off.
type Telemetry interface {
	Record(ctx context.Context, sample TelemetrySample)
}

// TelemetrySample is RetrievalTelemetrySample.from.
type TelemetrySample struct {
	Tenant     uuid.UUID
	SourcePath string
	SpaceID    *uuid.UUID
	Distances  []float32
	Limit      int
}

// Sink receives memory_upserted, when the agent's upsert tool saved a memory.
type Sink func(tenant uuid.UUID)

// ServiceOptions are HermesMemoryService's dependencies.
type ServiceOptions struct {
	Transport         llm.ChatTransport
	Memories          *memorystore.Repository
	Embeddings        embedding.Service
	DefaultModel      string
	MaxToolIterations int // 0 means 5
	VaultMapEnabled   bool
	Telemetry         Telemetry // nil means none
	Hybrid            *chunkindex.Hybrid
	OnUpserted        Sink
}

// Service is HermesMemoryService: a tool-calling loop over the tenant's memory store.
type Service struct{ opt ServiceOptions }

// NewService builds the service.
func NewService(o ServiceOptions) *Service {
	if o.MaxToolIterations == 0 {
		o.MaxToolIterations = defaultMaxToolIterations
	}
	return &Service{opt: o}
}

// SearchAnswer is MemorySearchAnswer.
type SearchAnswer struct {
	Hits    []memorystore.SearchResult
	Summary string
}

// UpsertResult is MemoryUpsertResult.
type UpsertResult struct {
	Memory  *memorystore.Memory
	Summary string
}

type agentOutcome struct {
	summary  string
	upserted []*memorystore.Memory
	hits     []memorystore.SearchResult
}

// upsertSystemPrompt is upsert's system prompt.
const upsertSystemPrompt = `You are Hermes, a per-user memory agent. The user wants to record a new memory.
Workflow — do BOTH steps before replying:
  1. Call ` + "`memory_upsert`" + ` exactly once with the content provided.
  2. Call ` + "`tag_extract`" + ` exactly once with 1–5 short topical tags
     summarizing the memory's subject. Tags must be lowercase,
     1–2 words each, no punctuation. Examples: "running",
     "work-meeting", "swift", "ios-bug".
After both tools return, reply with a one-sentence acknowledgement
summarizing what was stored.`

// Upsert is upsert.
func (s *Service) Upsert(ctx context.Context, tenant uuid.UUID, sessionKey, sessionID, content string) (UpsertResult, error) {
	outcome, err := s.runAgent(ctx, tenant, sessionKey, sessionID, []agentMessage{
		textMessage("system", upsertSystemPrompt),
		textMessage("user", content),
	}, []toolDefinition{memoryUpsertTool, sessionSearchTool, tagExtractTool})
	if err != nil {
		return UpsertResult{}, err
	}
	if len(outcome.upserted) == 0 {
		return UpsertResult{}, errNoUpsert
	}
	return UpsertResult{Memory: outcome.upserted[0], Summary: outcome.summary}, nil
}

// searchSystemPrompt is search's system prompt.
func searchSystemPrompt(limit int, vaultMap bool) string {
	prompt := `You are Hermes, a per-user memory agent. The user is asking you to recall.
Call the ` + "`session_search`" + ` tool with the user's query and a sensible ` + "`limit`" + `
(default ` + strconv.Itoa(limit) + `). After the tool returns, synthesize a concise answer
that cites the relevant memories you saw. If the search returns nothing,
say so plainly.`
	if vaultMap {
		prompt += "\nIf the query clearly belongs to one topic area, you MAY first call\n`vault_map` to see the user's Spaces (concern folders) and then pass the\nmatching Space `slug` to `session_search` to narrow the search."
	}
	return prompt
}

// Search is search.
func (s *Service) Search(ctx context.Context, tenant uuid.UUID, sessionKey, sessionID, query string, limit int) (SearchAnswer, error) {
	tools := []toolDefinition{sessionSearchTool}
	if s.opt.VaultMapEnabled {
		tools = append(tools, vaultMapTool)
	}
	outcome, err := s.runAgent(ctx, tenant, sessionKey, sessionID, []agentMessage{
		textMessage("system", searchSystemPrompt(limit, s.opt.VaultMapEnabled)),
		textMessage("user", query),
	}, tools)
	if err != nil {
		return SearchAnswer{}, err
	}
	return SearchAnswer{Hits: outcome.hits, Summary: outcome.summary}, nil
}

// NormalizeTags is normalizeTags: lowercase, trimmed, deduped in first-seen order, at most 5.
func NormalizeTags(raw []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range raw {
		cleaned := swiftstr.TrimWhitespacesAndNewlines(strings.ToLower(value))
		if cleaned == "" || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
		if len(out) == maxTags {
			break
		}
	}
	return out
}

func (s *Service) runAgent(ctx context.Context, tenant uuid.UUID, sessionKey, sessionID string, initial []agentMessage, tools []toolDefinition) (*agentOutcome, error) {
	conversation := initial
	outcome := &agentOutcome{}
	for range s.opt.MaxToolIterations {
		payload, err := json.Marshal(chatRequestBody{
			Model: s.opt.DefaultModel, Messages: conversation, Tools: tools, ToolChoice: "auto", Temperature: 0.2,
		})
		if err != nil {
			return nil, err
		}
		raw, err := s.opt.Transport.ChatCompletions(ctx, payload, sessionKey, sessionID)
		if err != nil {
			return nil, err
		}
		assistant, err := decodeChatResponse(raw.Data)
		if err != nil {
			return nil, err
		}
		conversation = append(conversation, *assistant)
		if len(assistant.ToolCalls) > 0 {
			for _, call := range assistant.ToolCalls {
				result := s.dispatch(ctx, tenant, call, outcome)
				id, name := call.ID, call.Function.Name
				conversation = append(conversation, agentMessage{Role: "tool", Content: &result, ToolCallID: &id, Name: &name})
			}
			continue
		}
		if assistant.Content != nil {
			outcome.summary = *assistant.Content
		}
		return outcome, nil
	}
	slog.WarnContext(ctx, "hermes agent loop hit max iterations", "iterations", s.opt.MaxToolIterations)
	return nil, errNotConverged
}

func (s *Service) dispatch(ctx context.Context, tenant uuid.UUID, call toolCall, outcome *agentOutcome) string {
	args := []byte(call.Function.Arguments)
	switch call.Function.Name {
	case "memory_upsert":
		result, err := s.memoryUpsert(ctx, tenant, args, outcome)
		if err != nil {
			return toolError("memory_upsert failed: " + err.Error())
		}
		return result
	case "session_search":
		result, err := s.sessionSearch(ctx, tenant, args, outcome)
		if err != nil {
			return toolError("session_search failed: " + err.Error())
		}
		return result
	case "vault_map":
		result, err := s.vaultMap(ctx, tenant)
		if err != nil {
			return toolError("vault_map failed: " + err.Error())
		}
		return result
	case "tag_extract":
		result, err := s.tagExtract(ctx, tenant, args, outcome)
		if err != nil {
			return toolError("tag_extract failed: " + err.Error())
		}
		return result
	}
	return toolError("unknown tool " + call.Function.Name)
}

func (s *Service) memoryUpsert(ctx context.Context, tenant uuid.UUID, raw []byte, outcome *agentOutcome) (string, error) {
	// Swift decodes camelCase keys: the schema's source_vault_file_id never matches
	// sourceVaultFileId, so what the model sends is dropped. Reproduced.
	args, err := wire.Decode[struct {
		Content           string     `json:"content"`
		SourceVaultFileID *wire.UUID `json:"sourceVaultFileId,omitempty"`
	}](raw)
	if err != nil {
		return "", err
	}
	vec, err := embedding.EmbedForWrite(ctx, s.opt.Embeddings, args.Content, tenant, "hermes.memoryUpsert")
	if err != nil {
		return "", err
	}
	p := memorystore.CreateParams{Tenant: tenant, Content: args.Content, Embedding: vec}
	if args.SourceVaultFileID != nil {
		p.SourceVaultFileID = &args.SourceVaultFileID.UUID
	}
	saved, err := s.opt.Memories.Create(ctx, p)
	if err != nil {
		return "", err
	}
	outcome.upserted = append(outcome.upserted, saved)
	if s.opt.OnUpserted != nil {
		s.opt.OnUpserted(tenant)
	}
	return encodeTool(map[string]string{"status": "ok", "id": ids.Upper(saved.ID)}), nil
}

func (s *Service) sessionSearch(ctx context.Context, tenant uuid.UUID, raw []byte, outcome *agentOutcome) (string, error) {
	args, err := wire.Decode[struct {
		Query string  `json:"query"`
		Limit *int    `json:"limit,omitempty"`
		Space *string `json:"space,omitempty"`
	}](raw)
	if err != nil {
		return "", err
	}
	limit := 5
	if args.Limit != nil {
		limit = *args.Limit
	}
	limit = max(1, min(limit, 20))
	// Absent or unknown slug means unscoped.
	var spaceID *uuid.UUID
	if args.Space != nil {
		if slug := swiftstr.TrimWhitespacesAndNewlines(*args.Space); slug != "" {
			spaces, err := s.opt.Memories.SpaceTopology(ctx, tenant)
			if err != nil {
				return "", err
			}
			for i := range spaces {
				if spaces[i].Slug == slug {
					spaceID = &spaces[i].ID
					break
				}
			}
		}
	}
	vec, err := embedding.EmbedForQuery(ctx, s.opt.Embeddings, args.Query, tenant, "hermes.sessionSearch")
	if err != nil {
		return "", err
	}
	opts := memorystore.SearchOptions{SpaceID: spaceID}
	var hits []memorystore.SearchResult
	if s.opt.Hybrid != nil {
		hits, err = s.opt.Hybrid.Search(ctx, tenant, args.Query, vec, limit, opts)
	} else {
		hits, err = s.opt.Memories.SearchDocuments(ctx, tenant, args.Query, vec, limit, opts)
	}
	if err != nil {
		return "", err
	}
	outcome.hits = hits
	if s.opt.Telemetry != nil {
		s.opt.Telemetry.Record(ctx, newSample(tenant, "agentic_search", spaceID, hits, limit))
	}
	rows := make([]map[string]string, len(hits))
	for i, hit := range hits {
		row := map[string]string{"id": ids.Upper(hit.ID), "content": hit.Content, "distance": strconv.FormatFloat(float64(hit.Distance), 'g', -1, 32)}
		if c := hit.Citation; c != nil {
			row["source"] = c.DisplayTrail()
			row["chunk_id"] = c.ChunkID
			row["document_id"] = c.DocumentID
			row["start_line"] = strconv.Itoa(c.StartLine)
			row["end_line"] = strconv.Itoa(c.EndLine)
			if c.Path != nil {
				row["path"] = *c.Path
			}
		}
		rows[i] = row
	}
	return encodeTool(map[string]any{"status": "ok", "results": rows}), nil
}

func newSample(tenant uuid.UUID, source string, spaceID *uuid.UUID, hits []memorystore.SearchResult, limit int) TelemetrySample {
	distances := make([]float32, len(hits))
	for i, h := range hits {
		distances[i] = h.Distance
	}
	return TelemetrySample{Tenant: tenant, SourcePath: source, SpaceID: spaceID, Distances: distances, Limit: limit}
}

func (s *Service) vaultMap(ctx context.Context, tenant uuid.UUID) (string, error) {
	spaces, err := s.opt.Memories.SpaceTopology(ctx, tenant)
	if err != nil {
		return "", err
	}
	list := make([]map[string]string, len(spaces))
	for i, sp := range spaces {
		row := map[string]string{"name": sp.Name, "slug": sp.Slug, "note_count": strconv.Itoa(sp.NoteCount)}
		if sp.LastCompiledAt != nil {
			row["last_compiled_at"] = sp.LastCompiledAt.UTC().Format(time.RFC3339)
		}
		list[i] = row
	}
	return encodeTool(map[string]any{"status": "ok", "spaces": list}), nil
}

func (s *Service) tagExtract(ctx context.Context, tenant uuid.UUID, raw []byte, outcome *agentOutcome) (string, error) {
	args, err := wire.Decode[struct {
		Tags []string `json:"tags"`
	}](raw)
	if err != nil {
		return "", err
	}
	normalized := NormalizeTags(args.Tags)
	if len(outcome.upserted) == 0 {
		return toolError("tag_extract called before memory_upsert"), nil
	}
	target := outcome.upserted[len(outcome.upserted)-1]
	updated, err := s.opt.Memories.UpdateTags(ctx, tenant, target.ID, normalized, nil)
	if err != nil {
		return "", err
	}
	if updated {
		target.Tags = normalized
		if len(normalized) == 0 {
			target.Tags = nil
		}
	}
	return encodeTool(map[string]string{"status": "ok", "tags_count": strconv.Itoa(len(normalized))}), nil
}

func tool(name, description string, properties map[string]propertySchema, required ...string) toolDefinition {
	if required == nil {
		required = []string{}
	}
	return toolDefinition{Type: "function", Function: toolDefinition2{
		Name: name, Description: description,
		Parameters: parameterSchema{Type: "object", Properties: properties, Required: required},
	}}
}

var (
	memoryUpsertTool = tool("memory_upsert",
		"Persist a memory for the current user's tenant. The memory content is embedded server-side and stored in the `memories` table with the user's tenant_id. Call this when the user wants to remember, save, or note something.",
		map[string]propertySchema{
			"content":              {Type: "string", Description: "The memory text to persist verbatim."},
			"source_vault_file_id": {Type: "string", Description: `Optional UUID of the vault file this memory was derived from. Pass this when the memory is grounded in a specific note or document the user just captured or that you read via vault_read so the system can build a "Hermes learned X from your <date> note" lineage trace. Omit when the memory is volunteered by the user with no anchor file.`},
		}, "content")
	sessionSearchTool = tool("session_search",
		"Semantic-search the current user's memories via pgvector cosine similarity. Returns up to `limit` memories ordered by relevance, scoped strictly to the authenticated tenant.",
		map[string]propertySchema{
			"query": {Type: "string", Description: "The natural-language search query."},
			"limit": {Type: "integer", Description: "Maximum number of memories to return (1-20, default 5)."},
			"space": {Type: "string", Description: "Optional Space slug (concern folder) to restrict the search to. Get valid slugs from the `vault_map` tool. Omit to search all Spaces."},
		}, "query")
	vaultMapTool = tool("vault_map",
		"List the current user's Spaces (concern folders) with their note counts, most-populated first. Call this to decide whether to scope a `session_search` to a single Space by passing its `slug`.",
		map[string]propertySchema{})
	tagExtractTool = tool("tag_extract",
		`Attach 1–5 short topical tags to the most recently upserted memory. Tags MUST be lowercase, 1–2 words each, no punctuation. Used by the UI's filter list and the spaced-repetition picker, so prefer stable, reusable tags ("running", "swift", "work-meeting") over one-off labels.`,
		map[string]propertySchema{
			"tags": {Type: "array", Description: "1–5 lowercase tag strings.", Items: &itemsSchema{Type: "string"}},
		}, "tags")
)
