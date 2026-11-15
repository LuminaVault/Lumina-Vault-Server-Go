package quickassistant

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/modules/spaces"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// The app-action tools let the assistant manage what the user otherwise
// manages by tapping: spaces, the files in them, and the text of a memory.
// They are not MCP tools. The MCP catalog promises external agents that no
// tool modifies the user's markdown, and its write grants were given for
// memories only, so these run on the spaces and vault services directly and
// are offered only to the in-app loop.
//
// The split follows the rest of the policy: a create, an edit or a move runs
// at once and shows a chip; a delete, and an overwrite that cannot be undone,
// shows a card and waits for the user.
//
// Changes are made as the user who asked. Nothing marks them as the
// assistant's in the activity feed; the trail is the write log line here, the
// assistant_pending_actions row of a confirmed card, and the saved reply.

// SpaceService is the part of the spaces module the tools use.
// *spaces.Module satisfies it.
type SpaceService interface {
	List(ctx context.Context, vault uuid.UUID) ([]spaces.SpaceDTO, error)
	Create(ctx context.Context, vault, actor uuid.UUID, in spaces.CreateInput) (spaces.SpaceDTO, error)
	Update(ctx context.Context, vault, actor, id uuid.UUID, in spaces.UpdateInput) (spaces.SpaceDTO, error)
	Delete(ctx context.Context, vault, actor, id uuid.UUID) (string, error)
}

// FileService is the part of the vault module the tools use. *vault.Files
// satisfies it.
type FileService interface {
	Find(ctx context.Context, vault uuid.UUID, path string) (*vaultfiles.VaultFile, error)
	Move(ctx context.Context, vault uuid.UUID, path, newPath string) (*vaultfiles.VaultFile, error)
	Delete(ctx context.Context, vault uuid.UUID, path string) error
}

const (
	maxSpaceNameLength        = 80
	maxSpaceDescriptionLength = 500
	maxSpaceCategoryLength    = 40

	inboxFolder = "inbox"
)

// spaceIcons are the icons a space can be given, the same twelve the app's
// editor offers (SpaceIcon.choices). An icon outside the list would draw as
// the fallback glyph.
var spaceIcons = []string{
	"folder.fill", "sparkles", "chart.line.uptrend.xyaxis", "heart.fill",
	"briefcase.fill", "lightbulb.fill", "brain.head.profile", "camera.fill",
	"house.fill", "wand.and.stars", "music.note", "globe",
}

var appActionTools = []Tool{
	{
		Name:        "space_list",
		Title:       "List spaces",
		Description: "The user's spaces: name, slug, category and how many notes each holds. Other tools take a space by its slug, so call this first when you don't know it.",
		InputSchema: object(map[string]any{}),
	},
	{
		Name:        "space_create",
		Title:       "Create a space",
		Description: "Create a new space, a named folder for notes. Its slug is made from the name.",
		InputSchema: object(map[string]any{
			"name":        schema("string", "The space's name, as the user will see it."),
			"description": schema("string", "Optional one-line description."),
			"icon":        iconSchema("Optional icon. Pick the closest; omit for a folder."),
			"category":    schema("string", "Optional short lowercase label that groups spaces, e.g. \"work\"."),
		}, "name"),
		Writes: true,
	},
	{
		Name:        "space_update",
		Title:       "Edit a space",
		Description: "Rename a space or change its description, icon or category. Give only what changes. The slug never changes.",
		InputSchema: object(map[string]any{
			"space":       schema("string", "Slug of the space to edit, from space_list."),
			"name":        schema("string", "New name."),
			"description": schema("string", "New description. An empty string removes it."),
			"icon":        iconSchema("New icon."),
			"category":    schema("string", "New category. An empty string removes it."),
		}, "space"),
		Writes: true,
	},
	{
		Name:        "space_delete",
		Title:       "Delete a space",
		Description: "Delete a space. The user is shown a card and it happens only if they confirm.",
		InputSchema: object(map[string]any{
			"space": schema("string", "Slug of the space to delete, from space_list."),
		}, "space"),
		Writes: true,
	},
	{
		Name:        "file_move",
		Title:       "Move a note",
		Description: "Move a note into a space, or back to the inbox. The file keeps its name.",
		InputSchema: object(map[string]any{
			"path":  schema("string", "The note's path, as browse or search returned it."),
			"space": schema("string", "Slug of the space to move it into, or \"inbox\"."),
		}, "path", "space"),
		Writes: true,
	},
	{
		Name:        "file_delete",
		Title:       "Delete a note",
		Description: "Delete a note. The user is shown a card and it happens only if they confirm.",
		InputSchema: object(map[string]any{
			"path": schema("string", "The note's path, as browse or search returned it."),
		}, "path"),
		Writes: true,
	},
	{
		Name:        "memory_update",
		Title:       "Correct a memory",
		Description: "Replace the text of a memory that has changed or was wrong. Use the memoryID from search or memory_save. The user is shown a card and it happens only if they confirm.",
		InputSchema: object(map[string]any{
			"memoryID": schema("string", "The memory to correct."),
			"content":  schema("string", "The corrected statement."),
		}, "memoryID", "content"),
		Writes: true,
	},
}

// openScreenWithSpace is open_screen as offered with the app-action tools: it
// can also open one space.
var openScreenWithSpace = Tool{
	Name:        openScreen.Name,
	Title:       openScreen.Title,
	Description: openScreen.Description,
	InputSchema: object(map[string]any{
		"screen": map[string]any{
			"type":        "string",
			"description": "Which screen to open.",
			"enum":        appActionScreens,
		},
		"memoryID": schema("string", "Optional memory UUID to open on that screen."),
		"space":    schema("string", "Slug of the space to open. Required when screen is \"space\"."),
	}, "screen"),
}

func iconSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": spaceIcons}
}

// MARK: - Spaces

func (r *Runner) listSpaces(ctx context.Context) (ToolResult, error) {
	vault, err := r.appVault(ctx, vaultaccess.Read)
	if err != nil {
		return ToolResult{}, err
	}
	all, err := r.Spaces.List(ctx, vault)
	if err != nil {
		return ToolResult{}, err
	}
	type row struct {
		Name      string  `json:"name"`
		Slug      string  `json:"slug"`
		Category  *string `json:"category,omitempty"`
		NoteCount int     `json:"noteCount"`
	}
	out := make([]row, 0, len(all))
	for _, s := range all {
		out = append(out, row{Name: s.Name, Slug: s.Slug, Category: s.Category, NoteCount: s.NoteCount})
	}
	return jsonResult(map[string]any{"spaces": out})
}

func (r *Runner) createSpace(ctx context.Context, args map[string]any) (ToolResult, error) {
	name, err := spaceName(args["name"])
	if err != nil {
		return ToolResult{}, err
	}
	in := spaces.CreateInput{Name: name}
	if in.Description, err = optionalText(args, "description", maxSpaceDescriptionLength); err != nil {
		return ToolResult{}, err
	}
	if in.Icon, err = optionalIcon(args); err != nil {
		return ToolResult{}, err
	}
	if in.Category, err = optionalCategory(args); err != nil {
		return ToolResult{}, err
	}
	vault, err := r.appVault(ctx, vaultaccess.Write)
	if err != nil {
		return ToolResult{}, err
	}
	if err := r.beginWrite(); err != nil {
		return ToolResult{}, err
	}
	made, err := r.Spaces.Create(ctx, vault, r.Actor, in)
	if err != nil {
		return ToolResult{}, readable(err)
	}
	r.wrote(ctx, "space_create", vault)
	result, err := jsonResult(map[string]any{"created": true, "name": made.Name, "slug": made.Slug})
	if err != nil {
		return ToolResult{}, err
	}
	result.Action = &Action{
		ID: wire.UUID{UUID: uuid.New()}, Kind: KindDone,
		Summary:  "Created the space " + quoted(made.Name, 60),
		Resource: resource(ResourceSpace, made.Id.UUID, ChangeCreated),
	}
	return result, nil
}

func (r *Runner) updateSpace(ctx context.Context, args map[string]any) (ToolResult, error) {
	var in spaces.UpdateInput
	var err error
	if value, present := args["name"]; present && value != nil {
		name, err := spaceName(value)
		if err != nil {
			return ToolResult{}, err
		}
		in.Name = &name
	}
	if in.Description, err = optionalText(args, "description", maxSpaceDescriptionLength); err != nil {
		return ToolResult{}, err
	}
	if in.Icon, err = optionalIcon(args); err != nil {
		return ToolResult{}, err
	}
	if in.Category, err = optionalCategory(args); err != nil {
		return ToolResult{}, err
	}
	if in.Name == nil && in.Description == nil && in.Icon == nil && in.Category == nil {
		return ToolResult{}, refuse("give at least one of name, description, icon or category to change.")
	}
	vault, err := r.appVault(ctx, vaultaccess.Write)
	if err != nil {
		return ToolResult{}, err
	}
	space, err := r.spaceBySlug(ctx, vault, args["space"])
	if err != nil {
		return ToolResult{}, err
	}
	if err := r.beginWrite(); err != nil {
		return ToolResult{}, err
	}
	updated, err := r.Spaces.Update(ctx, vault, r.Actor, space.Id.UUID, in)
	if err != nil {
		return ToolResult{}, readable(err)
	}
	r.wrote(ctx, "space_update", vault)
	summary := "Updated the space " + quoted(updated.Name, 60)
	if updated.Name != space.Name {
		summary = "Renamed the space " + quoted(space.Name, 40) + " to " + quoted(updated.Name, 40)
	}
	result, err := jsonResult(map[string]any{"updated": true, "name": updated.Name, "slug": updated.Slug})
	if err != nil {
		return ToolResult{}, err
	}
	result.Action = &Action{
		ID: wire.UUID{UUID: uuid.New()}, Kind: KindDone, Summary: summary,
		Resource: resource(ResourceSpace, updated.Id.UUID, ChangeUpdated),
	}
	return result, nil
}

// proposeSpaceDelete shows the card for a delete. The card stores the space's
// id, read from the vault-scoped list, so confirming deletes the space the
// card named even if another space takes its slug meanwhile.
func (r *Runner) proposeSpaceDelete(ctx context.Context, args map[string]any) (ToolResult, error) {
	vault, err := r.appVault(ctx, vaultaccess.Write)
	if err != nil {
		return ToolResult{}, err
	}
	space, err := r.spaceBySlug(ctx, vault, args["space"])
	if err != nil {
		return ToolResult{}, err
	}
	summary := "Delete the space " + quoted(space.Name, 60)
	switch space.NoteCount {
	case 0:
	case 1:
		summary += " and its 1 note"
	default:
		summary += fmt.Sprintf(" and its %d notes", space.NoteCount)
	}
	return r.proposeChange(ctx, "space_delete", vault,
		map[string]any{"spaceID": ids.Upper(space.Id.UUID)}, summary,
		resource(ResourceSpace, space.Id.UUID, ChangeDeleted))
}

// openSpace is open_screen for one space.
func (r *Runner) openSpace(ctx context.Context, args map[string]any) (ToolResult, error) {
	// Offered only with the app-action tools; without them "space" is not a
	// screen this turn was given.
	if r.Spaces == nil || r.Files == nil {
		return ToolResult{}, refuse("screen must be one of the listed screens.")
	}
	vault, err := r.appVault(ctx, vaultaccess.Read)
	if err != nil {
		return ToolResult{}, err
	}
	space, err := r.spaceBySlug(ctx, vault, args["space"])
	if err != nil {
		return ToolResult{}, err
	}
	screen := ScreenSpace
	return ToolResult{
		ModelContent: "The app is opening the space " + quoted(space.Name, 60) + " for the user.",
		Action: &Action{
			ID: wire.UUID{UUID: uuid.New()}, Kind: KindOpenScreen,
			Summary: "Opening " + Shortened(space.Name, 60), Screen: &screen,
			SpaceID: &wire.UUID{UUID: space.Id.UUID},
		},
	}, nil
}

// MARK: - Files

func (r *Runner) moveFile(ctx context.Context, args map[string]any) (ToolResult, error) {
	path, err := filePath(args["path"])
	if err != nil {
		return ToolResult{}, err
	}
	vault, err := r.appVault(ctx, vaultaccess.Write)
	if err != nil {
		return ToolResult{}, err
	}
	var slug *string
	where := "the inbox"
	if raw, _ := args["space"].(string); normalizedSlug(raw) != inboxFolder {
		space, err := r.spaceBySlug(ctx, vault, args["space"])
		if err != nil {
			return ToolResult{}, err
		}
		slug, where = &space.Slug, quoted(space.Name, 40)
	}
	destination, err := vaultfiles.MoveDestination(path, slug)
	if err != nil {
		return ToolResult{}, readable(err)
	}
	if err := r.beginWrite(); err != nil {
		return ToolResult{}, err
	}
	row, err := r.Files.Move(ctx, vault, path, destination)
	if err != nil {
		return ToolResult{}, readable(err)
	}
	r.wrote(ctx, "file_move", vault)
	result, err := jsonResult(map[string]any{"moved": true, "path": row.Path})
	if err != nil {
		return ToolResult{}, err
	}
	result.Action = &Action{
		ID: wire.UUID{UUID: uuid.New()}, Kind: KindDone,
		Summary:  "Moved " + quoted(vaultfiles.LastPathComponent(row.Path), 50) + " to " + where,
		Resource: resource(ResourceVaultFile, row.ID, ChangeMoved),
	}
	return result, nil
}

// proposeFileDelete shows the card for a delete. The card stores the path of
// the row it found in the turn's vault.
func (r *Runner) proposeFileDelete(ctx context.Context, args map[string]any) (ToolResult, error) {
	path, err := filePath(args["path"])
	if err != nil {
		return ToolResult{}, err
	}
	vault, err := r.appVault(ctx, vaultaccess.Write)
	if err != nil {
		return ToolResult{}, err
	}
	row, err := r.Files.Find(ctx, vault, path)
	if err != nil {
		return ToolResult{}, readable(err)
	}
	return r.proposeChange(ctx, "file_delete", vault,
		map[string]any{"path": row.Path},
		"Delete the note "+quoted(vaultfiles.LastPathComponent(row.Path), 60),
		resource(ResourceVaultFile, row.ID, ChangeDeleted))
}

// MARK: - Memory

// proposeMemoryUpdate shows the card for replacing a memory's text. The old
// wording stops being searchable and nothing brings it back, so this waits
// for the user as a forget does. The excerpt comes from the vault-scoped row.
func (r *Runner) proposeMemoryUpdate(ctx context.Context, args map[string]any) (ToolResult, error) {
	raw, isString := args["memoryID"].(string)
	memoryID, err := ids.Parse(raw)
	if !isString || err != nil {
		return ToolResult{}, refuse("memoryID must be a memory UUID from search.")
	}
	content := swiftstr.TrimWhitespacesAndNewlines(stringArg(args, "content"))
	if content == "" {
		return ToolResult{}, refuse("content is required and must be a non-empty string.")
	}
	if swiftstr.Count(content) > maxBodyLength {
		return ToolResult{}, refuse(fmt.Sprintf("content must be at most %d characters.", maxBodyLength))
	}
	vault, err := r.resolveVault(ctx, vaultaccess.Write)
	if err != nil {
		return ToolResult{}, err
	}
	// As memory_save: a contribution row references the user, so only the
	// caller's own vault can take the write (ruling 21).
	if vault != r.Actor {
		return modelOnly(sharedVaultUnavailable), nil
	}
	if r.Pool == nil {
		return ToolResult{}, errMissingDependency
	}
	memory, err := memorystore.New(r.Pool, nil).Find(ctx, vault, memoryID)
	if err != nil {
		return ToolResult{}, err
	}
	if memory == nil {
		return ToolResult{}, refuse("there is no memory with that ID in this vault.")
	}
	return r.proposeChange(ctx, "memory_update", vault,
		map[string]any{"memoryID": ids.Upper(memoryID), "content": content},
		"Change this memory: "+quoted(memory.Content, 50)+" → "+quoted(content, 50),
		resource(ResourceMemory, memoryID, ChangeUpdated))
}

// MARK: - Confirmed actions

func (m *Module) deleteSpace(ctx context.Context, args map[string]any, vault, user uuid.UUID) (bool, string) {
	raw, _ := args["spaceID"].(string)
	id, err := ids.Parse(raw)
	if err != nil || m.deps.Spaces == nil {
		return false, genericFailure
	}
	ctx, cancel := context.WithTimeout(ctx, m.forgetTimeout)
	defer cancel()
	name, err := m.deps.Spaces.Delete(ctx, vault, user, id)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			return false, spaceGone
		}
		m.log.WarnContext(ctx, "quick assistant space delete failed", "error_type", fmt.Sprintf("%T", err))
		return false, genericFailure
	}
	m.log.InfoContext(ctx, "quick assistant write", "tool", "space_delete", "vault", ids.Upper(vault), "actor", ids.Upper(user))
	return true, "Deleted the space " + quoted(name, 60) + "."
}

func (m *Module) deleteFile(ctx context.Context, args map[string]any, vault, user uuid.UUID) (bool, string) {
	path, _ := args["path"].(string)
	if path == "" || m.deps.Files == nil {
		return false, genericFailure
	}
	ctx, cancel := context.WithTimeout(ctx, m.forgetTimeout)
	defer cancel()
	if err := m.deps.Files.Delete(ctx, vault, path); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return false, fileGone
		}
		m.log.WarnContext(ctx, "quick assistant file delete failed", "error_type", fmt.Sprintf("%T", err))
		return false, genericFailure
	}
	m.log.InfoContext(ctx, "quick assistant write", "tool", "file_delete", "vault", ids.Upper(vault), "actor", ids.Upper(user))
	return true, "Deleted the note " + quoted(vaultfiles.LastPathComponent(path), 60) + "."
}

func (m *Module) updateMemory(ctx context.Context, args map[string]any, vault, user uuid.UUID) (bool, string) {
	if m.deps.MCP == nil {
		return false, genericFailure
	}
	ctx, cancel := context.WithTimeout(ctx, m.forgetTimeout)
	defer cancel()
	out, err := m.deps.MCP.CallAsSession(ctx, "memory_update", args, vault, user)
	if err != nil {
		m.log.WarnContext(ctx, "quick assistant memory update failed", "error_type", fmt.Sprintf("%T", err))
		return false, genericFailure
	}
	if updated, _ := out["updated"].(bool); updated {
		m.log.InfoContext(ctx, "quick assistant write", "tool", "memory_update", "vault", ids.Upper(vault), "actor", ids.Upper(user))
		return true, "Updated that memory."
	}
	return false, memoryMissing
}

// MARK: - Helpers

// appVault is resolveVault for a tool that needs the spaces and vault services.
func (r *Runner) appVault(ctx context.Context, perm vaultaccess.Permission) (uuid.UUID, error) {
	if r.Spaces == nil || r.Files == nil {
		return uuid.Nil, errMissingDependency
	}
	return r.resolveVault(ctx, perm)
}

// spaceBySlug is the vault's space with that slug. The model is asked for a
// slug, not an id: it can read one out of space_list and cannot invent the
// other.
func (r *Runner) spaceBySlug(ctx context.Context, vault uuid.UUID, raw any) (spaces.SpaceDTO, error) {
	text, _ := raw.(string)
	slug := normalizedSlug(text)
	if slug == "" {
		return spaces.SpaceDTO{}, refuse("space is required: the slug of a space from space_list.")
	}
	all, err := r.Spaces.List(ctx, vault)
	if err != nil {
		return spaces.SpaceDTO{}, err
	}
	for _, s := range all {
		if s.Slug == slug {
			return s, nil
		}
	}
	return spaces.SpaceDTO{}, refuse("there is no space with the slug '" + Shortened(slug, 40) + "'. Use space_list to see the user's spaces.")
}

func normalizedSlug(raw string) string {
	return strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(raw))
}

func spaceName(value any) (string, error) {
	text, _ := value.(string)
	name := swiftstr.TrimWhitespacesAndNewlines(text)
	if name == "" {
		return "", refuse("name is required and must be a non-empty string.")
	}
	if swiftstr.Count(name) > maxSpaceNameLength {
		return "", refuse(fmt.Sprintf("name must be at most %d characters.", maxSpaceNameLength))
	}
	return name, nil
}

// optionalText is an optional string argument, trimmed. Absent stays nil,
// which keeps a stored value; an empty string clears it.
func optionalText(args map[string]any, key string, limit int) (*string, error) {
	value, present := args[key]
	if !present || value == nil {
		return nil, nil
	}
	text, isString := value.(string)
	if !isString || swiftstr.Count(text) > limit {
		return nil, refuse(fmt.Sprintf("%s must be a string of at most %d characters.", key, limit))
	}
	text = swiftstr.TrimWhitespacesAndNewlines(text)
	return &text, nil
}

func optionalCategory(args map[string]any) (*string, error) {
	category, err := optionalText(args, "category", maxSpaceCategoryLength)
	if err != nil || category == nil {
		return nil, err
	}
	lowered := strings.ToLower(*category)
	return &lowered, nil
}

func optionalIcon(args map[string]any) (*string, error) {
	value, present := args["icon"]
	if !present || value == nil {
		return nil, nil
	}
	icon, _ := value.(string)
	for _, known := range spaceIcons {
		if icon == known {
			return &icon, nil
		}
	}
	return nil, refuse("icon must be one of the listed icons.")
}

func filePath(value any) (string, error) {
	path, _ := value.(string)
	if swiftstr.TrimWhitespacesAndNewlines(path) == "" {
		return "", refuse("path is required: a note's path from browse or search.")
	}
	return path, nil
}

// readable turns the services' 400, 404 and 409 into a refusal, so the model
// reads why ("slug already used in this vault", "destination path already
// exists") and can tell the user or correct itself. Any other error stays a
// failure: its text is not written for a reader.
func readable(err error) error {
	var we *wire.Error
	if errors.As(err, &we) {
		switch we.Status {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict:
			return refuse(we.Message + ".")
		}
	}
	return err
}

func statusOf(err error) int {
	var we *wire.Error
	if errors.As(err, &we) {
		return we.Status
	}
	return 0
}

// wrote counts a write that took effect and logs it: the tool, the vault and
// who asked, never the arguments.
func (r *Runner) wrote(ctx context.Context, tool string, vault uuid.UUID) {
	r.state().noteWrite()
	args := []any{"tool", tool, "vault", ids.Upper(vault), "actor", ids.Upper(r.Actor)}
	if r.ConversationID != nil {
		args = append(args, "conversation", ids.Upper(*r.ConversationID))
	}
	r.logger().InfoContext(ctx, "quick assistant write", args...)
}

// quoted is text in curly quotes, shortened, as the chips and cards show it.
func quoted(text string, limit int) string {
	return "“" + Shortened(text, limit) + "”"
}
