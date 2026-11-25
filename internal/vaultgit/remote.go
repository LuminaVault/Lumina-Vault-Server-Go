package vaultgit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
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
	t := githttp.NewClient(&guarded)
	client.InstallProtocol("https", t)
	client.InstallProtocol("http", t)
}

// SetRemote points origin at url, creating or replacing it. The URL must not
// hold credentials: they are passed per call and never written to .git/config.
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
	cfg.Remotes[RemoteName] = &config.RemoteConfig{
		Name:  RemoteName,
		URLs:  []string{url},
		Fetch: []config.RefSpec{config.RefSpec("+refs/heads/*:refs/remotes/" + RemoteName + "/*")},
	}
	if err := r.repo.Storer.SetConfig(cfg); err != nil {
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
		if err := r.repo.Storer.SetConfig(cfg); err != nil {
			return fmt.Errorf("vaultgit: write config: %w", err)
		}
	}
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

// Push sends the local branch to origin's branch with an installation token
// and returns the commit pushed. It is never forced: when the remote has
// commits the local history lacks, it fails with ErrNonFastForward and sends
// nothing. A remote already at the local tip is success. ctx bounds the
// transport. The token is used for this call only and written nowhere.
func (r *Repo) Push(ctx context.Context, branch, token string) (sha string, err error) {
	if !ValidBranchName(branch) {
		return "", fmt.Errorf("vaultgit: invalid branch %q", branch)
	}
	head, err := r.Head()
	if err != nil {
		return "", err
	}
	if head == "" {
		return "", errors.New("vaultgit: nothing to push")
	}
	remote, err := r.repo.Remote(RemoteName)
	if errors.Is(err, git.ErrRemoteNotFound) {
		return "", ErrNoRemote
	}
	if err != nil {
		return "", fmt.Errorf("vaultgit: remote: %w", err)
	}
	spec := config.RefSpec(plumbing.NewBranchReferenceName(localBranch).String() + ":" + plumbing.NewBranchReferenceName(branch).String())
	err = remote.PushContext(ctx, &git.PushOptions{
		RemoteName: RemoteName,
		RefSpecs:   []config.RefSpec{spec}, // no leading "+": never a forced update
		Auth:       basicAuth(token),
		Force:      false,
	})
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return head, nil
	}
	return "", pushError(err)
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

// pushError sorts a failed push into the errors above. Anything else is
// returned wrapped and is transient to the caller. The remote's own text is
// kept in the chain for the log; callers store only the class.
func pushError(err error) error {
	text := err.Error()
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("%w: %w", ErrRemoteNotFound, err)
	case redirected(err):
		return fmt.Errorf("%w: redirected", ErrRemoteNotFound)
	case errors.Is(err, transport.ErrAuthenticationRequired):
		return fmt.Errorf("%w: %w", ErrRemoteUnauthorized, err)
	case errors.Is(err, transport.ErrAuthorizationFailed):
		return fmt.Errorf("%w: %w", ErrRemoteForbidden, err)
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
