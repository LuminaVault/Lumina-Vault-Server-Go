// Package bundled is Swift's Bundle.module: the two resource trees Package.swift
// copies into the App target (.copy, so each skill's directory survives), checked in
// here so they outlive the Swift repo's archive and compiled in so they need no line
// in the Dockerfile.
//
//   - skills/ is Sources/App/Resources/Skills (518 files, 4.7 MB). Its one Swift
//     reader is SkillCatalog's builtin scan (Skills/SkillCatalog.swift:52), which
//     reads <name>/SKILL.md and nothing else, so only those 20 files (111 KB) are
//     embedded. The rest stays in the tree for the sync check and the archive.
//   - hermesskills/ is Sources/App/Resources/HermesSkills (27 files, 176 KB), the
//     kb-* skills HermesBundledSkills installs onto a user's Hermes
//     (Hermes/Mirror/HermesBundledSkills.swift:18) and the Hermes image bakes. It is
//     embedded whole: it's small, and the image copies the whole tree too.
//
// scripts/check-skills-sync.sh compares both trees, byte for byte, with the Swift
// image pinned in parity/SWIFT_IMAGE.
package bundled

import (
	"embed"
	"io/fs"
)

//go:embed skills/*/SKILL.md
var skillFiles embed.FS

//go:embed all:hermesskills
var hermesSkillFiles embed.FS

// Skills is Bundle.module's "Skills" directory: <name>/SKILL.md for each built-in.
func Skills() fs.FS { return sub(skillFiles, "skills") }

// HermesSkills is Bundle.module's "HermesSkills" directory: README.md and each
// kb-* skill with its references.
func HermesSkills() fs.FS { return sub(hermesSkillFiles, "hermesskills") }

func sub(f embed.FS, dir string) fs.FS {
	s, err := fs.Sub(f, dir)
	if err != nil {
		panic(err) // the embed directives guarantee the directory
	}
	return s
}
