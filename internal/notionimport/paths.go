package notionimport

import (
	"lumina/internal/vaultfiles"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// maxSlugSegment is vaultfiles.SlugifySegment's cap.
const maxSlugSegment = 80

var (
	idSuffix = regexp.MustCompile(`(?i)\s+([0-9a-f]{32})$`)
	anyID    = regexp.MustCompile(`(?i)[0-9a-f]{32}`)
)

// StripID removes Notion's trailing " <32 hex>" from one name (no extension).
func StripID(name string) (clean, id string) {
	if m := idSuffix.FindStringSubmatchIndex(name); m != nil {
		return name[:m[0]], strings.ToLower(name[m[2]:m[3]])
	}
	return name, ""
}

// Collision is an entry whose mapped name was taken by an earlier sibling.
type Collision struct {
	From string
	To   string
}

// Database is one exported database: its CSV and the vault folder its rows go to.
type Database struct {
	// Name is the database's name, id stripped.
	Name string
	// CSV is the entry to read: the _all.csv when the export has one.
	CSV string
	// Others are the database's other CSV entries.
	Others []string
	// Folder is the vault folder, "<space>/…/<db>".
	Folder string
}

// Mapping is MapPaths' result.
type Mapping struct {
	Slug string
	// New maps an entry's path to its vault path. A page maps to its note, an
	// attachment to its file, a CSV to its database folder. Hidden entries are
	// absent.
	New        map[string]string
	Collisions []Collision
	Databases  []Database

	ids      map[string]string
	nodes    map[string]string
	suffixed map[string]bool
	taken    map[string]bool
}

// IsHidden reports a path with a dotfile or dot-folder segment, or macOS's
// __MACOSX folder. Such entries are not imported.
func IsHidden(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "__MACOSX" || (seg != "." && seg != ".." && strings.HasPrefix(seg, ".")) {
			return true
		}
	}
	return false
}

// EntryKind sorts an entry by its extension.
type EntryKind int

// The kinds of entry.
const (
	KindAttachment EntryKind = iota
	KindPage
	KindCSV
)

// KindOf is the kind of the entry at p.
func KindOf(p string) EntryKind {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown":
		return KindPage
	case ".csv":
		return KindCSV
	}
	return KindAttachment
}

// MapPaths maps every entry to its vault path under the Space slug (spec D2).
// The trailing id is stripped from each segment, and _all before .csv; each
// segment is slugified as bulk import does, so imports.SafePath(slug, <path
// below the slug>) returns the same path. A page, its child folder and its
// CSVs share one name. Siblings that map to the same name get " 2", " 3" …
// before slugifying, in ZIP order.
func MapPaths(entries []Entry, slug string) *Mapping {
	m := &Mapping{
		Slug: slug, New: make(map[string]string, len(entries)),
		ids: map[string]string{}, nodes: map[string]string{}, suffixed: map[string]bool{}, taken: map[string]bool{},
	}
	dbAt := map[string]int{}
	for _, e := range entries {
		if IsHidden(e.Path) {
			continue
		}
		var segs []string
		for _, s := range strings.Split(e.Path, "/") {
			if s != "" && s != "." {
				segs = append(segs, s)
			}
		}
		if len(segs) == 0 {
			continue
		}
		parent := slug
		for _, dir := range segs[:len(segs)-1] {
			stem, _ := m.node(parent, dir)
			parent += "/" + stem
		}
		name := segs[len(segs)-1]
		ext := path.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		switch KindOf(name) {
		case KindPage:
			node, suffixed := m.node(parent, stem)
			to := parent + "/" + node + ".md"
			m.New[e.Path] = to
			if _, id := StripID(stem); id != "" {
				if _, dup := m.ids[id]; !dup {
					m.ids[id] = e.Path
				}
			}
			if suffixed {
				m.Collisions = append(m.Collisions, Collision{From: e.Path, To: to})
			}
		case KindCSV:
			all := strings.HasSuffix(stem, "_all")
			stem = strings.TrimSuffix(stem, "_all")
			node, suffixed := m.node(parent, stem)
			folder := parent + "/" + node
			m.New[e.Path] = folder
			at, known := dbAt[folder]
			if !known {
				clean, _ := StripID(stem)
				dbAt[folder] = len(m.Databases)
				m.Databases = append(m.Databases, Database{Name: clean, CSV: e.Path, Folder: folder})
				if suffixed {
					m.Collisions = append(m.Collisions, Collision{From: e.Path, To: folder})
				}
				break
			}
			db := &m.Databases[at]
			if all && !strings.HasSuffix(strings.TrimSuffix(db.CSV, path.Ext(db.CSV)), "_all") {
				db.Others, db.CSV = append(db.Others, db.CSV), e.Path
			} else {
				db.Others = append(db.Others, e.Path)
			}
		default:
			clean, _ := StripID(stem)
			suffix := ""
			if ext != "" {
				suffix = "." + trimDots(vaultfiles.SlugifySegment(ext[1:]))
			}
			to, n := m.claim(parent, clean, suffix)
			m.New[e.Path] = to
			if n > 1 {
				m.Collisions = append(m.Collisions, Collision{From: e.Path, To: to})
			}
		}
	}
	return m
}

// PageByID is the page entry whose name carries the Notion id.
func (m *Mapping) PageByID(id string) (string, bool) {
	p, ok := m.ids[strings.ToLower(id)]
	return p, ok
}

// Allocate claims a new note path for name in a vault folder: a database row
// that has no page of its own.
func (m *Mapping) Allocate(folder, name string) string {
	to, _ := m.claim(folder, name, ".md")
	return to
}

// Relative is a vault path below the Space slug: what bulk ingest takes.
func (m *Mapping) Relative(vaultPath string) string {
	return strings.TrimPrefix(vaultPath, m.Slug+"/")
}

// node is the slug of a page/folder/CSV name inside parent, the same for
// every entry that shares the name (id included).
func (m *Mapping) node(parent, name string) (stem string, suffixed bool) {
	key := parent + "\x00" + name
	if stem, ok := m.nodes[key]; ok {
		return stem, m.suffixed[key]
	}
	clean, _ := StripID(name)
	for n := 1; ; n++ {
		stem = slugStem(clean, n)
		if !m.taken[parent+"/"+stem] && !m.taken[parent+"/"+stem+".md"] {
			m.taken[parent+"/"+stem], m.taken[parent+"/"+stem+".md"] = true, true
			m.nodes[key], m.suffixed[key] = stem, n > 1
			return stem, n > 1
		}
	}
}

// claim takes the first free "<parent>/<slug of name [n]><suffix>".
func (m *Mapping) claim(parent, name, suffix string) (string, int) {
	for n := 1; ; n++ {
		to := parent + "/" + slugStem(name, n) + suffix
		if !m.taken[to] {
			m.taken[to] = true
			return to, n
		}
	}
}

// slugStem slugifies name, with " n" appended from the second on. A leading
// dot is dropped so the result is never a hidden file, and the number survives
// the 80-Character cap.
func slugStem(name string, n int) string {
	if n <= 1 {
		return trimDots(vaultfiles.SlugifySegment(name))
	}
	number := "-" + strconv.Itoa(n)
	out := trimDots(vaultfiles.SlugifySegment(name + " " + strconv.Itoa(n)))
	if strings.HasSuffix(out, number) {
		return out
	}
	base := []rune(trimDots(vaultfiles.SlugifySegment(name)))
	if keep := maxSlugSegment - len(number); len(base) > keep {
		base = base[:keep]
	}
	return strings.TrimRight(string(base), "-") + number
}

func trimDots(s string) string {
	if out := strings.TrimLeft(s, "."); out != "" {
		return out
	}
	return "x"
}
