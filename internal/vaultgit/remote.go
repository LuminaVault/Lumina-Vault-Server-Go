package vaultgit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// RemoteName is the one remote GitHub sync keeps (sync spec D13).
const RemoteName = "origin"

// localBranch is the branch every vault repository lives on (Init).
const localBranch = "main"

// tokenUser is the basic-auth user GitHub expects beside an installation token.
const tokenUser = "x-access-token"

var (
	// ErrNonFastForward: the remote branch has commits the local history does
	// not contain. The push was not sent, or the remote refused it; nothing is
	// ever forced. The caller pulls and tries again (sync spec D6).
	ErrNonFastForward = errors.New("vaultgit: push is not a fast-forward")
	// ErrPushRejected: the remote took the pack and refused the update of the
	// branch: branch protection, a ruleset or a hook.
	ErrPushRejected = errors.New("vaultgit: push rejected by the remote")
	// ErrRemoteNotFound: the remote answered 404 or a redirect, which is never
	// followed. The repository was renamed, transferred, deleted or hidden
	// from this token.
	ErrRemoteNotFound = errors.New("vaultgit: remote repository not found")
	// ErrRemoteUnauthorized is a 401: the token is not valid (any more).
	ErrRemoteUnauthorized = errors.New("vaultgit: remote refused the credentials")
	// ErrRemoteForbidden is a 403: the token may not write there.
	ErrRemoteForbidden = errors.New("vaultgit: remote forbids the push")
	// ErrNoRemote: SetRemote was never called.
	ErrNoRemote = errors.New("vaultgit: no remote")
	// ErrPackTooLarge: the pack a push would send is larger than the caller
	// allows (PushOptions.MaxPackBytes). The request was never sent.
	ErrPackTooLarge = errors.New("vaultgit: pack is larger than the limit")
)

// Git over http(s) never follows a redirect in this process, whoever set the
// client up: a renamed repository answers 301, and the caller has to learn
// the new name (Push fails with ErrRemoteNotFound) rather than have the token
// follow wherever a redirect points.
func init() { SetHTTPClient(&http.Client{}) }

// SetHTTPClient makes every git request over http(s) in this process go
// through c, with redirects refused. Call it once at start-up, before any
// Push; the registry it writes to is go-git's process-wide one and is not
// synchronized.
func SetHTTPClient(c *http.Client) {
	guarded := *c
	guarded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := guarded.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	guarded.Transport = budgetTransport{base}
	t := guardedTransport{githttp.NewClient(&guarded)}
	client.InstallProtocol("https", t)
	client.InstallProtocol("http", t)
}

// go-git's http transport copies the whole pack of a push into memory before
// it sends the request, and reads it from the encoder without looking at the
// context (transport/http/receive_pack.go, packp/updreq_encode.go). So a
// deadline or a cancellation did nothing until the request itself started,
// and nothing bounded the copy. guardedTransport puts a reader between the
// two that fails once the context is done or the pack passes the limit Push
// was given: the copy stops, the encoder is unblocked, and the request is
// never made. What it cannot shorten is the walk that decides which objects
// to send, which runs before the first byte and reads no context either.
type guardedTransport struct{ transport.Transport }

func (t guardedTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	s, err := t.Transport.NewReceivePackSession(ep, auth)
	if err != nil {
		return nil, err
	}
	return guardedSession{s}, nil
}

type guardedSession struct{ transport.ReceivePackSession }

func (s guardedSession) ReceivePack(ctx context.Context, req *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	if req.Packfile != nil {
		limit, _ := ctx.Value(packLimitKey{}).(int64)
		req.Packfile = &packGuard{ReadCloser: req.Packfile, ctx: ctx, limit: limit}
	}
	return s.ReceivePackSession.ReceivePack(ctx, req)
}

type packLimitKey struct{}

// A fetch has no pack of its own to guard: go-git writes what the remote
// sends straight to the object store, however much that is. fetchBudget is
// the most a fetch may read, over all of its requests; budgetTransport
// counts the answers of every request whose context carries one, and fails
// the read that goes past it.
type fetchBudget struct {
	limit int64
	read  atomic.Int64
	spent atomic.Bool
}

type fetchBudgetKey struct{}

type budgetTransport struct{ base http.RoundTripper }

func (t budgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err != nil || res == nil || res.Body == nil {
		return res, err
	}
	if b, ok := req.Context().Value(fetchBudgetKey{}).(*fetchBudget); ok {
		res.Body = &budgetBody{ReadCloser: res.Body, budget: b}
	}
	return res, nil
}

type budgetBody struct {
	io.ReadCloser
	budget *fetchBudget
}

func (b *budgetBody) Read(p []byte) (int, error) {
	if b.budget.spent.Load() {
		return 0, ErrPackTooLarge
	}
	n, err := b.ReadCloser.Read(p)
	if b.budget.read.Add(int64(n)) > b.budget.limit {
		b.budget.spent.Store(true)
		return 0, ErrPackTooLarge
	}
	return n, err
}

// packGuard is the pack of a push on its way into go-git's buffer.
type packGuard struct {
	io.ReadCloser
	ctx   context.Context
	limit int64 // 0: none
	n     int64
}

func (g *packGuard) Read(p []byte) (int, error) {
	if err := g.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := g.ReadCloser.Read(p)
	g.n += int64(n)
	if g.limit > 0 && g.n > g.limit {
		return 0, ErrPackTooLarge
	}
	return n, err
}

// ObjectsSize is the size on disk of .git/objects: every object the
// repository has, compressed. It is an upper bound on a first push's pack,
// and a loose one: right after a repack the old packs are still there.
func (r *Repo) ObjectsSize() (int64, error) {
	var total int64
	err := filepath.WalkDir(filepath.Join(r.root, git.GitDirName, "objects"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) { // repacked or pruned mid-walk
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("vaultgit: objects size: %w", err)
	}
	return total, nil
}

// writeConfig replaces .git/config in one step: a temp file beside it, synced,
// then renamed over it. go-git's own SetConfig truncates the file and writes
// it in place, so a crash between the two left a repository with no config.
func (r *Repo) writeConfig(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	b, err := cfg.Marshal()
	if err != nil {
		return err
	}
	dir := filepath.Join(r.root, git.GitDirName)
	tmp, err := os.CreateTemp(dir, "config.lv-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "config"))
}

// SetRemote points origin at url, creating or replacing it. The URL must not
// hold credentials: they are passed per call and never written to .git/config.
// When origin pointed somewhere else, its refs/remotes/origin/* refs go too:
// they describe the other repository (a disconnect whose clean-up lost the
// race with a reconnect leaves them), and nothing may read them as this one's.
func (r *Repo) SetRemote(url string) error {
	if u, err := neturl.Parse(url); err != nil || u.User != nil {
		return errors.New("vaultgit: remote URL is malformed or holds credentials")
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return fmt.Errorf("vaultgit: read config: %w", err)
	}
	if rc, ok := cfg.Remotes[RemoteName]; ok && len(rc.URLs) == 1 && rc.URLs[0] == url {
		return nil
	}
	if err := r.removeTrackingRefs(); err != nil {
		return err
	}
	cfg.Remotes[RemoteName] = &config.RemoteConfig{
		Name:  RemoteName,
		URLs:  []string{url},
		Fetch: []config.RefSpec{config.RefSpec("+refs/heads/*:refs/remotes/" + RemoteName + "/*")},
	}
	if err := r.writeConfig(cfg); err != nil {
		return fmt.Errorf("vaultgit: write config: %w", err)
	}
	return nil
}

// RemoteURL is origin's URL, or "" when there is no remote.
func (r *Repo) RemoteURL() (string, error) {
	cfg, err := r.repo.Config()
	if err != nil {
		return "", fmt.Errorf("vaultgit: read config: %w", err)
	}
	if rc, ok := cfg.Remotes[RemoteName]; ok && len(rc.URLs) > 0 {
		return rc.URLs[0], nil
	}
	return "", nil
}

// RemoveRemote deletes origin and every refs/remotes/origin/* ref (sync spec
// D13). Local history, the working tree and the held list stay. Removing a
// remote that does not exist is not an error.
func (r *Repo) RemoveRemote() error {
	cfg, err := r.repo.Config()
	if err != nil {
		return fmt.Errorf("vaultgit: read config: %w", err)
	}
	if _, ok := cfg.Remotes[RemoteName]; ok {
		delete(cfg.Remotes, RemoteName)
		if err := r.writeConfig(cfg); err != nil {
			return fmt.Errorf("vaultgit: write config: %w", err)
		}
	}
	return r.removeTrackingRefs()
}

func (r *Repo) removeTrackingRefs() error {
	refs, err := r.repo.References()
	if err != nil {
		return fmt.Errorf("vaultgit: list refs: %w", err)
	}
	var stale []plumbing.ReferenceName
	prefix := "refs/remotes/" + RemoteName + "/"
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if strings.HasPrefix(ref.Name().String(), prefix) {
			stale = append(stale, ref.Name())
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("vaultgit: list refs: %w", err)
	}
	for _, name := range stale {
		if err := r.repo.Storer.RemoveReference(name); err != nil {
			return fmt.Errorf("vaultgit: remove %s: %w", name, err)
		}
	}
	return nil
}

// RemoteRefs lists the refs/remotes/origin/* refs, for tests of RemoveRemote.
func (r *Repo) RemoteRefs() ([]string, error) {
	refs, err := r.repo.References()
	if err != nil {
		return nil, err
	}
	var out []string
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if strings.HasPrefix(ref.Name().String(), "refs/remotes/"+RemoteName+"/") {
			out = append(out, ref.Name().String())
		}
		return nil
	})
	return out, err
}

// PushOptions bound one push.
type PushOptions struct {
	// MaxPackBytes refuses, with ErrPackTooLarge and before anything is sent,
	// a push whose pack is larger. Zero is no limit.
	MaxPackBytes int64
}

// PushResult is what a push did.
type PushResult struct {
	// SHA is the commit the remote branch is at: the local tip.
	SHA string
	// Sent is false when the remote already had it and nothing was sent.
	Sent bool
}

// Push is PushWith without limits, for callers that only want the commit.
func (r *Repo) Push(ctx context.Context, branch, token string) (sha string, err error) {
	res, err := r.PushWith(ctx, branch, token, PushOptions{})
	return res.SHA, err
}

// PushWith sends the local branch to origin's branch with an installation
// token. It is never forced: when the remote has commits the local history
// lacks, it fails with ErrNonFastForward and sends nothing. A remote already
// at the local tip is success, with Sent false. ctx bounds the transport and
// the building of the pack (guardedTransport). The token is used for this
// call only and written nowhere.
func (r *Repo) PushWith(ctx context.Context, branch, token string, opts PushOptions) (PushResult, error) {
	if !ValidBranchName(branch) {
		return PushResult{}, fmt.Errorf("vaultgit: invalid branch %q", branch)
	}
	head, err := r.Head()
	if err != nil {
		return PushResult{}, err
	}
	if head == "" {
		return PushResult{}, errors.New("vaultgit: nothing to push")
	}
	remote, err := r.repo.Remote(RemoteName)
	if errors.Is(err, git.ErrRemoteNotFound) {
		return PushResult{}, ErrNoRemote
	}
	if err != nil {
		return PushResult{}, fmt.Errorf("vaultgit: remote: %w", err)
	}
	if opts.MaxPackBytes > 0 {
		ctx = context.WithValue(ctx, packLimitKey{}, opts.MaxPackBytes)
	}
	spec := config.RefSpec(plumbing.NewBranchReferenceName(localBranch).String() + ":" + plumbing.NewBranchReferenceName(branch).String())
	err = remote.PushContext(ctx, &git.PushOptions{
		RemoteName: RemoteName,
		RefSpecs:   []config.RefSpec{spec}, // no leading "+": never a forced update
		Auth:       basicAuth(token),
		Force:      false,
	})
	switch {
	case err == nil:
		return PushResult{SHA: head, Sent: true}, nil
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		return PushResult{SHA: head}, nil
	}
	return PushResult{}, pushError(err)
}

func basicAuth(token string) *githttp.BasicAuth {
	return &githttp.BasicAuth{Username: tokenUser, Password: token}
}

// nonFastForward is the text go-git's push fails with before sending anything
// when the remote tip is not an ancestor of the local one. go-git has no
// sentinel for it (git.ErrNonFastForwardUpdate belongs to the worktree), so
// the text is matched here and nowhere else; a test pins it.
const nonFastForward = "non-fast-forward update"

// movedStatuses are what a remote says when the branch moved between its
// advertisement and the update: the push lost a race and is retried after a
// pull, like any other non-fast-forward.
var movedStatuses = []string{"non-fast-forward", "fetch first", "failed to update ref", "cannot lock ref", "stale info"}

// remoteError sorts a failed request to the remote (a fetch, a listing, the
// start of a push) into the errors above. Anything else is returned wrapped
// and is transient to the caller.
func remoteError(op string, err error) error {
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("%w: %w", ErrRemoteNotFound, err)
	case redirected(err):
		return fmt.Errorf("%w: redirected", ErrRemoteNotFound)
	case errors.Is(err, transport.ErrAuthenticationRequired):
		return fmt.Errorf("%w: %w", ErrRemoteUnauthorized, err)
	case errors.Is(err, transport.ErrAuthorizationFailed):
		return fmt.Errorf("%w: %w", ErrRemoteForbidden, err)
	}
	return fmt.Errorf("vaultgit: %s: %w", op, err)
}

// pushError sorts a failed push into the errors above. Anything else is
// returned wrapped and is transient to the caller. The remote's own text is
// kept in the chain for the log; callers store only the class.
func pushError(err error) error {
	text := err.Error()
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound), redirected(err),
		errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
		return remoteError("push", err)
	case strings.Contains(text, nonFastForward):
		return fmt.Errorf("%w: %w", ErrNonFastForward, err)
	case strings.HasPrefix(text, "command error on "):
		for _, moved := range movedStatuses {
			if strings.Contains(text, moved) {
				return fmt.Errorf("%w: %w", ErrNonFastForward, err)
			}
		}
		return fmt.Errorf("%w: %w", ErrPushRejected, err)
	}
	return fmt.Errorf("vaultgit: push: %w", err)
}

// redirected reports a 3xx answer. go-git wraps it in an UnexpectedError,
// which has no Unwrap, so the chain is walked by hand.
func redirected(err error) bool {
	var unexpected *plumbing.UnexpectedError
	if errors.As(err, &unexpected) {
		err = unexpected.Err
	}
	var status *githttp.Err
	if !errors.As(err, &status) || status.Response == nil {
		return false
	}
	code := status.Response.StatusCode
	return code >= 300 && code < 400
}

// Contains reports whether commit sha is HEAD or an ancestor of HEAD. It is
// the question loop prevention asks about a commit GitHub announces (sync
// spec D12): the decision is by ancestry, never by who pushed. An unknown or
// malformed sha is false.
func (r *Repo) Contains(sha string) (bool, error) {
	if !plumbing.IsHash(sha) {
		return false, nil
	}
	headRef, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if headRef.Hash().String() == sha {
		return true, nil
	}
	c, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	head, err := r.repo.CommitObject(headRef.Hash())
	if err != nil {
		return false, err
	}
	return c.IsAncestor(head)
}
