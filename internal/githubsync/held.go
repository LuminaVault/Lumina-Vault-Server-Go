package githubsync

import (
	"crypto/sha1" //nolint:gosec // git's object id, not a security hash
	"encoding/hex"
	"errors"
	"io/fs"
	"lumina/internal/vaultfiles"
	"os"
	"strconv"
	"strings"
)

// HeldReason says why a remote path stays in git and never reaches raw/
// (spec D8). The empty reason means the path is materialized.
type HeldReason string

const (
	Materialized    HeldReason = ""
	HeldNotAFile    HeldReason = "not_a_file"
	HeldHidden      HeldReason = "hidden"
	HeldUnsupported HeldReason = "unsupported_type"
	HeldBadName     HeldReason = "unsupported_name"
	HeldTooLarge    HeldReason = "too_large"
)

// Git tree modes of a regular file.
const (
	modeFile       = "100644"
	modeExecutable = "100755"
)

// Classify applies D8's rule to one entry of a remote tree: a path is
// materialized only when it is a regular file, no segment starts with ".",
// SanitizePath returns it unchanged and it is at most maxSize bytes. A path
// with a ".git" segment is hidden before anything else is looked at.
func Classify(path, mode string, size, maxSize int64) HeldReason {
	segments := strings.Split(path, "/")
	for _, s := range segments {
		if strings.EqualFold(s, ".git") {
			return HeldHidden
		}
	}
	if mode != modeFile && mode != modeExecutable {
		return HeldNotAFile
	}
	for _, s := range segments {
		if strings.HasPrefix(s, ".") {
			return HeldHidden
		}
	}
	safe, err := vaultfiles.SanitizePath(path)
	if err != nil || safe != path {
		ext := vaultfiles.Lowercased(vaultfiles.PathExtension(path))
		if _, ok := vaultfiles.ContentTypeForExtension(ext); !ok {
			return HeldUnsupported
		}
		return HeldBadName
	}
	if size > maxSize {
		return HeldTooLarge
	}
	return Materialized
}

// IsNotePath is the vault's note rule: Markdown and plain text.
func IsNotePath(path string) bool {
	switch vaultfiles.Lowercased(vaultfiles.PathExtension(path)) {
	case "md", "markdown", "txt":
		return true
	}
	return false
}

// Summary is what a first connect to a non-empty repository would do
// (D11's 409 remote_not_empty body).
type Summary struct {
	// Files is how many remote files would be written to the vault.
	Files int `json:"files"`
	// Notes is the part of Files that are notes.
	Notes int `json:"notes"`
	// Conflicts is how many of those exist in the vault with other bytes, and
	// would be kept twice.
	Conflicts int `json:"conflicts"`
	// Held is how many remote files stay on GitHub only (D8).
	Held int `json:"held"`
	// Bytes is the size of every remote file.
	Bytes int64 `json:"bytes"`
}

// RemoteEntry is one entry of a remote tree, as Summarize reads it.
type RemoteEntry struct {
	Path string
	Mode string
	Type string // blob | tree | commit
	SHA  string
	Size int64
}

// Summarize counts a remote tree against the vault at rawRoot. A local file
// whose git blob id equals the remote one is the same file, not a conflict.
func Summarize(entries []RemoteEntry, rawRoot string, maxSize int64) (Summary, error) {
	var s Summary
	for _, e := range entries {
		if e.Type == "tree" {
			continue
		}
		s.Bytes += e.Size
		if Classify(e.Path, e.Mode, e.Size, maxSize) != Materialized {
			s.Held++
			continue
		}
		s.Files++
		if IsNotePath(e.Path) {
			s.Notes++
		}
		local, err := localBlobID(rawRoot, e.Path)
		if err != nil {
			return Summary{}, err
		}
		if local != "" && local != e.SHA {
			s.Conflicts++
		}
	}
	return s, nil
}

// localBlobID is the git blob id of the vault file at path, or "" when there
// is none.
func localBlobID(rawRoot, path string) (string, error) {
	target, err := vaultfiles.ResolveInside(rawRoot, path)
	if err != nil {
		return "", nil //nolint:nilerr // a path the vault cannot hold has no local file
	}
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	data, err := os.ReadFile(target) //nolint:gosec // target comes from ResolveInside
	if err != nil {
		return "", err
	}
	return BlobID(data), nil
}

// BlobID is git's object id for a blob with these bytes.
func BlobID(data []byte) string {
	h := sha1.New() //nolint:gosec // git's object id, not a security hash
	_, _ = h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
