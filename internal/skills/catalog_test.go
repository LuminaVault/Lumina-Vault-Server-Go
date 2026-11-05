package skills

import (
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skillFile(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\nmetadata:\n  capability: low\n---\nbody of " + name + "\n"
}

func writeVaultSkill(t *testing.T, paths vaultpaths.Paths, tenant uuid.UUID, dir, contents string) {
	t.Helper()
	path := filepath.Join(paths.TenantRoot(tenant), "skills", dir, "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
}

func names(manifests []Manifest) []string {
	out := make([]string, len(manifests))
	for i, m := range manifests {
		out[i] = m.Name
	}
	return out
}

func newPaths(t *testing.T) vaultpaths.Paths {
	t.Helper()
	return vaultpaths.Paths{Root: t.TempDir()}
}

func TestCatalogBuiltins(t *testing.T) {
	c := NewCatalog(newPaths(t))
	manifests := c.Manifests(uuid.New())
	require.Len(t, manifests, 20)
	assert.IsIncreasing(t, names(manifests))
	for _, m := range manifests {
		assert.Equal(t, SourceBuiltin, m.Source, m.Name)
	}
	// Category directories have nested skills but no SKILL.md of their own, and the
	// nested ones aren't in the catalog.
	assert.NotContains(t, names(manifests), "creative")
	assert.NotContains(t, names(manifests), "humanizer")
	daily, ok := c.Manifest("daily-brief", uuid.New())
	require.True(t, ok)
	assert.Equal(t, "daily-brief", daily.Name)
	_, ok = c.Manifest("nope", uuid.New())
	assert.False(t, ok)
}

func TestCatalogVaultOverridesBuiltin(t *testing.T) {
	paths := newPaths(t)
	tenant, other := uuid.New(), uuid.New()
	writeVaultSkill(t, paths, tenant, "daily-brief", skillFile("daily-brief", "mine"))
	writeVaultSkill(t, paths, tenant, "job-x", skillFile("job-x", "a job"))
	c := NewCatalog(paths)

	m, ok := c.Manifest("daily-brief", tenant)
	require.True(t, ok)
	assert.Equal(t, SourceVault, m.Source)
	assert.Equal(t, "mine", m.Description)
	_, ok = c.Manifest("job-x", tenant)
	assert.True(t, ok)
	assert.Len(t, c.Manifests(tenant), 21)

	// Tenants don't see each other's, and a built-in comes back for them.
	m, ok = c.Manifest("daily-brief", other)
	require.True(t, ok)
	assert.Equal(t, SourceBuiltin, m.Source)
	_, ok = c.Manifest("job-x", other)
	assert.False(t, ok)

	// Rescanned every call, unlike the built-ins.
	writeVaultSkill(t, paths, other, "fresh", skillFile("fresh", "new"))
	_, ok = c.Manifest("fresh", other)
	assert.True(t, ok)
}

func TestCatalogVaultScan(t *testing.T) {
	paths := newPaths(t)
	tenant := uuid.New()
	c := NewVaultCatalog(paths)
	assert.Empty(t, c.Manifests(tenant), "no skills directory")

	writeVaultSkill(t, paths, tenant, "zeta", skillFile("zeta", "z"))
	writeVaultSkill(t, paths, tenant, "alpha", skillFile("alpha", "a"))
	writeVaultSkill(t, paths, tenant, ".hidden", skillFile("hidden", "h"))
	writeVaultSkill(t, paths, tenant, "broken", "no frontmatter")
	writeVaultSkill(t, paths, tenant, "binary", "---\nname: \xff\n---\n")
	require.NoError(t, os.MkdirAll(filepath.Join(paths.TenantRoot(tenant), "skills", "empty-dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(paths.TenantRoot(tenant), "skills", "stray.md"), []byte(skillFile("stray", "s")), 0o644))
	assert.Equal(t, []string{"alpha", "hidden", "zeta"}, names(c.Manifests(tenant)))

	t.Run("a byte order mark is dropped when reading the file", func(t *testing.T) {
		writeVaultSkill(t, paths, tenant, "bom", "\xEF\xBB\xBF"+skillFile("bom", "b"))
		_, ok := c.Manifest("bom", tenant)
		assert.True(t, ok)
	})
	t.Run("the name comes from the manifest, and the later directory wins", func(t *testing.T) {
		writeVaultSkill(t, paths, tenant, "dir-a", skillFile("same", "from a"))
		writeVaultSkill(t, paths, tenant, "dir-b", skillFile("same", "from b"))
		m, ok := c.Manifest("same", tenant)
		require.True(t, ok)
		assert.Equal(t, "from b", m.Description)
	})
	t.Run("symlinks are followed", func(t *testing.T) {
		target := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(target, "SKILL.md"), []byte(skillFile("linked", "l")), 0o644))
		require.NoError(t, os.Symlink(target, filepath.Join(paths.TenantRoot(tenant), "skills", "link")))
		_, ok := c.Manifest("linked", tenant)
		assert.True(t, ok)
	})
	t.Run("a vault catalog has no built-ins", func(t *testing.T) {
		_, ok := c.Manifest("daily-brief", tenant)
		assert.False(t, ok)
	})
}
