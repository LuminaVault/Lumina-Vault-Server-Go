package bundled

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func files(t *testing.T, f fs.FS) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	require.NoError(t, fs.WalkDir(f, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(f, path)
		out[path] = data
		return err
	}))
	return out
}

// The embed holds each built-in's SKILL.md, byte for byte, and nothing else.
func TestSkillsEmbedsEachTopLevelSkillMD(t *testing.T) {
	embedded := files(t, Skills())
	onDisk, err := filepath.Glob(filepath.Join("skills", "*", "SKILL.md"))
	require.NoError(t, err)
	require.Len(t, embedded, len(onDisk))
	assert.Len(t, embedded, 20)
	for _, path := range onDisk {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, data, embedded[filepath.ToSlash(strings.TrimPrefix(path, "skills"+string(filepath.Separator)))], path)
	}
}

// HermesSkills is the whole checked-in tree, the five kb-* skills plus README.md.
func TestHermesSkillsEmbedsTheWholeTree(t *testing.T) {
	embedded := files(t, HermesSkills())
	onDisk := files(t, os.DirFS("hermesskills"))
	assert.Equal(t, onDisk, embedded)
	assert.Len(t, embedded, 27)

	entries, err := fs.ReadDir(HermesSkills(), ".")
	require.NoError(t, err)
	var kb []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "kb-") {
			kb = append(kb, e.Name())
			data, err := fs.ReadFile(HermesSkills(), e.Name()+"/SKILL.md")
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(string(data), "---\nname: "+e.Name()+"\n"), e.Name())
		}
	}
	slices.Sort(kb)
	assert.Equal(t, []string{"kb-compile", "kb-import", "kb-ingest", "kb-merge-vault", "kb-output"}, kb)
}
