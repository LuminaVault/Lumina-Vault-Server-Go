package vaultfiles

import (
	"os"
	"path/filepath"
	"strings"
)

// Foundation's limits on Linux: MAXSYMLINKS from glibc, and PATH_MAX + 128.
const (
	maxSymlinks = 20
	maxPathSize = 4096 + 128
)

// ResolveInside is VaultController.resolveInside, the second half of the path
// traversal boundary. It appends relative to rawRoot, and answers 400 unless
// the standardized result is strictly below the standardized root.
//
// As in Swift, the returned path is the appended one, not the standardized
// one: "a/../b.md" comes back with its ".." and the OS resolves it physically.
//
// WEAKNESS, kept on purpose and tested: Swift's doc comment claims this defends
// against symlink escape, and it mostly doesn't, because standardizing a path
// only consults the filesystem when the path has a ".." component.
//
//   - "link/secret.md" with link -> /outside passes. A symlink planted inside
//     the vault can read and write outside it, and SanitizePath can't help:
//     "link/secret.md" is a clean path.
//   - "link/../sib.md" with link -> /outside is refused if /outside/../sib.md
//     exists. If it doesn't, symlink resolution gives up, the check falls back
//     to the lexical "raw/sib.md" and passes, and the returned path still
//     reaches /outside/../sib.md. A write through it lands outside the root.
//     SanitizePath refuses "..", so this needs a caller that skips it.
//
// Nothing here calls realpath, because Swift doesn't and both servers must
// agree while they share the volume. Fixing it is a decision for both servers.
func ResolveInside(rawRoot, relative string) (string, error) {
	// URL(fileURLWithPath:) makes a relative root absolute.
	if !filepath.IsAbs(rawRoot) {
		abs, err := filepath.Abs(rawRoot)
		if err != nil {
			return "", err
		}
		rawRoot = abs
	}
	target := appendingPath(rawRoot, relative)
	rootPrefix := standardizingPath(rawRoot) + "/"
	if !hasPrefix(standardizingPath(target), rootPrefix) {
		return "", badRequest("resolved path escapes vault root")
	}
	return target, nil
}

// appendingPath is URL.appendingPathComponent(_:).path: one slash at the join,
// no trailing slash, and everything else (".", "..", "//", a literal "%2F")
// left as it is. Swift percent-encodes on the way in and decodes on the way
// out, so percent signs stay literal.
func appendingPath(root, relative string) string {
	root = droppingTrailingSlashes(root)
	relative = strings.TrimRight(relative, "/")
	switch {
	case relative == "":
		return root
	case strings.HasPrefix(relative, "/"):
		return root + relative
	default:
		return root + "/" + relative
	}
}

// standardizingPath is the path of URL.standardizedFileURL (swift-foundation
// String+Path.swift, _standardizingPath). Tilde expansion is left out: every
// caller passes a path under an absolute root.
func standardizingPath(path string) string {
	result := droppingTrailingSlashes(compressingSlashes(path))
	if hasDotDotComponent(result) {
		if resolved, ok := resolvingSymlinks(result); ok {
			result = resolved
		}
	}
	result = removingDotSegments(result)
	return droppingTrailingSlashes(strippingAutomountPrefix(result))
}

func compressingSlashes(path string) string {
	var b strings.Builder
	previousSlash := false
	for i := range len(path) {
		if path[i] == '/' && previousSlash {
			continue
		}
		previousSlash = path[i] == '/'
		b.WriteByte(path[i])
	}
	return b.String()
}

func droppingTrailingSlashes(path string) string {
	if path == "" {
		return path
	}
	if trimmed := strings.TrimRight(path, "/"); trimmed != "" {
		return trimmed
	}
	return "/"
}

func hasDotDotComponent(path string) bool {
	for _, s := range strings.Split(path, "/") {
		if s == ".." {
			return true
		}
	}
	return false
}

// resolvingSymlinks is _resolvingSymlinksInPath on Linux. It only expands
// symlinks. ".." stays in the text for removingDotSegments to remove lexically,
// and any component that lstat can't see makes the whole thing fail, which
// sends the caller back to its unresolved path.
func resolvingSymlinks(path string) (string, bool) {
	if len(path) >= maxPathSize {
		return "", false
	}
	buffer := path
	scan, links := 0, 0
	for {
		componentStart := scan
		for scan < len(buffer) && buffer[scan] == '/' {
			scan++
		}
		for scan < len(buffer) && buffer[scan] != '/' {
			scan++
		}
		info, err := os.Lstat(buffer[:scan])
		if err != nil {
			return "", false
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if links > maxSymlinks {
				return "", false
			}
			if link, err = os.Readlink(buffer[:scan]); err != nil {
				return "", false
			}
		}
		if link == "" {
			if scan == len(buffer) {
				return buffer, true
			}
			continue
		}
		links++
		preserved := 0
		if link[0] != '/' {
			preserved = componentStart + 1
		}
		if preserved+len(link)+len(buffer)-scan >= maxPathSize {
			return "", false
		}
		buffer = buffer[:preserved] + link + buffer[scan:]
		scan = componentStart
		if link[0] == '/' {
			scan = 0
		}
	}
}

// removingDotSegments is RFC 3986 remove_dot_segments for an absolute path.
// Unlike path.Clean it leaves "//" alone, which a symlink target can introduce.
func removingDotSegments(path string) string {
	out := make([]string, 0, 8)
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, s := range segments {
		last := i == len(segments)-1
		switch s {
		case ".":
			if last {
				out = append(out, "")
			}
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, s)
		}
	}
	return "/" + strings.Join(out, "/")
}

// strippingAutomountPrefix is the end of _standardizingPath. It isn't behind
// an #if in swift-foundation, so it runs on Linux too, and on macOS it is why a
// temp directory under /private/var compares equal to /var.
func strippingAutomountPrefix(path string) string {
	for _, prefix := range []string{"/private/var/automount/", "/var/automount/", "/private/"} {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		stripped := path[len(prefix)-1:]
		for _, excluded := range []string{
			"/Applications", "/Library", "/System", "/Users", "/Volumes", "/bin", "/cores",
			"/dev", "/opt", "/private", "/sbin", "/usr",
		} {
			if stripped == excluded || strings.HasPrefix(stripped, excluded+"/") {
				return path
			}
		}
		if fileExists(stripped) {
			return stripped
		}
		return path
	}
	return path
}

// fileExists is FileManager.fileExists(atPath:): it follows symlinks, so a
// dangling symlink does not exist.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// moveItem is FileManager.moveItem(at:to:): unlike os.Rename, it refuses a
// destination that exists, a dangling symlink included. Foundation also falls
// back to copying on EXDEV; every caller here moves within one raw directory
// on one volume, so that branch isn't ported.
func moveItem(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return &os.LinkError{Op: "move", Old: src, New: dst, Err: os.ErrExist}
	}
	return os.Rename(src, dst)
}

// dirPerm is vaultpaths' mode: what Foundation's createDirectory leaves under
// the usual umask.
const dirPerm = 0o755

func createParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), dirPerm)
}
