package chunkindex

import (
	"lumina/internal/swiftstr"

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
// Swift's dictionaries compare keys by canonical equivalence, so [[café]] in NFC
// finds a file stored in NFD. CanonicalKey reproduces that.
func ResolveWikilinks(links []ParsedWikilink, candidates []WikilinkTarget) []ResolvedWikilink {
	if len(links) == 0 {
		return []ResolvedWikilink{}
	}
	byPath := make(map[string]uuid.UUID, len(candidates))
	byStem := make(map[string][]uuid.UUID, len(candidates))
	for _, c := range candidates {
		path := NormalizeSlug(c.Path)
		byPath[swiftstr.CanonicalKey(path)] = c.VaultFileID
		stem := swiftstr.CanonicalKey(LinkStem(path))
		byStem[stem] = append(byStem[stem], c.VaultFileID)
	}

	out := make([]ResolvedWikilink, len(links))
	for i, link := range links {
		out[i] = resolveOne(link, byPath, byStem)
	}
	return out
}

func resolveOne(link ParsedWikilink, byPath map[string]uuid.UUID, byStem map[string][]uuid.UUID) ResolvedWikilink {
	if id, ok := byPath[swiftstr.CanonicalKey(link.TargetSlug)]; ok {
		return ResolvedWikilink{Link: link, TargetVaultFileID: &id, State: Resolved}
	}
	for _, suffix := range LinkSuffixes {
		if id, ok := byPath[swiftstr.CanonicalKey(link.TargetSlug+suffix)]; ok {
			return ResolvedWikilink{Link: link, TargetVaultFileID: &id, State: Resolved}
		}
	}
	matches := byStem[swiftstr.CanonicalKey(LinkStem(link.TargetSlug))]
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
