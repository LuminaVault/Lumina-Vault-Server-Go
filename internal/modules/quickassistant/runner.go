package quickassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/devicerpc"
	"lumina/internal/ids"
	"lumina/internal/modules/apple"
	"lumina/internal/modules/mcp"
	"lumina/internal/modules/reminders"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits and model-facing lines, QuickAssistantToolRunner's verbatim.
const (
	notAvailable           = "This is not available yet. Tell the user you can't do it right now."
	toolFailed             = "The tool failed. Do not try it again; tell the user you couldn't get that."
	denied                 = "The user doesn't have access to that in this vault."
	sharedVaultRefusal     = "The tool did not run: this turn used the user's private data, so nothing can be saved to a shared vault. Tell the user."
	sharedVaultUnavailable = "The tool did not run: saving to a shared vault is not available yet. Tell the user."
	tooManyWrites          = "The tool did not run: too many changes in one turn. Tell the user what is done and ask them to send the rest separately."
	// proposalPending is what the model reads after proposing. It must not
	// believe the action happened.
	proposalPending = "A confirmation card is showing for the user. Nothing has been done yet: it only happens if the user confirms. Do not say it is done; tell the user it is waiting for their confirmation."
	tooManyCards    = "The tool did not run: too many confirmations are waiting in this turn. Tell the user what is waiting and ask them to send the rest separately."
	noConversation  = "The tool did not run: there is nowhere to show a confirmation here. Tell the user you can't do it right now."
	permissionCard  = "The user has not allowed Calendar changes, so a card is showing that points them to Data Access. Nothing has been done. Tell the user."

	maxTitleLength         = 200
	maxBodyLength          = 2000
	maxWritesPerTurn       = 5 // ruling 23
	maxConfirmCardsPerTurn = 3 // ruling 27

	// toolTimeout bounds one tool call. The loop hands Run a context the turn
	// does not cancel, so a started write finishes (ruling 73); this is what
	// stops a hung database, embedding provider or phone holding the turn open.
	toolTimeout = 30 * time.Second
)

// Deps are the services the tools run on, shared by every turn.
type Deps struct {
	// Pool is where the personal reads (devicetools) run.
	Pool   *pgxpool.Pool
	Access *vaultaccess.Service
	// MCP runs the vault tools' bodies (Decision 1: one implementation with
	// /v1/mcp). The runner keeps its own authorisation and caps ahead of it.
	MCP       *mcp.Service
	Reminders *reminders.Store
	Consent   *apple.ConsentReader
	// Devices reaches the user's phone. Nil until P5 (Decision 3, ruling 61):
	// calendar_create is not offered, the Apple Reminders mirror is skipped
	// and a device read answers offline at once.
	Devices DeviceRequester
	Store   *Store
}

// DeviceRequester sends one command to the tenant's phone and waits for its
// result. *devicerpc.Broker satisfies it.
type DeviceRequester interface {
	Request(ctx context.Context, tenant uuid.UUID, cmd devicerpc.Command) (devicerpc.Result, error)
}

// TurnState is what one turn has done so far: whether a personal tool ran,
// writes that took effect and confirm cards shown. Every call of the turn's
// runner counts against the same state.
type TurnState struct {
	mu          sync.Mutex
	personalRan bool
	writes      int
	proposals   int
}

// PersonalRan reports whether a personal tool ran earlier in the turn.
func (s *TurnState) PersonalRan() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.personalRan
}

func (s *TurnState) notePersonal() {
	s.mu.Lock()
	s.personalRan = true
	s.mu.Unlock()
}

// Writes are the writes that took effect this turn.
func (s *TurnState) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func (s *TurnState) noteWrite() {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
}

// Proposals are the confirm cards shown this turn. Nothing was written for them.
func (s *TurnState) Proposals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proposals
}

func (s *TurnState) noteProposal() {
	s.mu.Lock()
	s.proposals++
	s.mu.Unlock()
}

// Runner is QuickAssistantToolRunner: it runs the assistant's tools through
// the same gate MCP callers use. Vault tools run on the turn's vault, resolved
// per call with write access for a write; personal tools run on the actor's
// own account whichever vault the turn is in (Decision 2).
//
// Everything here answers the model and never fails the turn. A chip
// (Action) is returned only for a write that took effect, built from the
// validated arguments and never from anything the model wrote as prose.
//
// One Runner serves one turn: it remembers whether a personal tool ran and
// counts the turn's writes and cards.
type Runner struct {
	Deps
	Actor, Vault uuid.UUID
	// Loc is the turn's zone when known. Chips show times in it, and a time
	// the model wrote with no offset is read in it. Nil means unknown.
	Loc *time.Location
	// ConversationID is where a confirm card belongs. Nil means there is
	// nowhere to put one.
	ConversationID *uuid.UUID
	Now            func() time.Time
	Log            *slog.Logger
	turn           *TurnState
	once           sync.Once
	timeout        time.Duration
}

// NewRunner is a runner for one turn.
func NewRunner(d Deps, actor, vault uuid.UUID, loc *time.Location, conv *uuid.UUID, log *slog.Logger) *Runner {
	return &Runner{
		Deps: d, Actor: actor, Vault: vault, Loc: loc,
		ConversationID: conv, Now: time.Now, Log: log, turn: &TurnState{}, timeout: toolTimeout,
	}
}

// refusalError is a tool error the model should read and, usually, correct:
// Swift's Refusal and MCPError.invalidParams, which both read
// "The tool did not run: <msg>".
type refusalError struct{ msg string }

func (e *refusalError) Error() string { return e.msg }

func refuse(msg string) error { return &refusalError{msg: msg} }

// Run implements ToolRunner.
func (r *Runner) Run(ctx context.Context, name string, args map[string]any) ToolResult {
	tool, ok := r.definition(name)
	if ActionFor(name) == Refused || !ok {
		return modelOnly(notAvailable)
	}
	if unknown := unknownArgument(tool, args); unknown != "" {
		return modelOnly("The tool did not run: it has no argument named '" + unknown + "'.")
	}
	turn := r.state()
	personalRanBefore := turn.PersonalRan()
	if TouchesPersonalData(name) {
		turn.notePersonal()
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = toolTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := r.dispatch(ctx, tool, args, personalRanBefore)
	if err == nil {
		return result
	}
	var refusal *refusalError
	if errors.As(err, &refusal) {
		return modelOnly("The tool did not run: " + refusal.msg)
	}
	if msg, ok := mcp.ToolErrorMessage(err); ok {
		return modelOnly("The tool did not run: " + msg)
	}
	var we *wire.Error
	if errors.As(err, &we) && we.Status == http.StatusForbidden {
		return modelOnly(denied)
	}
	// The type only: the arguments and the vault's text stay out of logs.
	r.logger().WarnContext(ctx, "quick assistant tool failed",
		"tool", name, "error_type", fmt.Sprintf("%T", err))
	return modelOnly(toolFailed)
}

// errMissingDependency is a Runner built without a service the tool needs. It
// answers the model like any failure instead of panicking.
var errMissingDependency = errors.New("quick assistant runner is missing a dependency")

// state is the turn's state, made on first use so a Runner built without
// NewRunner still counts its turn.
func (r *Runner) state() *TurnState {
	r.once.Do(func() {
		if r.turn == nil {
			r.turn = &TurnState{}
		}
	})
	return r.turn
}

func (r *Runner) dispatch(ctx context.Context, tool Tool, args map[string]any, personalRanBefore bool) (ToolResult, error) {
	switch tool.Name {
	case "open_screen":
		return r.openScreen(args)
	case "memory_save":
		return r.saveMemory(ctx, args, personalRanBefore)
	case "app_reminder_create", "app_reminders_list":
		return r.personal(ctx, tool.Name, args)
	case "calendar_create":
		return r.proposeEvent(ctx, args)
	case "memory_forget":
		return r.proposeForget(ctx, args)
	}
	if tool.Writes {
		return modelOnly(notAvailable), nil
	}
	if tool.Personal {
		return r.personal(ctx, tool.Name, args)
	}
	vault, err := r.resolveVault(ctx, vaultaccess.Read)
	if err != nil {
		return ToolResult{}, err
	}
	out, err := r.MCP.CallAsSession(ctx, tool.Name, args, vault, r.Actor)
	if err != nil {
		return ToolResult{}, err
	}
	return jsonResult(out)
}

// resolveVault is the turn's vault for the actor with perm, checked on every
// call (Decision 2). It also checks the vault tools' services are wired.
func (r *Runner) resolveVault(ctx context.Context, perm vaultaccess.Permission) (uuid.UUID, error) {
	if r.Access == nil || r.MCP == nil {
		return uuid.Nil, errMissingDependency
	}
	access, err := r.Access.ResolveVault(ctx, r.Actor, r.Vault, perm)
	if err != nil {
		return uuid.Nil, err
	}
	return access.VaultID, nil
}

// definition is the offered tool with that name. calendar_create is offered
// only with a device to write to (Decision 3).
func (r *Runner) definition(name string) (Tool, bool) {
	for _, t := range Definitions(r.definitionOptions()) {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func (r *Runner) definitionOptions() DefinitionOptions {
	return DefinitionOptions{DeviceWrites: r.Devices != nil}
}

// unknownArgument is MCPToolGate.unknownArgument: the first argument, in
// sorted order, that the tool's schema does not name.
func unknownArgument(tool Tool, args map[string]any) string {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema.Properties == nil {
		return ""
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if _, ok := schema.Properties[k]; !ok {
			return k
		}
	}
	return ""
}

// MARK: - memory_save

func (r *Runner) saveMemory(ctx context.Context, args map[string]any, personalRanBefore bool) (ToolResult, error) {
	vault, err := r.resolveVault(ctx, vaultaccess.Write)
	if err != nil {
		return ToolResult{}, err
	}
	// A save in a shared vault is retrievable by its co-members, and this turn
	// has read the caller's private data (ruling 18). The caller's own vault
	// is theirs alone, so it stays allowed.
	if vault != r.Actor && personalRanBefore {
		return modelOnly(sharedVaultRefusal), nil
	}
	// memory_contributions.tenant_id references users, so only the caller's
	// own vault can take a save; a team vault would write the memory and then
	// fail (ruling 21).
	if vault != r.Actor {
		return modelOnly(sharedVaultUnavailable), nil
	}
	if r.state().Writes() >= maxWritesPerTurn {
		// Swift throws Refusal(message: tooManyWrites), so the model reads the
		// prefix twice; kept verbatim.
		return ToolResult{}, refuse(tooManyWrites)
	}
	out, err := r.MCP.CallAsSession(ctx, "memory_save", args, vault, r.Actor)
	if err != nil {
		return ToolResult{}, err
	}
	duplicate, _ := out["duplicate"].(bool)
	content, err := jsonResult(out)
	if err != nil {
		return ToolResult{}, err
	}
	// An identical memory already existed: nothing was written.
	if duplicate {
		return content, nil
	}
	r.state().noteWrite()
	text := swiftstr.TrimWhitespacesAndNewlines(stringArg(args, "content"))
	content.Action = &Action{
		ID: wire.UUID{UUID: uuid.New()}, Kind: KindDone,
		Summary: "Saved to memory: " + Shortened(text, 80),
	}
	return content, nil
}

// MARK: - open_screen

func (r *Runner) openScreen(args map[string]any) (ToolResult, error) {
	raw, _ := args["screen"].(string)
	screen, ok := ParseScreen(raw)
	if !ok {
		return ToolResult{}, refuse("screen must be one of the listed screens.")
	}
	var memoryID *uuid.UUID
	if value, present := args["memoryID"]; present && value != nil {
		text, _ := value.(string)
		id, err := ids.Parse(text)
		if err != nil {
			return ToolResult{}, refuse("memoryID must be a memory UUID from search or memory_save.")
		}
		memoryID = &id
	}
	if screen == ScreenMemory && memoryID == nil {
		return ToolResult{}, refuse("memoryID is required to open a memory.")
	}
	label := screenLabel(screen)
	action := &Action{
		ID: wire.UUID{UUID: uuid.New()}, Kind: KindOpenScreen,
		Summary: "Opening " + label, Screen: &screen,
	}
	if screen == ScreenMemory {
		action.MemoryID = &wire.UUID{UUID: *memoryID}
	}
	return ToolResult{ModelContent: "The app is opening " + label + " for the user.", Action: action}, nil
}

func screenLabel(s Screen) string {
	switch s {
	case ScreenHome:
		return "Home"
	case ScreenSpaces:
		return "Spaces"
	case ScreenAI:
		return "AI"
	case ScreenBrain:
		return "Brain"
	case ScreenInsights:
		return "Insights"
	case ScreenSettings:
		return "Settings"
	case ScreenDataAccess:
		return "Data Access"
	case ScreenBrief:
		return "Brief"
	case ScreenLearned:
		return "Learned"
	case ScreenMemory:
		return "the memory"
	default:
		return "that screen"
	}
}

// MARK: - Helpers

// Shortened is QuickAssistantToolRunner.shortened: one line, no control or
// format characters (ANSI escapes, bidi overrides), cut to limit Characters.
// Everything a chip or card quotes goes through it (ruling 33).
func Shortened(text string, limit int) string {
	var lines []string
	var line strings.Builder
	for _, c := range swiftstr.Characters(text) {
		if isNewlineCharacter(c) {
			if line.Len() > 0 {
				lines = append(lines, line.String())
				line.Reset()
			}
			continue
		}
		line.WriteString(c)
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	flat := strings.Join(lines, " ")
	clean := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, flat)
	if swiftstr.Count(clean) <= limit {
		return clean
	}
	return swiftstr.Prefix(clean, limit-1) + "…"
}

// isNewlineCharacter is Character.isNewline: "\r\n" or one of the newline
// scalars.
func isNewlineCharacter(c string) bool {
	if c == "\r\n" {
		return true
	}
	switch c {
	case "\n", "\r", "\u000B", "\u000C", "\u0085", " ", " ":
		return true
	}
	return false
}

func modelOnly(content string) ToolResult { return ToolResult{ModelContent: content} }

// jsonResult is the model-only result carrying v as JSON.
func jsonResult(v any) (ToolResult, error) {
	s, err := encodeJSON(v)
	if err != nil {
		return ToolResult{}, err
	}
	return modelOnly(s), nil
}

// encodeJSON encodes without HTML escaping, so the model reads "<" and "&".
func encodeJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// stringArg is JSONValue.stringValue: the value when it is a string, else "".
func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// intArg is JSONValue.intValue: a number truncated toward zero, or false when
// the value is missing or not a number.
func intArg(args map[string]any, key string) (int, bool) {
	f, ok := args[key].(float64)
	if !ok || math.IsNaN(f) {
		return 0, false
	}
	// Out of any limit's range either way; clamped so the conversion is defined.
	f = math.Trunc(math.Max(math.Min(f, 1e15), -1e15))
	return int(f), true
}

// intArgPtr is intArg as an optional.
func intArgPtr(args map[string]any, key string) *int {
	if n, ok := intArg(args, key); ok {
		return &n
	}
	return nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}
