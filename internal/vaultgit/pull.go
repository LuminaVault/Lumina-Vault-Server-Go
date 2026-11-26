package vaultgit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
)

// This file is what a pull needs from a repository (sync spec D10). Nothing
// here ever writes the working tree: raw/ is the live vault, and a pull
// changes it only through the vault's own write pipeline. The repository
// gets objects (Fetch), is read (Tree, BlobByID, MergeBase), and has its
// index and branch moved (FastForward, MergeCommit).

// FetchOptions bound one fetch.
type FetchOptions struct {
	// MaxBytes refuses, with ErrPackTooLarge, a fetch whose answers are
	// larger: the remote grew past what its owner's limit allows between the
	// question about its size and the fetch. Zero is no limit.
	MaxBytes int64
}

// Fetch is FetchWith without limits.
func (r *Repo) Fetch(ctx context.Context, branch, token string) (tip string, err error) {
	return r.FetchWith(ctx, branch, token, FetchOptions{})
}

// FetchWith brings origin's branch into refs/remotes/origin/<branch>, with
// its whole history and no tags, and returns its tip. A repository with no
// such branch, or with no commits at all, is "" and no error. The tracking
// ref is set to whatever the remote has, a rewritten history included: it is
// a copy of the remote, not the vault's branch. ctx bounds the transport; the
// token is used for this call only.
func (r *Repo) FetchWith(ctx context.Context, branch, token string, opts FetchOptions) (tip string, err error) {
	if !ValidBranchName(branch) {
		return "", fmt.Errorf("vaultgit: invalid branch %q", branch)
	}
	remote, err := r.repo.Remote(RemoteName)
	if errors.Is(err, git.ErrRemoteNotFound) {
		return "", ErrNoRemote
	}
	if err != nil {
		return "", fmt.Errorf("vaultgit: remote: %w", err)
	}
	var budget *fetchBudget
	if opts.MaxBytes > 0 {
		budget = &fetchBudget{limit: opts.MaxBytes}
		ctx = context.WithValue(ctx, fetchBudgetKey{}, budget)
	}
	tracking := plumbing.NewRemoteReferenceName(RemoteName, branch)
	spec := config.RefSpec("+" + plumbing.NewBranchReferenceName(branch).String() + ":" + tracking.String())
	err = remote.FetchContext(ctx, &git.FetchOptions{
		RemoteName: RemoteName,
		RefSpecs:   []config.RefSpec{spec}, // this branch only, not the remote's refs/heads/*
		Auth:       basicAuth(token),
		Tags:       git.NoTags,
	})
	switch {
	case budget != nil && budget.spent.Load():
		// Whatever go-git made of the read that failed, this is why.
		return "", fmt.Errorf("%w: fetch", ErrPackTooLarge)
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
	case errors.Is(err, transport.ErrEmptyRemoteRepository), errors.Is(err, git.NoMatchingRefSpecError{}):
		return "", nil
	default:
		return "", remoteError("fetch", err)
	}
	ref, err := r.repo.Storer.Reference(tracking)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("vaultgit: fetch: %w", err)
	}
	return ref.Hash().String(), nil
}

// LsRemote asks the repository at url for its branch's tip without fetching
// anything and without a local repository: the poll's one small request
// (sync spec D9). No such branch, or an empty repository, is "".
func LsRemote(ctx context.Context, url, branch, token string) (tip string, err error) {
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: RemoteName, URLs: []string{url}})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: basicAuth(token)})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return "", nil
	}
	if err != nil {
		return "", remoteError("ls-remote", err)
	}
	want := plumbing.NewBranchReferenceName(branch)
	for _, ref := range refs {
		if ref.Name() == want && ref.Type() == plumbing.HashReference {
			return ref.Hash().String(), nil
		}
	}
	return "", nil
}

// TreeEntry is one file-like entry of a commit's tree: a blob, a symlink or a
// submodule link. Folders are not entries.
type TreeEntry struct {
	Path string
	// Mode is git's own text: 100644, 100755, 120000 (symlink), 160000
	// (submodule).
	Mode string
	// ID is the blob's object id (for a submodule, the commit it points at).
	ID string
	// Size is the blob's size in bytes; 0 for a submodule.
	Size int64
}

func (r *Repo) commit(sha string) (*object.Commit, error) {
	if !plumbing.IsHash(sha) {
		return nil, ErrNotFound
	}
	c, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, ErrNotFound
	}
	return c, err
}

// Tree lists every entry of commit sha's tree by path. "" is the empty tree.
func (r *Repo) Tree(sha string) (map[string]TreeEntry, error) {
	out := map[string]TreeEntry{}
	err := r.TreeEach(sha, func(e TreeEntry) error {
		out[e.Path] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TreeEach calls fn for every entry of commit sha's tree, and stops at the
// first error fn returns: a caller that will not take a tree past some size
// says so while it is being read, not after. "" is the empty tree.
func (r *Repo) TreeEach(sha string, fn func(TreeEntry) error) error {
	if sha == "" {
		return nil
	}
	c, err := r.commit(sha)
	if err != nil {
		return err
	}
	return r.walk(c, func(name string, e object.TreeEntry) error {
		entry := TreeEntry{Path: name, Mode: fmt.Sprintf("%06o", uint32(e.Mode)), ID: e.Hash.String()}
		if e.Mode != filemode.Submodule {
			size, err := r.repo.Storer.EncodedObjectSize(e.Hash)
			if err != nil {
				return fmt.Errorf("vaultgit: size of %s: %w", e.Hash, err)
			}
			entry.Size = size
		}
		return fn(entry)
	})
}

// maxTreeDepth bounds walk: a tree nested deeper than any real repository is
// refused rather than followed.
const maxTreeDepth = 256

// walk calls fn for every entry of commit c's tree that is not a folder,
// with its full path. It reads the tree objects itself: go-git's own walker
// refuses a tree with a name git allows and a checkout must not trust (a
// control character, a disguised .git), and one such file on GitHub would
// then fail every pull. Nothing here ever becomes a file name on disk, so
// every name is listed as it is; the caller decides what the vault can hold.
func (r *Repo) walk(c *object.Commit, fn func(name string, e object.TreeEntry) error) error {
	var visit func(id plumbing.Hash, prefix string, depth int) error
	visit = func(id plumbing.Hash, prefix string, depth int) error {
		if depth > maxTreeDepth {
			return errors.New("vaultgit: tree is nested too deep")
		}
		tree, err := r.repo.TreeObject(id)
		if err != nil {
			return fmt.Errorf("vaultgit: tree: %w", err)
		}
		for _, e := range tree.Entries {
			if e.Mode == filemode.Dir {
				if err := visit(e.Hash, prefix+e.Name+"/", depth+1); err != nil {
					return err
				}
				continue
			}
			if err := fn(prefix+e.Name, e); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(c.TreeHash, "", 0)
}

// BlobByID returns a blob's bytes by its object id, or ErrNotFound.
func (r *Repo) BlobByID(id string) ([]byte, error) {
	if !plumbing.IsHash(id) {
		return nil, ErrNotFound
	}
	blob, err := r.repo.BlobObject(plumbing.NewHash(id))
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rd, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rd.Close() }()
	return io.ReadAll(rd)
}

// CommitTime is when commit sha was committed, UTC.
func (r *Repo) CommitTime(sha string) (time.Time, error) {
	c, err := r.commit(sha)
	if err != nil {
		return time.Time{}, err
	}
	return c.Committer.When.UTC(), nil
}

// IsAncestor reports whether commit a is b or an ancestor of b.
func (r *Repo) IsAncestor(a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	ca, err := r.commit(a)
	if err != nil {
		return false, err
	}
	cb, err := r.commit(b)
	if err != nil {
		return false, err
	}
	return ca.IsAncestor(cb)
}

// MergeBase is the best common ancestor of commits a and b, or "" when their
// histories share nothing. With several best ancestors (a criss-cross) the
// first by id is taken, so every replica picks the same one.
func (r *Repo) MergeBase(a, b string) (string, error) {
	ca, err := r.commit(a)
	if err != nil {
		return "", err
	}
	cb, err := r.commit(b)
	if err != nil {
		return "", err
	}
	bases, err := ca.MergeBase(cb)
	if err != nil {
		return "", fmt.Errorf("vaultgit: merge base: %w", err)
	}
	if len(bases) == 0 {
		return "", nil
	}
	ids := make([]string, 0, len(bases))
	for _, c := range bases {
		ids = append(ids, c.Hash.String())
	}
	sort.Strings(ids)
	return ids[0], nil
}

// setIndex makes the index the tree of commit sha, plus head's entries for
// the paths in carry. No file is touched.
func (r *Repo) setIndex(sha string, head *object.Commit, carry []string) error {
	c, err := r.commit(sha)
	if err != nil {
		return err
	}
	idx := &index.Index{Version: 2}
	seen := map[string]bool{}
	err = r.walk(c, func(name string, e object.TreeEntry) error {
		idx.Entries = append(idx.Entries, &index.Entry{Name: name, Hash: e.Hash, Mode: e.Mode})
		seen[name] = true
		return nil
	})
	if err != nil {
		return err
	}
	if head != nil && len(carry) > 0 {
		want := map[string]bool{}
		for _, p := range carry {
			want[p] = !seen[p]
		}
		err := r.walk(head, func(name string, e object.TreeEntry) error {
			if want[name] {
				idx.Entries = append(idx.Entries, &index.Entry{Name: name, Hash: e.Hash, Mode: e.Mode})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Name < idx.Entries[j].Name })
	if err := r.repo.Storer.SetIndex(idx); err != nil {
		return fmt.Errorf("vaultgit: write index: %w", err)
	}
	return nil
}

// FastForward moves the branch and the index to commit sha without touching
// a file (sync spec D10 step 9): the index first, then the ref, in one
// update. The caller has checked that HEAD is an ancestor of sha; that is
// checked again here, and anything else is refused. No commit is made.
// Whatever raw/ has beyond sha's tree is an ordinary pending change for the
// next commit.
func (r *Repo) FastForward(sha string) error {
	head, err := r.Head()
	if err != nil {
		return err
	}
	if head != "" {
		ok, err := r.IsAncestor(head, sha)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("vaultgit: %s is not a fast-forward of HEAD", sha)
		}
	}
	if err := r.setIndex(sha, nil, nil); err != nil {
		return err
	}
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(localBranch), plumbing.NewHash(sha))
	if err := r.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("vaultgit: move %s: %w", localBranch, err)
	}
	return nil
}

// MergeCommit joins commit theirs into the branch without touching a file
// (sync spec D10 step 9). The index becomes theirs' tree, plus HEAD's entries
// for the paths in carry (held paths the other side never had); the working
// tree is staged on top of it as any commit stages it, honouring the held
// list, which the caller wrote first; and the result is committed with two
// parents, HEAD and theirs. The branch moves once, to that commit, so a
// crash before it leaves HEAD where it was and nothing unreachable. A merge
// that changes no file is still a commit: it is what makes the next push a
// fast-forward for the remote.
func (r *Repo) MergeCommit(theirs string, carry []string, msg Message, maxFileSize int64, when time.Time) (sha string, err error) {
	headRef, err := r.repo.Head()
	if err != nil {
		return "", fmt.Errorf("vaultgit: merge: %w", err)
	}
	head, err := r.repo.CommitObject(headRef.Hash())
	if err != nil {
		return "", fmt.Errorf("vaultgit: merge: %w", err)
	}
	if err := r.setIndex(theirs, head, carry); err != nil {
		return "", err
	}
	wt, _, err := r.stage(maxFileSize)
	if err != nil {
		return "", err
	}
	sig := Author
	sig.When = when
	h, err := wt.Commit(msg.render(nil), &git.CommitOptions{
		Author: &sig, Committer: &sig, AllowEmptyCommits: true,
		Parents: []plumbing.Hash{headRef.Hash(), plumbing.NewHash(theirs)},
	})
	if err != nil {
		return "", fmt.Errorf("vaultgit: merge commit: %w", err)
	}
	return h.String(), nil
}

// foreignFile lists the commits a pull brought into the repository: commits
// made somewhere else. It lives inside .git, like the held list.
const foreignFile = "lv-foreign"

func (r *Repo) foreignPath() string { return filepath.Join(r.root, ".git", foreignFile) }

// MarkForeign records every commit that theirs has and head ("" = no
// history) has not as made elsewhere, before a pull makes them part of the
// branch (sync spec D10 step 9). Log reads the record: who a commit says made
// a change (the LV-Actor trailer) is believed only for commits this server
// made, and the committer's e-mail cannot say which those are, since anyone
// who can push to the repository can set it.
func (r *Repo) MarkForeign(theirs, head string) error {
	ours := map[plumbing.Hash]bool{}
	if head != "" {
		hc, err := r.commit(head)
		if err != nil {
			return err
		}
		err = object.NewCommitPreorderIter(hc, nil, nil).ForEach(func(c *object.Commit) error {
			ours[c.Hash] = true
			return nil
		})
		if err != nil {
			return fmt.Errorf("vaultgit: history: %w", err)
		}
	}
	tc, err := r.commit(theirs)
	if err != nil {
		return err
	}
	known, err := r.foreign()
	if err != nil {
		return err
	}
	var add []string
	if !ours[tc.Hash] {
		err = object.NewCommitPreorderIter(tc, ours, nil).ForEach(func(c *object.Commit) error {
			if !known[c.Hash] {
				add = append(add, c.Hash.String())
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("vaultgit: history: %w", err)
		}
	}
	if len(add) == 0 {
		return nil
	}
	f, err := os.OpenFile(r.foreignPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("vaultgit: record remote commits: %w", err)
	}
	_, err = f.WriteString(strings.Join(add, "\n") + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("vaultgit: record remote commits: %w", err)
	}
	return nil
}

// foreign is the recorded set. No file is none. A line that is not a commit
// id (a write cut short) is skipped.
func (r *Repo) foreign() (map[plumbing.Hash]bool, error) {
	b, err := os.ReadFile(r.foreignPath())
	if errors.Is(err, fs.ErrNotExist) {
		return map[plumbing.Hash]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vaultgit: read remote commits: %w", err)
	}
	out := map[plumbing.Hash]bool{}
	for line := range strings.SplitSeq(string(b), "\n") {
		if plumbing.IsHash(line) {
			out[plumbing.NewHash(line)] = true
		}
	}
	return out, nil
}
