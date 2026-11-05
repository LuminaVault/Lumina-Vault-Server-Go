package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// filingSnippetChars is CaptureFilingService.snippetChars.
const filingSnippetChars = 1200

// FilingService is CaptureFilingService: it asks the model which of the user's existing
// Spaces fits an unfiled note and stores the answer as metadata.suggestedSpaceID. It
// never moves anything and never invents a Space. No temperature is sent.
type FilingService struct {
	Pool         *pgxpool.Pool
	Transport    llm.ChatTransport
	Paths        vaultpaths.Paths
	Store        *vaultfiles.Store
	DefaultModel string
}

// SuggestBestEffort is suggestBestEffort: every failure is logged and swallowed, since
// a missing suggestion is the normal state of a capture.
func (s *FilingService) SuggestBestEffort(ctx context.Context, tenant, file uuid.UUID) {
	slug, err := s.Suggest(ctx, tenant, file)
	if err != nil {
		slog.WarnContext(ctx, "capture.autofile.failed", "tenant", tenant, "file", file, "error", err)
		return
	}
	if slug != "" {
		slog.InfoContext(ctx, "capture.autofile.suggested", "tenant", tenant, "file", file, "space", slug)
	}
}

type filingSpace struct {
	id          uuid.UUID
	slug, name  string
	description *string
}

func (s *FilingService) spaces(ctx context.Context, tenant uuid.UUID) ([]filingSpace, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, slug, name, description FROM spaces WHERE tenant_id = $1 AND slug <> 'imported'`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []filingSpace
	for rows.Next() {
		var sp filingSpace
		if err := rows.Scan(&sp.id, &sp.slug, &sp.name, &sp.description); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// FilingSystemPrompt is the system prompt over the catalog, one Space per line.
func FilingSystemPrompt(catalog string) string {
	return `You file one note into the user's existing Spaces (folders).
Spaces (slug: name — description):
` + catalog + `

Pick the single Space this note clearly belongs in, or "none" when no
Space clearly fits. Sharing a word with a Space name is not enough.
Return ONLY JSON: {"space":"<slug or none>"}.`
}

// Suggest is suggest. It returns the suggested Space's slug, or "" when nothing fits or
// the file is no longer unfiled. The file is reached through ResolveInside, without
// SanitizePath: the row's path passed it on upload.
func (s *FilingService) Suggest(ctx context.Context, tenant, fileID uuid.UUID) (string, error) {
	file, err := s.Store.FindByID(ctx, tenant, fileID)
	if err != nil || file == nil || file.SpaceID != nil {
		return "", err
	}
	spaces, err := s.spaces(ctx, tenant)
	if err != nil || len(spaces) == 0 {
		return "", err
	}
	path, err := vaultfiles.ResolveInside(s.Paths.Raw(tenant), file.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path) //nolint:gosec // resolved inside the tenant's root
	if err != nil || !utf8.Valid(data) || swiftstr.TrimWhitespacesAndNewlines(string(data)) == "" {
		return "", nil //nolint:nilerr // `try? String(contentsOf:)`
	}
	lines := make([]string, len(spaces))
	for i, sp := range spaces {
		lines[i] = "- " + sp.slug + ": " + sp.name
		if sp.description != nil {
			lines[i] += " — " + *sp.description
		}
	}
	payload, err := json.Marshal(struct {
		Model    string            `json:"model"`
		Messages []llm.ChatMessage `json:"messages"`
		Stream   bool              `json:"stream"`
	}{Model: s.DefaultModel, Messages: []llm.ChatMessage{
		{Role: "system", Content: FilingSystemPrompt(strings.Join(lines, "\n"))},
		{Role: "user", Content: swiftstr.Prefix(strings.TrimPrefix(string(data), "\ufeff"), filingSnippetChars)},
	}})
	if err != nil {
		return "", err
	}
	raw, err := s.Transport.ChatCompletions(ctx, payload, ids.Upper(tenant), "")
	if err != nil {
		return "", err
	}
	content, err := filingContent(raw.Data)
	if err != nil {
		return "", err
	}
	slug, ok := ParseSuggestion(content)
	if !ok {
		return "", nil
	}
	var space *filingSpace
	for i := range spaces {
		if spaces[i].slug == slug {
			space = &spaces[i]
			break
		}
	}
	if space == nil {
		return "", nil
	}
	// Re-check: the user may have filed it while the model was thinking.
	fresh, err := s.Store.FindByID(ctx, tenant, fileID)
	if err != nil || fresh == nil || fresh.SpaceID != nil {
		return "", err
	}
	metadata := fresh.Metadata
	if metadata == nil {
		metadata = &vaultfiles.Metadata{}
	}
	metadata.SuggestedSpaceID = &space.id
	if err := s.Store.UpdateMetadata(ctx, fresh.ID, metadata); err != nil {
		return "", err
	}
	return slug, nil
}

func filingContent(data []byte) (string, error) {
	var body struct {
		Choices *[]struct {
			Message *struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", fmt.Errorf("capture: decode filing response: %w", err)
	}
	if body.Choices == nil {
		return "", errors.New("capture: filing response has no choices")
	}
	for _, c := range *body.Choices {
		if c.Message == nil {
			return "", errors.New("capture: filing choice has no message")
		}
	}
	if len(*body.Choices) == 0 || (*body.Choices)[0].Message.Content == nil {
		return "", nil
	}
	return *(*body.Choices)[0].Message.Content, nil
}

// ParseSuggestion is parseSuggestion: `{"space":"work"}` is work; none, empty or
// unparseable is no suggestion.
func ParseSuggestion(raw string) (string, bool) {
	s := swiftstr.TrimWhitespacesAndNewlines(raw)
	if strings.Contains(s, "```") {
		s = strings.ReplaceAll(s, "```json", "")
		s = strings.ReplaceAll(s, "```", "")
	}
	if lo, hi := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}'); lo >= 0 && lo < hi {
		s = s[lo : hi+1]
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(s), &object); err != nil {
		return "", false
	}
	value, ok := object["space"].(string)
	if !ok {
		return "", false
	}
	slug := vaultfiles.Lowercased(swiftstr.TrimWhitespaces(value))
	if slug == "" || slug == "none" {
		return "", false
	}
	return slug, true
}
