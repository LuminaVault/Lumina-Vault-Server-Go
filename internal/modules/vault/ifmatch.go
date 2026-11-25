package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"io/fs"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Backlog E5.1: a conflict-safe save. An upload that carries If-Match only
// replaces the file when the bytes on disk still hash to one of the listed
// sha256 values. A stale save gets 412 and the client reloads instead of
// silently overwriting another device's edit. Without the header, an upload
// overwrites as before.

// ErrPreconditionFailed is the 412 a stale save, move or delete gets. Callers
// with no request (GitHub sync) test for it with errors.Is.
var ErrPreconditionFailed = wire.NewError(http.StatusPreconditionFailed,
	"the file changed since you opened it; reload it and save again")

// parseIfMatch splits an If-Match value into the sha256 values it lists.
// Entity tags may be quoted ("abc") or bare (abc); a weak tag (W/"abc") never
// matches, because a save needs the exact bytes. wildcard reports "*".
func parseIfMatch(header string) (tags []string, wildcard bool) {
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		switch {
		case tag == "":
			continue
		case tag == "*":
			wildcard = true
		case strings.HasPrefix(tag, "W/"):
			continue
		default:
			tags = append(tags, strings.ToLower(strings.Trim(tag, `"`)))
		}
	}
	return tags, wildcard
}

// checkIfMatch compares the If-Match header against the file at target. The
// file on disk is the truth, not the row: a writer that bypasses the row
// (imports, todos) still counts as a change. A missing file fails every
// If-Match, "*" included (RFC 9110 13.1.1).
func checkIfMatch(header, target string) error {
	tags, anyTag := parseIfMatch(header)
	current, err := os.ReadFile(target) //nolint:gosec // target comes from ResolveInside
	if errors.Is(err, fs.ErrNotExist) {
		return ErrPreconditionFailed
	}
	if err != nil {
		return err
	}
	if anyTag {
		return nil
	}
	sum := sha256.Sum256(current)
	digest := hex.EncodeToString(sum[:])
	for _, tag := range tags {
		if tag == digest {
			return nil
		}
	}
	return ErrPreconditionFailed
}

// checkAbsent is `If-None-Match: *`: 412 when the file already exists.
func checkAbsent(target string) error {
	_, err := os.Stat(target)
	if err == nil {
		return ErrPreconditionFailed
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// pathLocks serializes the check-and-write of one file, so two saves with the
// same If-Match can't both pass the check. Locks are striped by tenant and
// path: two files sharing a stripe only wait for each other, which is cheap.
type pathLocks struct {
	stripes [64]sync.Mutex
}

func (l *pathLocks) stripe(tenant uuid.UUID, path string) int {
	h := fnv.New32a()
	_, _ = h.Write(tenant[:])
	_, _ = h.Write([]byte(path))
	return int(h.Sum32() % uint32(len(l.stripes)))
}

func (l *pathLocks) lock(tenant uuid.UUID, path string) func() {
	mu := &l.stripes[l.stripe(tenant, path)]
	mu.Lock()
	return mu.Unlock
}

// lock2 holds the stripes of two paths, for a move. The stripes are taken in
// index order, and once when both paths share one, so two moves in opposite
// directions can't deadlock and a move can't wait for itself.
func (l *pathLocks) lock2(tenant uuid.UUID, a, b string) func() {
	i, j := l.stripe(tenant, a), l.stripe(tenant, b)
	if i == j {
		l.stripes[i].Lock()
		return l.stripes[i].Unlock
	}
	if i > j {
		i, j = j, i
	}
	l.stripes[i].Lock()
	l.stripes[j].Lock()
	return func() {
		l.stripes[j].Unlock()
		l.stripes[i].Unlock()
	}
}
