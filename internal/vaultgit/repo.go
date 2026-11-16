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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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

// ensureIgnore rewrites raw/.gitignore when it has drifted from GitIgnore.
func (r *Repo) ensureIgnore() error {
	p := filepath.Join(r.root, ".gitignore")
	if b, err := os.ReadFile(p); err == nil && string(b) == GitIgnore { //nolint:gosec // fixed name under the tenant root
		return nil
	}
	if err := os.WriteFile(p, []byte(GitIgnore), 0o600); err != nil {
		return fmt.Errorf("vaultgit: write .gitignore: %w", err)
	}
	return nil
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

// pending lists the changes a commit would take: added or modified files of at
// most maxFileSize bytes, and deletions. Files over the limit are skipped; a
// tracked file that grows past it keeps its last committed version.
func (r *Repo) pending(maxFileSize int64) (add, remove []string, err error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		return nil, nil, err
	}
	status, err := wt.Status()
	if err != nil {
		return nil, nil, fmt.Errorf("vaultgit: status: %w", err)
	}
	for p, s := range status {
		if s.Worktree == git.Unmodified && s.Staging == git.Unmodified {
			continue
		}
		info, err := os.Lstat(filepath.Join(r.root, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, os.ErrNotExist):
			if s.Staging != git.Untracked && s.Staging != git.Added {
				remove = append(remove, p)
			}
		case err != nil:
			return nil, nil, err
		case !info.Mode().IsRegular():
			// Symlinks and devices never enter history.
		case maxFileSize > 0 && info.Size() > maxFileSize:
		default:
			add = append(add, p)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove, nil
}

// Dirty reports whether a commit would record anything.
func (r *Repo) Dirty(maxFileSize int64) (bool, error) {
	add, remove, err := r.pending(maxFileSize)
	return len(add)+len(remove) > 0, err
}

// Commit snapshots the working tree (git add -A, bounded by maxFileSize) and
// commits it. It returns the new commit's hash, or changed=false and no commit
// when there was nothing to record.
func (r *Repo) Commit(msg Message, maxFileSize int64, when time.Time) (sha string, changed bool, err error) {
	add, remove, err := r.pending(maxFileSize)
	if err != nil {
		return "", false, err
	}
	if len(add)+len(remove) == 0 {
		return "", false, nil
	}
	wt, err := r.repo.Worktree()
	if err != nil {
		return "", false, err
	}
	for _, p := range add {
		if _, err := wt.Add(p); err != nil {
			// A file deleted between status and add is caught by the next commit.
			if _, statErr := os.Lstat(filepath.Join(r.root, filepath.FromSlash(p))); errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return "", false, fmt.Errorf("vaultgit: add %s: %w", p, err)
		}
	}
	for _, p := range remove {
		if _, err := wt.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, index.ErrEntryNotFound) {
			return "", false, fmt.Errorf("vaultgit: remove %s: %w", p, err)
		}
	}
	paths := append(append([]string{}, add...), remove...)
	sort.Strings(paths)
	sig := Author
	sig.When = when
	h, err := wt.Commit(msg.render(paths), &git.CommitOptions{Author: &sig, Committer: &sig, AllowEmptyCommits: false})
	if err != nil {
		return "", false, fmt.Errorf("vaultgit: commit: %w", err)
	}
	return h.String(), true, nil
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
	head, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return []Version{}, nil
	}
	if err != nil {
		return nil, err
	}
	iter, err := r.repo.Log(&git.LogOptions{From: head.Hash(), FileName: &path})
	if err != nil {
		return nil, fmt.Errorf("vaultgit: log: %w", err)
	}
	defer iter.Close()
	out := []Version{}
	skipping := before != ""
	for {
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("vaultgit: log: %w", err)
		}
		if skipping {
			if c.Hash.String() == before {
				skipping = false
			}
			continue
		}
		v := Version{SHA: c.Hash.String(), CommittedAt: c.Committer.When.UTC()}
		v.Message, v.Reason, v.ActorID = parseMessage(c.Message)
		if f, err := c.File(path); err == nil {
			v.SizeBytes = f.Size
		} else {
			v.Deleted = true
		}
		out = append(out, v)
		if len(out) == limit {
			return out, nil
		}
	}
}

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

// Head is the newest commit's hash, or "" before the first commit.
func (r *Repo) Head() (string, error) {
	h, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return h.Hash().String(), nil
}
