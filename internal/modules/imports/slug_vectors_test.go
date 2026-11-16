package imports_test

import (
	"lumina/internal/modules/imports"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSlugVectors pins the names bulk import gives Spaces and files. The web
// importer's dry run ports these rules (LuminaVaultWebApp
// src/lib/import/markdown-folder/slug.test.ts holds the same table); change both
// together.
func TestSlugVectors(t *testing.T) {
	spaces := map[string]string{
		"World":                 "world",
		"Lore & Legends":        "lore-legends",
		"My_Vault":              "my-vault",
		"Café":                  "café",
		"Ünïcödé Notes":         "ünïcödé-notes",
		"🚀":                     "imported",
		"A":                     "imported",
		strings.Repeat("x", 40): strings.Repeat("x", 31),
		"  2024 Journal  ":      "2024-journal",
	}
	for in, want := range spaces {
		assert.Equal(t, want, imports.Slugify(in), "Slugify(%q)", in)
	}
	paths := map[string]string{
		"Lore/The Last Light.md": "world/lore/the-last-light.md",
		"My Note [draft].md":     "world/my-note-draft.md",
		"a/../b.md":              "world/a/b.md",
		"Bible v0.1.md":          "world/bible-v0.1.md",
		"README":                 "world/readme.md",
		"🚀.md":                   "world/x.md",
		"notes.txt":              "world/notes.md",
		"Ćwiczenia/Zażółć.md":    "world/ćwiczenia/zażółć.md",
		"Daily/2024-01-01.md":    "world/daily/2024-01-01.md",
		"snake_case_name.md":     "world/snake_case_name.md",
	}
	for in, want := range paths {
		assert.Equal(t, want, imports.SafePath("world", in), "SafePath(%q)", in)
	}
}
