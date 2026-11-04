package marketplace

import (
	"encoding/json"
)

// catalogEntry is the part of PluginCatalog.Entry (Sources/App/Plugins/PluginCatalog.swift)
// the marketplace seeds from. Every first-party listing is a published, native
// 1.0.0 version from the "luminavault" publisher.
type catalogEntry struct {
	slug, name, summary, description, category string
	featured                                   bool
	configFields                               []PluginConfigField
}

func placeholder(s string) *string { return &s }

// firstPartyCatalog is sorted by slug, the order Swift seeds it in.
var firstPartyCatalog = []catalogEntry{
	{
		slug: "byok-embeddings", name: "Your own embeddings key",
		summary: "Use your own API key for memory embeddings (same model — no re-embedding).",
		description: "Routes this workspace's embedding requests through your own " +
			"provider API key instead of the shared one. It uses the same " +
			"embedding model the platform already uses, so your existing " +
			"memories stay searchable — no re-embedding. Provide the API key " +
			"for the active embedding provider (OpenAI or Nomic).",
		category: "memory",
		configFields: []PluginConfigField{
			{Key: "access_token", Label: "Embedding API key", Placeholder: placeholder("sk-… (OpenAI) or nomic key"), Kind: "secret", IsRequired: true},
		},
	},
	{
		slug: "news-ticker", name: "Breaking news",
		summary: "A strip of breaking headlines on Home, from publishers' own feeds.",
		description: "Shows headline, source, time and a link for the world's top " +
			"stories on your Home screen, read through the cluster's shared " +
			"feed aggregator — no API key, no vendor. Add your own site or " +
			"feed addresses to follow them too. Links open the publisher; " +
			"nothing is stored beyond the headline.",
		category: "ui", featured: true,
		configFields: []PluginConfigField{
			{Key: "feed_urls", Label: "Your feeds", Placeholder: placeholder("https://example.com, https://blog.example/feed.xml"), Kind: "text", IsRequired: false},
		},
	},
	{
		slug: "raindrop", name: "Raindrop.io",
		summary: "Import your Raindrop.io bookmarks into your vault.",
		description: "Pulls your saved Raindrop.io bookmarks and stages each link into " +
			"your Imported inbox, where Smart Import files them into Spaces and " +
			"compiles them into memories. Provide a Raindrop test token " +
			"(app.raindrop.io → Settings → Integrations).",
		category: "connector",
		configFields: []PluginConfigField{
			{Key: "access_token", Label: "Access token", Placeholder: placeholder("test token from raindrop.io integrations"), Kind: "secret", IsRequired: true},
		},
	},
	{
		slug: "reading-time", name: "Reading Time",
		summary: "Add an estimated reading time to articles you capture.",
		description: "Estimates how long each captured article takes to read (at " +
			"~200 words per minute) and writes it into the note's " +
			"frontmatter as `reading_time`. Runs automatically after a " +
			"link is captured and enriched — no configuration needed.",
		category:     "capture",
		configFields: []PluginConfigField{},
	},
	{
		slug: "readwise", name: "Readwise",
		summary: "Import your Readwise highlights' source articles into your vault.",
		description: "Pulls the source articles behind your Readwise highlights and " +
			"stages them into your Imported inbox, where Smart Import files " +
			"them into Spaces and compiles them into memories. Provide a " +
			"Readwise access token (readwise.io/access_token).",
		category: "connector", featured: true,
		configFields: []PluginConfigField{
			{Key: "access_token", Label: "Access token", Placeholder: placeholder("readwise.io/access_token"), Kind: "secret", IsRequired: true},
		},
	},
	{
		slug: "rss", name: "RSS / Atom feed",
		summary: "Pull new articles from any RSS or Atom feed into your vault.",
		description: "Fetches a feed you paste and stages each item's article into your " +
			"Imported inbox, where Smart Import files them into Spaces and " +
			"compiles them into memories. Works with any public RSS or Atom URL.",
		category: "connector",
		configFields: []PluginConfigField{
			{Key: "feed_url", Label: "Feed URL", Placeholder: placeholder("https://example.com/feed.xml"), Kind: "url", IsRequired: true},
		},
	},
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
