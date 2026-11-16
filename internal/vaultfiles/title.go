package vaultfiles

import (
	"lumina/internal/swiftstr"
	"strings"
)

// MarkdownTitle is title(from:fallback:): the first H1 among the file's first 21 nonempty
// lines, else the filename without its extension. Splitting on the Character "\n" leaves
// CRLF lines joined, and the last piece holds the rest of the file, as `maxSplits: 20` does.
// Bulk import and note upload both title notes with it.
func MarkdownTitle(content, fallback string) string {
	rest := content
	for pieces := 0; pieces < 21 && rest != ""; pieces++ {
		at := -1
		for start := 0; pieces < 20; {
			next := strings.IndexByte(rest[start:], '\n')
			if next < 0 {
				break
			}
			if next+start == 0 || rest[next+start-1] != '\r' {
				at = next + start
				break
			}
			start += next + 1
		}
		line := rest
		if at >= 0 {
			line, rest = rest[:at], rest[at+1:]
		} else {
			rest = ""
		}
		if line == "" {
			pieces--
			continue
		}
		t := swiftstr.TrimWhitespaces(line)
		if swiftstr.HasASCIIPrefix(t, "# ") {
			return swiftstr.TrimWhitespaces(t[2:])
		}
	}
	return DeleteExtension(fallback)
}

// DeleteExtension is `(name as NSString).deletingPathExtension` on Linux, for a single
// path component: "/" is "", and a trailing dot or a leading one is no extension.
func DeleteExtension(name string) string {
	if name == "/" {
		return ""
	}
	ext := PathExtension(name)
	if ext == "" {
		return name
	}
	return strings.TrimSuffix(name, "."+ext)
}
