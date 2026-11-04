package vaultfiles_test

import (
	"lumina/internal/vaultfiles"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const escapes = "resolved path escapes vault root"

// tree builds <root>/raw with symlinks into <root>/outside, a sibling of raw:
//
//	raw/real/a.md
//	raw/link  -> <root>/outside (absolute)
//	raw/rel   -> ../outside     (relative)
//	raw/inner -> real           (stays inside)
//	outside/secret.md, sib.md
func tree(t *testing.T) (root, raw string) {
	t.Helper()
	root = t.TempDir()
	raw = filepath.Join(root, "raw")
	require.NoError(t, os.MkdirAll(filepath.Join(raw, "real"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "outside"), 0o755))
	write(t, filepath.Join(raw, "real", "a.md"), "inside")
	write(t, filepath.Join(root, "outside", "secret.md"), "secret")
	write(t, filepath.Join(root, "sib.md"), "sibling")
	require.NoError(t, os.Symlink(filepath.Join(root, "outside"), filepath.Join(raw, "link")))
	require.NoError(t, os.Symlink("../outside", filepath.Join(raw, "rel")))
	require.NoError(t, os.Symlink("real", filepath.Join(raw, "inner")))
	return root, raw
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// The expected results are what Swift printed for the same tree, running
// rawRoot.appendingPathComponent(relative) and the standardizedFileURL check.
func TestResolveInsideMatchesSwift(t *testing.T) {
	_, raw := tree(t)
	passes := map[string]string{
		"a.md":               "a.md",
		"a/b.md":             "a/b.md",
		" ":                  " ",
		"a/../b.md":          "a/../b.md",
		"/etc/passwd":        "etc/passwd", // absolute is not absolute here: it lands inside
		"//etc/passwd":       "/etc/passwd",
		"./a.md":             "./a.md",
		"a/./b.md":           "a/./b.md",
		"a//b.md":            "a//b.md",
		"a.md/":              "a.md",
		"real/../a.md":       "real/../a.md",
		"inner/a.md":         "inner/a.md",
		"inner/../real/a.md": "inner/../real/a.md",
		// Encoded separators stay literal file names.
		"..%2f..%2fx.md": "..%2f..%2fx.md",
		"%2e%2e/x.md":    "%2e%2e/x.md",
		"a%2fb.md":       "a%2fb.md",
		"..\\x.md":       "..\\x.md",
		"~/x.md":         "~/x.md",
	}
	for relative, want := range passes {
		got, err := vaultfiles.ResolveInside(raw, relative)
		require.NoError(t, err, "%+q", relative)
		require.Equal(t, raw+"/"+want, got, "%+q", relative)
	}
	for _, relative := range []string{
		"", ".", "./", "/", "..", "../", "../x.md", "../../../../etc/passwd", "a/../../x.md", "a/b/../../../x.md",
		"real/..", "real/../",
		"́x.md", // the root's last "/" joins the mark, so the Character prefix fails
	} {
		_, err := vaultfiles.ResolveInside(raw, relative)
		requireWireError(t, err, http.StatusBadRequest, escapes)
	}
}

func TestResolveInsideSiblingWithRootAsPrefix(t *testing.T) {
	_, raw := tree(t)
	require.NoError(t, os.MkdirAll(raw+"-other", 0o755))
	_, err := vaultfiles.ResolveInside(raw, "../raw-other/x.md")
	requireWireError(t, err, http.StatusBadRequest, escapes)
}

func TestResolveInsideTrailingSlashOnRoot(t *testing.T) {
	_, raw := tree(t)
	got, err := vaultfiles.ResolveInside(raw+"/", "a.md")
	require.NoError(t, err)
	require.Equal(t, raw+"/a.md", got)
	_, err = vaultfiles.ResolveInside(raw+"/", "../a.md")
	requireWireError(t, err, http.StatusBadRequest, escapes)
}

// A ".." that crosses a symlink is judged physically when the whole path
// exists. This is the only symlink defence Swift really has.
func TestResolveInsideRefusesExistingPathThroughSymlink(t *testing.T) {
	root, raw := tree(t)
	write(t, filepath.Join(root, "sib2.md"), "x")
	for _, relative := range []string{"link/../sib2.md", "rel/../sib2.md", "link/../outside/secret.md", "link/.."} {
		_, err := vaultfiles.ResolveInside(raw, relative)
		requireWireError(t, err, http.StatusBadRequest, escapes)
	}
}

// SWIFT WEAKNESS 1, copied on purpose. Swift's doc comment says resolveInside
// defends against symlink escape. Without a ".." the path is never looked at on
// disk, so a symlink inside the vault reaches outside it. SanitizePath accepts
// these paths too: they are clean. If both servers ever fix this, flip this test.
func TestSwiftWeaknessSymlinkEscapeIsNotDetected(t *testing.T) {
	_, raw := tree(t)
	for _, relative := range []string{"link/secret.md", "rel/secret.md"} {
		clean, err := vaultfiles.SanitizePath(relative)
		require.NoError(t, err)
		target, err := vaultfiles.ResolveInside(raw, clean)
		require.NoError(t, err, "Swift passes %s", relative)
		body, err := os.ReadFile(target)
		require.NoError(t, err)
		require.Equal(t, "secret", string(body), "the file outside the vault is readable")
	}
}

// A symlink as the last component escapes the same way.
func TestSwiftWeaknessSymlinkFileEscapeIsNotDetected(t *testing.T) {
	root, raw := tree(t)
	require.NoError(t, os.Symlink(filepath.Join(root, "outside", "secret.md"), filepath.Join(raw, "note.md")))
	target, err := vaultfiles.ResolveInside(raw, "note.md")
	require.NoError(t, err)
	body, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "secret", string(body))
}

// SWIFT WEAKNESS 2, copied on purpose. With a ".." through a symlink, when the
// final file doesn't exist yet: lstat fails, the check falls back to the lexical
// raw/nope.md and passes, and the returned path still crosses the symlink. A
// write lands outside the vault. It takes a caller that skips SanitizePath, and
// every Swift caller that skips it passes a path it read back from a row.
func TestSwiftWeaknessDotDotThroughSymlinkToNewFile(t *testing.T) {
	root, raw := tree(t)
	target, err := vaultfiles.ResolveInside(raw, "link/../nope.md")
	require.NoError(t, err, "Swift passes this")
	require.Equal(t, raw+"/link/../nope.md", target)
	write(t, target, "escaped")
	body, err := os.ReadFile(filepath.Join(root, "nope.md"))
	require.NoError(t, err)
	require.Equal(t, "escaped", string(body), "the write landed outside raw")

	_, err = vaultfiles.SanitizePath("link/../nope.md")
	requireWireError(t, err, http.StatusBadRequest, "path contains illegal segment")
}

// Links that can't be followed make resolution fail, which is the same lexical
// fallback as a missing file.
func TestResolveInsideUnresolvableLinksFallBackToLexical(t *testing.T) {
	root, raw := tree(t)
	require.NoError(t, os.Symlink("loop", filepath.Join(raw, "loop")))
	require.NoError(t, os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(raw, "dangling")))
	for _, relative := range []string{"loop/../x.md", "dangling/../x.md"} {
		_, err := vaultfiles.ResolveInside(raw, relative)
		require.NoError(t, err, relative)
	}
	_, err := vaultfiles.ResolveInside(raw, "loop/../../x.md")
	requireWireError(t, err, http.StatusBadRequest, escapes)
}

// QUIRK: the root is standardized lexically and a target with ".." physically,
// so under a symlinked root a harmless "a/../b.md" is refused once it exists.
func TestResolveInsideSymlinkedRootRefusesDotDot(t *testing.T) {
	root, raw := tree(t)
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(raw, alias))
	got, err := vaultfiles.ResolveInside(alias, "real/a.md")
	require.NoError(t, err)
	require.Equal(t, alias+"/real/a.md", got)
	_, err = vaultfiles.ResolveInside(alias, "real/../real/a.md")
	requireWireError(t, err, http.StatusBadRequest, escapes)
}

func TestResolveInsideRelativeRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	got, err := vaultfiles.ResolveInside("vault/raw", "a.md")
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(got), got)
	require.True(t, strings.HasSuffix(got, "/vault/raw/a.md"), got)
	_, err = vaultfiles.ResolveInside("vault/raw", "../a.md")
	requireWireError(t, err, http.StatusBadRequest, escapes)
}
