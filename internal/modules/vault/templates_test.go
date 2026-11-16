package vault

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRenderTemplate(t *testing.T) {
	got := renderTemplate("# {{title}} {{date}} {{time}} {{space}} {{date:YYYY}} {{other}}",
		templateVars{date: "2026-11-16", time: "09:30", title: "T", space: "Work"})
	require.Equal(t, "# T 2026-11-16 09:30 Work {{date:YYYY}} {{other}}", got)
}

func TestNoteFileName(t *testing.T) {
	for in, want := range map[string]string{
		"Weekly sync!":           "Weekly-sync",
		"  a / b \\ c  ":         "a-b-c",
		"café_notes.v2":          "café_notes.v2",
		"???":                    "untitled",
		"":                       "untitled",
		"..hidden":               "hidden",
		strings.Repeat("é", 100): strings.Repeat("é", 60),
	} {
		require.Equal(t, want, noteFileName(in), in)
	}
}

type noteDTO struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

func decodeNote(t *testing.T, r response) noteDTO {
	t.Helper()
	var out noteDTO
	require.NoError(t, json.Unmarshal(r.body, &out), string(r.body))
	return out
}

func (v env) daily(user uuid.UUID, query string) response {
	v.t.Helper()
	return v.do(user, http.MethodPost, "/v1/vault/daily"+query, nil, nil)
}

func (v env) fromTemplate(user uuid.UUID, body string) response {
	v.t.Helper()
	return v.do(user, http.MethodPost, "/v1/vault/notes/from-template", map[string]string{"Content-Type": "application/json"}, []byte(body))
}

func (v env) seed(user uuid.UUID) VaultTemplatesSeedResponse {
	v.t.Helper()
	r := v.do(user, http.MethodPost, "/v1/vault/templates/seed", nil, nil)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out VaultTemplatesSeedResponse
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out
}

func (v env) readRaw(user uuid.UUID, path string) string {
	v.t.Helper()
	b, err := os.ReadFile(filepath.Join(v.module.paths.Raw(user), path))
	require.NoError(v.t, err)
	return string(b)
}

func TestDailyNote(t *testing.T) {
	v := setup(t)

	t.Run("created once, then returned", func(t *testing.T) {
		u := v.user("trial")
		first := v.daily(u, "?date=2026-11-16&tz=Europe/Lisbon")
		require.Equal(t, http.StatusCreated, first.code, string(first.body))
		note := decodeNote(t, first)
		require.Equal(t, "daily/2026-11-16.md", note.Path)
		require.True(t, strings.HasPrefix(v.readRaw(u, note.Path), "# 2026-11-16\n"))

		again := v.daily(u, "?date=2026-11-16")
		require.Equal(t, http.StatusOK, again.code, string(again.body))
		require.Equal(t, note.ID, decodeNote(t, again).ID)

		list := v.list(u, "?space=daily")
		require.NotNil(t, findFile(list.Files, note.Path))
	})

	t.Run("two devices at once get one note", func(t *testing.T) {
		u := v.user("trial")
		codes := make([]int, 6)
		paths := make([]string, 6)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := v.daily(u, "?date=2026-01-02")
				codes[i] = r.code
				paths[i] = decodeNote(t, r).Path
			}()
		}
		wg.Wait()
		created := 0
		for i, c := range codes {
			require.Contains(t, []int{http.StatusOK, http.StatusCreated}, c)
			require.Equal(t, "daily/2026-01-02.md", paths[i])
			if c == http.StatusCreated {
				created++
			}
		}
		require.Equal(t, 1, created)
	})

	t.Run("uses the user's daily template", func(t *testing.T) {
		u := v.user("trial")
		seeded := v.seed(u)
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=daily.md&space_id="+seeded.SpaceId.String(),
			map[string]string{"Content-Type": "text/markdown"}, []byte("Mine: {{date}} in {{space}}"))
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		d := v.daily(u, "?date=2026-03-04")
		require.Equal(t, http.StatusCreated, d.code, string(d.body))
		require.Equal(t, "Mine: 2026-03-04 in Daily", v.readRaw(u, decodeNote(t, d).Path))
	})

	t.Run("bad input is 400", func(t *testing.T) {
		u := v.user("trial")
		require.Equal(t, http.StatusBadRequest, v.daily(u, "?date=16-11-2026").code)
		require.Equal(t, http.StatusBadRequest, v.daily(u, "?tz=Mars/Base").code)
	})
}

func TestTemplates(t *testing.T) {
	v := setup(t)

	t.Run("seed writes the starter set once", func(t *testing.T) {
		u := v.user("trial")
		first := v.seed(u)
		require.ElementsMatch(t, []string{"daily.md", "meeting.md", "project.md"}, first.Created)
		second := v.seed(u)
		require.Empty(t, second.Created)
		require.Equal(t, first.SpaceId, second.SpaceId)
		require.Len(t, v.list(u, "?space=templates").Files, 3)
	})

	t.Run("seed keeps an edited template", func(t *testing.T) {
		u := v.user("trial")
		s := v.seed(u)
		r := v.do(u, http.MethodPost, "/v1/vault/files?path=meeting.md&space_id="+s.SpaceId.String(),
			map[string]string{"Content-Type": "text/markdown"}, []byte("custom"))
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		require.Empty(t, v.seed(u).Created)
		require.Equal(t, "custom", v.readRaw(u, "templates/meeting.md"))
	})

	t.Run("note from template renders and refuses a duplicate", func(t *testing.T) {
		u := v.user("trial")
		v.seed(u)
		work := v.space(u, "work")
		body := `{"template":"meeting.md","title":"Weekly sync!","tz":"Europe/Lisbon","spaceId":"` + work.String() + `"}`
		r := v.fromTemplate(u, body)
		require.Equal(t, http.StatusCreated, r.code, string(r.body))
		note := decodeNote(t, r)
		require.Equal(t, "work/Weekly-sync.md", note.Path)
		content := v.readRaw(u, note.Path)
		require.True(t, strings.HasPrefix(content, "# Weekly sync!\n"), content)
		require.Contains(t, content, " · work") // the test helper names the Space by its slug
		require.NotContains(t, content, "{{")

		require.Equal(t, http.StatusConflict, v.fromTemplate(u, body).code)
	})

	t.Run("unfiled note goes to inbox", func(t *testing.T) {
		u := v.user("trial")
		v.seed(u)
		r := v.fromTemplate(u, `{"template":"project.md","title":"Garden"}`)
		require.Equal(t, http.StatusCreated, r.code, string(r.body))
		require.Equal(t, "inbox/Garden.md", decodeNote(t, r).Path)
	})

	t.Run("errors", func(t *testing.T) {
		u := v.user("trial")
		v.seed(u)
		require.Equal(t, http.StatusNotFound, v.fromTemplate(u, `{"template":"nope.md","title":"x"}`).code)
		require.Equal(t, http.StatusBadRequest, v.fromTemplate(u, `{"template":"daily.md","title":"  "}`).code)
		require.Equal(t, http.StatusBadRequest, v.fromTemplate(u, `{"template":"daily.md","title":"x","tz":"Nowhere"}`).code)
		// A path in `template` can't escape templates/.
		require.Equal(t, http.StatusNotFound, v.fromTemplate(u, `{"template":"../inbox/a.md","title":"x"}`).code)
	})
}
