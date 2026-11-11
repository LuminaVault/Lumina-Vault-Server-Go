package plugins

import (
	"encoding/json"
	"lumina/internal/swiftstr"
	"sort"
	"strings"
)

// ConfigField is PluginConfigField.
type ConfigField struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Placeholder *string `json:"placeholder,omitempty"`
	Kind        string  `json:"kind"`
	IsRequired  bool    `json:"isRequired"`
}

// CatalogEntryDTO is PluginCatalogEntryDTO.
type CatalogEntryDTO struct {
	Slug           string        `json:"slug"`
	Name           string        `json:"name"`
	Summary        string        `json:"summary"`
	Description    string        `json:"description"`
	Category       string        `json:"category"`
	CapabilityKind string        `json:"capabilityKind"`
	IconSlug       string        `json:"iconSlug"`
	Version        string        `json:"version"`
	Publisher      string        `json:"publisher"`
	Verified       bool          `json:"verified"`
	ConfigFields   []ConfigField `json:"configFields"`
}

// entry is PluginCatalog.Entry: the DTO, the connector binding, and the
// server-only curation flags.
type entry struct {
	dto               CatalogEntryDTO
	binding           string
	featured, premium bool
}

func ptr(s string) *string { return &s }

func lv(slug, name, summary, description, category, capability, icon string, fields ...ConfigField) CatalogEntryDTO {
	if fields == nil {
		fields = []ConfigField{}
	}
	return CatalogEntryDTO{
		Slug: slug, Name: name, Summary: summary, Description: description, Category: category,
		CapabilityKind: capability, IconSlug: icon, Version: "1.0.0", Publisher: "LuminaVault", Verified: true,
		ConfigFields: fields,
	}
}

// catalog is PluginCatalog.entries (Sources/App/Plugins/PluginCatalog.swift).
var catalog = map[string]entry{
	"readwise": {
		dto: lv("readwise", "Readwise", "Import your Readwise highlights' source articles into your vault.",
			"Pulls the source articles behind your Readwise highlights and "+
				"stages them into your Imported inbox, where Smart Import files "+
				"them into Spaces and compiles them into memories. Provide a "+
				"Readwise access token (readwise.io/access_token).",
			"connector", "connector", "readwise",
			ConfigField{Key: "access_token", Label: "Access token", Placeholder: ptr("readwise.io/access_token"), Kind: "secret", IsRequired: true}),
		binding: "readwise", featured: true,
	},
	"rss": {
		dto: lv("rss", "RSS / Atom feed", "Pull new articles from any RSS or Atom feed into your vault.",
			"Fetches a feed you paste and stages each item's article into your "+
				"Imported inbox, where Smart Import files them into Spaces and "+
				"compiles them into memories. Works with any public RSS or Atom URL.",
			"connector", "connector", "rss",
			ConfigField{Key: "feed_url", Label: "Feed URL", Placeholder: ptr("https://example.com/feed.xml"), Kind: "url", IsRequired: true}),
		binding: "rss",
	},
	"news-ticker": {
		dto: lv("news-ticker", "Breaking news", "A strip of breaking headlines on Home, from publishers' own feeds.",
			"Shows headline, source, time and a link for the world's top "+
				"stories on your Home screen, read through the cluster's shared "+
				"feed aggregator — no API key, no vendor. Add your own site or "+
				"feed addresses to follow them too. Links open the publisher; "+
				"nothing is stored beyond the headline.",
			"ui", "connector", "news-ticker",
			ConfigField{Key: "feed_urls", Label: "Your feeds", Placeholder: ptr("https://example.com, https://blog.example/feed.xml"), Kind: "text", IsRequired: false}),
		binding: "news-ticker", featured: true,
	},
	"raindrop": {
		dto: lv("raindrop", "Raindrop.io", "Import your Raindrop.io bookmarks into your vault.",
			"Pulls your saved Raindrop.io bookmarks and stages each link into "+
				"your Imported inbox, where Smart Import files them into Spaces and "+
				"compiles them into memories. Provide a Raindrop test token "+
				"(app.raindrop.io → Settings → Integrations).",
			"connector", "connector", "raindrop",
			ConfigField{Key: "access_token", Label: "Access token", Placeholder: ptr("test token from raindrop.io integrations"), Kind: "secret", IsRequired: true}),
		binding: "raindrop",
	},
	"byok-embeddings": {
		dto: lv("byok-embeddings", "Your own embeddings key", "Use your own API key for memory embeddings (same model — no re-embedding).",
			"Routes this workspace's embedding requests through your own "+
				"provider API key instead of the shared one. It uses the same "+
				"embedding model the platform already uses, so your existing "+
				"memories stay searchable — no re-embedding. Provide the API key "+
				"for the active embedding provider (OpenAI or Nomic).",
			"memory", "memory", "memory",
			ConfigField{Key: "access_token", Label: "Embedding API key", Placeholder: ptr("sk-… (OpenAI) or nomic key"), Kind: "secret", IsRequired: true}),
		binding: "byok-embeddings", premium: true,
	},
	"reading-time": {
		dto: lv("reading-time", "Reading Time", "Add an estimated reading time to articles you capture.",
			"Estimates how long each captured article takes to read (at "+
				"~200 words per minute) and writes it into the note's "+
				"frontmatter as `reading_time`. Runs automatically after a "+
				"link is captured and enriched — no configuration needed.",
			"capture", "captureHook", "capture"),
		binding: "reading-time",
	},
}

var categories = map[string]bool{"connector": true, "skill": true, "memory": true, "export": true, "ui": true, "theme": true, "capture": true}

// catalogDTOs is PluginCatalog.catalog(category:featured:premium:), sorted by slug.
func catalogDTOs(category string, featured, premium *bool) []CatalogEntryDTO {
	out := []CatalogEntryDTO{}
	for _, e := range catalog {
		if (category == "" || e.dto.Category == category) &&
			(featured == nil || e.featured == *featured) &&
			(premium == nil || e.premium == *premium) {
			out = append(out, e.dto)
		}
	}
	sortBySlug(out)
	return out
}

func sortBySlug(entries []CatalogEntryDTO) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
}

func isPremium(slug string) bool { return catalog[slug].premium }

// validation is PluginCatalog.ValidationResult.
type validation struct {
	kind string // "", "unknown_plugin", "missing", "unknown_field"
	key  string
}

// validate is PluginCatalog.validate. Swift walks a Dictionary's keys, whose
// order is random; with more than one unknown key the reported one varies. Go
// reports the smallest, so a multi-unknown body is not a parity case.
func validate(slug string, config map[string]string) validation {
	e, ok := catalog[slug]
	if !ok {
		return validation{kind: "unknown_plugin"}
	}
	allowed := map[string]bool{}
	for _, f := range e.dto.ConfigFields {
		allowed[f.Key] = true
	}
	keys := make([]string, 0, len(config))
	for k := range config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] {
			return validation{kind: "unknown_field", key: k}
		}
	}
	for _, f := range e.dto.ConfigFields {
		if !f.IsRequired {
			continue
		}
		if v, ok := config[f.Key]; !ok || swiftstr.TrimWhitespacesAndNewlines(v) == "" {
			return validation{kind: "missing", key: f.Key}
		}
	}
	return validation{}
}

const (
	lvPrefix     = "skill-"
	hermesPrefix = "hermes-"
)

// lvSkillName is SkillPluginCatalog.lvSkillName(fromSlug:).
func lvSkillName(slug string) (string, bool) {
	if !strings.HasPrefix(slug, lvPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(slug, lvPrefix)
	return name, name != ""
}

// skillEntry is SkillPluginCatalog.entry(forSkillName:description:).
func skillEntry(name, description string) CatalogEntryDTO {
	return CatalogEntryDTO{
		Slug: lvPrefix + name, Name: titleize(name), Summary: summarize(description), Description: description,
		Category: "skill", CapabilityKind: "skill", IconSlug: "skill", Version: "1.0.0", Publisher: "LuminaVault",
		Verified: true, ConfigFields: []ConfigField{},
	}
}

// hermesEntry is SkillPluginCatalog.hermesEntry(name:description:).
func hermesEntry(name string, description *string) CatalogEntryDTO {
	d := "Installed in your Hermes agent."
	if description != nil {
		d = *description
	}
	return CatalogEntryDTO{
		Slug: hermesPrefix + name, Name: titleize(name), Summary: summarize(d), Description: d,
		Category: "skill", CapabilityKind: "skill", IconSlug: "hermes", Version: "1.0.0", Publisher: "Hermes",
		Verified: true, ConfigFields: []ConfigField{},
	}
}

// parseHermesSkills is SkillPluginCatalog.parseHermesSkills: {"skills"|"data"|"items":[…]}
// or a bare array, each element a name or an object with name/id and description.
func parseHermesSkills(data []byte) []CatalogEntryDTO {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return []CatalogEntryDTO{}
	}
	var list []any
	switch v := root.(type) {
	case []any:
		list = v
	case map[string]any:
		for _, k := range []string{"skills", "data", "items"} {
			if arr, ok := v[k].([]any); ok {
				list = arr
				break
			}
		}
	}
	out := []CatalogEntryDTO{}
	seen := map[string]bool{}
	for _, item := range list {
		var name string
		var description *string
		switch o := item.(type) {
		case string:
			name = o
		case map[string]any:
			if s, ok := o["name"].(string); ok {
				name = s
			} else if s, ok := o["id"].(string); ok {
				name = s
			}
			if s, ok := o["description"].(string); ok {
				description = &s
			}
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, hermesEntry(name, description))
	}
	sortBySlug(out)
	return out
}

// titleize is SkillPluginCatalog.titleize: "pattern-detector" → "Pattern Detector".
func titleize(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		first := swiftstr.Prefix(p, 1)
		parts[i] = strings.ToUpper(first) + p[len(first):]
	}
	return strings.Join(parts, " ")
}

// summarize is SkillPluginCatalog.summarize: the first line, trimmed of spaces,
// capped at 140 Characters.
func summarize(text string) string {
	first := text
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			first = line
			break
		}
	}
	trimmed := swiftstr.TrimWhitespaces(first)
	if swiftstr.Count(trimmed) <= 140 {
		return trimmed
	}
	return swiftstr.Prefix(trimmed, 139) + "…"
}
