package quickassistant

// ToolAction is what the assistant may do with a tool the model asks for
// (Swift's QuickAssistantToolAction). It is not named Action because that is
// the wire type in dto.go.
type ToolAction int

const (
	// Run executes immediately on the server.
	Run ToolAction = iota
	// Confirm parks until the user confirms.
	Confirm
	// Client is performed by the app (navigation); the server only relays.
	Client
	// Refused is never offered and never executed.
	Refused
)

// ActionFor is QuickAssistantToolPolicy.action(for:). Anything not named here
// is refused, so a tool added to the MCP catalog stays unreachable until
// someone classifies it on purpose.
func ActionFor(name string) ToolAction {
	switch name {
	case "search", "browse", "read", "recent", "memory_context",
		"health_query", "calendar_query", "app_reminders_list",
		"reminders_list", "memory_save", "app_reminder_create":
		return Run
	case "calendar_create", "memory_forget":
		return Confirm
	case "open_screen":
		return Client
	default:
		return Refused
	}
}

// personalDataTools is the catalog's personal tools (health_query,
// calendar_query, reminders_list, calendar_create, reminder_create) plus
// LuminaVault's own reminders (ruling 20).
var personalDataTools = map[string]bool{
	"health_query": true, "calendar_query": true, "reminders_list": true,
	"calendar_create": true, "reminder_create": true,
	"app_reminders_list": true, "app_reminder_create": true,
}

// TouchesPersonalData reports whether the tool reads or writes the caller's own
// account data. A turn where one ran can quote that data, so its reply must not
// become a shared memory.
func TouchesPersonalData(name string) bool { return personalDataTools[name] }

// IsWrite reports whether a repeat of the tool with the same arguments must not
// run twice.
func IsWrite(name string) bool {
	switch name {
	case "memory_save", "memory_forget", "calendar_create", "app_reminder_create":
		return true
	default:
		return false
	}
}
