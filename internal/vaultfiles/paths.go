// Package vaultfiles holds the vault file helpers every wave 4 module shares:
// the Go port of the static helpers on LuminaVaultServer
// Sources/App/Vault/VaultController.swift, Models/VaultFile.swift and
// Vault/VaultFileMover.swift.
//
// Both servers write the same tree and the same vault_files rows while the
// migration runs, so this reproduces Swift's behaviour, quirks included. Swift
// was read at fdbed9b0 (image in production), and Foundation at swift-6.3-RELEASE,
// the toolchain of its Dockerfile. Production is Linux, so `x as NSString` means
// swift-corelibs-foundation and URL means swift-foundation; Darwin differs in
// places (noted where it matters).
package vaultfiles

import (
	"fmt"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"unicode"
)

// maxPathCharacters is sanitizePath's limit, in Swift Characters.
const maxPathCharacters = 512

// allowedExtensions is VaultController.allowedExtensions: file extension to
// the content types it accepts.
var allowedExtensions = map[string][]string{
	"md":       {"text/markdown", "text/x-markdown", "text/plain"},
	"markdown": {"text/markdown", "text/x-markdown", "text/plain"},
	"txt":      {"text/plain"},
	"png":      {"image/png"},
	"jpg":      {"image/jpeg"},
	"jpeg":     {"image/jpeg"},
	"webp":     {"image/webp"},
	"gif":      {"image/gif"},
	"heic":     {"image/heic", "image/heif"},
	"heif":     {"image/heif", "image/heic"},
	"pdf":      {"application/pdf"},
	"mp3":      {"audio/mpeg", "audio/mp3"},
	"m4a":      {"audio/mp4", "audio/x-m4a"},
	"aac":      {"audio/aac"},
	"wav":      {"audio/wav", "audio/x-wav"},
	"flac":     {"audio/flac", "audio/x-flac"},
	"mp4":      {"video/mp4"},
	"mov":      {"video/quicktime"},
	"webm":     {"video/webm"},
}

// SanitizePath is VaultController.sanitizePath, the first half of the path
// traversal boundary: a client path becomes a relative path whose segments are
// all letters, marks, numbers, ".", "_" or "-", with an allowed extension. The
// checks run in Swift's order, because each has its own message on the wire.
//
// It sees the path as Swift's handler does, so a route must not decode it. A
// query parameter arrives decoded (wire.QueryValue). A catch-all path parameter
// arrives raw, because Hummingbird's router never decodes (wire.RawPath), and
// "%" is an illegal character here: "inbox/caf%C3%A9.md" is a 400 even though
// upload can store "inbox/café.md".
func SanitizePath(raw string) (string, error) {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(raw)
	if trimmed == "" || swiftstr.Count(trimmed) > maxPathCharacters {
		return "", badRequest("path empty or too long")
	}
	if hasPrefix(trimmed, "/") {
		return "", badRequest("path must be relative")
	}
	segments := splitCharacter(trimmed, "/")
	for _, s := range segments {
		if s == "" || s == "." || s == ".." {
			return "", badRequest("path contains illegal segment")
		}
	}
	for _, s := range segments {
		if strings.IndexFunc(s, notAllowedInSegment) >= 0 {
			return "", badRequest("path contains illegal characters")
		}
	}
	ext := Lowercased(PathExtension(trimmed))
	if _, ok := allowedExtensions[ext]; !ok {
		return "", unsupportedExtension(ext)
	}
	return trimmed, nil
}

// ValidateContentType is VaultController.validateContentType. Parameters after
// ";" are ignored. ext is expected lowercase, as Lowercased(PathExtension(p)).
func ValidateContentType(contentType, ext string) error {
	allowed, ok := allowedExtensions[ext]
	if !ok {
		return unsupportedExtension(ext)
	}
	mime := Lowercased(contentType)
	// Swift's split drops empty pieces, so ";text/plain" is text/plain and ";"
	// has no first piece and stays as it is.
	for _, piece := range splitCharacter(contentType, ";", "\u037e") {
		if piece != "" {
			mime = Lowercased(strings.TrimFunc(piece, isWhitespace))
			break
		}
	}
	for _, a := range allowed {
		if a == mime {
			return nil
		}
	}
	return wire.NewError(http.StatusUnsupportedMediaType, fmt.Sprintf("Content-Type `%s` not allowed for `.%s`", mime, ext))
}

// PathExtension is `(path as NSString).pathExtension` on Linux
// (swift-corelibs-foundation NSPathUtilities). Note that "..md" has the
// extension "md" here and "" on Darwin; ".md" has none on either.
func PathExtension(path string) string {
	g := graphemes(fixingSlashes(path))
	if utf16Length(g) <= 1 {
		return ""
	}
	start := lastComponentStart(g)
	for i := len(g) - 1; i >= start; i-- {
		if g[i] != "." {
			continue
		}
		if i == start || (i == len(g)-1 && i-1 == start && g[i-1] == ".") {
			return ""
		}
		return strings.Join(g[i+1:], "")
	}
	return ""
}

// LastPathComponent is `(path as NSString).lastPathComponent` on Linux.
func LastPathComponent(path string) string {
	fixed := fixingSlashes(path)
	g := graphemes(fixed)
	if utf16Length(g) <= 1 {
		return fixed
	}
	return strings.Join(g[lastComponentStart(g):], "")
}

// Lowercased is Swift's String.lowercased(): the standard library maps each
// scalar with its full lowercase mapping and no context. Go's strings.ToLower
// uses the simple mapping, which differs only for U+0130: "g\u0130f" must not
// become the allowed extension "gif".
func Lowercased(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\u0130", "i\u0307"))
}

func badRequest(message string) *wire.Error {
	return wire.NewError(http.StatusBadRequest, message)
}

func unsupportedExtension(ext string) *wire.Error {
	return badRequest("unsupported file extension `" + ext + "`")
}

// notAllowedInSegment is the complement of CharacterSet.alphanumerics plus
// "._-". Foundation documents alphanumerics as the General Categories L*, M*
// and N*, so non-ASCII letters and combining marks are legal.
//
// Measured against Foundation on Darwin (Swift 6.4.1) with Go's Unicode 17
// tables: Foundation's set is a subset of this one, and 6,227 code points are
// only here, all letters, marks or numbers by Go's tables. So this can accept a
// name in a recent script that Swift refuses, but no dot or separator. Linux's
// CoreFoundation tables, the ones production uses, were not measured.
func notAllowedInSegment(r rune) bool {
	if r == '.' || r == '_' || r == '-' {
		return false
	}
	return !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsNumber(r)
}

// isWhitespace is CharacterSet.whitespaces as CoreFoundation implements it:
// swiftstr.IsWhitespaceOrNewline less the newlines.
func isWhitespace(r rune) bool {
	switch {
	case r >= '\n' && r <= '\r', r == 0x85, r == 0x2028, r == 0x2029:
		return false
	default:
		return swiftstr.IsWhitespaceOrNewline(r)
	}
}

// graphemes splits s into Swift Characters.
func graphemes(s string) []string {
	var out []string
	for s != "" {
		c := swiftstr.Prefix(s, 1)
		out = append(out, c)
		s = s[len(c):]
	}
	return out
}

// hasPrefix is Swift's String.hasPrefix, which compares Characters: "/\u0301a"
// does not start with "/", because that slash is part of a larger Character.
func hasPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	rest := 0
	for rest < len(prefix) {
		rest += len(swiftstr.Prefix(s[rest:], 1))
	}
	return rest == len(prefix)
}

// splitCharacter is `s.split(separator:, omittingEmptySubsequences: false)` on
// a Character. Swift compares Characters by canonical equivalence, which is why
// ValidateContentType passes U+037E, canonically ";", as a second separator.
func splitCharacter(s string, separators ...string) []string {
	out := []string{}
	var b strings.Builder
	for _, c := range graphemes(s) {
		isSeparator := false
		for _, sep := range separators {
			isSeparator = isSeparator || c == sep
		}
		if isSeparator {
			out = append(out, b.String())
			b.Reset()
			continue
		}
		b.WriteString(c)
	}
	return append(out, b.String())
}

// fixingSlashes is corelibs' _stringByFixingSlashes(): runs of "/" compress to
// one and one trailing "/" goes, unless the path is only "/".
func fixingSlashes(path string) string {
	var b strings.Builder
	previousSlash := false
	for _, c := range graphemes(path) {
		if c == "/" && previousSlash {
			continue
		}
		previousSlash = c == "/"
		b.WriteString(c)
	}
	fixed := b.String()
	if len(fixed) > 1 && hasSuffixCharacter(fixed, "/") {
		return fixed[:len(fixed)-1]
	}
	return fixed
}

func hasSuffixCharacter(s, c string) bool {
	g := graphemes(s)
	return len(g) > 0 && g[len(g)-1] == c
}

// lastComponentStart is corelibs' _startOfLastPathComponent.
func lastComponentStart(g []string) int {
	for i := len(g) - 1; i >= 0; i-- {
		if g[i] == "/" {
			return i + 1
		}
	}
	return 0
}

// utf16Length is corelibs' String.length, used only to compare with 1.
func utf16Length(g []string) int {
	n := 0
	for _, c := range g {
		for _, r := range c {
			n++
			if r >= 0x10000 {
				n++
			}
		}
	}
	return n
}
