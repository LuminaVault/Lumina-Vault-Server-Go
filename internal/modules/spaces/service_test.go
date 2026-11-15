package spaces_test

import (
	"context"
	"lumina/internal/modules/spaces"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The service methods are what a caller with no HTTP request uses. Each test
// drives one directly and checks what the route would have left behind.

func ptr(s string) *string { return &s }

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var coded interface{ StatusCode() int }
	require.ErrorAs(t, err, &coded)
	return coded.StatusCode()
}

func (v env) activity() [][]string {
	v.t.Helper()
	rows, err := v.pool.Query(context.Background(),
		`SELECT action, target_title FROM vault_activity_events ORDER BY created_at, action`)
	require.NoError(v.t, err)
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var action, title string
		require.NoError(v.t, rows.Scan(&action, &title))
		out = append(out, []string{action, title})
	}
	require.NoError(v.t, rows.Err())
	return out
}

func TestServiceCreateDerivesSlugMakesFolderAndRecords(t *testing.T) {
	v := setup(t)
	ctx := context.Background()

	got, err := v.m.Create(ctx, alice, alice, spaces.CreateInput{Name: "Meal Plans", Icon: ptr("heart.fill"), Category: ptr("health")})
	require.NoError(t, err)
	require.Equal(t, "Meal Plans", got.Name)
	require.Equal(t, "meal-plans", got.Slug, "no slug given: derived from the name")
	require.Equal(t, "heart.fill", *got.Icon)
	require.Equal(t, "health", *got.Category)

	info, err := os.Stat(filepath.Join(v.paths.Raw(alice), "meal-plans"))
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Contains(t, v.activity(), []string{"space.created", "Meal Plans"})

	var by uuid.UUID
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT created_by_user_id FROM spaces WHERE id = $1`, got.Id.UUID).Scan(&by))
	require.Equal(t, alice, by, "the acting user is recorded as the creator")
}

func TestServiceCreateRejectsTakenSlugAndBlankName(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	_, err := v.m.Create(ctx, alice, alice, spaces.CreateInput{Name: "Recipes"})
	require.NoError(t, err)

	_, err = v.m.Create(ctx, alice, alice, spaces.CreateInput{Name: "Recipes"})
	require.Equal(t, http.StatusConflict, statusOf(t, err))
	require.ErrorContains(t, err, "slug already used in this vault")

	_, err = v.m.Create(ctx, alice, alice, spaces.CreateInput{Name: "   "})
	require.Equal(t, http.StatusBadRequest, statusOf(t, err))
	require.ErrorContains(t, err, "name required")
}

func TestServiceListMatchesTheRoute(t *testing.T) {
	v := setup(t)
	got, err := v.m.List(context.Background(), alice)
	require.NoError(t, err)

	want := v.list(alice)
	require.Len(t, got, len(want))
	for i := range want {
		require.Equal(t, want[i].Slug, got[i].Slug)
		require.Equal(t, want[i].Name, got[i].Name)
		require.Equal(t, want[i].NoteCount, got[i].NoteCount)
	}
}

func TestServiceUpdateKeepsClearsAndRecords(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	made, err := v.m.Create(ctx, alice, alice, spaces.CreateInput{Name: "Trips", Icon: ptr("globe"), Category: ptr("travel")})
	require.NoError(t, err)

	got, err := v.m.Update(ctx, alice, alice, made.Id.UUID, spaces.UpdateInput{Name: ptr("  Journeys "), Category: ptr("")})
	require.NoError(t, err)
	require.Equal(t, "Journeys", got.Name)
	require.Equal(t, "trips", got.Slug, "a slug never changes")
	require.Equal(t, "globe", *got.Icon, "nil keeps the stored value")
	require.Nil(t, got.Category, `"" clears`)
	require.Contains(t, v.activity(), []string{"space.updated", "Journeys"})

	_, err = v.m.Update(ctx, alice, alice, made.Id.UUID, spaces.UpdateInput{Name: ptr(" ")})
	require.Equal(t, http.StatusBadRequest, statusOf(t, err))

	_, err = v.m.Update(ctx, alice, alice, uuid.New(), spaces.UpdateInput{Name: ptr("x")})
	require.Equal(t, http.StatusNotFound, statusOf(t, err))

	_, err = v.m.Update(ctx, bob, bob, made.Id.UUID, spaces.UpdateInput{Name: ptr("x")})
	require.Equal(t, http.StatusNotFound, statusOf(t, err), "another vault's space is not found")
}

func TestServiceDeleteKeepsTheNotesAndRecords(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	made, err := v.m.Create(ctx, alice, alice, spaces.CreateInput{Name: "Drafts"})
	require.NoError(t, err)
	note := filepath.Join(v.paths.Raw(alice), "drafts", "a.md")
	require.NoError(t, os.WriteFile(note, []byte("kept"), 0o600))

	name, err := v.m.Delete(ctx, alice, alice, made.Id.UUID)
	require.NoError(t, err)
	require.Equal(t, "Drafts", name)

	require.NoDirExists(t, filepath.Join(v.paths.Raw(alice), "drafts"))
	moved, err := filepath.Glob(filepath.Join(v.paths.Raw(alice), "_deleted_*_drafts", "a.md"))
	require.NoError(t, err)
	require.Len(t, moved, 1, "the folder is renamed, not removed")
	require.Contains(t, v.activity(), []string{"space.deleted", "Drafts"})

	_, err = v.m.Delete(ctx, alice, alice, made.Id.UUID)
	require.Equal(t, http.StatusNotFound, statusOf(t, err))
}
