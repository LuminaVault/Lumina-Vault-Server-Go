package quickassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/modules/spaces"
	"lumina/internal/modules/vault"
	"lumina/internal/wire"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// appRig is a rig whose runners are offered the app-action tools, wired the
// way cmd/App wires them.
func appRig(t *testing.T) rig {
	g := newRig(t)
	g.deps.Spaces = spaces.New(g.pool, g.paths, nil)
	g.deps.Files = vault.NewFiles(g.pool, g.paths)
	return g
}

func (g rig) spaceNames(vault uuid.UUID) []string {
	g.t.Helper()
	rows, err := g.pool.Query(context.Background(), `SELECT name FROM spaces WHERE tenant_id = $1 ORDER BY name`, vault)
	require.NoError(g.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		require.NoError(g.t, rows.Scan(&n))
		out = append(out, n)
	}
	return out
}

func (g rig) createdSpace(r *Runner, name string) {
	g.t.Helper()
	result := r.Run(context.Background(), "space_create", map[string]any{"name": name})
	require.NotNil(g.t, result.Action, result.ModelContent)
}

func (g rig) filePaths(vault uuid.UUID) []string {
	g.t.Helper()
	rows, err := g.pool.Query(context.Background(), `SELECT path FROM vault_files WHERE tenant_id = $1 ORDER BY path`, vault)
	require.NoError(g.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		require.NoError(g.t, rows.Scan(&p))
		out = append(out, p)
	}
	return out
}

func (g rig) memoryText(id uuid.UUID) string {
	g.t.Helper()
	var s string
	require.NoError(g.t, g.pool.QueryRow(context.Background(), `SELECT content FROM memories WHERE id = $1`, id).Scan(&s))
	return s
}

// MARK: - Offering

func TestAppActionToolsAreOfferedOnlyWhenWired(t *testing.T) {
	plain := names(Definitions(DefinitionOptions{DeviceWrites: true}))
	for _, n := range slices.Concat(appRunTools, appConfirmTools) {
		require.NotContains(t, plain, n)
	}

	with := names(Definitions(DefinitionOptions{DeviceWrites: true, AppActions: true}))
	require.Equal(t, slices.Concat(plain, appRunTools[:3], []string{"space_delete", "file_move", "file_delete", "memory_update"}), with,
		"the app-action tools come after the others and change none of them")

	for _, d := range Definitions(DefinitionOptions{DeviceWrites: true, AppActions: true}) {
		require.NotEqual(t, Refused, ActionFor(d.Name), d.Name)
		require.Equal(t, d.Writes, IsWrite(d.Name), d.Name)
		var s struct {
			AdditionalProperties *bool `json:"additionalProperties"`
		}
		require.NoError(t, json.Unmarshal(d.InputSchema, &s))
		require.NotNil(t, s.AdditionalProperties, d.Name)
		require.False(t, *s.AdditionalProperties, "%s must refuse unknown arguments", d.Name)
	}
}

func TestOpenScreenOffersASpaceOnlyWithAppActions(t *testing.T) {
	screensOf := func(o DefinitionOptions) []any {
		props, _ := schemaOf(t, find(t, Definitions(o), "open_screen"))
		return props["screen"].(map[string]any)["enum"].([]any)
	}
	require.NotContains(t, screensOf(DefinitionOptions{}), "space")
	require.Contains(t, screensOf(DefinitionOptions{AppActions: true}), "space")
}

func TestARunnerWithoutTheServicesDoesNotOfferOrRunThem(t *testing.T) {
	g := newRig(t)
	me := g.user()
	for _, n := range slices.Concat(appRunTools, appConfirmTools) {
		result := g.runner(me, me).Run(context.Background(), n, map[string]any{"name": "Health"})
		require.Nil(t, result.Action, n)
		require.Equal(t, notAvailable, result.ModelContent, n)
	}
	require.Empty(t, g.spaceNames(me))

	result := g.runner(me, me).Run(context.Background(), "open_screen", map[string]any{"screen": "space"})
	require.Nil(t, result.Action)
	require.Equal(t, "The tool did not run: screen must be one of the listed screens.", result.ModelContent)
}

// MARK: - Spaces

func TestSpaceCreateMakesTheSpaceAndADoneChip(t *testing.T) {
	g := appRig(t)
	me := g.user()
	r := g.runner(me, me)

	result := r.Run(context.Background(), "space_create", map[string]any{
		"name": "  Meal Plans ", "icon": "heart.fill", "category": "Health", "description": "What we cook",
	})

	require.NotNil(t, result.Action, result.ModelContent)
	require.Equal(t, KindDone, result.Action.Kind)
	require.Equal(t, "Created the space “Meal Plans”", result.Action.Summary)
	require.Equal(t, ResourceSpace, result.Action.Resource.Kind)
	require.Equal(t, ChangeCreated, result.Action.Resource.Change)
	out := decodeObject(t, result.ModelContent)
	require.Equal(t, "meal-plans", out["slug"], "the slug is derived from the name")

	var id uuid.UUID
	var icon, category, description string
	var by uuid.UUID
	require.NoError(t, g.pool.QueryRow(context.Background(),
		`SELECT id, icon, category, description, created_by_user_id FROM spaces WHERE tenant_id = $1 AND slug = 'meal-plans'`, me).
		Scan(&id, &icon, &category, &description, &by))
	require.Equal(t, id, result.Action.Resource.ID.UUID, "the chip names the space it made")
	require.Equal(t, "heart.fill", icon)
	require.Equal(t, "health", category, "categories are lowercase labels")
	require.Equal(t, "What we cook", description)
	require.Equal(t, me, by, "made as the user who asked")
	require.DirExists(t, filepath.Join(g.paths.Raw(me), "meal-plans"))
	require.Equal(t, 1, g.count(`SELECT count(*) FROM vault_activity_events WHERE vault_id = $1 AND action = 'space.created'`, me))
	require.Equal(t, 1, r.state().Writes())
}

func TestSpaceCreateTellsTheModelWhyItDidNotRun(t *testing.T) {
	g := appRig(t)
	me := g.user()
	r := g.runner(me, me)
	g.createdSpace(r, "Recipes")

	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"name": "Recipes"}, "The tool did not run: slug already used in this vault."},
		{map[string]any{"name": "   "}, "The tool did not run: name is required and must be a non-empty string."},
		{map[string]any{}, "The tool did not run: name is required and must be a non-empty string."},
		{map[string]any{"name": "X", "icon": "trash.fill"}, "The tool did not run: icon must be one of the listed icons."},
		{map[string]any{"name": "X", "slug": "x"}, "The tool did not run: it has no argument named 'slug'."},
		{map[string]any{"name": "X", "color": "#fff"}, "The tool did not run: it has no argument named 'color'."},
		{map[string]any{"name": string(make([]byte, 0)) + fmt.Sprintf("%081d", 0)}, "The tool did not run: name must be at most 80 characters."},
	}
	for _, c := range cases {
		result := r.Run(context.Background(), "space_create", c.args)
		require.Nil(t, result.Action, c.want)
		require.Equal(t, c.want, result.ModelContent)
	}
	require.Equal(t, []string{"Recipes"}, g.spaceNames(me))
	require.Equal(t, 1, r.state().Writes(), "a refused create is not a write")
}

func TestSpaceListShowsTheTurnVaultOnly(t *testing.T) {
	g := appRig(t)
	me, other := g.user(), g.user()
	g.createdSpace(g.runner(me, me), "Work")
	g.createdSpace(g.runner(me, me), "Ideas")
	g.createdSpace(g.runner(other, other), "Private")
	g.exec(`INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		SELECT $1, $2, id, 'work/a.md', 'text/markdown', 1, 'x', now(), now() FROM spaces WHERE tenant_id = $2 AND slug = 'work'`, uuid.New(), me)

	result := g.runner(me, me).Run(context.Background(), "space_list", map[string]any{})

	require.Nil(t, result.Action, "reading shows no chip")
	require.JSONEq(t, `{"spaces":[{"name":"Ideas","slug":"ideas","noteCount":0},{"name":"Work","slug":"work","noteCount":1}]}`, result.ModelContent)
}

func TestSpaceUpdateRenamesAndClears(t *testing.T) {
	g := appRig(t)
	me := g.user()
	r := g.runner(me, me)
	require.NotNil(t, r.Run(context.Background(), "space_create", map[string]any{"name": "Trips", "category": "travel", "icon": "globe"}).Action)

	renamed := r.Run(context.Background(), "space_update", map[string]any{"space": " TRIPS ", "name": "Journeys"})
	require.NotNil(t, renamed.Action, renamed.ModelContent)
	require.Equal(t, "Renamed the space “Trips” to “Journeys”", renamed.Action.Summary)
	require.Equal(t, ChangeUpdated, renamed.Action.Resource.Change)
	require.Equal(t, "trips", decodeObject(t, renamed.ModelContent)["slug"], "a slug never changes")

	cleared := r.Run(context.Background(), "space_update", map[string]any{"space": "trips", "category": ""})
	require.Equal(t, "Updated the space “Journeys”", cleared.Action.Summary)
	var category *string
	var icon string
	require.NoError(t, g.pool.QueryRow(context.Background(), `SELECT category, icon FROM spaces WHERE tenant_id = $1`, me).Scan(&category, &icon))
	require.Nil(t, category, `"" clears`)
	require.Equal(t, "globe", icon, "what is not named stays")
	require.Equal(t, 3, r.state().Writes())

	for args, want := range map[*map[string]any]string{
		{"space": "trips"}:                    "The tool did not run: give at least one of name, description, icon or category to change.",
		{"space": "nope", "name": "X"}:        "The tool did not run: there is no space with the slug 'nope'. Use space_list to see the user's spaces.",
		{"name": "X"}:                         "The tool did not run: space is required: the slug of a space from space_list.",
		{"space": "trips", "name": " "}:       "The tool did not run: name is required and must be a non-empty string.",
		{"space": "trips", "icon": "nothing"}: "The tool did not run: icon must be one of the listed icons.",
	} {
		result := r.Run(context.Background(), "space_update", *args)
		require.Nil(t, result.Action, want)
		require.Equal(t, want, result.ModelContent)
	}
	require.Equal(t, []string{"Journeys"}, g.spaceNames(me))
}

func TestWritesStopAtFivePerTurnAcrossTools(t *testing.T) {
	g := appRig(t)
	me := g.user()
	r := g.runner(me, me)
	for i := range 4 {
		g.createdSpace(r, fmt.Sprintf("Space %d", i))
	}
	require.NotNil(t, r.Run(context.Background(), "memory_save", map[string]any{"content": "I like tea"}).Action)

	sixth := r.Run(context.Background(), "space_create", map[string]any{"name": "One too many"})

	require.Nil(t, sixth.Action)
	require.Equal(t, "The tool did not run: "+tooManyWrites, sixth.ModelContent)
	require.Len(t, g.spaceNames(me), 4)
	renamed := r.Run(context.Background(), "space_update", map[string]any{"space": "space-0", "name": "Renamed"})
	require.Nil(t, renamed.Action, "the cap covers every tool that writes")
}

func TestAViewerCanListButNotChangeSpaces(t *testing.T) {
	g := appRig(t)
	owner, viewer := g.user(), g.user()
	shared := g.sharedVault(owner, viewer, "viewer")
	// The team's owner holds no membership of their own in this rig, so an
	// editor makes the space the viewer will try to change.
	editor := g.user()
	g.exec(`INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
		VALUES ($1, $2, $3, 'editor', true, $4, now(), now())`, uuid.New(), shared, editor, owner)
	g.createdSpace(g.runner(editor, shared), "Team Notes")
	g.file(shared, "team-notes/a.md", time.Now())
	r := g.runner(viewer, shared)

	listed := r.Run(context.Background(), "space_list", map[string]any{})
	require.Contains(t, listed.ModelContent, "team-notes")

	for name, args := range map[string]map[string]any{
		"space_create": {"name": "Mine"},
		"space_update": {"space": "team-notes", "name": "Mine"},
		"space_delete": {"space": "team-notes"},
		"file_move":    {"path": "team-notes/a.md", "space": "inbox"},
		"file_delete":  {"path": "team-notes/a.md"},
	} {
		result := r.Run(context.Background(), name, args)
		require.Nil(t, result.Action, name)
		require.Equal(t, denied, result.ModelContent, name)
	}
	require.Equal(t, []string{"Team Notes"}, g.spaceNames(shared))
	require.Equal(t, []string{"team-notes/a.md"}, g.filePaths(shared))
	require.Zero(t, g.count(`SELECT count(*) FROM assistant_pending_actions`))
}

func TestAnEditorChangesTheSharedVaultNotTheirOwn(t *testing.T) {
	g := appRig(t)
	owner, editor := g.user(), g.user()
	shared := g.sharedVault(owner, editor, "editor")

	g.createdSpace(g.runner(editor, shared), "Roadmap")

	require.Equal(t, []string{"Roadmap"}, g.spaceNames(shared))
	require.Empty(t, g.spaceNames(editor))
}

func TestSpaceDeleteWaitsForTheCardThenDeletes(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	r := w.runner(w.user)
	g.createdSpace(r, "Drafts")
	g.exec(`INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		SELECT gen_random_uuid(), $1, id, 'drafts/' || n || '.md', 'text/markdown', 1, n::text, now(), now()
		FROM spaces, generate_series(1, 2) n WHERE tenant_id = $1 AND slug = 'drafts'`, w.user)
	require.NoError(t, os.WriteFile(filepath.Join(g.paths.Raw(w.user), "drafts", "1.md"), []byte("kept"), 0o600))

	result := r.Run(context.Background(), "space_delete", map[string]any{"space": "drafts"})

	require.NotNil(t, result.Action, result.ModelContent)
	require.Equal(t, KindNeedsConfirm, result.Action.Kind)
	require.Equal(t, "Delete the space “Drafts” and its 2 notes", result.Action.Summary)
	require.Equal(t, ChangeDeleted, result.Action.Resource.Change)
	require.Equal(t, proposalPending, result.ModelContent, "the model must not say it is done")
	require.Equal(t, []string{"Drafts"}, g.spaceNames(w.user), "nothing happens before the user confirms")
	require.Equal(t, 1, r.state().Writes(), "a card is not a write")

	out := w.confirm(result.Action.ID.UUID)

	require.Equal(t, StatusConfirmed, out.resolution.Status)
	require.Equal(t, "Deleted the space “Drafts”.", out.resolution.Message)
	require.Empty(t, g.spaceNames(w.user))
	kept, err := filepath.Glob(filepath.Join(g.paths.Raw(w.user), "_deleted_*_drafts", "1.md"))
	require.NoError(t, err)
	require.Len(t, kept, 1, "the folder is renamed, so the notes stay on disk")
	require.Equal(t, 1, g.count(`SELECT count(*) FROM vault_activity_events WHERE vault_id = $1 AND action = 'space.deleted'`, w.user))

	again := w.confirm(result.Action.ID.UUID)
	require.Equal(t, StatusAlreadyResolved, again.resolution.Status)
}

func TestConfirmingADeleteOfASpaceAlreadyGoneFails(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	r := w.runner(w.user)
	g.createdSpace(r, "Drafts")
	card := r.Run(context.Background(), "space_delete", map[string]any{"space": "drafts"})
	require.NotNil(t, card.Action)
	g.exec(`DELETE FROM spaces WHERE tenant_id = $1`, w.user)

	out := w.confirm(card.Action.ID.UUID)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, spaceGone, out.resolution.Message)
	require.Equal(t, "failed", w.status(card.Action.ID.UUID))
}

func TestACancelledSpaceDeleteChangesNothing(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	r := w.runner(w.user)
	g.createdSpace(r, "Drafts")
	card := r.Run(context.Background(), "space_delete", map[string]any{"space": "drafts"})

	require.Equal(t, StatusCancelled, w.cancel(card.Action.ID.UUID).resolution.Status)
	require.Equal(t, []string{"Drafts"}, g.spaceNames(w.user))
}

func TestAMemberDemotedBeforeConfirmDeletesNothing(t *testing.T) {
	g := appRig(t)
	owner := g.user()
	w := newWorld(g, worldOptions{})
	shared := g.sharedVault(owner, w.user, "editor")
	r := w.runner(shared)
	g.createdSpace(r, "Roadmap")
	card := r.Run(context.Background(), "space_delete", map[string]any{"space": "roadmap"})
	require.NotNil(t, card.Action, card.ModelContent)
	g.exec(`UPDATE vault_memberships SET role = 'viewer' WHERE vault_id = $1 AND user_id = $2`, shared, w.user)

	out := w.confirm(card.Action.ID.UUID)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, accessLost, out.resolution.Message)
	require.Equal(t, []string{"Roadmap"}, g.spaceNames(shared))
}

func TestOpenScreenOpensOneSpace(t *testing.T) {
	g := appRig(t)
	me := g.user()
	r := g.runner(me, me)
	g.createdSpace(r, "Health")

	result := r.Run(context.Background(), "open_screen", map[string]any{"screen": "space", "space": "health"})

	require.NotNil(t, result.Action, result.ModelContent)
	require.Equal(t, KindOpenScreen, result.Action.Kind)
	require.Equal(t, ScreenSpace, *result.Action.Screen)
	require.Equal(t, "Opening Health", result.Action.Summary)
	var id uuid.UUID
	require.NoError(t, g.pool.QueryRow(context.Background(), `SELECT id FROM spaces WHERE tenant_id = $1`, me).Scan(&id))
	require.Equal(t, id, result.Action.SpaceID.UUID)

	missing := r.Run(context.Background(), "open_screen", map[string]any{"screen": "space"})
	require.Nil(t, missing.Action)
	require.Equal(t, "The tool did not run: space is required: the slug of a space from space_list.", missing.ModelContent)

	home := r.Run(context.Background(), "open_screen", map[string]any{"screen": "home"})
	require.Equal(t, ScreenHome, *home.Action.Screen, "the other screens are unchanged")
}

// MARK: - Files

func TestFileMoveFilesANoteIntoASpaceAndBack(t *testing.T) {
	g := appRig(t)
	me := g.user()
	r := g.runner(me, me)
	g.createdSpace(r, "Recipes")
	g.file(me, "inbox/soup.md", time.Now())
	require.NoError(t, os.MkdirAll(filepath.Join(g.paths.Raw(me), "inbox"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(g.paths.Raw(me), "inbox", "soup.md"), []byte("x"), 0o600))

	moved := r.Run(context.Background(), "file_move", map[string]any{"path": "inbox/soup.md", "space": "recipes"})

	require.NotNil(t, moved.Action, moved.ModelContent)
	require.Equal(t, "Moved “soup.md” to “Recipes”", moved.Action.Summary)
	require.Equal(t, ResourceVaultFile, moved.Action.Resource.Kind)
	require.Equal(t, ChangeMoved, moved.Action.Resource.Change)
	require.Equal(t, "recipes/soup.md", decodeObject(t, moved.ModelContent)["path"])
	require.Equal(t, []string{"recipes/soup.md"}, g.filePaths(me))
	require.FileExists(t, filepath.Join(g.paths.Raw(me), "recipes", "soup.md"))
	require.Equal(t, 1, g.count(`SELECT count(*) FROM vault_files f JOIN spaces s ON s.id = f.space_id WHERE f.tenant_id = $1 AND s.slug = 'recipes'`, me),
		"the row is filed under the space")

	back := r.Run(context.Background(), "file_move", map[string]any{"path": "recipes/soup.md", "space": "Inbox"})
	require.Equal(t, "Moved “soup.md” to the inbox", back.Action.Summary)
	require.Equal(t, []string{"inbox/soup.md"}, g.filePaths(me))
	require.Equal(t, 3, r.state().Writes())
}

func TestFileMoveTellsTheModelWhyItDidNotRun(t *testing.T) {
	g := appRig(t)
	me, other := g.user(), g.user()
	r := g.runner(me, me)
	g.createdSpace(r, "Recipes")
	g.file(me, "inbox/soup.md", time.Now())
	g.file(me, "recipes/soup.md", time.Now())
	g.file(other, "inbox/theirs.md", time.Now())

	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"path": "inbox/soup.md", "space": "recipes"}, "The tool did not run: destination path already exists."},
		{map[string]any{"path": "inbox/soup.md", "space": "nope"}, "The tool did not run: there is no space with the slug 'nope'. Use space_list to see the user's spaces."},
		{map[string]any{"path": "inbox/missing.md", "space": "recipes"}, "The tool did not run: vault file not found."},
		{map[string]any{"path": "inbox/theirs.md", "space": "recipes"}, "The tool did not run: vault file not found."},
		{map[string]any{"path": "", "space": "recipes"}, "The tool did not run: path is required: a note's path from browse or search."},
		{map[string]any{"path": "inbox/soup.md", "newPath": "x.md", "space": "recipes"}, "The tool did not run: it has no argument named 'newPath'."},
	} {
		result := r.Run(context.Background(), "file_move", c.args)
		require.Nil(t, result.Action, c.want)
		require.Equal(t, c.want, result.ModelContent)
	}
	require.Equal(t, []string{"inbox/soup.md", "recipes/soup.md"}, g.filePaths(me))
	require.Equal(t, []string{"inbox/theirs.md"}, g.filePaths(other))
}

func TestFileDeleteWaitsForTheCardThenDeletes(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	g.file(w.user, "inbox/old.md", time.Now())
	// The note itself, so the delete has a file to move to the trash.
	onDisk := filepath.Join(g.paths.Raw(w.user), "inbox", "old.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(onDisk), 0o755))
	require.NoError(t, os.WriteFile(onDisk, []byte("x"), 0o600))
	r := w.runner(w.user)

	card := r.Run(context.Background(), "file_delete", map[string]any{"path": "inbox/old.md"})

	require.NotNil(t, card.Action, card.ModelContent)
	require.Equal(t, KindNeedsConfirm, card.Action.Kind)
	require.Equal(t, "Delete the note “old.md”", card.Action.Summary)
	require.Equal(t, ChangeDeleted, card.Action.Resource.Change)
	require.Equal(t, []string{"inbox/old.md"}, g.filePaths(w.user), "nothing happens before the user confirms")
	require.Equal(t, "inbox/old.md", g.storedArg(card.Action.ID.UUID, "path"))

	out := w.confirm(card.Action.ID.UUID)

	require.Equal(t, StatusConfirmed, out.resolution.Status)
	require.Equal(t, "Deleted the note “old.md”.", out.resolution.Message)
	require.Empty(t, g.filePaths(w.user))
	// In the trash, against the user who confirmed, so it can be restored.
	require.Equal(t, 1, g.count(
		`SELECT count(*) FROM vault_trash WHERE tenant_id = $1 AND original_path = 'inbox/old.md' AND deleted_by_user_id = $2`,
		w.user, w.user))
	require.NoFileExists(t, onDisk)
}

func TestFileDeleteOfAMissingOrForeignNoteMakesNoCard(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	other := g.user()
	g.file(other, "inbox/theirs.md", time.Now())

	for _, path := range []string{"inbox/missing.md", "inbox/theirs.md", "../../etc/passwd"} {
		result := w.runner(w.user).Run(context.Background(), "file_delete", map[string]any{"path": path})
		require.Nil(t, result.Action, path)
		require.Contains(t, result.ModelContent, "The tool did not run: ", path)
	}
	require.Zero(t, g.count(`SELECT count(*) FROM assistant_pending_actions`))
	require.Equal(t, []string{"inbox/theirs.md"}, g.filePaths(other))
}

func TestConfirmingADeleteOfANoteAlreadyGoneFails(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	g.file(w.user, "inbox/old.md", time.Now())
	card := w.runner(w.user).Run(context.Background(), "file_delete", map[string]any{"path": "inbox/old.md"})
	require.NotNil(t, card.Action)
	g.exec(`DELETE FROM vault_files WHERE tenant_id = $1`, w.user)

	out := w.confirm(card.Action.ID.UUID)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, fileGone, out.resolution.Message)
}

// MARK: - Memory

func TestMemoryUpdateWaitsForTheCardThenReplacesTheText(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	memoryID := g.memory(w.user, "I live in Lisbon", nil, time.Now())

	card := w.runner(w.user).Run(context.Background(), "memory_update",
		map[string]any{"memoryID": memoryID.String(), "content": "  I live in Porto "})

	require.NotNil(t, card.Action, card.ModelContent)
	require.Equal(t, KindNeedsConfirm, card.Action.Kind)
	require.Equal(t, "Change this memory: “I live in Lisbon” → “I live in Porto”", card.Action.Summary,
		"the old text is read from the stored memory")
	require.Equal(t, ResourceMemory, card.Action.Resource.Kind)
	require.Equal(t, memoryID, card.Action.Resource.ID.UUID)
	require.Equal(t, "I live in Lisbon", g.memoryText(memoryID), "nothing happens before the user confirms")

	out := w.confirm(card.Action.ID.UUID)

	require.Equal(t, StatusConfirmed, out.resolution.Status)
	require.Equal(t, "Updated that memory.", out.resolution.Message)
	require.Equal(t, "I live in Porto", g.memoryText(memoryID))
}

func TestMemoryUpdateMakesNoCardItCouldNotHonour(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	other := g.user()
	mine := g.memory(w.user, "I like tea", nil, time.Now())
	theirs := g.memory(other, "their secret", nil, time.Now())
	shared := g.sharedVault(other, w.user, "editor")
	inShared := g.memory(shared, "team fact", nil, time.Now())

	for _, c := range []struct {
		vault uuid.UUID
		args  map[string]any
		want  string
	}{
		{w.user, map[string]any{"memoryID": theirs.String(), "content": "x"}, "The tool did not run: there is no memory with that ID in this vault."},
		{w.user, map[string]any{"memoryID": "nope", "content": "x"}, "The tool did not run: memoryID must be a memory UUID from search."},
		{w.user, map[string]any{"memoryID": mine.String(), "content": "  "}, "The tool did not run: content is required and must be a non-empty string."},
		{shared, map[string]any{"memoryID": inShared.String(), "content": "x"}, sharedVaultUnavailable},
	} {
		result := w.runner(c.vault).Run(context.Background(), "memory_update", c.args)
		require.Nil(t, result.Action, c.want)
		require.Equal(t, c.want, result.ModelContent)
	}
	require.Zero(t, g.count(`SELECT count(*) FROM assistant_pending_actions`))
	require.Equal(t, "their secret", g.memoryText(theirs))
}

func TestConfirmingAnUpdateOfAMemoryAlreadyGoneFails(t *testing.T) {
	g := appRig(t)
	w := newWorld(g, worldOptions{})
	memoryID := g.memory(w.user, "I like tea", nil, time.Now())
	card := w.runner(w.user).Run(context.Background(), "memory_update", map[string]any{"memoryID": memoryID.String(), "content": "I like coffee"})
	require.NotNil(t, card.Action)
	g.exec(`DELETE FROM memories WHERE id = $1`, memoryID)

	out := w.confirm(card.Action.ID.UUID)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, memoryMissing, out.resolution.Message)
}

// MARK: - Wire

func TestAnActionNamesWhatItChanged(t *testing.T) {
	space := ScreenSpace
	id := wire.UUID{UUID: testID}
	got, err := json.Marshal(Action{
		ID: id, Kind: KindDone, Summary: "s",
		Resource: resource(ResourceSpace, testID, ChangeCreated),
	})
	require.NoError(t, err)
	require.Equal(t, `{"id":"`+testIDUpper+`","kind":"done","summary":"s","resource":{"kind":"space","id":"`+testIDUpper+`","change":"created"}}`, string(got))

	got, err = json.Marshal(Action{ID: id, Kind: KindOpenScreen, Summary: "s", Screen: &space, SpaceID: &id})
	require.NoError(t, err)
	require.Contains(t, string(got), `"screen":"space","spaceID":"`+testIDUpper+`"`)

	got, err = json.Marshal(Action{ID: id, Kind: KindDone, Summary: "s"})
	require.NoError(t, err)
	require.NotContains(t, string(got), "resource", "older tools send exactly what they sent before")
	require.NotContains(t, string(got), "spaceID")
}
