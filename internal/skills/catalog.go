package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
)

// builtinFiles holds Resources/Skills/<name>/SKILL.md. The whole tree (518 files,
// 4.7 MB) is checked in under builtin/ so it survives the Swift repo's archive, but
// only these 20 files (111 KB) are embedded: Bundle.module's "Skills" resource has
// no other reader in Swift. Being compiled in, they need no line in the Dockerfile.
// scripts/check-skills-sync.sh compares the tree with the pinned Swift image.
//
//go:embed builtin/*/SKILL.md
var builtinFiles embed.FS

// skillFileName is the one file a skill directory must hold.
const skillFileName = "SKILL.md"

// utf8BOM is dropped by Foundation's String(contentsOf:encoding: .utf8).
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Catalog is SkillCatalog: built-ins, cached per process, then the tenant's vault
// skills, rescanned on every call, which replace a built-in of the same name.
type Catalog struct {
	paths   vaultpaths.Paths
	builtin fs.FS

	once     sync.Once
	builtins []Manifest
}

// NewCatalog serves the embedded built-ins and each tenant's vault skills.
func NewCatalog(paths vaultpaths.Paths) *Catalog {
	sub, err := fs.Sub(builtinFiles, "builtin")
	if err != nil {
		panic(err) // the directive above guarantees the directory
	}
	return &Catalog{paths: paths, builtin: sub}
}

// NewVaultCatalog serves vault skills only, Swift's scanBuiltin: false.
func NewVaultCatalog(paths vaultpaths.Paths) *Catalog {
	return &Catalog{paths: paths}
}

// Manifests is manifests(for:), sorted by name. A missing directory, or a skill
// that can't be read or parsed, is skipped, the latter with a warning.
func (c *Catalog) Manifests(tenant uuid.UUID) []Manifest {
	byName := map[string]Manifest{}
	if c.builtin != nil {
		c.once.Do(func() { c.builtins = scan(c.builtin, SourceBuiltin) })
		for _, m := range c.builtins {
			byName[swiftstr.CanonicalKey(m.Name)] = m
		}
	}
	vault := filepath.Join(c.paths.TenantRoot(tenant), "skills")
	for _, m := range scan(os.DirFS(vault), SourceVault) {
		byName[swiftstr.CanonicalKey(m.Name)] = m
	}
	manifests := make([]Manifest, 0, len(byName))
	for _, m := range byName {
		manifests = append(manifests, m)
	}
	slices.SortFunc(manifests, func(a, b Manifest) int { return compareSwift(a.Name, b.Name) })
	return manifests
}

// Manifest is manifest(named:for:).
func (c *Catalog) Manifest(name string, tenant uuid.UUID) (Manifest, bool) {
	key := swiftstr.CanonicalKey(name)
	for _, m := range c.Manifests(tenant) {
		if swiftstr.CanonicalKey(m.Name) == key {
			return m, true
		}
	}
	return Manifest{}, false
}

// scan reads <dir>/<name>/SKILL.md. Directories go in name order, so of two vault
// directories declaring one manifest name the later wins. Hidden directories count,
// and symlinks are followed.
func scan(dir fs.FS, source Source) []Manifest {
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return compareSwift(a.Name(), b.Name()) })
	var manifests []Manifest
	for _, entry := range entries {
		info, err := fs.Stat(dir, entry.Name())
		if err != nil || !info.IsDir() {
			continue
		}
		file := entry.Name() + "/" + skillFileName
		if _, err := fs.Stat(dir, file); err != nil {
			continue
		}
		m, err := readManifest(dir, file, source)
		if err != nil {
			slog.Warn("skill_catalog parse failure", "skill", entry.Name(), "source", string(source), "error", err)
			continue
		}
		manifests = append(manifests, m)
	}
	return manifests
}

func readManifest(dir fs.FS, file string, source Source) (Manifest, error) {
	data, err := fs.ReadFile(dir, file)
	if err != nil {
		return Manifest{}, err
	}
	if !utf8.Valid(data) {
		return Manifest{}, fmt.Errorf("%s is not valid UTF-8", file)
	}
	return Parse(source, string(bytes.TrimPrefix(data, utf8BOM)))
}

// compareSwift is String's ordering: NFC scalar values, and UTF-8 sorts as scalars.
func compareSwift(a, b string) int {
	return strings.Compare(swiftstr.CanonicalKey(a), swiftstr.CanonicalKey(b))
}
