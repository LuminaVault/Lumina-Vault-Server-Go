package capture

import (
	"regexp"
	"strings"
)

// MaxURLsPerExtraction caps ExtractURLs, so one chat turn full of URLs can't fan
// out hundreds of capture writes.
const MaxURLsPerExtraction = 32

// ExtractedURL is one link found in chat text.
type ExtractedURL struct {
	// Raw is url.absoluteString, which is what gets persisted.
	Raw string
	// Normalized is the dedupe key.
	Normalized string
}

// urlInTextRe is URLExtractionService's pattern. ICU's \s also takes the Unicode
// separators, which Go's doesn't, hence the \p{Z}.
var urlInTextRe = regexp.MustCompile("(?i)https?://[^\\s\\p{Z}()<>]+(?:\\([^\\s\\p{Z}()<>]+\\)|[^\\s\\p{Z}`!()\\[\\]{};:'\".,<>?«»“”‘’])")

// ExtractURLs is URLExtractionService.extract: the unique public http(s) URLs in
// text, in order, at most MaxURLsPerExtraction. Wave 5's chat auto-save calls it.
func ExtractURLs(text string) []ExtractedURL {
	if text == "" {
		return nil
	}
	var out []ExtractedURL
	seen := map[string]bool{}
	for _, match := range urlInTextRe.FindAllString(text, -1) {
		u, ok := ParseURL(match)
		if !ok || !IsPublic(u) {
			continue
		}
		normalized := u.Normalize()
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, ExtractedURL{Raw: u.Absolute, Normalized: normalized})
		if len(out) >= MaxURLsPerExtraction {
			break
		}
	}
	return out
}

// hostContains reports whether the lowercased host holds any of names.
func hostContains(u *URL, names ...string) bool {
	if u.Host == nil {
		return false
	}
	host := strings.ToLower(*u.Host)
	for _, name := range names {
		if strings.Contains(host, name) {
			return true
		}
	}
	return false
}
