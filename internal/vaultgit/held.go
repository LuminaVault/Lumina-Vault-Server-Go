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
// the index and in every commit while their bytes never reach raw/. It lives
// inside .git, so it is never committed, and it survives a disconnect.
const heldFile = "lv-held"

func (r *Repo) heldPath() string { return filepath.Join(r.root, ".git", heldFile) }

// Held is the held paths, sorted. No file means none: behaviour is then
// exactly what it was before sync existed.
func (r *Repo) Held() ([]string, error) {
	b, err := os.ReadFile(r.heldPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vaultgit: read held paths: %w", err)
	}
	var out []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *Repo) heldSet() (map[string]struct{}, error) {
	paths, err := r.Held()
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return set, nil
}

// SetHeld replaces the held paths (one per line, written atomically). A pull
// writes the list for the remote tree before it stages anything (sync spec
// D10 step 8): a listed path that is not in the index is harmless, an
// unlisted one that is missing from raw/ would be committed as a deletion and
// pushed. A path with a newline cannot be listed and is refused.
func (r *Repo) SetHeld(paths []string) error {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	var b bytes.Buffer
	for _, p := range sorted {
		if p == "" || strings.ContainsAny(p, "\n\r") {
			return fmt.Errorf("vaultgit: held path %q cannot be listed", p)
		}
		b.WriteString(p + "\n")
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
