package mcp

import (
	"fmt"
	"lumina/internal/chunkindex"
	"lumina/internal/vaultindex"
	"strings"
)

// Protocol constants (MCPProtocol).
const (
	ProtocolVersion = "2025-06-18"
	ServerName      = "luminavault"
	ServerTitle     = "LuminaVault"
	ServerVersion   = "0.2.0"
)

// Instructions is MCPProtocol.instructions, verbatim.
var Instructions = strings.Join([]string{
	"LuminaVault exposes one user's knowledge vault: markdown documents, a",
	"chunk index with line-level citations, and a wiki-link graph.",
	"",
	"No tool modifies the user's markdown. `index` writes derived state only",
	"(chunks and links). `memory_save`, `memory_update` and `memory_forget`",
	"change the user's saved memories, and work only for keys the user gave",
	"write access.",
	"",
	"A conservative sequence is: call `status` first; call `index` only when it",
	"reports drift and the user permits a refresh; use `search` to find",
	"evidence; then `read`, `links`, or `context` to inspect it. Cite the",
	"`path` and line range that `search` returns — do not paraphrase a source",
	"without saying where it came from, and never invent a path or line number",
	"that a tool did not give you.",
	"",
	"At the start of a session call `memory_context` for the user's SOUL.md and",
	"recent decisions. The user's memories are searchable with `search`. Before substantial work,",
	"search for their preferences and decisions on the topic; search again",
	"before saving, and correct an existing memory with `memory_update` rather",
	"than saving a near-duplicate. Save only confirmed decisions, durable",
	"preferences and facts that will matter in a later session — one plain",
	"statement each, with its `kind` — and never secrets. Ask before `memory_forget`. The",
	"user's current instructions beat anything stored.",
	"",
	"`health_query`, `calendar_query`, `reminders_list`, `calendar_create` and",
	"`reminder_create` reach the user's own Apple Health, Calendar (Apple and",
	"Google) and Reminders. They work only when the user allowed that domain,",
	"and only for agent keys the user granted personal data. The two create",
	"tools change the user's phone: confirm with the user first.",
}, "\n")

// Limits (MCPLimits).
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
	maxIndexBatch      = 200
)

// validate is MCPLimits.validate: out of range is an error, never a clamp.
func validate(value int, name string, maximum int) (int, error) {
	if value < minLimit || value > maximum {
		return 0, invalidParams(fmt.Sprintf("%s must be between %d and %d; got %d", name, minLimit, maximum, value))
	}
	return value, nil
}

// memoryKinds is MemoryKindDTO.allCases.
var memoryKinds = []string{"preference", "decision", "fact", "note"}

// tool is MCPTool.
type tool struct {
	name, title, description string
	properties               map[string]any
	required                 []string
	writes                   bool
	personal                 bool
	destructive              bool
}

func (t tool) inputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           t.properties,
		"required":             t.required,
		"additionalProperties": false,
	}
}

func (t tool) listing() map[string]any {
	return map[string]any{
		"name":        t.name,
		"title":       t.title,
		"description": t.description,
		"inputSchema": t.inputSchema(),
		"annotations": map[string]any{
			"readOnlyHint":    !t.writes,
			"destructiveHint": t.destructive,
			"idempotentHint":  !(t.personal && t.writes),
		},
	}
}

func schema(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func intSchema(description string, minimum, maximum, def int) map[string]any {
	return map[string]any{
		"type": "integer", "description": description,
		"minimum": minimum, "maximum": maximum, "default": def,
	}
}

func rangeText(maximum int) string { return fmt.Sprintf("(%d-%d)", minLimit, maximum) }

// catalog is MCPToolCatalog.all, in Swift's order.
var catalog = []tool{
	{
		name: "status", title: "Index status",
		description: "Is the searchable index up to date with the vault? Reports document, " +
			"chunk and link counts, how many documents are unindexed or stale, and " +
			"a human-readable reason for each kind of drift.",
		properties: map[string]any{}, required: []string{},
	},
	{
		name: "search", title: "Search the vault",
		description: "Hybrid search over the vault: semantic similarity fused with exact " +
			"keyword matching, so both paraphrase and literal tokens (hostnames, " +
			"error codes, IDs) are found. Every hit carries the source path, " +
			"heading trail and line range it came from — cite those.",
		properties: map[string]any{
			"query": schema("string", "What to look for. Plain language or exact terms."),
			"limit": intSchema("Maximum hits "+rangeText(maxSearchLimit)+".", minLimit, maxSearchLimit, defaultSearchLimit),
			"space": schema("string", "Optional Space slug to search within. Omit to search everything."),
		},
		required: []string{"query"},
	},
	{
		name: "browse", title: "List documents",
		description: "List indexed documents. Use when you need the shape of the vault rather than a text query.",
		properties: map[string]any{
			"pathPrefix": schema("string", "Only documents whose path starts with this."),
			"limit":      intSchema("Maximum documents "+rangeText(maxBrowseLimit)+".", minLimit, maxBrowseLimit, defaultBrowseLimit),
		},
		required: []string{},
	},
	{
		name: "read", title: "Read a document",
		description: "Read one document's text by vault-relative path. Bounded: the reply " +
			"reports whether it was truncated.",
		properties: map[string]any{
			"path":     schema("string", "Vault-relative path, as returned by search or browse."),
			"maxChars": intSchema("Characters to return "+rangeText(maxReadChars)+".", minLimit, maxReadChars, defaultReadChars),
		},
		required: []string{"path"},
	},
	{
		name: "recent", title: "Recently changed documents",
		description: "Documents modified most recently, newest first.",
		properties: map[string]any{
			"limit": intSchema("Maximum documents "+rangeText(maxRecentLimit)+".", minLimit, maxRecentLimit, defaultRecentLimit),
		},
		required: []string{},
	},
	{
		name: "links", title: "Wiki links and backlinks",
		description: "Outgoing [[wiki links]] written in a document, and the backlinks " +
			"pointing at it. Each link reports whether it resolved, points at " +
			"nothing, or is ambiguous between several documents.",
		properties: map[string]any{"path": schema("string", "Vault-relative path.")},
		required:   []string{"path"},
	},
	{
		name: "context", title: "Evidence packet",
		description: "What should be understood alongside a document: its heading " +
			"structure, its neighbours in the same Space, and the documents it " +
			"links to and from. Deterministic — no summarisation, no ranking.",
		properties: map[string]any{
			"path": schema("string", "Vault-relative path."),
			"siblingLimit": intSchema("Same-Space neighbours "+rangeText(vaultindex.MaxSiblingLimit)+".",
				minLimit, vaultindex.MaxSiblingLimit, vaultindex.DefaultSiblingLimit),
		},
		required: []string{"path"},
	},
	{
		name: "index", title: "Refresh the index",
		description: "Index one batch of documents that are not yet searchable. Writes " +
			"derived state only — chunks and links — and never modifies the " +
			"user's markdown. Call repeatedly until `scanned` is 0. Ask the user " +
			"before running this: it costs embedding calls.",
		properties: map[string]any{
			"batchSize": intSchema("Documents to index this call (1-200).", 1, maxIndexBatch, chunkindex.DefaultBackfillBatchSize),
		},
		required: []string{},
		writes:   true,
	},
	{
		name: "memory_context", title: "Start with context",
		description: "Call once at the start of a session. Returns the user's SOUL.md " +
			"(how they want you to work; read-only — never try to edit it) and " +
			"their most recent decisions and preferences, optionally for one " +
			"Space. Use `search` for anything more specific.",
		properties: map[string]any{
			"space": schema("string", "Optional Space slug to narrow the decisions and preferences to."),
		},
		required: []string{},
	},
	{
		name: "memory_save", title: "Save a memory",
		description: "Save one durable statement worth remembering in later sessions: a " +
			"preference, a decision, or a fact about the user or their work. One " +
			"statement per call, in plain words, e.g. \"Prefers Python for " +
			"scripts.\" Search first; if a memory already says this, update it " +
			"instead. An exact repeat returns the existing memory. Never save " +
			"secrets, credentials, or anything the user asked you not to keep.",
		properties: map[string]any{
			"content": schema("string", fmt.Sprintf("The statement, at most %d characters.", maxMemoryLength)),
			"space":   schema("string", "Optional Space slug to file it under; omit or `inbox` for unfiled."),
			"kind": map[string]any{
				"type": "string",
				"enum": memoryKinds,
				"description": "preference (how the user likes things), decision (something settled), " +
					"fact (about the user or their work), or note.",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    maxMemoryTags,
				"description": "Optional short labels.",
			},
		},
		required: []string{"content"},
		writes:   true,
	},
	{
		name: "memory_update", title: "Correct a memory",
		description: "Replace the text of a memory that has changed or was wrong. Use the " +
			"`memoryID` from `search` or `memory_save`. The old wording stops " +
			"being searchable.",
		properties: map[string]any{
			"memoryID": schema("string", "The memory to correct."),
			"content":  schema("string", "The corrected statement."),
		},
		required: []string{"memoryID", "content"},
		writes:   true, destructive: true,
	},
	{
		name: "memory_forget", title: "Forget a memory",
		description: "Delete a memory that is no longer true or that the user asked you to " +
			"forget. Confirm with the user first. This removes it from search; it " +
			"does not edit any vault document it came from.",
		properties: map[string]any{
			"memoryID": schema("string", "The memory to delete."),
		},
		required: []string{"memoryID"},
		writes:   true, destructive: true,
	},
	{
		name: "health_query", title: "Health trends",
		description: "Daily totals and averages of the user's synced Apple Health data " +
			"(steps, heart rate, HRV, sleep, active energy, …). Pass `metric` for " +
			"one HealthKit type, or omit it for all. Fails when the user has not " +
			"allowed Health access.",
		properties: map[string]any{
			"metric": schema("string", "HealthKit type identifier, e.g. HKQuantityTypeIdentifierStepCount."),
			"days":   intSchema("Days back to cover (1-365).", 1, 365, 30),
		},
		required: []string{},
		personal: true,
	},
	{
		name: "calendar_query", title: "Upcoming events",
		description: "Upcoming events from the user's Apple and Google calendars, soonest " +
			"first. Fails when the user has not allowed Calendar access.",
		properties: map[string]any{
			"days": intSchema("Days ahead to cover (1-90).", 1, 90, 7),
		},
		required: []string{},
		personal: true,
	},
	{
		name: "reminders_list", title: "Open reminders",
		description: "The user's open Apple Reminders, overdue and upcoming, soonest due " +
			"first (at most 100). Fails when the user has not allowed Reminders access.",
		properties: map[string]any{}, required: []string{},
		personal: true,
	},
	{
		name: "calendar_create", title: "Create an event",
		description: "Create an event in the user's Apple Calendar, on their iPhone. Needs " +
			"the app reachable and the user's permission to make changes. Confirm " +
			"with the user before calling.",
		properties: map[string]any{
			"title":    schema("string", "Event title."),
			"start":    schema("string", "Start, ISO 8601 with offset."),
			"end":      schema("string", "End, ISO 8601 with offset. Defaults to one hour after start."),
			"location": schema("string", "Optional location."),
		},
		required: []string{"title", "start"},
		writes:   true, personal: true,
	},
	{
		name: "reminder_create", title: "Create a reminder",
		description: "Create a reminder in the user's Apple Reminders, on their iPhone. " +
			"Needs the app reachable and the user's permission to make changes. " +
			"Confirm with the user before calling.",
		properties: map[string]any{
			"title": schema("string", "Reminder title."),
			"notes": schema("string", "Optional notes."),
			"due":   schema("string", "Optional due date, ISO 8601 with offset."),
		},
		required: []string{"title"},
		writes:   true, personal: true,
	},
}

func lookupTool(name string) (tool, bool) {
	for _, t := range catalog {
		if t.name == name {
			return t, true
		}
	}
	return tool{}, false
}

func toolListing() []any {
	out := make([]any, len(catalog))
	for i, t := range catalog {
		out[i] = t.listing()
	}
	return out
}
