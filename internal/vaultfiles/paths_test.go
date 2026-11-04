package vaultfiles_test

import (
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireWireError(t *testing.T, err error, status int, message string) {
	t.Helper()
	var we *wire.Error
	require.ErrorAs(t, err, &we)
	require.Equal(t, status, we.Status)
	require.Equal(t, message, we.Message)
}

func TestSanitizePathAccepts(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"a.md", "a.md"},
		{"inbox/a.md", "inbox/a.md"},
		{"deep/er/still/a.md", "deep/er/still/a.md"},
		{"a-b_c.d.md", "a-b_c.d.md"},
		{"a.MD", "a.MD"},                       // the extension check lowercases, the result keeps its case
		{"  a.md\n", "a.md"},                   // whitespace is trimmed, not rejected
		{"\u200ba.md\u3000", "a.md"},           // Foundation counts U+200B as whitespace
		{"日本語/ノート.md", "日本語/ノート.md"},           // alphanumerics is Unicode letters...
		{"\u0661\u0662.md", "\u0661\u0662.md"}, // ...numbers...
		{"e\u0301.md", "e\u0301.md"},           // ...and marks
		{"\u0301x.md", "\u0301x.md"},           // a leading mark passes here; ResolveInside refuses it
		{"..md", "..md"},                       // a plain name on Linux (Darwin sees no extension)
		{"a..md", "a..md"},                     //
		{"...md", "...md"},                     // not a dot segment
		{"a.mar\u212adown", "a.mar\u212adown"}, // the Kelvin sign lowercases to k
		{"_deleted_1_a.md", "_deleted_1_a.md"}, // the trash prefix isn't reserved
	}
	for _, c := range cases {
		got, err := vaultfiles.SanitizePath(c.raw)
		require.NoError(t, err, "%+q", c.raw)
		require.Equal(t, c.want, got, "%+q", c.raw)
	}
	for ext := range map[string]bool{
		"md": true, "markdown": true, "txt": true, "png": true, "jpg": true, "jpeg": true, "webp": true,
		"gif": true, "heic": true, "heif": true, "pdf": true, "mp3": true, "m4a": true, "aac": true,
		"wav": true, "flac": true, "mp4": true, "mov": true, "webm": true,
	} {
		_, err := vaultfiles.SanitizePath("x." + ext)
		require.NoError(t, err, ext)
		_, err = vaultfiles.SanitizePath("x." + strings.ToUpper(ext))
		require.NoError(t, err, ext)
	}
}

func TestSanitizePathLengthCountsCharacters(t *testing.T) {
	const ext = ".md" // 3 Characters
	exact := strings.Repeat("a", 509) + ext
	got, err := vaultfiles.SanitizePath(exact)
	require.NoError(t, err)
	require.Equal(t, exact, got)

	_, err = vaultfiles.SanitizePath("a" + exact)
	requireWireError(t, err, http.StatusBadRequest, "path empty or too long")

	// 509 Characters of two scalars each: 1018 code points, still 512 Characters.
	marks := strings.Repeat("e\u0301", 509) + ext
	_, err = vaultfiles.SanitizePath(marks)
	require.NoError(t, err)

	// The length is checked after trimming.
	_, err = vaultfiles.SanitizePath("   " + exact + "   ")
	require.NoError(t, err)
}

func TestSanitizePathRejects(t *testing.T) {
	const (
		tooLong = "path empty or too long"
		rel     = "path must be relative"
		segment = "path contains illegal segment"
		chars   = "path contains illegal characters"
	)
	cases := []struct {
		name, raw, message string
	}{
		{"empty", "", tooLong},
		{"only spaces", " \t\n", tooLong},
		{"only ZWSP", "\u200b", tooLong},

		{"absolute", "/etc/passwd.md", rel},
		{"absolute double", "//etc/passwd.md", rel},
		{"absolute after trim", "  /etc/passwd.md", rel},
		{"root", "/", rel},

		{"parent", "../x.md", segment},
		{"parent only", "..", segment},
		{"parent deep", "../../../etc/passwd.md", segment},
		{"parent middle", "a/../b.md", segment},
		{"parent end", "a/..", segment},
		{"parent after trim", "\t../x.md", segment},
		{"dot", ".", segment},
		{"dot start", "./a.md", segment},
		{"dot middle", "a/./b.md", segment},
		{"empty segment", "a//b.md", segment},
		{"trailing slash", "a.md/", segment},
		// Segments are judged before characters.
		{"segment beats characters", "a b/../c.md", segment},
		{"segment beats extension", "../x.exe", segment},

		{"percent slash", "..%2f..%2fetc%2fpasswd.md", chars},
		{"percent slash upper", "a%2Fb.md", chars},
		{"percent dots", "%2e%2e/x.md", chars},
		{"percent backslash", "..%5cx.md", chars},
		{"double encoded", "%252e%252e%252fx.md", chars},
		{"percent nul", "a%00.md", chars},
		{"nul", "a\x00.md", chars},
		{"backslash", "..\\x.md", chars},
		{"backslash separator", "a\\b.md", chars},
		{"fullwidth", "\uff0e\uff0e\uff0fx.md", chars},
		{"fullwidth slash", "a\uff0fb.md", chars},
		{"division slash", "a\u2215b.md", chars},
		{"fraction slash", "a\u2044b.md", chars},
		{"one dot leader", "\u2024\u2024/x.md", chars},
		{"tilde", "~/x.md", chars},
		{"space", "a b.md", chars},
		{"newline inside", "a\nb.md", chars},
		{"colon", "a:b.md", chars},
		{"invalid UTF-8", "a\xff.md", chars},
		{"overlong slash", "a\xc0\xafb.md", chars},
		// A slash that a combining mark joins isn't Swift's "/" Character: no
		// "must be relative" and no split, but the scalar itself is still illegal.
		{"slash with a mark, leading", "/\u0301x.md", chars},
		{"slash with a mark, middle", "a/\u0301b.md", chars},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := vaultfiles.SanitizePath(c.raw)
			requireWireError(t, err, http.StatusBadRequest, c.message)
			require.Empty(t, got)
		})
	}
}

func TestSanitizePathRejectsExtensions(t *testing.T) {
	cases := []struct{ raw, ext string }{
		{"a", ""},
		{"a.", ""},
		{".md", ""}, // a dotfile has no extension
		{"dir/.md", ""},
		{"a.md/b", ""}, // only the last component counts
		{"a.exe", "exe"},
		{"a.EXE", "exe"}, // the message has it lowercased
		{"a.md.exe", "exe"},
		{"a.tar.gz", "gz"},
		{"x.m\u0301d", "m\u0301d"},
		{"x.\u0301md", ""},          // that dot is part of a larger Character
		{"a.g\u0130f", "gi\u0307f"}, // full case mapping: not "gif"
	}
	for _, c := range cases {
		_, err := vaultfiles.SanitizePath(c.raw)
		requireWireError(t, err, http.StatusBadRequest, "unsupported file extension `"+c.ext+"`")
	}
}

func TestPathExtensionAndLastComponent(t *testing.T) {
	cases := []struct{ path, ext, last string }{
		{"", "", ""},
		{"/", "", "/"},
		{"a", "", "a"},
		{"a.md", "md", "a.md"},
		{"a.b.MD", "MD", "a.b.MD"},
		{".md", "", ".md"},
		{"..md", "md", "..md"}, // corelibs; Darwin says ""
		{"..", "", ".."},
		{"a.", "", "a."},
		{"a..md", "md", "a..md"},
		{".a.md", "md", ".a.md"},
		{"dir/.md", "", ".md"},
		{"dir/x", "", "x"},
		{"dir.md/x", "", "x"},
		{"a.md/", "md", "a.md"},
		{"a//b.md", "md", "b.md"},
		{"/a/b.png", "png", "b.png"},
		{"x.\u0301md", "", "x.\u0301md"}, // corelibs compares Characters; Darwin says "\u0301md"
	}
	for _, c := range cases {
		require.Equal(t, c.ext, vaultfiles.PathExtension(c.path), "extension of %+q", c.path)
		require.Equal(t, c.last, vaultfiles.LastPathComponent(c.path), "last component of %+q", c.path)
	}
}

func TestValidateContentTypeAcceptsEveryPair(t *testing.T) {
	pairs := map[string][]string{
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
	for ext, types := range pairs {
		for _, ct := range types {
			require.NoError(t, vaultfiles.ValidateContentType(ct, ext), "%s %s", ct, ext)
		}
	}
}

func TestValidateContentTypeForms(t *testing.T) {
	for _, ct := range []string{
		"text/markdown; charset=utf-8",
		"text/markdown;charset=utf-8;x=y",
		"TEXT/Markdown",
		"  text/markdown  ",
		"\ttext/markdown\t;x",
		";text/markdown",            // Swift's split drops the empty piece
		";;text/markdown;",          //
		"\u037etext/markdown",       // U+037E is canonically ";"
		"\u200btext/markdown\u00a0", // CharacterSet.whitespaces has both
	} {
		require.NoError(t, vaultfiles.ValidateContentType(ct, "md"), "%+q", ct)
	}
}

func TestValidateContentTypeRejects(t *testing.T) {
	cases := []struct{ contentType, ext, mime string }{
		{"image/jpeg", "heic", "image/jpeg"}, // VaultCRUDTests "upload rejects image_jpeg content type for .heic path"
		{"text/plain", "png", "text/plain"},
		{"IMAGE/GIF; a=b", "png", "image/gif"},
		{"application/octet-stream", "md", "application/octet-stream"}, // what a missing header becomes
		{"", "md", ""},
		{";", "md", ";"},                       // no first piece: the whole value, lowercased
		{" ;text/plain", "md", ""},             // the first piece is " ", which trims to nothing
		{"\ntext/plain", "md", "\ntext/plain"}, // .whitespaces has no newlines
		{"appl\u0130cation/pdf", "pdf", "appli\u0307cation/pdf"},
	}
	for _, c := range cases {
		err := vaultfiles.ValidateContentType(c.contentType, c.ext)
		requireWireError(t, err, http.StatusUnsupportedMediaType, "Content-Type `"+c.mime+"` not allowed for `."+c.ext+"`")
	}
	requireWireError(t, vaultfiles.ValidateContentType("text/plain", "exe"), http.StatusBadRequest, "unsupported file extension `exe`")
	requireWireError(t, vaultfiles.ValidateContentType("text/plain", ""), http.StatusBadRequest, "unsupported file extension ``")
}

func TestMoveDestination(t *testing.T) {
	slug := "work"
	got, err := vaultfiles.MoveDestination("inbox/deep/a.md", &slug)
	require.NoError(t, err)
	require.Equal(t, "work/a.md", got)

	got, err = vaultfiles.MoveDestination("work/a.md", nil)
	require.NoError(t, err)
	require.Equal(t, "inbox/a.md", got)

	bad := "../up"
	_, err = vaultfiles.MoveDestination("a.md", &bad)
	requireWireError(t, err, http.StatusBadRequest, "path contains illegal segment")
}
