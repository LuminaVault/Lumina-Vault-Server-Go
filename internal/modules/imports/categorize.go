package imports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/imports/db"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxNewSpaces = 6
	snippetChars = 400
)

// Categorizer is ImportCategorizationService: one model call that proposes a Space per
// item. Same one-shot JSON as the compile loop; it asks for JSON and parses leniently.
type Categorizer struct {
	Service      *Service
	Transport    llm.ChatTransport
	DefaultModel string
}

type categorizePayload struct {
	Model          string            `json:"model"`
	Messages       []llm.ChatMessage `json:"messages"`
	ResponseFormat map[string]string `json:"response_format"`
	Temperature    float64           `json:"temperature"`
	Stream         bool              `json:"stream"`
}

// CategorizeSystem is the system prompt over a list of spaces, one line each.
func CategorizeSystem(existing string) string {
	if existing == "" {
		existing = "(none yet)"
	}
	return `You are organizing a user's freshly imported items into Spaces (folders).
Existing Spaces (slug: name):
` + existing + `

For EACH item, choose the single best target:
- an existing Space slug above, when it clearly fits;
- "new:<Short Title>" only when several items share a clear theme not
  covered by an existing Space (at most ` + strconv.Itoa(maxNewSpaces) + ` distinct new
  Spaces total across all items);
- "imported" when nothing fits.
Prefer reusing existing Spaces. Return ONLY JSON of the form
{"mappings":[{"id":"<item id>","space":"<slug | new:Name | imported>"}]}.`
}

// Categorize is categorize.
//
// The snippet path is rawRoot + vf.path from the row, with no SanitizePath, as in Swift.
func (g *Categorizer) Categorize(ctx context.Context, tenant, session uuid.UUID) error {
	s := g.Service
	if _, err := s.findSession(ctx, tenant, session); err != nil {
		return err
	}
	items, err := s.q.ActiveItems(ctx, db.ActiveItemsParams{TenantID: tenant, SessionID: session})
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return s.setStatus(ctx, session, StatusReview)
	}
	spaces, err := s.q.TenantSpaces(ctx, tenant)
	if err != nil {
		return err
	}
	lines := []string{}
	for _, sp := range spaces {
		if sp.Slug == ImportedSlug {
			continue
		}
		line := "- " + sp.Slug + ": " + sp.Name
		if sp.Description != nil {
			line += " — " + *sp.Description
		}
		lines = append(lines, line)
	}

	rawRoot := s.paths.Raw(tenant)
	blocks := make([]string, len(items))
	for i, item := range items {
		snippet := ""
		if item.VaultFileID != nil {
			file, err := s.store.FindByID(ctx, tenant, *item.VaultFileID)
			if err != nil {
				return err
			}
			if file != nil {
				if data, err := os.ReadFile(rawRoot + "/" + file.Path); err == nil && utf8.Valid(data) { //nolint:gosec // path from the tenant's row, as in Swift
					snippet = swiftstr.Prefix(strings.TrimPrefix(string(data), "\ufeff"), snippetChars)
				}
			}
		}
		url := ""
		if item.Url != nil {
			url = *item.Url
		}
		blocks[i] = "[id=" + ids.Upper(item.ID) + "] url=" + url + "\n" + snippet
	}
	payload, err := json.Marshal(categorizePayload{
		Model: g.DefaultModel,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: CategorizeSystem(strings.Join(lines, "\n"))},
			{Role: "user", Content: "Items:\n\n" + strings.Join(blocks, "\n\n")},
		},
		ResponseFormat: map[string]string{"type": "json_object"},
		Temperature:    0.1,
	})
	if err != nil {
		return err
	}
	raw, err := g.Transport.ChatCompletions(ctx, payload, ids.Upper(tenant), "")
	if err != nil {
		return err
	}
	content, err := firstChoiceContent(raw.Data)
	if err != nil {
		return err
	}
	mapping := ParseMappings(content)

	// Enforce the new-Space cap; everything beyond it falls back to the inbox.
	newSpaces := []string{}
	for _, item := range items {
		target, ok := mapping[ids.Upper(item.ID)]
		if !ok {
			target = ImportedSlug
		}
		if swiftstr.HasASCIIPrefix(target, "new:") {
			name := vaultfiles.Lowercased(swiftstr.TrimWhitespaces(strings.TrimPrefix(target, "new:")))
			if !contains(newSpaces, name) {
				if len(newSpaces) >= maxNewSpaces {
					target = ImportedSlug
				} else {
					newSpaces = append(newSpaces, name)
				}
			}
		}
		if err := s.q.SetItemCategorized(ctx, db.SetItemCategorizedParams{ID: item.ID, ProposedSpace: &target}); err != nil {
			return err
		}
	}
	return s.setStatus(ctx, session, StatusReview)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// firstChoiceContent decodes CategorizeResponse: choices and message are required, so
// their absence is a decoding error, a 500; no choices or no content is empty.
func firstChoiceContent(data []byte) (string, error) {
	var body struct {
		Choices *[]struct {
			Message *struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", fmt.Errorf("imports: decode categorize response: %w", err)
	}
	if body.Choices == nil {
		return "", errors.New("imports: categorize response has no choices")
	}
	for _, c := range *body.Choices {
		if c.Message == nil {
			return "", errors.New("imports: categorize choice has no message")
		}
	}
	if len(*body.Choices) == 0 || (*body.Choices)[0].Message.Content == nil {
		return "", nil
	}
	return *(*body.Choices)[0].Message.Content, nil
}

// ParseMappings is parseMappings: tolerates code fences and prose around the object.
func ParseMappings(raw string) map[string]string {
	s := swiftstr.TrimWhitespacesAndNewlines(raw)
	if strings.Contains(s, "```") {
		s = strings.ReplaceAll(s, "```json", "")
		s = strings.ReplaceAll(s, "```", "")
		s = swiftstr.TrimWhitespacesAndNewlines(s)
	}
	root, ok := jsonObject(s)
	if !ok {
		lo, hi := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
		if lo < 0 || hi <= lo {
			return map[string]string{}
		}
		if root, ok = jsonObject(s[lo : hi+1]); !ok {
			return map[string]string{}
		}
	}
	list, ok := root["mappings"].([]any)
	if !ok {
		return map[string]string{}
	}
	out := map[string]string{}
	for _, entry := range list {
		// `as? [[String: Any]]` fails whole if one element isn't an object.
		m, ok := entry.(map[string]any)
		if !ok {
			return map[string]string{}
		}
		id, idOK := m["id"].(string)
		space, spaceOK := m["space"].(string)
		if idOK && spaceOK {
			out[id] = space
		}
	}
	return out
}

func jsonObject(s string) (map[string]any, bool) {
	var root map[string]any
	if err := json.Unmarshal([]byte(s), &root); err != nil || root == nil {
		return nil, false
	}
	return root, true
}
