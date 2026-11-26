package vaultgit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// heldFile lists the paths GitHub sync keeps in git only (sync spec D8): files
// a connected repository has that the vault cannot hold. Their entries stay in
// the index and in every commit while their bytes never reach raw/: a commit
// never removes a held path that is missing from raw/, and never takes a file
// that turned up at one (the entry GitHub has must not be shadowed). It lives
// inside .git, so it is never committed, and it survives a disconnect.
const heldFile = "lv-held"

func (r *Repo) heldPath() string { return filepath.Join(r.root, ".git", heldFile) }

// Held is the held paths, sorted. No file means none: behaviour is then
// exactly what it was before sync existed.
func (r *Repo) Held() ([]string, error) { return readHeld(r.heldPath()) }

func readHeld(file string) ([]string, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the vault's own .git plus a constant
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vaultgit: read held paths: %w", err)
	}
	// One path per line, or, when the list has a NUL, NUL-separated: git
	// allows a newline in a path, and one such remote file must not fail
	// every pull.
	sep := "\n"
	if bytes.IndexByte(b, 0) >= 0 {
		sep = "\x00"
	}
	var out []string
	for line := range strings.SplitSeq(string(b), sep) {
		if line != "" {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out, nil
}

// HeldAt reports the held path that stands in the way of a vault file at
// path (slash-separated, relative to rawRoot): a held entry that is one of
// path's folders (GitHub has a link, a submodule or a file named like the
// folder), or one that lies inside path as if path were a folder. Git cannot
// hold a file and a folder under one name, so a file written there could
// never be committed beside the entry. "" is none. A held entry at path
// itself is not reported: such a file is simply never committed (Held), and
// a pull writes there when the entry became one the vault can hold.
//
// It reads one small file and no repository: a vault that never synced has
// no list, and this is then one failed open.
func HeldAt(rawRoot, path string) (string, error) {
	held, err := readHeld(filepath.Join(rawRoot, ".git", heldFile))
	if err != nil || len(held) == 0 {
		return "", err
	}
	return newHeldPaths(held).at(path), nil
}

// heldPaths answers, for one list, whether a path is held and whether a held
// path stands in its way (HeldAt).
type heldPaths struct {
	set   map[string]struct{}
	above map[string]string // every folder of a held path -> that path
}

func newHeldPaths(paths []string) *heldPaths {
	h := &heldPaths{set: make(map[string]struct{}, len(paths)), above: map[string]string{}}
	for _, p := range paths {
		h.set[p] = struct{}{}
		for i := strings.IndexByte(p, '/'); i >= 0; {
			h.above[p[:i]] = p
			next := strings.IndexByte(p[i+1:], '/')
			if next < 0 {
				break
			}
			i += 1 + next
		}
	}
	return h
}

func (h *heldPaths) has(path string) bool {
	if h == nil {
		return false
	}
	_, ok := h.set[path]
	return ok
}

// at is HeldAt's answer for path.
func (h *heldPaths) at(path string) string {
	if h == nil {
		return ""
	}
	if p, ok := h.above[path]; ok {
		return p
	}
	for i := strings.IndexByte(path, '/'); i >= 0; {
		if _, ok := h.set[path[:i]]; ok {
			return path[:i]
		}
		next := strings.IndexByte(path[i+1:], '/')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return ""
}

func (r *Repo) heldPaths() (*heldPaths, error) {
	paths, err := r.Held()
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	return newHeldPaths(paths), nil
}

// SetHeld replaces the held paths (written atomically). A pull writes the
// list for the remote tree before it stages anything (sync spec D10 step 8):
// a listed path that is not in the index is harmless, an unlisted one that is
// missing from raw/ would be committed as a deletion and pushed. Only an
// empty path or one with a NUL, which git cannot hold either, is refused.
//
// The list is one path per line, the format every release has read, unless a
// path has a newline in it: only then is it NUL-separated. So a binary from
// before the NUL format, put back by a rollback, still reads every list a
// vault without such a path has, instead of taking the whole file for one
// path and committing every held entry as a deletion.
func (r *Repo) SetHeld(paths []string) error {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	sep := "\n"
	for _, p := range sorted {
		if p == "" || strings.ContainsRune(p, 0) {
			return fmt.Errorf("vaultgit: held path %q cannot be listed", p)
		}
		if strings.Contains(p, "\n") {
			sep = "\x00"
		}
	}
	var b bytes.Buffer
	for _, p := range sorted {
		b.WriteString(p + sep)
	}
	tmp := r.heldPath() + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return fmt.Errorf("vaultgit: write held paths: %w", err)
	}
	if err := os.Rename(tmp, r.heldPath()); err != nil {
		return fmt.Errorf("vaultgit: write held paths: %w", err)
	}
	return nil
}
