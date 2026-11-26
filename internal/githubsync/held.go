package githubsync

import (
	"context"
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
// (spec D8). The empty reason means the path is materialized. IgnoreFile is
// neither.
type HeldReason string

const (
	Materialized HeldReason = ""
	// IgnoreFile is the root .gitignore, the one dot-file that lives in raw/
	// (D8). It is not held, and it is not materialized either: it never goes
	// through Files.Write, never gets a row and is never a conflict copy. A
	// pull takes the remote file as is and re-adds the managed block.
	IgnoreFile      HeldReason = "gitignore"
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
// with a ".git" segment is hidden before anything else is looked at. The
// root .gitignore, when it is a regular file within the limit, is IgnoreFile;
// a .gitignore in a folder is hidden like any other dot-file.
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
	if path == IgnorePath {
		if size > maxSize {
			return HeldTooLarge
		}
		return IgnoreFile
	}
	for _, s := range segments {
		// A dot-file, or a name the vault's own .gitignore keeps out of history
		// (the trash's _deleted_ prefix): written to raw/ it would be ignored by
		// the next commit, or purged with the trash.
		if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "_deleted_") {
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

// IgnorePath is the one dot-file sync keeps in raw/.
const IgnorePath = ".gitignore"

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
// A local file of another size cannot be the same blob, so it is counted
// without being read; only files of the remote's size are read and hashed.
// The root .gitignore adds to Bytes only: it is merged, not written as a
// vault file. ctx stops the walk.
func Summarize(ctx context.Context, entries []RemoteEntry, rawRoot string, maxSize int64) (Summary, error) {
	var s Summary
	for _, e := range entries {
		if e.Type == "tree" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		s.Bytes += e.Size
		switch Classify(e.Path, e.Mode, e.Size, maxSize) {
		case Materialized:
		case IgnoreFile:
			continue
		default:
			s.Held++
			continue
		}
		s.Files++
		if IsNotePath(e.Path) {
			s.Notes++
		}
		differs, err := localDiffers(rawRoot, e)
		if err != nil {
			return Summary{}, err
		}
		if differs {
			s.Conflicts++
		}
	}
	return s, nil
}

// localDiffers reports a vault file at e.Path whose bytes are not the remote
// blob's. No local file is no conflict.
func localDiffers(rawRoot string, e RemoteEntry) (bool, error) {
	target, err := vaultfiles.ResolveInside(rawRoot, e.Path)
	if err != nil {
		return false, nil //nolint:nilerr // a path the vault cannot hold has no local file
	}
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	if info.Size() != e.Size {
		return true, nil
	}
	data, err := os.ReadFile(target) //nolint:gosec // target comes from ResolveInside
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return BlobID(data) != e.SHA, nil
}

// BlobID is git's object id for a blob with these bytes.
func BlobID(data []byte) string {
	h := sha1.New() //nolint:gosec // git's object id, not a security hash
	_, _ = h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
