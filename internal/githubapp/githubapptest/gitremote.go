package githubapptest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
)

// GitPush is one branch update the stub's git side accepted.
type GitPush struct {
	Repo     int64
	Ref      string
	Old, New string
}

// GitRemote is the stub's git side (spec D17): one bare repository per
// repository id, served over smart HTTP at /{owner}/{name}.git by the same
// server. Every request needs basic auth x-access-token:<a token the stub
// minted for that repository and has not revoked>. A repository's old name
// answers 301 to the new one, as GitHub does.
//
// go-git's own receive-pack sets whatever ref it is told to. This one checks
// each update the way GitHub does: the old value must be the branch's current
// tip, and the new one must descend from it. Anything else is refused and,
// when it is a rewrite or a deletion rather than a lost race, counted in
// Forced, so a test can prove nothing was ever forced.
//
// The fields below are guarded by Server.Lock/Unlock when a test changes
// them while requests run.
type GitRemote struct {
	// Reject maps a repository id to the reason every update of it is
	// refused with ("protected branch hook declined").
	Reject map[int64]string
	// Status maps a repository id to an HTTP status every git request for it
	// is answered with (503, 403, ...).
	Status map[int64]int
	// BeforeReceive, when set, runs after a push is authenticated and before
	// it is applied: the place to move the branch under a push in flight.
	BeforeReceive func(repo int64)

	Pushes []GitPush
	// Forced counts refused rewrites and deletions. It must stay 0.
	Forced int
	// Requests counts authenticated git requests; Unauthorized the others.
	Requests     int
	Unauthorized int

	t     testing.TB
	owner *Server
	dir   string
	mu    sync.Mutex // one git operation at a time
	repos map[int64]*git.Repository
}

func newGitRemote(t testing.TB, owner *Server) *GitRemote {
	return &GitRemote{
		Reject: map[int64]string{}, Status: map[int64]int{},
		t: t, owner: owner, dir: t.TempDir(), repos: map[int64]*git.Repository{},
	}
}

// Repo is the bare repository behind a repository id, created empty on first
// use.
func (g *GitRemote) Repo(id int64) *git.Repository {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.repo(id)
}

func (g *GitRemote) repo(id int64) *git.Repository {
	if r := g.repos[id]; r != nil {
		return r
	}
	r, err := git.PlainInit(filepath.Join(g.dir, strconv.FormatInt(id, 10)), true)
	if err != nil {
		g.t.Fatalf("githubapptest: init bare repository: %v", err)
	}
	g.repos[id] = r
	return r
}

// Tip is the commit a branch of the repository is at, or "".
func (g *GitRemote) Tip(id int64, branch string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ref, err := g.repo(id).Storer.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

// Commit adds a commit to a branch as another git client would, straight
// into the bare repository: files are written (a nil content removes the
// path) on top of the branch's tree. It returns the new tip.
func (g *GitRemote) Commit(id int64, branch, message string, files map[string]*string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	bare := g.repo(id)
	name := plumbing.NewBranchReferenceName(branch)
	fs := memfs.New()
	// A second handle on the same objects with a throw-away working tree.
	// Its index is the bare repository's, which nothing else uses.
	repo, err := git.Open(bare.Storer, fs)
	if err != nil {
		g.t.Fatalf("githubapptest: open: %v", err)
	}
	if err := bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, name)); err != nil {
		g.t.Fatalf("githubapptest: set HEAD: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		g.t.Fatalf("githubapptest: worktree: %v", err)
	}
	if _, err := bare.Storer.Reference(name); err == nil {
		if err := wt.Reset(&git.ResetOptions{Mode: git.HardReset}); err != nil {
			g.t.Fatalf("githubapptest: checkout: %v", err)
		}
	}
	for path, content := range files {
		if content == nil {
			if _, err := wt.Remove(path); err != nil {
				g.t.Fatalf("githubapptest: remove %s: %v", path, err)
			}
			continue
		}
		f, err := fs.Create(path)
		if err != nil {
			g.t.Fatalf("githubapptest: create %s: %v", path, err)
		}
		_, _ = f.Write([]byte(*content))
		_ = f.Close()
		if _, err := wt.Add(path); err != nil {
			g.t.Fatalf("githubapptest: add %s: %v", path, err)
		}
	}
	sig := &object.Signature{Name: "Obsidian Git", Email: "someone@example.test", When: time.Now()}
	h, err := wt.Commit(message, &git.CommitOptions{Author: sig, Committer: sig, AllowEmptyCommits: true})
	if err != nil {
		g.t.Fatalf("githubapptest: commit: %v", err)
	}
	return h.String()
}

type oneRepo struct{ s storer.Storer }

func (l oneRepo) Load(*transport.Endpoint) (storer.Storer, error) { return l.s, nil }

const (
	gitInfoRefs    = "/info/refs"
	gitReceivePack = "/git-receive-pack"
	gitUploadPack  = "/git-upload-pack"
)

// serveGit answers a git smart-HTTP request and reports whether the request
// was one.
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request) bool {
	var rest string
	for _, suffix := range []string{gitInfoRefs, gitReceivePack, gitUploadPack} {
		if strings.HasSuffix(r.URL.Path, suffix) {
			rest = suffix
		}
	}
	if rest == "" || !strings.HasSuffix(strings.TrimSuffix(r.URL.Path, rest), ".git") {
		return false
	}
	full := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, rest), "/"), ".git")
	g := s.Git
	_, token, _ := r.BasicAuth()

	s.mu.Lock()
	var id int64
	for _, repo := range s.Repos {
		if repo.FullName == full {
			id = repo.ID
		}
	}
	moved := ""
	if oldID, ok := s.Renamed[full]; ok && id == 0 {
		moved = s.Repos[oldID].FullName
	}
	mint, minted := s.installTokens[token]
	user, _, _ := r.BasicAuth()
	authorized := minted && user == "x-access-token" && slices.Contains(mint.RepositoryIDs, id) &&
		mint.Permissions["contents"] == "write"
	status := g.Status[id]
	reject := g.Reject[id]
	hook := g.BeforeReceive
	switch {
	case moved != "", id == 0:
	case !authorized:
		g.Unauthorized++
	default:
		g.Requests++
	}
	s.mu.Unlock()

	switch {
	case moved != "":
		target := "/" + moved + ".git" + rest
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusMovedPermanently)
		return true
	case id == 0:
		w.WriteHeader(http.StatusNotFound)
		return true
	case !authorized:
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.WriteHeader(http.StatusUnauthorized)
		return true
	case status != 0:
		w.WriteHeader(status)
		return true
	}

	switch {
	case rest == gitInfoRefs && r.Method == http.MethodGet:
		g.advertise(w, r, id)
	case rest == gitReceivePack && r.Method == http.MethodPost:
		if hook != nil {
			hook(id)
		}
		g.receive(w, r, id, reject)
	case rest == gitUploadPack && r.Method == http.MethodPost:
		g.upload(w, r, id)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
	return true
}

func (g *GitRemote) advertise(w http.ResponseWriter, r *http.Request, id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	service := r.URL.Query().Get("service")
	srv := server.NewServer(oneRepo{g.repo(id).Storer})
	var (
		ar  *packp.AdvRefs
		err error
	)
	switch service {
	case "git-receive-pack":
		var sess transport.ReceivePackSession
		if sess, err = srv.NewReceivePackSession(&transport.Endpoint{}, nil); err == nil {
			ar, err = sess.AdvertisedReferencesContext(r.Context())
		}
	case "git-upload-pack":
		var sess transport.UploadPackSession
		if sess, err = srv.NewUploadPackSession(&transport.Endpoint{}, nil); err == nil {
			ar, err = sess.AdvertisedReferencesContext(r.Context())
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ar.Prefix = [][]byte{[]byte("# service=" + service), pktline.Flush}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	_ = ar.Encode(w)
}

func (g *GitRemote) receive(w http.ResponseWriter, r *http.Request, id int64, reject string) {
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(r.Body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	repo := g.repo(id)
	rs := packp.NewReportStatus()
	rs.UnpackStatus = "ok"
	if req.Packfile != nil {
		pack, err := io.ReadAll(req.Packfile)
		if err == nil && len(pack) > 0 {
			err = packfile.UpdateObjectStorage(repo.Storer, bytes.NewReader(pack))
		}
		if err != nil {
			rs.UnpackStatus = "unpacker error"
		}
	}
	var accepted []GitPush
	forced := 0
	for _, cmd := range req.Commands {
		current := plumbing.ZeroHash
		if ref, err := repo.Storer.Reference(cmd.Name); err == nil {
			current = ref.Hash()
		}
		status := "ok"
		switch {
		case rs.UnpackStatus != "ok":
			status = "unpacker error"
		case reject != "":
			status = reject
		case cmd.Old != current:
			status = "failed to update ref" // the branch moved under the push
		case cmd.New.IsZero():
			forced++
			status = "deletion of the current branch prohibited"
		case !current.IsZero() && !descends(repo, cmd.New, current):
			forced++
			status = "non-fast-forward"
		default:
			if err := repo.Storer.SetReference(plumbing.NewHashReference(cmd.Name, cmd.New)); err != nil {
				status = "failed to update ref"
				break
			}
			accepted = append(accepted, GitPush{Repo: id, Ref: cmd.Name.String(), Old: cmd.Old.String(), New: cmd.New.String()})
		}
		rs.CommandStatuses = append(rs.CommandStatuses, &packp.CommandStatus{ReferenceName: cmd.Name, Status: status})
	}
	g.owner.mu.Lock()
	g.Pushes = append(g.Pushes, accepted...)
	g.Forced += forced
	g.owner.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	_ = rs.Encode(w)
}

// descends reports whether commit tip has ancestor base.
func descends(repo *git.Repository, tip, base plumbing.Hash) bool {
	if tip == base {
		return true
	}
	t, err := repo.CommitObject(tip)
	if err != nil {
		return false
	}
	b, err := repo.CommitObject(base)
	if err != nil {
		return false
	}
	ok, err := b.IsAncestor(t)
	return err == nil && ok
}

func (g *GitRemote) upload(w http.ResponseWriter, r *http.Request, id int64) {
	req := packp.NewUploadPackRequest()
	if err := req.Decode(r.Body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	sess, err := server.NewServer(oneRepo{g.repo(id).Storer}).NewUploadPackSession(&transport.Endpoint{}, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp, err := sess.UploadPack(r.Context(), req)
	if err != nil {
		if errors.Is(err, transport.ErrEmptyUploadPackRequest) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	_ = resp.Encode(w)
}

// GitPushes is a copy of the updates accepted so far.
func (s *Server) GitPushes() []GitPush {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Git.Pushes)
}

// GitForced is how many rewrites or deletions the git side refused.
func (s *Server) GitForced() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Git.Forced
}

// Entry is one tree entry for CommitEntries. Mode "" is a regular file
// (100644); 120000 is a symlink whose target is Data; 160000 is a submodule
// link whose commit id is Data, as 40 hex characters. A nil *Entry removes
// the path.
type Entry struct {
	Mode string
	Data []byte
}

// CommitEntries adds a commit to a branch the way another git client could,
// built from objects alone: any path git allows (a space, a dot-folder), a
// symlink, a submodule link, a file of any size. parents, when given,
// replace the branch's tip as the commit's parents (none: an unrelated
// history); the entries always go on top of the branch's current tree. It
// returns the new tip.
func (g *GitRemote) CommitEntries(id int64, branch, message string, entries map[string]*Entry, parents ...string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	bare := g.repo(id)
	name := plumbing.NewBranchReferenceName(branch)
	type leaf struct {
		mode filemode.FileMode
		hash plumbing.Hash
	}
	files := map[string]leaf{}
	var tip []plumbing.Hash
	if ref, err := bare.Storer.Reference(name); err == nil {
		tip = []plumbing.Hash{ref.Hash()}
		c, err := bare.CommitObject(ref.Hash())
		if err != nil {
			g.t.Fatalf("githubapptest: tip: %v", err)
		}
		g.leaves(bare, c.TreeHash, "", func(p string, e object.TreeEntry) { files[p] = leaf{e.Mode, e.Hash} })
	}
	if parents != nil {
		tip = nil
		for _, p := range parents {
			if p != "" {
				tip = append(tip, plumbing.NewHash(p))
			}
		}
	}
	for p, e := range entries {
		if e == nil {
			delete(files, p)
			continue
		}
		mode := filemode.Regular
		switch e.Mode {
		case "", "100644":
		case "100755":
			mode = filemode.Executable
		case "120000":
			mode = filemode.Symlink
		case "160000":
			files[p] = leaf{filemode.Submodule, plumbing.NewHash(string(e.Data))}
			continue
		default:
			g.t.Fatalf("githubapptest: mode %q", e.Mode)
		}
		obj := bare.Storer.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, err := obj.Writer()
		if err != nil {
			g.t.Fatalf("githubapptest: blob: %v", err)
		}
		_, _ = w.Write(e.Data)
		_ = w.Close()
		h, err := bare.Storer.SetEncodedObject(obj)
		if err != nil {
			g.t.Fatalf("githubapptest: blob: %v", err)
		}
		files[p] = leaf{mode, h}
	}
	var build func(prefix string) plumbing.Hash
	build = func(prefix string) plumbing.Hash {
		var tree object.Tree
		dirs := map[string]bool{}
		for p, l := range files {
			rest, ok := strings.CutPrefix(p, prefix)
			if !ok {
				continue
			}
			if dir, _, nested := strings.Cut(rest, "/"); nested {
				dirs[dir] = true
				continue
			}
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: rest, Mode: l.mode, Hash: l.hash})
		}
		for dir := range dirs {
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: dir, Mode: filemode.Dir, Hash: build(prefix + dir + "/")})
		}
		sort.Sort(object.TreeEntrySorter(tree.Entries))
		obj := bare.Storer.NewEncodedObject()
		if err := tree.Encode(obj); err != nil {
			g.t.Fatalf("githubapptest: tree: %v", err)
		}
		h, err := bare.Storer.SetEncodedObject(obj)
		if err != nil {
			g.t.Fatalf("githubapptest: tree: %v", err)
		}
		return h
	}
	sig := object.Signature{Name: "Obsidian Git", Email: "someone@example.test", When: time.Now()}
	commit := object.Commit{Author: sig, Committer: sig, Message: message, TreeHash: build(""), ParentHashes: tip}
	obj := bare.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		g.t.Fatalf("githubapptest: commit: %v", err)
	}
	h, err := bare.Storer.SetEncodedObject(obj)
	if err != nil {
		g.t.Fatalf("githubapptest: commit: %v", err)
	}
	if err := bare.Storer.SetReference(plumbing.NewHashReference(name, h)); err != nil {
		g.t.Fatalf("githubapptest: set %s: %v", name, err)
	}
	if err := bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, name)); err != nil {
		g.t.Fatalf("githubapptest: set HEAD: %v", err)
	}
	return h.String()
}

// Entries reads a branch's tree: path to mode and bytes (for a submodule
// link, its commit id).
func (g *GitRemote) Entries(id int64, branch string) map[string]Entry {
	g.mu.Lock()
	defer g.mu.Unlock()
	bare := g.repo(id)
	out := map[string]Entry{}
	ref, err := bare.Storer.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return out
	}
	c, err := bare.CommitObject(ref.Hash())
	if err != nil {
		g.t.Fatalf("githubapptest: tip: %v", err)
	}
	g.leaves(bare, c.TreeHash, "", func(p string, e object.TreeEntry) {
		if e.Mode == filemode.Submodule {
			out[p] = Entry{Mode: "160000", Data: []byte(e.Hash.String())}
			return
		}
		blob, err := bare.BlobObject(e.Hash)
		if err != nil {
			g.t.Fatalf("githubapptest: blob %s: %v", p, err)
		}
		r, _ := blob.Reader()
		data, _ := io.ReadAll(r)
		_ = r.Close()
		out[p] = Entry{Mode: fmt.Sprintf("%06o", uint32(e.Mode)), Data: data}
	})
	return out
}

// leaves calls fn for every entry of a tree that is not a folder. It reads
// the tree objects itself: go-git's walker refuses names git allows (a
// control character), and a test has to be able to see them.
func (g *GitRemote) leaves(repo *git.Repository, id plumbing.Hash, prefix string, fn func(path string, e object.TreeEntry)) {
	tree, err := repo.TreeObject(id)
	if err != nil {
		g.t.Fatalf("githubapptest: tree: %v", err)
	}
	for _, e := range tree.Entries {
		if e.Mode == filemode.Dir {
			g.leaves(repo, e.Hash, prefix+e.Name+"/", fn)
			continue
		}
		fn(prefix+e.Name, e)
	}
}

// Parents is the parent commits of a commit in the repository.
func (g *GitRemote) Parents(id int64, sha string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, err := g.repo(id).CommitObject(plumbing.NewHash(sha))
	if err != nil {
		g.t.Fatalf("githubapptest: commit %s: %v", sha, err)
	}
	var out []string
	for _, p := range c.ParentHashes {
		out = append(out, p.String())
	}
	return out
}
