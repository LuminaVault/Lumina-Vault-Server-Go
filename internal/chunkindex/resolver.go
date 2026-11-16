package chunkindex

import (
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"strings"

	"github.com/google/uuid"
)

// ResolutionState says whether a wikilink points at exactly one document. The
// values are stored in vault_links.resolution_state.
type ResolutionState string

const (
	Resolved   ResolutionState = "resolved"
	Unresolved ResolutionState = "unresolved"
	Ambiguous  ResolutionState = "ambiguous"
)

// LinkSuffixes are stripped when matching, longest first.
var LinkSuffixes = []string{".markdown", ".md", ".txt"}

// WikilinkTarget is one indexed vault file a link can point at.
type WikilinkTarget struct {
	VaultFileID uuid.UUID
	Path        string
}

// ResolvedWikilink is a parsed link plus the document it points at. The target is
// nil unless State is Resolved.
type ResolvedWikilink struct {
	Link              ParsedWikilink
	TargetVaultFileID *uuid.UUID
	State             ResolutionState
}

// ResolveWikilinks is WikilinkResolver.resolve: an exact path (bare, then with
// each suffix), then a filename stem only if it is unique. Anything matching more
// than one document is ambiguous, never guessed.
//
// Past Swift, a link that matches nothing exactly gets one more try with both
// sides slug-folded (vaultfiles.FoldSegment): bulk import writes Obsidian's
// "Lore/The Last Light.md" as "<space>/lore/the-last-light.md", so [[The Last
// Light]] and [[Lore/The Last Light]] still find it. The same uniqueness rule
// applies: two folded matches are ambiguous.
//
// Swift's dictionaries compare keys by canonical equivalence, so [[café]] in NFC
// finds a file stored in NFD. CanonicalKey reproduces that.
func ResolveWikilinks(links []ParsedWikilink, candidates []WikilinkTarget) []ResolvedWikilink {
	if len(links) == 0 {
		return []ResolvedWikilink{}
	}
	byPath := make(map[string]uuid.UUID, len(candidates))
	byStem := make(map[string][]uuid.UUID, len(candidates))
	folded := foldIndex{byStem: make(map[string][]uuid.UUID, len(candidates))}
	for _, c := range candidates {
		path := NormalizeSlug(c.Path)
		byPath[swiftstr.CanonicalKey(path)] = c.VaultFileID
		stem := swiftstr.CanonicalKey(LinkStem(path))
		byStem[stem] = append(byStem[stem], c.VaultFileID)
		if f := FoldLinkPath(path); f != "" {
			folded.paths = append(folded.paths, foldedPath{path: f, id: c.VaultFileID})
			last := f[strings.LastIndexByte(f, '/')+1:]
			folded.byStem[last] = append(folded.byStem[last], c.VaultFileID)
		}
	}

	out := make([]ResolvedWikilink, len(links))
	for i, link := range links {
		out[i] = resolveOne(link, byPath, byStem, folded)
	}
	return out
}

type foldedPath struct {
	path string
	id   uuid.UUID
}

// foldIndex is the candidates slug-folded: by last segment, and whole paths for
// a link that names folders.
type foldIndex struct {
	byStem map[string][]uuid.UUID
	paths  []foldedPath
}

// FoldLinkPath folds every segment of a link target or vault path with
// vaultfiles.FoldSegment, the last one after LinkStem drops a known suffix. A
// segment that folds to nothing makes the whole result "", which never matches.
func FoldLinkPath(path string) string {
	var parts []string
	for _, p := range strings.Split(NormalizeSlug(path), "/") {
		if p != "" && p != "." && p != ".." {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	parts[len(parts)-1] = LinkStem(parts[len(parts)-1])
	for i, p := range parts {
		f := vaultfiles.FoldSegment(p)
		if f == "" {
			return ""
		}
		parts[i] = swiftstr.CanonicalKey(f)
	}
	return strings.Join(parts, "/")
}

func (f foldIndex) match(target string) []uuid.UUID {
	folded := FoldLinkPath(target)
	if folded == "" {
		return nil
	}
	if !strings.Contains(folded, "/") {
		return f.byStem[folded]
	}
	var ids []uuid.UUID
	for _, c := range f.paths {
		if c.path == folded || strings.HasSuffix(c.path, "/"+folded) {
			ids = append(ids, c.id)
		}
	}
	return ids
}

func resolveOne(link ParsedWikilink, byPath map[string]uuid.UUID, byStem map[string][]uuid.UUID, folded foldIndex) ResolvedWikilink {
	if id, ok := byPath[swiftstr.CanonicalKey(link.TargetSlug)]; ok {
		return ResolvedWikilink{Link: link, TargetVaultFileID: &id, State: Resolved}
	}
	for _, suffix := range LinkSuffixes {
		if id, ok := byPath[swiftstr.CanonicalKey(link.TargetSlug+suffix)]; ok {
			return ResolvedWikilink{Link: link, TargetVaultFileID: &id, State: Resolved}
		}
	}
	matches := byStem[swiftstr.CanonicalKey(LinkStem(link.TargetSlug))]
	if len(matches) == 0 {
		matches = folded.match(link.TargetSlug)
	}
	switch len(matches) {
	case 0:
		return ResolvedWikilink{Link: link, State: Unresolved}
	case 1:
		id := matches[0]
		return ResolvedWikilink{Link: link, TargetVaultFileID: &id, State: Resolved}
	default:
		return ResolvedWikilink{Link: link, State: Ambiguous}
	}
}

// LinkStem is WikilinkResolver.stem: the filename without directories or a known
// suffix.
func LinkStem(path string) string {
	name := path
	if rest := lastPathComponent(path); rest != "" {
		name = rest
	}
	for _, suffix := range LinkSuffixes {
		if swiftstr.HasASCIISuffix(name, suffix) {
			return name[:len(name)-len(suffix)]
		}
	}
	return name
}

// lastPathComponent is path.split(separator: "/").last, which compares Characters
// and drops empty pieces: "a/́b" has one component, not two.
func lastPathComponent(path string) string {
	last := ""
	for start := 0; start < len(path); {
		next := firstCharacter(path[start:], '/')
		if next < 0 {
			return path[start:]
		}
		if next > 0 {
			last = path[start : start+next]
		}
		start += next + 1
	}
	return last
}
