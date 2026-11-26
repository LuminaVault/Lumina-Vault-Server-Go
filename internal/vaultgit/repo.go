// Package vaultgit keeps a git repository inside each tenant's raw vault root, so
// every saved change becomes a commit (backlog E4, spec
// docs/superpowers/specs/2026-11-15-e4-git-backed-vault.md).
//
// Writers never call git. They signal a change with Touch, and a per-tenant
// Committer takes a snapshot of the whole working tree after a short debounce
// (spec D1). A periodic sweep catches any writer that forgot to Touch.
//
// The production image has no git binary, so this is pure Go (go-git).
package vaultgit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GitIgnore is the managed raw/.gitignore (spec D2). Trashed files, OS litter,
// temp files and LuminaVault's own dot-files stay out of history.
const GitIgnore = "_deleted_*\n.DS_Store\n*.tmp\n.lv-*\n"

// DefaultMaxFileSize is the per-file history limit (owner answer Q2): larger
// files save normally but get no history. It is a setting, not a constant, so
// a plan can raise it later.
const DefaultMaxFileSize int64 = 5 << 20

// Author is the commit identity; users have no git identity (spec D4). Who made
// a change is in the LV-Actor trailer.
var Author = object.Signature{Name: "LuminaVault", Email: "vault@luminavault.fyi"}

var (
	// ErrNoRepo means the tenant's vault has no repository yet.
	ErrNoRepo = errors.New("vaultgit: no repository")
	// ErrNotFound means the path did not exist at that commit, or the commit is unknown.
	ErrNotFound = errors.New("vaultgit: not found")
)

// Repo is one tenant's repository. It is not safe for concurrent use: the
// Committer serializes all work on a tenant (spec D3).
type Repo struct {
	root string
	repo *git.Repository
	// alive, set by Committer.WithRepo, asks whether the tenant's lock is
	// still held (StillLocked).
	alive func(context.Context) error
}

// Root is the working tree, the tenant's raw directory.
func (r *Repo) Root() string { return r.root }

// Open opens the repository in rawRoot, or returns ErrNoRepo.
func Open(rawRoot string) (*Repo, error) {
	repo, err := git.PlainOpen(rawRoot)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, ErrNoRepo
	}
	if err != nil {
		return nil, fmt.Errorf("vaultgit: open: %w", err)
	}
	return &Repo{root: rawRoot, repo: repo}, nil
}

// Init opens the repository in rawRoot, creating it when missing: git init on
// branch main, the managed .gitignore and one "Initial import of existing
// vault" commit of whatever is already there (spec D5). created reports whether
// it was new.
func Init(rawRoot string, maxFileSize int64) (r *Repo, created bool, err error) {
	if r, err := Open(rawRoot); err == nil {
		return r, false, r.ensureIgnore()
	} else if !errors.Is(err, ErrNoRepo) {
		return nil, false, err
	}
	if err := os.MkdirAll(rawRoot, 0o750); err != nil {
		return nil, false, fmt.Errorf("vaultgit: init: %w", err)
	}
	repo, err := git.PlainInitWithOptions(rawRoot, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		return nil, false, fmt.Errorf("vaultgit: init: %w", err)
	}
	r = &Repo{root: rawRoot, repo: repo}
	if err := r.ensureIgnore(); err != nil {
		return nil, false, err
	}
	if _, _, err := r.Commit(Message{Reason: "init", Summary: "Initial import of existing vault"}, maxFileSize, time.Now()); err != nil {
		return nil, false, err
	}
	return r, true, nil
}

// The managed patterns live in a marked block when a .gitignore has lines of
// its own (sync spec D8): a connected repository's ignore rules are the
// user's and are never rewritten.
const (
	ignoreBlockOpen  = "# LuminaVault (managed)"
	ignoreBlockClose = "# end"
)

// WithManagedIgnore returns a .gitignore text that has every managed pattern.
// A text that already has them all, in a block or not, is returned as it is:
// a vault from before sync keeps its file byte for byte. An empty one becomes
// GitIgnore. Anything else keeps every line and gains the marked block.
func WithManagedIgnore(existing string) string {
	if strings.TrimSpace(existing) == "" {
		return GitIgnore
	}
	have := map[string]bool{}
	for line := range strings.SplitSeq(existing, "\n") {
		have[strings.TrimRight(line, "\r")] = true
	}
	missing := false
	for pattern := range strings.SplitSeq(strings.TrimSuffix(GitIgnore, "\n"), "\n") {
		if !have[pattern] {
			missing = true
		}
	}
	if !missing {
		return existing
	}
	if !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	return existing + ignoreBlockOpen + "\n" + GitIgnore + ignoreBlockClose + "\n"
}

// ensureIgnore gives raw/.gitignore the managed patterns when it lacks them,
// and leaves every other line alone.
func (r *Repo) ensureIgnore() error {
	b, err := os.ReadFile(r.ignorePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("vaultgit: read .gitignore: %w", err)
	}
	_, err = r.writeIgnore(string(b), string(b), err == nil)
	return err
}

func (r *Repo) ignorePath() string { return filepath.Join(r.root, ".gitignore") }

// writeIgnore writes WithManagedIgnore(text) unless the file already is that.
func (r *Repo) writeIgnore(text, current string, exists bool) (changed bool, err error) {
	want := WithManagedIgnore(text)
	if exists && current == want {
		return false, nil
	}
	if err := os.WriteFile(r.ignorePath(), []byte(want), 0o600); err != nil { //nolint:gosec // the path is the vault's own root plus a constant
		return false, fmt.Errorf("vaultgit: write .gitignore: %w", err)
	}
	return true, nil
}

// TakeIgnore makes raw/.gitignore the remote's file, as it is, plus the
// managed block when the remote's lacks the managed patterns (sync spec D8).
// It reports whether the file changed. The file is not a symlink target: a
// link at that name is replaced.
func (r *Repo) TakeIgnore(remote []byte) (changed bool, err error) {
	info, err := os.Lstat(r.ignorePath())
	if err == nil && !info.Mode().IsRegular() {
		if err := os.Remove(r.ignorePath()); err != nil {
			return false, fmt.Errorf("vaultgit: replace .gitignore: %w", err)
		}
	}
	b, err := os.ReadFile(r.ignorePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("vaultgit: read .gitignore: %w", err)
	}
	return r.writeIgnore(string(remote), string(b), err == nil)
}

// Message is a commit's reason, paths and trailers (spec D4).
type Message struct {
	Reason  string // upload, move, delete, restore, import, todo, capture, memo, skill, soul, pull, sweep, init
	Summary string // replaces the generated subject when set (the initial commit)
	Actor   string // LV-Actor trailer: the latest actor's user UUID
	Source  string // LV-Source trailer: the client, when known
}

const maxBodyPaths = 50

func (m Message) render(paths []string) string {
	var b strings.Builder
	switch {
	case m.Summary != "":
		b.WriteString(m.Summary)
	case len(paths) == 0:
		b.WriteString(m.Reason)
	default:
		fmt.Fprintf(&b, "%s: %s", m.Reason, paths[0])
		if len(paths) > 1 {
			fmt.Fprintf(&b, " (+%d more)", len(paths)-1)
		}
	}
	b.WriteString("\n")
	if m.Summary == "" && len(paths) > 1 {
		b.WriteString("\n")
		for i, p := range paths {
			if i == maxBodyPaths {
				fmt.Fprintf(&b, "… and %d more\n", len(paths)-maxBodyPaths)
				break
			}
			b.WriteString(p + "\n")
		}
	}
	trailers := []string{"LV-Reason: " + m.Reason}
	if m.Actor != "" {
		trailers = append(trailers, "LV-Actor: "+m.Actor)
	}
	if m.Source != "" {
		trailers = append(trailers, "LV-Source: "+m.Source)
	}
	b.WriteString("\n" + strings.Join(trailers, "\n") + "\n")
	return b.String()
}

// unignore turns a pattern that ignores a path into one that takes it back.
type unignore struct{ p gitignore.Pattern }

func (u unignore) Match(path []string, isDir bool) gitignore.MatchResult {
	if u.p.Match(path, isDir) == gitignore.Exclude {
		return gitignore.Include
	}
	return gitignore.NoMatch
}

// worktree is the repository's working tree, set up so that only the managed
// patterns (GitIgnore) decide what a commit leaves out (sync spec D8, owner
// decision 2026-11-26). A connected repository's .gitignore has the user's
// own lines, and they stay in the file byte for byte, but they are rules for
// the user's other git clients: here a line of theirs must neither hide a
// vault file from sync nor, with a "!", bring the trash into history.
//
// go-git reads every .gitignore under the working tree itself and offers no
// way not to. It does let Excludes have the last word, and the last pattern
// that matches a path wins: so every pattern it will read is undone here,
// and the managed ones follow.
func (r *Repo) worktree() (*git.Worktree, error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		return nil, err
	}
	read, err := gitignore.ReadPatterns(wt.Filesystem, nil)
	if err != nil {
		return nil, fmt.Errorf("vaultgit: read ignore patterns: %w", err)
	}
	excludes := make([]gitignore.Pattern, 0, len(read)+4)
	for _, p := range read {
		excludes = append(excludes, unignore{p})
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(GitIgnore, "\n"), "\n") {
		excludes = append(excludes, gitignore.ParsePattern(line, nil))
	}
	wt.Excludes = excludes
	return wt, nil
}

// status is the working tree's status. A repository can have a file and a
// folder under one name, a state an earlier release could stage and commit
// (a file saved inside a path GitHub sync holds). go-git commits such an
// index again without complaint, and cannot read a status at all once HEAD's
// own tree has it. So the index of a vault that synced is mended first, and
// where HEAD has the clash too, a commit of the mended index is made on top
// of it; then the status is read again. One such path does not stop every
// later commit of the vault.
func (r *Repo) status(held *heldPaths) (*git.Worktree, git.Status, error) {
	wt, err := r.worktree()
	if err != nil {
		return nil, nil, err
	}
	if held != nil {
		// Only a vault that synced can be in that state. go-git reads such
		// an index without complaint and would commit it again.
		if _, err := r.mendIndex(held); err != nil {
			return nil, nil, err
		}
	}
	status, err := wt.Status()
	if err == nil {
		return wt, status, nil
	}
	failed := fmt.Errorf("vaultgit: status: %w", err)
	clash, cerr := r.headClashes()
	if cerr != nil || !clash {
		return nil, nil, failed
	}
	sig := Author
	sig.When = time.Now()
	msg := Message{Reason: "sweep", Summary: "sweep: mend a path that was both a file and a folder"}
	if _, err := wt.Commit(msg.render(nil), &git.CommitOptions{Author: &sig, Committer: &sig, AllowEmptyCommits: true}); err != nil {
		return nil, nil, failed
	}
	if status, err = wt.Status(); err != nil {
		return nil, nil, fmt.Errorf("vaultgit: status: %w", err)
	}
	return wt, status, nil
}

// headClashes reports whether HEAD's tree has a name that is both an entry
// and the folder of other entries.
func (r *Repo) headClashes() (bool, error) {
	ref, err := r.headRef()
	if err != nil {
		return false, err
	}
	c, err := r.repo.CommitObject(ref.Hash())
	if err != nil {
		return false, err
	}
	names := map[string]bool{}
	err = r.walk(c, func(name string, _ object.TreeEntry) error {
		names[name] = true
		return nil
	})
	if err != nil {
		return false, err
	}
	for name := range names {
		for dir := name; ; {
			i := strings.LastIndexByte(dir, '/')
			if i < 0 {
				break
			}
			if dir = dir[:i]; names[dir] {
				return true, nil
			}
		}
	}
	return false, nil
}

// mendIndex removes from the index one side of every name that is both an
// entry and the folder of other entries, and reports whether it removed any.
// The held side stays (GitHub has it, and it must not be committed as a
// deletion); with no held side, what raw/ has stays. No file is touched: an
// entry dropped here whose file exists is an ordinary untracked file again.
func (r *Repo) mendIndex(held *heldPaths) (bool, error) {
	idx, err := r.repo.Storer.Index()
	if err != nil {
		return false, err
	}
	names := make(map[string]bool, len(idx.Entries))
	for _, e := range idx.Entries {
		names[e.Name] = true
	}
	drop := map[string]bool{}
	for _, e := range idx.Entries {
		for i := strings.IndexByte(e.Name, '/'); i >= 0; {
			folder := e.Name[:i]
			if names[folder] {
				info, serr := os.Lstat(filepath.Join(r.root, filepath.FromSlash(folder)))
				isFile := serr == nil && !info.IsDir()
				switch {
				case held.has(e.Name) && !held.has(folder):
					drop[folder] = true
				case held.has(folder), isFile:
					drop[e.Name] = true
				default:
					drop[folder] = true
				}
			}
			next := strings.IndexByte(e.Name[i+1:], '/')
			if next < 0 {
				break
			}
			i += 1 + next
		}
	}
	if len(drop) == 0 {
		return false, nil
	}
	kept := idx.Entries[:0]
	for _, e := range idx.Entries {
		if !drop[e.Name] {
			kept = append(kept, e)
		}
	}
	idx.Entries = kept
	if err := r.repo.Storer.SetIndex(idx); err != nil {
		return false, fmt.Errorf("vaultgit: write index: %w", err)
	}
	return true, nil
}

// pending lists the changes a commit would take: added or modified files of at
// most maxFileSize bytes, and deletions. Files over the limit are skipped; a
// tracked file that grows past it keeps its last committed version. A held
// path (held.go) is left as the index has it whatever raw/ holds: missing
// there is never a deletion, and a file that appeared there is never added.
// Nor is a file added inside a held path, or at a folder of one (HeldAt):
// git cannot hold both, and the entry GitHub has stays.
func (r *Repo) pending(maxFileSize int64) (wt *git.Worktree, add, remove []string, err error) {
	held, err := r.heldPaths()
	if err != nil {
		return nil, nil, nil, err
	}
	wt, status, err := r.status(held)
	if err != nil {
		return nil, nil, nil, err
	}
	for p, s := range status {
		if s.Worktree == git.Unmodified && s.Staging == git.Unmodified {
			continue
		}
		if held.has(p) {
			continue
		}
		info, err := os.Lstat(filepath.Join(r.root, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			if s.Staging != git.Untracked && s.Staging != git.Added {
				remove = append(remove, p)
			}
		case err != nil:
			return nil, nil, nil, err
		case !info.Mode().IsRegular():
			// Symlinks and devices never enter history.
		case maxFileSize > 0 && info.Size() > maxFileSize:
		case held.at(p) != "":
		default:
			add = append(add, p)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return wt, add, remove, nil
}

// Dirty reports whether a commit would record anything.
func (r *Repo) Dirty(maxFileSize int64) (bool, error) {
	_, add, remove, err := r.pending(maxFileSize)
	return len(add)+len(remove) > 0, err
}

// Commit snapshots the working tree (git add -A, bounded by maxFileSize) and
// commits it. It returns the new commit's hash, or changed=false and no commit
// when there was nothing to record.
func (r *Repo) Commit(msg Message, maxFileSize int64, when time.Time) (sha string, changed bool, err error) {
	wt, paths, err := r.stage(maxFileSize)
	if err != nil {
		return "", false, err
	}
	if len(paths) == 0 {
		return "", false, nil
	}
	sig := Author
	sig.When = when
	h, err := wt.Commit(msg.render(paths), &git.CommitOptions{Author: &sig, Committer: &sig, AllowEmptyCommits: false})
	if err != nil {
		return "", false, fmt.Errorf("vaultgit: commit: %w", err)
	}
	return h.String(), true, nil
}

// stage is git add -A, bounded by maxFileSize and the held list: it puts
// pending's changes in the index and returns their paths, sorted.
func (r *Repo) stage(maxFileSize int64) (*git.Worktree, []string, error) {
	wt, add, remove, err := r.pending(maxFileSize)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range add {
		if _, err := wt.Add(p); err != nil {
			// A file deleted between status and add is caught by the next commit.
			if _, statErr := os.Lstat(filepath.Join(r.root, filepath.FromSlash(p))); errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return nil, nil, fmt.Errorf("vaultgit: add %s: %w", p, err)
		}
	}
	for _, p := range remove {
		if _, err := wt.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, index.ErrEntryNotFound) {
			return nil, nil, fmt.Errorf("vaultgit: remove %s: %w", p, err)
		}
	}
	paths := append(append([]string{}, add...), remove...)
	sort.Strings(paths)
	return wt, paths, nil
}

// Version is one commit that changed a path, newest first in Log.
type Version struct {
	SHA         string
	CommittedAt time.Time
	Message     string // the subject line
	Reason      string // LV-Reason trailer
	ActorID     string // LV-Actor trailer, empty when unknown
	SizeBytes   int64  // the path's size at this commit; 0 when Deleted
	Deleted     bool   // this commit removed the path
}

// Log lists the commits that changed path, newest first. before, when set, is a
// commit hash from a previous page: the list starts after it. limit ≤ 0 means 50.
func (r *Repo) Log(path string, limit int, before string) ([]Version, error) {
	if limit <= 0 {
		limit = 50
	}
	head, err := r.headRef()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return []Version{}, nil
	}
	if err != nil {
		return nil, err
	}
	// go-git's own path filter compares each commit with the next one in its
	// walk, which is its parent only while history is a line. With merges
	// (GitHub sync) that lists the wrong commits, so the comparison is made
	// here: a commit is a version of the path when what it has there differs
	// from what every one of its parents has. For a merge that leaves out a
	// path it took from either side (sync spec D10).
	iter, err := r.repo.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("vaultgit: log: %w", err)
	}
	defer iter.Close()
	type entry struct {
		hash plumbing.Hash
		ok   bool
	}
	seen := map[plumbing.Hash]entry{}
	at := func(c *object.Commit) entry {
		if e, ok := seen[c.Hash]; ok {
			return e
		}
		var e entry
		if tree, err := c.Tree(); err == nil {
			if te, err := tree.FindEntry(path); err == nil && te.Mode != filemode.Dir {
				e = entry{hash: te.Hash, ok: true}
			}
		}
		seen[c.Hash] = e
		return e
	}
	out := []Version{}
	skipping := before != ""
	var foreign map[plumbing.Hash]bool // read when the first commit asks
	for {
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("vaultgit: log: %w", err)
		}
		here := at(c)
		changed := here.ok || c.NumParents() > 0
		err = c.Parents().ForEach(func(p *object.Commit) error {
			if at(p) == here {
				changed = false
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("vaultgit: log: %w", err)
		}
		delete(seen, c.Hash) // its children have all been looked at
		if !changed {
			continue
		}
		if skipping {
			if c.Hash.String() == before {
				skipping = false
			}
			continue
		}
		v := Version{SHA: c.Hash.String(), CommittedAt: c.Committer.When.UTC(), Deleted: !here.ok}
		v.Message, v.Reason, v.ActorID = parseMessage(c.Message)
		mine := c.Committer.Email == Author.Email
		if mine {
			// The e-mail is a claim anyone with push access can make: a
			// commit a pull brought in is not this server's, whatever it says.
			if foreign == nil {
				if foreign, err = r.foreign(); err != nil {
					return nil, err
				}
			}
			mine = !foreign[c.Hash]
		}
		if !mine {
			// Made on GitHub, or by another git client: the subject is the
			// user's own, and there is no LuminaVault actor.
			v.Reason, v.ActorID = ReasonGitHub, ""
		}
		if here.ok {
			if size, err := r.repo.Storer.EncodedObjectSize(here.hash); err == nil {
				v.SizeBytes = size
			}
		}
		out = append(out, v)
		if len(out) == limit {
			return out, nil
		}
	}
}

// ReasonGitHub is the reason Log gives a commit LuminaVault did not make.
const ReasonGitHub = "github"

func parseMessage(msg string) (subject, reason, actor string) {
	lines := strings.Split(msg, "\n")
	subject = lines[0]
	for _, l := range lines[1:] {
		if v, ok := strings.CutPrefix(l, "LV-Reason: "); ok {
			reason = v
		} else if v, ok := strings.CutPrefix(l, "LV-Actor: "); ok {
			actor = v
		}
	}
	return subject, reason, actor
}

// Blob returns path's bytes at commit sha, or ErrNotFound when the commit is
// unknown or the path didn't exist there.
func (r *Repo) Blob(sha, path string) ([]byte, error) {
	if !plumbing.IsHash(sha) {
		return nil, ErrNotFound
	}
	c, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return nil, ErrNotFound
	}
	f, err := c.File(path)
	if err != nil {
		return nil, ErrNotFound
	}
	rd, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rd.Close() }()
	return io.ReadAll(rd)
}

// headRefWait bounds how long headRef waits for a ref being written.
const headRefWait = 250 * time.Millisecond

// headRef resolves HEAD. go-git moves a branch by truncating its ref file and
// then writing the new hash, and it reads an empty ref file as a reference
// that does not exist. A reader without the tenant's lock (history, the
// GitHub sync's peeks and status) that lands between the two steps would take
// a repository with a commit in progress for one with no commits at all. A
// branch that has a ref file has commits, so while the file is there a
// missing reference is read again, for at most headRefWait: a file a crash
// left empty still reads as no commits, as before.
func (r *Repo) headRef() (*plumbing.Reference, error) {
	deadline := time.Now().Add(headRefWait)
	for wait := 500 * time.Microsecond; ; wait = min(2*wait, 20*time.Millisecond) {
		ref, err := r.repo.Head()
		if !errors.Is(err, plumbing.ErrReferenceNotFound) || !r.headHasRefFile() || time.Now().After(deadline) {
			return ref, err
		}
		time.Sleep(wait)
	}
}

// headHasRefFile reports whether the branch HEAD names has a ref file of its
// own. A repository before its first commit has none.
func (r *Repo) headHasRefFile() bool {
	head, err := r.repo.Storer.Reference(plumbing.HEAD)
	if err != nil || head.Type() != plumbing.SymbolicReference {
		return false
	}
	info, err := os.Stat(filepath.Join(r.root, ".git", filepath.FromSlash(head.Target().String())))
	return err == nil && info.Mode().IsRegular()
}

// Head is the newest commit's hash, or "" before the first commit.
func (r *Repo) Head() (string, error) {
	h, err := r.headRef()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return h.Hash().String(), nil
}
