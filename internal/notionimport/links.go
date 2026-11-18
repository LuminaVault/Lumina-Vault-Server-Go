package notionimport

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// LinkStats counts what RewriteLinks did with a page's links.
type LinkStats struct {
	// Resolved links pointed into the export and were rewritten.
	Resolved int
	// External links have a scheme (https, mailto …) and were left alone.
	External int
	// Unresolved links are relative but match nothing in the export.
	Unresolved int
	// UnresolvedTargets are those targets, decoded, in order.
	UnresolvedTargets []string
}

var (
	// mdLink is an inline Markdown link or image. The target may hold one
	// level of parentheses; Notion percent-encodes spaces.
	mdLink    = regexp.MustCompile(`(!?)\[((?:[^\[\]]|\[[^\[\]]*\])*)\]\(\s*<?((?:[^()\s<>]|\([^()\s]*\))+)>?(?:\s+"[^"]*")?\s*\)`)
	hasScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	dashedID  = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// RewriteLinks rewrites the links of the page at entry path from (spec D4):
//
//   - a relative link to an exported page becomes [[<vault path without .md>|text]];
//   - a notion.so link whose id is an exported page becomes the same;
//   - a link to an exported attachment keeps its Markdown form with the new
//     path, relative to the page's new folder;
//   - a link to a database's CSV keeps its Markdown form and points at the
//     database's folder (there is no note to link to);
//   - anything else is left as written.
//
// Fenced code is skipped.
func RewriteLinks(body, from string, m *Mapping) (string, LinkStats) {
	var stats LinkStats
	fromDir := m.Slug
	if to, ok := m.New[from]; ok {
		fromDir = path.Dir(to)
	}
	lines := strings.Split(body, "\n")
	fence := ""
	for at, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			fence = "```"
			continue
		}
		if strings.HasPrefix(trimmed, "~~~") {
			fence = "~~~"
			continue
		}
		if !strings.Contains(line, "](") {
			continue
		}
		lines[at] = mdLink.ReplaceAllStringFunc(line, func(match string) string {
			parts := mdLink.FindStringSubmatch(match)
			return rewriteOne(match, parts[1], parts[2], parts[3], from, fromDir, m, &stats)
		})
	}
	return strings.Join(lines, "\n"), stats
}

func rewriteOne(match, bang, text, target, from, fromDir string, m *Mapping, stats *LinkStats) string {
	if strings.HasPrefix(target, "#") {
		return match
	}
	old := ""
	if hasScheme.MatchString(target) {
		if id := notionID(target); id != "" {
			old, _ = m.PageByID(id)
		}
		if old == "" {
			stats.External++
			return match
		}
	} else {
		clean := target
		if i := strings.IndexAny(clean, "#?"); i >= 0 {
			clean = clean[:i]
		}
		decoded, err := url.PathUnescape(clean)
		if err != nil {
			decoded = clean
		}
		if decoded == "" {
			return match
		}
		candidate := path.Join(path.Dir(from), decoded)
		if _, ok := m.New[candidate]; ok {
			old = candidate
		} else if id := anyID.FindString(path.Base(decoded)); id != "" && KindOf(decoded) == KindPage {
			// The page moved in the export, or the link is to another copy of it.
			old, _ = m.PageByID(id)
		}
		if old == "" {
			stats.Unresolved++
			stats.UnresolvedTargets = append(stats.UnresolvedTargets, decoded)
			return match
		}
	}
	to := m.New[old]
	stats.Resolved++
	switch KindOf(old) {
	case KindPage:
		label := strings.ReplaceAll(strings.ReplaceAll(text, "|", "-"), "]]", "]")
		link := strings.TrimSuffix(to, ".md")
		if strings.TrimSpace(label) == "" {
			return "[[" + link + "]]"
		}
		return "[[" + link + "|" + label + "]]"
	case KindCSV:
		return "[" + text + "](" + relativeURL(fromDir, to) + "/)"
	}
	return bang + "[" + text + "](" + relativeURL(fromDir, to) + ")"
}

// notionID is the page id in a notion.so or notion.site URL, or "".
func notionID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "notion.so" && !strings.HasSuffix(host, ".notion.so") && !strings.HasSuffix(host, ".notion.site") {
		return ""
	}
	if all := anyID.FindAllString(u.Path, -1); len(all) > 0 {
		return strings.ToLower(all[len(all)-1])
	}
	if id := dashedID.FindString(u.Path); id != "" {
		return strings.ToLower(strings.ReplaceAll(id, "-", ""))
	}
	return ""
}

// relativeURL is the URL-encoded path from the folder fromDir to the vault
// path to.
func relativeURL(fromDir, to string) string {
	a, b := strings.Split(fromDir, "/"), strings.Split(to, "/")
	common := 0
	for common < len(a) && common < len(b)-1 && a[common] == b[common] {
		common++
	}
	var out []string
	for range a[common:] {
		out = append(out, "..")
	}
	for _, seg := range b[common:] {
		out = append(out, url.PathEscape(seg))
	}
	return strings.Join(out, "/")
}
