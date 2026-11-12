package quickassistant

import "encoding/json"

// Tool is one tool the assistant loop offers the model. Names, titles,
// descriptions and schemas are Swift's, byte for byte: MCPToolCatalog in
// MCPProtocol.swift and QuickAssistantToolPolicy.swift at e79f601e.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Writes mirrors MCPTool.writes: false is a promise the tool changes nothing.
	Writes bool `json:"writes"`
	// Personal mirrors MCPTool.personal: Apple data, never a vault.
	Personal bool `json:"personal"`
}

// DefinitionOptions says which optional tools to offer.
type DefinitionOptions struct {
	// DeviceWrites offers calendar_create, which needs a device that can answer.
	DeviceWrites bool
}

// Definitions returns the tools to offer: the catalog tools the policy does not
// refuse, in catalog order, then the assistant-only tools. calendar_create is
// the loop's own version and appears only when o.DeviceWrites is set.
func Definitions(o DefinitionOptions) []Tool {
	out := make([]Tool, 0, len(catalogTools)+3)
	for _, t := range catalogTools {
		if t.Name == "calendar_create" && !o.DeviceWrites {
			continue
		}
		out = append(out, t)
	}
	return append(out, appReminderCreate, appRemindersList, openScreen)
}

// OpenAITools renders defs as an OpenAI-compatible tools array.
func OpenAITools(defs []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        d.Name,
				"description": d.Description,
				"parameters":  d.InputSchema,
			},
		})
	}
	return out
}

// object is MCPToolCatalog.object: strict, so an unknown argument is an error.
func object(properties map[string]any, required ...string) json.RawMessage {
	if required == nil {
		required = []string{}
	}
	raw, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		panic(err)
	}
	return raw
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

// catalogTools are the MCPToolCatalog entries the policy does not refuse, in
// catalog order, with the loop's calendar_create in the catalog's place.
var catalogTools = []Tool{
	{
		Name:  "search",
		Title: "Search the vault",
		Description: "Hybrid search over the vault: semantic similarity fused with exact keyword matching, " +
			"so both paraphrase and literal tokens (hostnames, error codes, IDs) are found. Every hit carries " +
			"the source path, heading trail and line range it came from — cite those.",
		InputSchema: object(map[string]any{
			"query": schema("string", "What to look for. Plain language or exact terms."),
			"limit": intSchema("Maximum hits (1-50).", 1, 50, 10),
			"space": schema("string", "Optional Space slug to search within. Omit to search everything."),
		}, "query"),
	},
	{
		Name:        "browse",
		Title:       "List documents",
		Description: "List indexed documents. Use when you need the shape of the vault rather than a text query.",
		InputSchema: object(map[string]any{
			"pathPrefix": schema("string", "Only documents whose path starts with this."),
			"limit":      intSchema("Maximum documents (1-500).", 1, 500, 50),
		}),
	},
	{
		Name:  "read",
		Title: "Read a document",
		Description: "Read one document's text by vault-relative path. Bounded: the reply reports whether " +
			"it was truncated.",
		InputSchema: object(map[string]any{
			"path":     schema("string", "Vault-relative path, as returned by search or browse."),
			"maxChars": intSchema("Characters to return (1-100000).", 1, 100000, 20000),
		}, "path"),
	},
	{
		Name:        "recent",
		Title:       "Recently changed documents",
		Description: "Documents modified most recently, newest first.",
		InputSchema: object(map[string]any{
			"limit": intSchema("Maximum documents (1-100).", 1, 100, 10),
		}),
	},
	{
		Name:  "memory_context",
		Title: "Start with context",
		Description: "Call once at the start of a session. Returns the user's SOUL.md (how they want you to work; " +
			"read-only — never try to edit it) and their most recent decisions and preferences, optionally for one " +
			"Space. Use `search` for anything more specific.",
		InputSchema: object(map[string]any{
			"space": schema("string", "Optional Space slug to narrow the decisions and preferences to."),
		}),
	},
	{
		Name:  "memory_save",
		Title: "Save a memory",
		Description: "Save one durable statement worth remembering in later sessions: a preference, a decision, " +
			"or a fact about the user or their work. One statement per call, in plain words, e.g. " +
			"\"Prefers Python for scripts.\" Search first; if a memory already says this, update it instead. " +
			"An exact repeat returns the existing memory. Never save secrets, credentials, or anything the user " +
			"asked you not to keep.",
		InputSchema: object(map[string]any{
			"content": schema("string", "The statement, at most 4000 characters."),
			"space":   schema("string", "Optional Space slug to file it under; omit or `inbox` for unfiled."),
			"kind": map[string]any{
				"type": "string",
				"enum": []string{"preference", "decision", "fact", "note"},
				"description": "preference (how the user likes things), decision (something settled), " +
					"fact (about the user or their work), or note.",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    10,
				"description": "Optional short labels.",
			},
		}, "content"),
		Writes: true,
	},
	{
		Name:  "memory_forget",
		Title: "Forget a memory",
		Description: "Delete a memory that is no longer true or that the user asked you to forget. Confirm with " +
			"the user first. This removes it from search; it does not edit any vault document it came from.",
		InputSchema: object(map[string]any{
			"memoryID": schema("string", "The memory to delete."),
		}, "memoryID"),
		Writes: true,
	},
	{
		Name:  "health_query",
		Title: "Health trends",
		Description: "Daily totals and averages of the user's synced Apple Health data (steps, heart rate, HRV, " +
			"sleep, active energy, …). Pass `metric` for one HealthKit type, or omit it for all. Fails when the " +
			"user has not allowed Health access.",
		InputSchema: object(map[string]any{
			"metric": schema("string", "HealthKit type identifier, e.g. HKQuantityTypeIdentifierStepCount."),
			"days":   intSchema("Days back to cover (1-365).", 1, 365, 30),
		}),
		Personal: true,
	},
	{
		Name:  "calendar_query",
		Title: "Upcoming events",
		Description: "Upcoming events from the user's Apple and Google calendars, soonest first. Fails when the " +
			"user has not allowed Calendar access.",
		InputSchema: object(map[string]any{
			"days": intSchema("Days ahead to cover (1-90).", 1, 90, 7),
		}),
		Personal: true,
	},
	{
		Name:  "reminders_list",
		Title: "Open reminders",
		Description: "The user's open Apple Reminders, overdue and upcoming, soonest due first (at most 100). " +
			"Fails when the user has not allowed Reminders access.",
		InputSchema: object(map[string]any{}),
		Personal:    true,
	},
	// The loop's calendar_create (QuickAssistantToolPolicy.calendarCreate). The
	// catalog's asks for an offset; a model working the offset out itself gets it
	// wrong across DST, so the loop asks for local times (commit e79f601e).
	{
		Name:  "calendar_create",
		Title: "Create an event",
		Description: "Propose an event in the user's Apple Calendar, on their iPhone. The user confirms it on a " +
			"card before anything is added. Needs the app reachable and the user's permission to make changes.",
		InputSchema: object(map[string]any{
			"title": schema("string", "Event title."),
			"start": schema("string", "Start, ISO 8601. With no offset it is read in the user's time zone."),
			"end": schema("string",
				"End, ISO 8601. With no offset it is read in the user's time zone. Defaults to one hour after start."),
			"location": schema("string", "Optional location."),
		}, "title", "start"),
		Writes:   true,
		Personal: true,
	},
}

// LuminaVault's own reminders, not Apple Reminders, so not Personal.
var appReminderCreate = Tool{
	Name:        "app_reminder_create",
	Title:       "Create a LuminaVault reminder",
	Description: "Create a reminder in LuminaVault that notifies the user at a given time.",
	InputSchema: object(map[string]any{
		"title":  schema("string", "Short reminder title."),
		"body":   schema("string", "Optional longer text."),
		"fireAt": schema("string", "When to fire, ISO 8601. With no offset it is read in the user's time zone."),
	}, "title", "fireAt"),
	Writes: true,
}

var appRemindersList = Tool{
	Name:        "app_reminders_list",
	Title:       "List LuminaVault reminders",
	Description: "The user's upcoming LuminaVault reminders, soonest first.",
	InputSchema: object(map[string]any{}),
}

var openScreen = Tool{
	Name:        "open_screen",
	Title:       "Open a screen in the app",
	Description: "Take the user to a screen in the app. The app performs the navigation.",
	InputSchema: object(map[string]any{
		"screen": map[string]any{
			"type":        "string",
			"description": "Which screen to open.",
			// "unknown" is the decoder's fallback, not a destination.
			"enum": screens,
		},
		"memoryID": schema("string", "Optional memory UUID to open on that screen."),
	}, "screen"),
}
