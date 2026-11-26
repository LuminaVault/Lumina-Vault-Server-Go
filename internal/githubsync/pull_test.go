package githubsync_test

import (
	"bytes"
	"context"
	"lumina/internal/chunkindex"
	"lumina/internal/embedding"
	"lumina/internal/githubapp/githubapptest"
	"lumina/internal/githubsync"
	"lumina/internal/modules/vault"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// MARK: environment

// hookFiles is the vault's write pipeline with a place to stand in front of
// a write: where a test lands a save mid-pull, or holds an apply open.
type hookFiles struct {
	*vault.Files
	mu     sync.Mutex
	before func(in vault.WriteInput)
}

func (h *hookFiles) Write(ctx context.Context, in vault.WriteInput) (*vault.WriteResult, error) {
	h.mu.Lock()
	before := h.before
	h.mu.Unlock()
	if before != nil && in.Reason == "pull" {
		before(in)
	}
	return h.Files.Write(ctx, in)
}

func (h *hookFiles) hook(fn func(in vault.WriteInput)) {
	h.mu.Lock()
	h.before = fn
	h.mu.Unlock()
}

// pullEnv is syncEnv with the real pull behind the syncer: the vault's own
// write pipeline (rows, note memories, chunks, links) over the same root.
type pullEnv struct {
	*syncEnv
	files *hookFiles
	trash *vaultfiles.Store
	pull  *githubsync.Pull
	opts  []func(*githubsync.PullOptions)
}

func newPullEnv(t *testing.T, opts ...func(*githubsync.PullOptions)) *pullEnv {
	t.Helper()
	pe := &pullEnv{opts: opts}
	pe.syncEnv = newSyncEnv(t, pe.realPull())
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		rows, err := pe.pool.Query(context.Background(), `SELECT vault_id::text || ' ' || state || ' err=' || coalesce(error_code, '-') || ' remote=' || coalesce(remote_sha, '-') || ' report=' || report::text FROM github_sync_connections`)
		if err != nil {
			t.Log(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			t.Log(line)
		}
		t.Log(pe.logs.String())
	})
	return pe
}

// realPull builds the pull for one replica's syncer. Every replica shares
// the vault's files; nothing else.
func (pe *pullEnv) realPull() envOption {
	return func(v *syncEnv, o *githubsync.SyncerOptions) {
		if pe.files == nil {
			paths := vaultpaths.Paths{Root: v.root}
			indexer := chunkindex.NewIndexer(chunkindex.NewChunkRepository(v.pool), embedding.Deterministic{}, chunkindex.NewLinkRepository(v.pool))
			pe.files = &hookFiles{Files: vault.NewFiles(v.pool, paths, vault.FilesOptions{Embeddings: embedding.Deterministic{}, Indexer: indexer})}
			pe.trash = vaultfiles.NewStore(v.pool, paths)
		}
		po := githubsync.PullOptions{Files: pe.files, Store: v.store, RawRoot: v.raw, Now: v.clock.now, Log: v.logger()}
		for _, opt := range pe.opts {
			opt(&po)
		}
		p := githubsync.NewPull(po)
		if pe.pull == nil {
			pe.pull = p
		}
		o.Pull = p
	}
}

// upload saves a file the way the vault's routes do: bytes, row, memory.
func (pe *pullEnv) upload(vaultID uuid.UUID, path, body string) *vault.WriteResult {
	pe.t.Helper()
	res, err := pe.files.Files.Write(context.Background(), vault.WriteInput{Vault: vaultID, Actor: vaultID, Path: path, Data: []byte(body)})
	require.NoError(pe.t, err)
	return res
}

func (pe *pullEnv) remove(vaultID uuid.UUID, path string) {
	pe.t.Helper()
	require.NoError(pe.t, pe.files.Delete(context.Background(), vaultID, vaultID, path))
}

// seed uploads files, commits them and waits until GitHub has them.
func (pe *pullEnv) seed(l link, files map[string]string) string {
	pe.t.Helper()
	for p, body := range files {
		pe.upload(l.vault, p, body)
	}
	pe.flush(l.vault)
	return pe.synced(l)
}

func text(body string) *githubapptest.Entry { return &githubapptest.Entry{Data: []byte(body)} }

// remote commits to the connected branch as another git client would.
func (pe *pullEnv) remote(l link, entries map[string]*githubapptest.Entry, parents ...string) string {
	return pe.gh.Git.CommitEntries(l.repo, l.branch, "Edit on GitHub", entries, parents...)
}

func (pe *pullEnv) remoteEntries(l link) map[string]githubapptest.Entry {
	return pe.gh.Git.Entries(l.repo, l.branch)
}

// pullNow asks for a pull, as a webhook would, and waits for the sync.
func (pe *pullEnv) pullNow(l link) {
	pe.t.Helper()
	pe.syncer.NotifyPull(l.vault)
	pe.idle(l.vault)
}

// disk is every vault file under raw/, by path, without .git and the trash.
func (pe *pullEnv) disk(vaultID uuid.UUID) map[string]string {
	pe.t.Helper()
	root := pe.raw(vaultID)
	out := map[string]string{}
	require.NoError(pe.t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(info.Name(), "_deleted_") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	}))
	return out
}

func (pe *pullEnv) fileRow(vaultID uuid.UUID, path string) *vaultfiles.VaultFile {
	pe.t.Helper()
	var id uuid.UUID
	err := pe.pool.QueryRow(context.Background(), `SELECT id FROM vault_files WHERE tenant_id = $1 AND path = $2`, vaultID, path).Scan(&id)
	if err != nil {
		return nil
	}
	row, err := pe.files.Find(context.Background(), vaultID, path)
	require.NoError(pe.t, err)
	return row
}

func (pe *pullEnv) rowPaths(vaultID uuid.UUID) []string {
	pe.t.Helper()
	rows, err := pe.pool.Query(context.Background(), `SELECT path FROM vault_files WHERE tenant_id = $1 ORDER BY path`, vaultID)
	require.NoError(pe.t, err)
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		require.NoError(pe.t, rows.Scan(&p))
		out = append(out, p)
	}
	return out
}

// memory is the note memory made from a vault file: its content and how
// many chunks it has.
func (pe *pullEnv) memory(vaultID, file uuid.UUID) (content string, chunks int, ok bool) {
	pe.t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pe.pool.QueryRow(ctx, `SELECT id, content FROM memories WHERE tenant_id = $1 AND source_vault_file_id = $2`, vaultID, file).Scan(&id, &content); err != nil {
		return "", 0, false
	}
	require.NoError(pe.t, pe.pool.QueryRow(ctx, `SELECT count(*) FROM memory_chunks WHERE memory_id = $1`, id).Scan(&chunks))
	return content, chunks, true
}

// stamp is the conflict-copy name a pull of the remote commit gives path.
func (pe *pullEnv) conflictName(l link, path, remoteCommit string) string {
	pe.t.Helper()
	repo, err := vaultgit.Open(pe.raw(l.vault))
	require.NoError(pe.t, err)
	at, err := repo.CommitTime(remoteCommit)
	require.NoError(pe.t, err)
	return githubsync.ConflictName(path, at, 1)
}

func (pe *pullEnv) held(vaultID uuid.UUID) []string {
	pe.t.Helper()
	repo, err := vaultgit.Open(pe.raw(vaultID))
	require.NoError(pe.t, err)
	held, err := repo.Held()
	require.NoError(pe.t, err)
	return held
}

// MARK: D17, E4.9

// D17 done-when: a commit pushed to GitHub by another git client shows up in
// the app: file, row, note memory, chunks, and a wikilink in another note
// that was waiting for it resolves. It is a fast-forward: HEAD is the remote
// commit itself, no commit is made and no push follows.
func TestRemoteCommitShowsUpInTheVault(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"inbox/a.md": "# A\n\nSee [[plan]] for the rest.\n"})
	ctx := context.Background()
	var resolved bool
	require.NoError(t, pe.pool.QueryRow(ctx, `SELECT resolved FROM vault_links WHERE tenant_id = $1 AND source_path = 'inbox/a.md'`, l.vault).Scan(&resolved))
	require.False(t, resolved, "the link waits for a note that does not exist yet")

	// "Obsidian Git": a second clone commits and pushes.
	token, err := pe.client.InstallationToken(ctx, l.installation, l.repo)
	require.NoError(t, err)
	auth := &githttp.BasicAuth{Username: "x-access-token", Password: token}
	dir := t.TempDir()
	clone, err := git.PlainClone(dir, false, &git.CloneOptions{URL: pe.gh.URL + "/" + l.name + ".git", Auth: auth, ReferenceName: plumbing.NewBranchReferenceName(l.branch), SingleBranch: true})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plan.md"), []byte("# Plan\n\nShip the pull.\n"), 0o600))
	wt, err := clone.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("plan.md")
	require.NoError(t, err)
	theirs, err := wt.Commit("vault backup", &git.CommitOptions{Author: &vaultgitOther, Committer: &vaultgitOther})
	require.NoError(t, err)
	require.NoError(t, clone.Push(&git.PushOptions{Auth: auth}))
	require.Equal(t, theirs.String(), pe.tip(l))

	commits, pushes := pe.commits(l.vault), len(pe.pushes(l))
	pe.pullNow(l)

	require.Equal(t, "# Plan\n\nShip the pull.\n", pe.disk(l.vault)["plan.md"])
	row := pe.fileRow(l.vault, "plan.md")
	require.NotNil(t, row)
	require.Nil(t, row.ProcessedAt, "new to the vault: not compiled yet, like an upload")
	content, chunks, ok := pe.memory(l.vault, row.ID)
	require.True(t, ok)
	require.Contains(t, content, "Ship the pull.")
	require.Positive(t, chunks)
	require.NoError(t, pe.pool.QueryRow(ctx, `SELECT resolved FROM vault_links WHERE tenant_id = $1 AND source_path = 'inbox/a.md'`, l.vault).Scan(&resolved))
	require.True(t, resolved, "the link that waited now points at the pulled note")

	// Fast-forward (D10 step 9, D12).
	require.Equal(t, theirs.String(), pe.head(l.vault), "HEAD is the remote commit")
	require.Equal(t, commits+1, pe.commits(l.vault), "theirs, and no commit of ours")
	c := pe.row(l.vault)
	require.Equal(t, theirs.String(), *c.RemoteSHA)
	require.Equal(t, pe.clock.now(), c.LastPullAt.UTC())
	require.Nil(t, c.ErrorCode)
	pe.flush(l.vault)
	pe.idle(l.vault)
	require.Equal(t, commits+1, pe.commits(l.vault), "the tree is clean: nothing to commit")
	require.Len(t, pe.pushes(l), pushes, "and nothing to push back")

	// D17: the file's history shows the remote commit as GitHub's, and the
	// version restores byte for byte.
	repo, err := vaultgit.Open(pe.raw(l.vault))
	require.NoError(t, err)
	versions, err := repo.Log("plan.md", 0, "")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.Equal(t, theirs.String(), versions[0].SHA)
	require.Equal(t, vaultgit.ReasonGitHub, versions[0].Reason)
	require.Empty(t, versions[0].ActorID)
	old, err := repo.Blob(theirs.String(), "plan.md")
	require.NoError(t, err)
	require.Equal(t, "# Plan\n\nShip the pull.\n", string(old))

	// Asked again, there is nothing to fetch.
	requests := pe.gitRequests()
	pe.pullNow(l)
	require.Equal(t, requests+1, pe.gitRequests(), "one listing, no fetch")
}

var vaultgitOther = object.Signature{Name: "Someone Else", Email: "someone@example.com", When: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}

// D17: each row of D10's table, as a merge of two histories that both moved.
// A conflict keeps both: the original has the local bytes, the copy the
// remote bytes, both have rows, both are on GitHub afterwards. Then D12: the
// merge's own push webhook starts nothing.
func TestPullMergesWhatBothSidesDid(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{
		"keep.md": "kept", "rmod.md": "v1", "rdel.md": "to be deleted on GitHub", "same.md": "v1",
		"conflict.md": "v1", "rdel-lmod.md": "v1", "ldel-rmod.md": "v1", "both-del.md": "v1",
		"old/name.md": "# Moved\n\nthe same bytes\n", "lmod.md": "v1",
	})
	moved := pe.fileRow(l.vault, "old/name.md")
	gone := pe.fileRow(l.vault, "rdel.md")
	base := pe.head(l.vault)

	theirs := pe.remote(l, map[string]*githubapptest.Entry{
		"rmod.md": text("remote v2"), "radd.md": text("# Added\n\non GitHub\n"), "rdel.md": nil, "same.md": text("agreed"),
		"conflict.md": text("remote side"), "rdel-lmod.md": nil, "ldel-rmod.md": text("remote v2"), "both-del.md": nil,
		"old/name.md": nil, "new/name.md": text("# Moved\n\nthe same bytes\n"),
	})
	pe.upload(l.vault, "same.md", "agreed")
	pe.upload(l.vault, "conflict.md", "local side")
	pe.upload(l.vault, "rdel-lmod.md", "local v2")
	pe.upload(l.vault, "lmod.md", "local v2")
	pe.upload(l.vault, "ladd.md", "local only")
	pe.remove(l.vault, "ldel-rmod.md")
	pe.remove(l.vault, "both-del.md")
	pe.listen(nil) // the commit below is pushed by the pull's own sync
	ours := pe.flush(l.vault)
	pe.listen(pe.syncer.OnCommit)

	pe.pullNow(l)
	merge := pe.synced(l)
	copyName := pe.conflictName(l, "conflict.md", theirs)

	want := map[string]string{
		".gitignore": vaultgit.GitIgnore,
		"keep.md":    "kept", "rmod.md": "remote v2", "radd.md": "# Added\n\non GitHub\n", "same.md": "agreed",
		"conflict.md": "local side", copyName: "remote side", "rdel-lmod.md": "local v2", "ldel-rmod.md": "remote v2",
		"new/name.md": "# Moved\n\nthe same bytes\n", "lmod.md": "local v2", "ladd.md": "local only",
	}
	require.Equal(t, want, pe.disk(l.vault))
	delete(want, ".gitignore")
	rows := pe.rowPaths(l.vault)
	require.Len(t, rows, len(want))
	for p := range want {
		require.Contains(t, rows, p)
	}
	require.Regexp(t, regexp.MustCompile(`^conflict\.conflict-\d{8}-\d{6}\.md$`), copyName)

	// Remote delete: in the trash, restorable, its memory gone.
	_, _, hasMemory := pe.memory(l.vault, gone.ID)
	require.False(t, hasMemory)
	trash, err := pe.trash.ListTrash(context.Background(), l.vault)
	require.NoError(t, err)
	var entry *vaultfiles.TrashEntry
	for i := range trash {
		if trash[i].OriginalPath == "rdel.md" {
			entry = &trash[i]
		}
	}
	require.NotNil(t, entry, "the file GitHub deleted is in the trash")
	// Remote rename with identical bytes keeps the vault file id.
	require.Equal(t, moved.ID, pe.fileRow(l.vault, "new/name.md").ID)

	// One two-parent commit, pushed, never forced; GitHub has both versions.
	require.Equal(t, merge, pe.tip(l))
	require.Equal(t, []string{ours, theirs}, pe.gh.Git.Parents(l.repo, merge))
	require.Zero(t, pe.gh.GitForced())
	remote := pe.remoteFiles(l)
	delete(remote, ".gitignore")
	require.Equal(t, want, remote)
	local := pe.localFiles(l.vault)
	require.Equal(t, "pull: merge main from GitHub (1 conflicts)", strings.SplitN(commitMessage(t, pe.raw(l.vault), merge), "\n", 2)[0])
	require.Contains(t, commitMessage(t, pe.raw(l.vault), merge), "LV-Reason: pull")
	require.Equal(t, remote["conflict.md"], local["conflict.md"])
	c := pe.row(l.vault)
	require.Equal(t, merge, *c.RemoteSHA)
	require.Equal(t, []githubsync.Conflict{{Path: "conflict.md", ConflictPath: copyName, At: pe.clock.now()}}, c.Report.Conflicts)
	require.NotEqual(t, base, merge)

	// The file comes back out of the trash, byte for byte.
	_, err = pe.trash.Restore(context.Background(), pe.raw(l.vault), entry, l.vault)
	require.NoError(t, err)
	require.Equal(t, "to be deleted on GitHub", pe.disk(l.vault)["rdel.md"])

	// D12: GitHub announces our own merge. By ancestry it is ours: ignored,
	// with no fetch, no commit and no push.
	pe.flush(l.vault)
	pe.synced(l)
	requests, commits, pushes := pe.gitRequests(), pe.commits(l.vault), len(pe.pushes(l))
	d := githubsync.Delivery{ID: "own-merge", Event: "push", RepoID: l.repo, InstallationID: l.installation, Ref: "refs/heads/main", After: merge}
	inserted, err := pe.store.InsertDelivery(context.Background(), d, pe.clock.now())
	require.NoError(t, err)
	require.True(t, inserted)
	require.Equal(t, githubsync.DeliveryIgnored, pe.syncer.Deliver(context.Background(), d))
	pe.idle(l.vault)
	require.Equal(t, requests, pe.gitRequests(), "nothing was fetched")
	require.Equal(t, commits, pe.commits(l.vault))
	require.Len(t, pe.pushes(l), pushes)
	status, err := pe.store.DeliveryStatus(context.Background(), "own-merge")
	require.NoError(t, err)
	require.Equal(t, githubsync.DeliveryIgnored, status)
}

func commitMessage(t *testing.T, root, sha string) string {
	t.Helper()
	repo, err := git.PlainOpen(root)
	require.NoError(t, err)
	c, err := repo.CommitObject(mustHash(sha))
	require.NoError(t, err)
	return c.Message
}

// D11, D17: a first connect to a repository with a history of its own. With
// the owner's consent the two are merged with an empty base: remote-only
// files are added, local-only files are pushed, a file on both sides with
// other bytes is kept twice. Without it nothing is merged and nothing is
// overwritten: the connection stops and says why.
func TestFirstConnectWithUnrelatedHistories(t *testing.T) {
	pe := newPullEnv(t)
	seedRemote := func(l link) string {
		return pe.remote(l, map[string]*githubapptest.Entry{
			"theirs.md": text("from GitHub"), "shared.md": text("their words"), "same.md": text("identical"),
		})
	}
	local := func(l link) {
		pe.upload(l.vault, "ours.md", "from the vault")
		pe.upload(l.vault, "shared.md", "our words")
		pe.upload(l.vault, "same.md", "identical")
	}

	// No consent.
	refused := pe.connect()
	theirs := seedRemote(refused)
	local(refused)
	pe.flush(refused.vault)
	pe.idle(refused.vault)
	c := pe.row(refused.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State)
	require.Equal(t, githubsync.CodeRemoteNotEmpty, *c.ErrorCode)
	require.Nil(t, c.RemoteSHA)
	require.Equal(t, theirs, pe.tip(refused), "GitHub is untouched")
	require.NotContains(t, pe.disk(refused.vault), "theirs.md", "and so is the vault")
	require.Equal(t, "our words", pe.disk(refused.vault)["shared.md"])
	require.Zero(t, pe.gh.GitForced())
	// "Sync now" does not talk it round: only a reconnect asks the question.
	require.NoError(t, pe.syncer.SyncNow(context.Background(), refused.vault))
	pe.idle(refused.vault)
	require.Equal(t, githubsync.StateNeedsAttention, pe.row(refused.vault).State)

	// Consent, as the connect route stored it.
	l := pe.connect(func(c *githubsync.Connection) { c.MergeExisting = true })
	theirs = seedRemote(l)
	local(l)
	ours := pe.flush(l.vault)
	merge := pe.synced(l)
	copyName := pe.conflictName(l, "shared.md", theirs)
	want := map[string]string{
		"theirs.md": "from GitHub", "ours.md": "from the vault", "same.md": "identical",
		"shared.md": "our words", copyName: "their words",
	}
	disk := pe.disk(l.vault)
	delete(disk, ".gitignore")
	require.Equal(t, want, disk)
	remote := pe.remoteFiles(l)
	delete(remote, ".gitignore")
	require.Equal(t, want, remote)
	require.Equal(t, []string{ours, theirs}, pe.gh.Git.Parents(l.repo, merge))
	require.Zero(t, pe.gh.GitForced())
	require.Len(t, pe.row(l.vault).Report.Conflicts, 1)
	require.NotNil(t, pe.fileRow(l.vault, copyName))
	require.NotNil(t, pe.fileRow(l.vault, "theirs.md"))
}

// D10 step 6, D17: a save that lands between the plan and the apply is not
// overwritten. The remote version becomes a conflict copy.
func TestSaveDuringAPullIsNotOverwritten(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"note.md": "v1", "other.md": "v1"})
	theirs := pe.remote(l, map[string]*githubapptest.Entry{"note.md": text("remote v2"), "fresh.md": text("remote new")})

	var once sync.Once
	pe.files.hook(func(in vault.WriteInput) {
		// The pull is about to write: a user saves both paths first.
		once.Do(func() {
			pe.upload(l.vault, "note.md", "saved mid-pull")
			pe.upload(l.vault, "fresh.md", "created mid-pull")
		})
	})
	pe.pullNow(l)
	pe.files.hook(nil)
	pe.flush(l.vault)
	pe.synced(l)

	disk := pe.disk(l.vault)
	require.Equal(t, "saved mid-pull", disk["note.md"])
	require.Equal(t, "remote v2", disk[pe.conflictName(l, "note.md", theirs)])
	require.Equal(t, "created mid-pull", disk["fresh.md"])
	require.Equal(t, "remote new", disk[pe.conflictName(l, "fresh.md", theirs)])
	remote := pe.remoteFiles(l)
	require.Equal(t, "saved mid-pull", remote["note.md"])
	require.Equal(t, "remote v2", remote[pe.conflictName(l, "note.md", theirs)])
	require.Zero(t, pe.gh.GitForced())
}

const submodule = "1234567890abcdef1234567890abcdef12345678"

func heldEntries() map[string]*githubapptest.Entry {
	return map[string]*githubapptest.Entry{
		".obsidian/app.json":     text(`{"theme":"dark"}`),
		"My Note.md":             text("a name with a space"),
		"board.canvas":           text("{}"),
		"link.md":                {Mode: "120000", Data: []byte("ok.md")},
		"big.pdf":                {Data: bytes.Repeat([]byte("p"), 6<<20)},
		"vendor/lib":             {Mode: "160000", Data: []byte(submodule)},
		"odd\nname.md":           text("git allows a newline"),
		"_deleted_1_abc_note.md": text("looks like the trash"),
		"sub/.gitignore":         text("*.bak\n"),
	}
}

func requireHeldOnRemote(t *testing.T, pe *pullEnv, l link, held map[string]*githubapptest.Entry) {
	t.Helper()
	got := pe.remoteEntries(l)
	for p, e := range held {
		mode := e.Mode
		if mode == "" {
			mode = "100644"
		}
		require.Equal(t, mode, got[p].Mode, "%q", p)
		require.True(t, bytes.Equal(e.Data, got[p].Data), "%q is byte-identical on GitHub", p)
	}
}

// D8, D17: files the vault cannot hold. None reaches raw/, none has a row,
// all are counted and explained, and after the next local save and push,
// after a merge, after a crash between the held list and the ref move, and
// after a disconnect and reconnect, every one is still on GitHub,
// byte-identical. The remote .gitignore keeps its lines and gains the block.
func TestHeldFilesStayOnGitHub(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"mine.md": "local"})
	held := heldEntries()
	entries := map[string]*githubapptest.Entry{"ok.md": text("fine"), ".gitignore": text("node_modules/\n*.log\n")}
	for p, e := range held {
		entries[p] = e
	}
	theirs := pe.remote(l, entries)
	pe.pullNow(l)

	ignore := "node_modules/\n*.log\n# LuminaVault (managed)\n" + vaultgit.GitIgnore + "# end\n"
	require.Equal(t, map[string]string{"mine.md": "local", "ok.md": "fine", ".gitignore": ignore}, pe.disk(l.vault))
	require.Equal(t, []string{"mine.md", "ok.md"}, pe.rowPaths(l.vault))
	require.Len(t, pe.held(l.vault), len(held))
	c := pe.row(l.vault)
	require.Equal(t, len(held), c.Report.HeldCount)
	reasons := map[string]githubsync.HeldReason{}
	for _, h := range c.Report.Held {
		reasons[h.Path] = h.Reason
	}
	require.Equal(t, map[string]githubsync.HeldReason{
		".obsidian/app.json": githubsync.HeldHidden, "My Note.md": githubsync.HeldBadName,
		"board.canvas": githubsync.HeldUnsupported, "link.md": githubsync.HeldNotAFile, "big.pdf": githubsync.HeldTooLarge,
		"vendor/lib": githubsync.HeldNotAFile, "odd\nname.md": githubsync.HeldBadName,
		"_deleted_1_abc_note.md": githubsync.HeldHidden, "sub/.gitignore": githubsync.HeldHidden,
	}, reasons)
	require.Equal(t, theirs, pe.head(l.vault), "a fast-forward")

	// The next local save and push: the managed block goes up with it, and
	// nothing held is lost.
	pe.upload(l.vault, "mine.md", "local, edited")
	pe.flush(l.vault)
	pe.synced(l)
	requireHeldOnRemote(t, pe, l, held)
	got := pe.remoteEntries(l)
	require.Equal(t, ignore, string(got[".gitignore"].Data))
	require.Equal(t, "local, edited", string(got["mine.md"].Data))

	// A held file arrives while the vault also changed: a merge, and the
	// pushed merge commit still has it. A held file GitHub edits is updated
	// there and stays out of the vault.
	held["Another Note.md"] = text("second")
	held["My Note.md"] = text("a name with a space, edited on GitHub")
	theirs = pe.remote(l, map[string]*githubapptest.Entry{"Another Note.md": held["Another Note.md"], "My Note.md": held["My Note.md"]})
	pe.upload(l.vault, "mine.md", "local, edited twice")
	pe.listen(nil)
	ours := pe.flush(l.vault)
	pe.listen(pe.syncer.OnCommit)
	pe.pullNow(l)
	merge := pe.synced(l)
	require.Equal(t, []string{ours, theirs}, pe.gh.Git.Parents(l.repo, merge))
	requireHeldOnRemote(t, pe, l, held)
	require.NotContains(t, pe.disk(l.vault), "Another Note.md")
	require.Equal(t, len(held), pe.row(l.vault).Report.HeldCount)

	// Killed between writing the held list and moving the ref: the list
	// names a path the index does not have yet. The next save, commit and
	// pull lose nothing.
	held["Third Note.md"] = text("third")
	pe.remote(l, map[string]*githubapptest.Entry{"Third Note.md": held["Third Note.md"]})
	require.NoError(t, pe.committer.WithRepo(context.Background(), l.vault, func(r *vaultgit.Repo) error {
		return r.SetHeld(append(pe.held(l.vault), "Third Note.md"))
	}))
	pe.upload(l.vault, "mine.md", "after the crash")
	pe.flush(l.vault)
	pe.synced(l)
	requireHeldOnRemote(t, pe, l, held)
	require.Equal(t, "after the crash", string(pe.remoteEntries(l)["mine.md"].Data))

	// A file that turns up at a held path does not replace GitHub's entry.
	pe.write(l.vault, "big.pdf", "a small file with the big one's name")
	pe.flush(l.vault)
	pe.idle(l.vault)
	requireHeldOnRemote(t, pe, l, held)
	require.NoError(t, os.Remove(filepath.Join(pe.raw(l.vault), "big.pdf")))

	// D13, D17: disconnect, a local commit, reconnect to the same repository.
	conn, err := pe.store.Delete(context.Background(), l.vault)
	require.NoError(t, err)
	pe.syncer.Disconnected(context.Background(), l.vault)
	require.Len(t, pe.held(l.vault), len(held), "the list survives a disconnect")
	pe.upload(l.vault, "mine.md", "while disconnected")
	pe.flush(l.vault)
	pe.idle(l.vault)
	conn.RemoteSHA, conn.LastPullAt, conn.LastPushAt = nil, nil, nil
	conn.Report = githubsync.Report{}
	require.NoError(t, pe.store.Insert(context.Background(), *conn))
	pe.syncer.Notify(l.vault)
	pe.synced(l)
	requireHeldOnRemote(t, pe, l, held)
	require.Equal(t, "while disconnected", string(pe.remoteEntries(l)["mine.md"].Data))
	require.Zero(t, pe.gh.GitForced())
}

// D8, D17: a vault file that becomes something the vault cannot hold on the
// remote. The local file must not shadow it: unchanged, it goes to the
// trash; edited, it becomes a conflict copy first. The remote entry is
// unchanged after the next push. And back: a held path that becomes
// holdable is written to the vault by the pull that sees it.
func TestPathThatBecomesHeld(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"plain.md": "unchanged", "edited.md": "v1", "target.md": "target"})
	link1 := &githubapptest.Entry{Mode: "120000", Data: []byte("target.md")}
	theirs := pe.remote(l, map[string]*githubapptest.Entry{"plain.md": link1, "edited.md": link1})
	pe.upload(l.vault, "edited.md", "local edit")
	pe.listen(nil)
	pe.flush(l.vault)
	pe.listen(pe.syncer.OnCommit)
	pe.pullNow(l)
	pe.synced(l)

	copyName := pe.conflictName(l, "edited.md", theirs)
	disk := pe.disk(l.vault)
	delete(disk, ".gitignore")
	require.Equal(t, map[string]string{"target.md": "target", copyName: "local edit"}, disk)
	trash, err := pe.trash.ListTrash(context.Background(), l.vault)
	require.NoError(t, err)
	require.Len(t, trash, 1)
	require.Equal(t, "plain.md", trash[0].OriginalPath)
	got := pe.remoteEntries(l)
	require.Equal(t, githubapptest.Entry{Mode: "120000", Data: []byte("target.md")}, got["plain.md"])
	require.Equal(t, githubapptest.Entry{Mode: "120000", Data: []byte("target.md")}, got["edited.md"])
	require.Equal(t, "local edit", string(got[copyName].Data))
	require.Equal(t, []string{"edited.md", "plain.md"}, pe.held(l.vault))

	// The link becomes a file again on GitHub: the pull that sees it writes it.
	pe.remote(l, map[string]*githubapptest.Entry{"plain.md": text("a file again")})
	pe.pullNow(l)
	require.Equal(t, "a file again", pe.disk(l.vault)["plain.md"])
	require.NotNil(t, pe.fileRow(l.vault, "plain.md"))
	require.Equal(t, []string{"edited.md"}, pe.held(l.vault))
}

// D7, D17: a tracked file with no row (a raw writer made it) that the remote
// deletes cannot be trashed. It is kept, reported, and goes back to GitHub.
func TestRemoteDeleteOfAFileWithNoRowKeepsIt(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.commit(l.vault, "rowless.md", "written straight to raw/")
	pe.seed(l, map[string]string{"a.md": "one"})
	pe.remote(l, map[string]*githubapptest.Entry{"rowless.md": nil, "a.md": nil})
	pe.pullNow(l)

	disk := pe.disk(l.vault)
	require.Equal(t, "written straight to raw/", disk["rowless.md"])
	require.NotContains(t, disk, "a.md")
	require.Equal(t, []string{"rowless.md"}, pe.row(l.vault).Report.Kept)
	pe.flush(l.vault)
	pe.synced(l)
	require.Equal(t, "written straight to raw/", pe.remoteFiles(l)["rowless.md"])
}

// A pull stopped between a file's bytes and its row (cancelled, crashed, or
// beaten by another replica) leaves the bytes with no row. The next pull
// finds the file equal to the remote and completes it.
func TestPullCompletesAFileLeftWithoutARow(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"a.md": "one"})
	theirs := pe.remote(l, map[string]*githubapptest.Entry{"half.md": text("# Half\n\nwritten, never recorded\n")})
	pe.write(l.vault, "half.md", "# Half\n\nwritten, never recorded\n")
	require.Nil(t, pe.fileRow(l.vault, "half.md"))

	pe.pullNow(l)
	row := pe.fileRow(l.vault, "half.md")
	require.NotNil(t, row)
	_, chunks, ok := pe.memory(l.vault, row.ID)
	require.True(t, ok)
	require.Positive(t, chunks)
	merge := pe.synced(l)
	require.Contains(t, pe.gh.Git.Parents(l.repo, merge), theirs)
	require.Zero(t, pe.gh.GitForced())
}

// D10, D17: the plan and the finish hold the vault's lock; the apply does
// not. While a slow apply is in progress an upload completes and the commit
// sweep commits. The commit made mid-pull turns the fast-forward into a
// merge, which is pushed.
func TestApplyHoldsNoLock(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"a.md": "one"})
	theirs := pe.remote(l, map[string]*githubapptest.Entry{"slow.md": text("remote")})

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	pe.files.hook(func(vault.WriteInput) {
		once.Do(func() { close(entered) })
		<-release
	})
	pe.syncer.NotifyPull(l.vault)
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the apply never started")
	}
	progress, applying := pullOf(pe).Progress(l.vault)
	require.True(t, applying)
	require.Equal(t, githubsync.Progress{Done: 0, Total: 1}, progress)

	// The apply is parked. Nothing of the vault is locked.
	done := make(chan string, 1)
	go func() {
		pe.upload(l.vault, "during.md", "saved while pulling")
		committed, failed, err := pe.committer.Sweep(context.Background(), filepath.Join(pe.root, "tenants"))
		if err != nil || failed != 0 || committed != 1 {
			done <- "sweep did not commit"
			return
		}
		done <- ""
	}()
	select {
	case msg := <-done:
		require.Empty(t, msg)
	case <-time.After(20 * time.Second):
		t.Fatal("an upload and a sweep waited for the pull: the apply holds the vault's lock")
	}
	close(release)
	pe.files.hook(nil)
	pe.idle(l.vault)
	merge := pe.synced(l)
	_, applying = pullOf(pe).Progress(l.vault)
	require.False(t, applying)

	parents := pe.gh.Git.Parents(l.repo, merge)
	require.Len(t, parents, 2, "the commit made mid-pull makes it a merge")
	require.Equal(t, theirs, parents[1])
	remote := pe.remoteFiles(l)
	require.Equal(t, "remote", remote["slow.md"])
	require.Equal(t, "saved while pulling", remote["during.md"])
	require.Zero(t, pe.gh.GitForced())
}

// pullOf is the pull behind the environment's own syncer.
func pullOf(pe *pullEnv) *githubsync.Pull { return pe.pull }

func mustHash(sha string) plumbing.Hash { return plumbing.NewHash(sha) }

// D9, D17: the queue. A push delivery for the connected branch is pulled and
// closed; the same for one whose in-memory nudge was lost, which the sweep
// queues again after a minute; other refs, unknown repositories and our own
// commits are ignored; a missed webhook is found by the poll; and a delivery
// for one vault's repository never touches another vault.
func TestDeliveriesAndThePoll(t *testing.T) {
	pe := newPullEnv(t)
	ctx := context.Background()
	a, b := pe.connect(), pe.connect()
	pe.seed(a, map[string]string{"a.md": "one"})
	pe.seed(b, map[string]string{"b.md": "one"})
	bBefore, bRow := pe.disk(b.vault), pe.row(b.vault)
	deliver := func(d githubsync.Delivery, notify bool) string {
		inserted, err := pe.store.InsertDelivery(ctx, d, pe.clock.now())
		require.NoError(t, err)
		require.True(t, inserted)
		if !notify {
			return githubsync.DeliveryQueued
		}
		return pe.syncer.Deliver(ctx, d)
	}
	statusOf := func(id string) string {
		s, err := pe.store.DeliveryStatus(ctx, id)
		require.NoError(t, err)
		return s
	}

	// A push to the connected branch.
	tip := pe.remote(a, map[string]*githubapptest.Entry{"one.md": text("first")})
	push := githubsync.Delivery{ID: "d1", Event: "push", RepoID: a.repo, InstallationID: a.installation, Ref: "refs/heads/main", After: tip}
	require.Equal(t, githubsync.DeliveryQueued, deliver(push, true))
	pe.idle(a.vault)
	require.Equal(t, "first", pe.disk(a.vault)["one.md"])
	require.Equal(t, githubsync.DeliveryDone, statusOf("d1"))
	inserted, err := pe.store.InsertDelivery(ctx, push, pe.clock.now())
	require.NoError(t, err)
	require.False(t, inserted, "a delivery id is taken once")

	// Ignored: another branch, a tag, a repository nobody connected, a
	// deleted branch, an event this server has no use for.
	for i, d := range []githubsync.Delivery{
		{Event: "push", RepoID: a.repo, Ref: "refs/heads/drafts", After: tip},
		{Event: "push", RepoID: a.repo, Ref: "refs/tags/v1", After: tip},
		{Event: "push", RepoID: 999_999, Ref: "refs/heads/main", After: tip},
		{Event: "push", RepoID: a.repo, Ref: "refs/heads/main", After: strings.Repeat("0", 40)},
		{Event: "ping"},
		{Event: "github_app_authorization", Action: "revoked"},
	} {
		d.ID = "ignored-" + string(rune('a'+i))
		require.Equal(t, githubsync.DeliveryIgnored, deliver(d, true), "%+v", d)
		require.Equal(t, githubsync.DeliveryIgnored, statusOf(d.ID))
	}

	// The nudge was lost (a restart between the 202 and the pull). The sweep
	// leaves a fresh row alone and queues it again once it is a minute old.
	tip = pe.remote(a, map[string]*githubapptest.Entry{"two.md": text("second")})
	deliver(githubsync.Delivery{ID: "d2", Event: "push", RepoID: a.repo, Ref: "refs/heads/main", After: tip}, false)
	orphan := githubsync.Delivery{ID: "d3", Event: "push", RepoID: 888_888, Ref: "refs/heads/main", After: tip}
	deliver(orphan, false)
	pe.sweep()
	require.NotContains(t, pe.disk(a.vault), "two.md")
	require.Equal(t, githubsync.DeliveryQueued, statusOf("d2"))
	pe.clock.advance(61 * time.Second)
	pe.sweep()
	require.Equal(t, "second", pe.disk(a.vault)["two.md"])
	require.Equal(t, githubsync.DeliveryDone, statusOf("d2"))
	require.Equal(t, githubsync.DeliveryIgnored, statusOf("d3"), "nothing waits for it")

	// A missed webhook: the poll finds the new tip. A poll with nothing new
	// costs one listing per connection and takes no lock.
	pe.remote(a, map[string]*githubapptest.Entry{"three.md": text("third")})
	looked, err := pe.syncer.Poll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, looked)
	pe.idle(a.vault)
	require.Equal(t, "third", pe.disk(a.vault)["three.md"])
	requests := pe.gitRequests()
	_, err = pe.syncer.Poll(ctx)
	require.NoError(t, err)
	pe.idle(a.vault)
	pe.idle(b.vault)
	require.Equal(t, requests+2, pe.gitRequests())
	require.Equal(t, "GitHubSyncPoll", pe.syncer.PollJob().Name)

	// Vault B never moved.
	require.Equal(t, bBefore, pe.disk(b.vault))
	after := pe.row(b.vault)
	require.Equal(t, bRow.RemoteSHA, after.RemoteSHA)
	require.Nil(t, after.LastPullAt)

	// GitHubSyncPurge: rows older than 7 days go.
	pe.clock.advance(7*24*time.Hour + time.Minute)
	require.Equal(t, "GitHubSyncPurge", pe.syncer.PurgeJob().Name)
	require.NoError(t, pe.syncer.PurgeJob().Run(ctx))
	require.Empty(t, statusOf("d1"))
	var left int
	require.NoError(t, pe.pool.QueryRow(ctx, `SELECT count(*) FROM github_webhook_deliveries`).Scan(&left))
	require.Zero(t, left)
}

// D9, D17: installation events set the documented states, for the
// connections of that installation and no other.
func TestInstallationEvents(t *testing.T) {
	pe := newPullEnv(t)
	ctx := context.Background()
	a, b := pe.connect(), pe.connect()
	pe.seed(a, map[string]string{"a.md": "one"})
	pe.seed(b, map[string]string{"b.md": "one"})
	n := 0
	send := func(d githubsync.Delivery) {
		n++
		d.ID = "evt-" + string(rune('a'+n))
		_, err := pe.store.InsertDelivery(ctx, d, pe.clock.now())
		require.NoError(t, err)
		require.Equal(t, githubsync.DeliveryDone, pe.syncer.Deliver(ctx, d))
	}
	state := func(l link) (string, string) {
		c := pe.row(l.vault)
		if c.ErrorCode == nil {
			return c.State, ""
		}
		return c.State, *c.ErrorCode
	}

	send(githubsync.Delivery{Event: "installation", Action: "suspend", InstallationID: a.installation})
	st, code := state(a)
	require.Equal(t, githubsync.StateNeedsAttention, st)
	require.Equal(t, githubsync.CodeInstallationSuspended, code)
	st, _ = state(b)
	require.Equal(t, githubsync.StateActive, st, "another installation")

	// Suspended: a remote change is not pulled. Unsuspend: back to active,
	// and a sync is queued that brings it.
	pe.remote(a, map[string]*githubapptest.Entry{"while-suspended.md": text("x")})
	pe.pullNow(a)
	require.NotContains(t, pe.disk(a.vault), "while-suspended.md")
	send(githubsync.Delivery{Event: "installation", Action: "unsuspend", InstallationID: a.installation})
	pe.idle(a.vault)
	st, code = state(a)
	require.Equal(t, githubsync.StateActive, st)
	require.Empty(t, code)
	require.Equal(t, "x", pe.disk(a.vault)["while-suspended.md"])

	send(githubsync.Delivery{Event: "installation_repositories", Action: "removed", InstallationID: b.installation, Removed: []int64{b.repo, 12345}})
	st, code = state(b)
	require.Equal(t, githubsync.StateNeedsAttention, st)
	require.Equal(t, githubsync.CodeRepoAccessRemoved, code)
	// An unsuspend does not revive a connection stopped for another reason.
	send(githubsync.Delivery{Event: "installation", Action: "unsuspend", InstallationID: b.installation})
	st, _ = state(b)
	require.Equal(t, githubsync.StateNeedsAttention, st)

	send(githubsync.Delivery{Event: "installation", Action: "deleted", InstallationID: a.installation})
	st, code = state(a)
	require.Equal(t, githubsync.StateNeedsAttention, st)
	require.Equal(t, githubsync.CodeInstallationRemoved, code)
	// Nothing local is deleted (D13).
	require.Equal(t, "one", pe.disk(a.vault)["a.md"])
}

// D11, D17: the limits hold on every fetch. Too many files the vault would
// hold, or a repository grown past the size limit, stops the connection
// with repo_too_large and changes nothing.
func TestPullLimits(t *testing.T) {
	pe := newPullEnv(t, func(o *githubsync.PullOptions) { o.MaxFiles = 3 })
	l := pe.connect()
	pe.seed(l, map[string]string{"a.md": "one"})
	// Held files do not count: only what the vault would hold.
	pe.remote(l, map[string]*githubapptest.Entry{"b.md": text("2"), "c.md": text("3"), "My Note.md": text("held"), ".obsidian/x.json": text("{}")})
	pe.pullNow(l)
	require.Equal(t, githubsync.StateActive, pe.row(l.vault).State, "three files: at the limit")
	require.Equal(t, "3", pe.disk(l.vault)["c.md"])

	tip := pe.remote(l, map[string]*githubapptest.Entry{"d.md": text("4")})
	pe.pullNow(l)
	c := pe.row(l.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State)
	require.Equal(t, githubsync.CodeRepoTooLarge, *c.ErrorCode)
	require.NotContains(t, pe.disk(l.vault), "d.md")
	require.Equal(t, tip, pe.tip(l))

	// The owner deletes a file on GitHub and presses "Sync now".
	pe.remote(l, map[string]*githubapptest.Entry{"b.md": nil})
	require.NoError(t, pe.syncer.SyncNow(context.Background(), l.vault))
	pe.idle(l.vault)
	c = pe.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Equal(t, "4", pe.disk(l.vault)["d.md"])
	require.NotContains(t, pe.disk(l.vault), "b.md")

	// The repository's size, as GitHub reports it (KiB).
	sized := newPullEnv(t)
	big := sized.connect()
	sized.seed(big, map[string]string{"a.md": "one"})
	replica := sized.newSyncer(sized.committer, sized.realPull(), func(_ *syncEnv, o *githubsync.SyncerOptions) { o.MaxRepoBytes = 200 << 20 })
	sized.remote(big, map[string]*githubapptest.Entry{"b.md": text("2")})
	sized.gh.Lock()
	r := sized.gh.Repos[big.repo]
	r.Size = 200<<10 + 1
	sized.gh.Repos[big.repo] = r
	sized.gh.Unlock()
	replica.NotifyPull(big.vault)
	require.Eventually(t, func() bool { return !replica.Syncing(big.vault) }, 20*time.Second, 5*time.Millisecond)
	c = sized.row(big.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State)
	require.Equal(t, githubsync.CodeRepoTooLarge, *c.ErrorCode)
	require.NotContains(t, sized.disk(big.vault), "b.md")
}

// D6, D9: a repository renamed on GitHub is followed by its id when a pull
// meets the old name, and the row learns the new one.
func TestPullFollowsARename(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"a.md": "one"})
	pe.remote(l, map[string]*githubapptest.Entry{"b.md": text("two")})
	pe.gh.Lock()
	r := pe.gh.Repos[l.repo]
	r.FullName = "octocat/renamed"
	pe.gh.Repos[l.repo] = r
	pe.gh.Renamed[l.name] = l.repo
	pe.gh.Unlock()

	pe.pullNow(l)
	require.Equal(t, "two", pe.disk(l.vault)["b.md"])
	c := pe.row(l.vault)
	require.Equal(t, "octocat/renamed", c.RepoFullName)
	require.Nil(t, c.ErrorCode)
	require.Zero(t, pe.gh.Followed, "no redirect was followed")
}

// D5, the E4.8 review: workers are safe with more than one replica. Two
// replicas with nothing in common but the database and the volume pull the
// same remote at once: every file is written once, a conflict is kept twice
// and not three times, one merge reaches GitHub, nothing is forced.
func TestTwoReplicasPullAtOnce(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	seed := map[string]string{"conflict.md": "v1", "gone.md": "v1", "old.md": "# moved\n\nsame bytes\n"}
	pe.seed(l, seed)
	entries := map[string]*githubapptest.Entry{
		"conflict.md": text("remote side"), "gone.md": nil, "old.md": nil, "new.md": text("# moved\n\nsame bytes\n"),
	}
	for i := range 40 {
		entries["bulk/n"+string(rune('a'+i%26))+string(rune('a'+i/26))+".md"] = text("note " + string(rune('a'+i%26)))
	}
	theirs := pe.remote(l, entries)
	pe.upload(l.vault, "conflict.md", "local side")
	pe.listen(nil)
	pe.flush(l.vault)

	other := pe.newSyncer(pe.newCommitter(), pe.realPull())
	var wg sync.WaitGroup
	for _, s := range []*githubsync.Syncer{pe.syncer, other} {
		wg.Go(func() { s.NotifyPull(l.vault) })
	}
	wg.Wait()
	require.Eventually(t, func() bool {
		if pe.syncer.Syncing(l.vault) || other.Syncing(l.vault) {
			return false
		}
		c, err := pe.store.Get(context.Background(), l.vault)
		return err == nil && c.RemoteSHA != nil && *c.RemoteSHA == pe.head(l.vault) && pe.tip(l) == pe.head(l.vault)
	}, 60*time.Second, 20*time.Millisecond)

	disk := pe.disk(l.vault)
	copies := 0
	for p := range disk {
		if strings.Contains(p, ".conflict-") {
			copies++
		}
	}
	require.Equal(t, 1, copies, "one conflict copy, whoever made it")
	require.Equal(t, "local side", disk["conflict.md"])
	require.Equal(t, "remote side", disk[pe.conflictName(l, "conflict.md", theirs)])
	require.NotContains(t, disk, "gone.md")
	require.NotContains(t, disk, "old.md")
	require.Len(t, disk, 1+2+1+40, ".gitignore, the pair, the moved note, the bulk")
	require.Len(t, pe.rowPaths(l.vault), 2+1+40, "a row per file, none twice")
	remote := pe.remoteFiles(l)
	delete(disk, ".gitignore")
	delete(remote, ".gitignore")
	require.Equal(t, disk, remote)
	require.Zero(t, pe.gh.GitForced())
	fsck(t, mustOpen(t, pe.raw(l.vault)), pe.head(l.vault))
}

func mustOpen(t *testing.T, root string) *git.Repository {
	t.Helper()
	repo, err := git.PlainOpen(root)
	require.NoError(t, err)
	return repo
}

// The E4.8 review: the vault's cross-replica lock is a database session. If
// it dies during a pull's locked phase, nothing is applied and the ref does
// not move.
func TestPullStopsWhenTheLockIsLost(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"a.md": "one"})
	pe.remote(l, map[string]*githubapptest.Entry{"b.md": text("two")})
	head := pe.head(l.vault)

	var dead atomic.Bool
	dead.Store(true)
	lossy := vaultgit.NewCommitter(vaultgit.Options{RawRoot: pe.raw, Lock: lostLock{dead: &dead}})
	t.Cleanup(func() { lossy.Close(context.Background()) })
	replica := pe.newSyncer(lossy, pe.realPull())
	replica.NotifyPull(l.vault)
	require.Eventually(t, func() bool { return !replica.Syncing(l.vault) }, 20*time.Second, 5*time.Millisecond)
	require.NotContains(t, pe.disk(l.vault), "b.md")
	require.Equal(t, head, pe.head(l.vault))
	c := pe.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode, "a failure that is retried")

	// The retry, with a lock that holds, brings the file.
	dead.Store(false)
	pe.clock.advance(time.Minute)
	pe.pullNow(l)
	require.Equal(t, "two", pe.disk(l.vault)["b.md"])
	require.Nil(t, pe.row(l.vault).ErrorCode)
}

type lostLock struct{ dead *atomic.Bool }

func (l lostLock) TryLock(context.Context, uuid.UUID) (func(), bool, error) {
	return func() {}, true, nil
}

func (l lostLock) TryLockChecked(context.Context, uuid.UUID) (func(), func(context.Context) error, bool, error) {
	return func() {}, func(context.Context) error {
		if l.dead.Load() {
			return context.DeadlineExceeded
		}
		return nil
	}, true, nil
}

// D10, D17: no git command ever writes the working tree. raw/ is the live
// vault: a pull, a checkout or a hard reset there would overwrite or delete
// files with no row, no memory and no trash. Nothing in sync may name one.
func TestNoWorktreeOperation(t *testing.T) {
	forbidden := regexp.MustCompile(`\.Pull(Context)?\(|\.Checkout\(|HardReset|MergeReset|\.Clean\(|\.Reset\(|PlainClone`)
	for _, dir := range []string{".", "../vaultgit", "../modules/githubsync"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		require.NotEmpty(t, files)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			require.NoError(t, err)
			for i, line := range strings.Split(string(src), "\n") {
				code, _, _ := strings.Cut(line, "//")
				code = strings.ReplaceAll(code, "timer.Reset(", "") // a time.Timer, not a tree
				if forbidden.MatchString(code) {
					t.Errorf("%s:%d names a working-tree operation: %s", f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

// The E4.8 review: PGLocker's lock is a database session. alive asks that
// session, for keys of either sign, and fails once the session is gone.
func TestPGLockerNoticesALostSession(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	locker := vaultgit.PGLocker{Pool: p}
	for range 40 { // random keys: both halves, both signs
		unlock, alive, ok, err := locker.TryLockChecked(ctx, uuid.New())
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, alive(ctx))
		unlock()
	}

	vault := uuid.New()
	unlock, alive, ok, err := locker.TryLockChecked(ctx, vault)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, alive(ctx))
	_, _, again, err := locker.TryLockChecked(ctx, vault)
	require.NoError(t, err)
	require.False(t, again, "held by the first session")

	// The session dies, as on a failover or an idle timeout.
	var killed int
	require.NoError(t, p.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype = 'advisory' AND pid <> pg_backend_pid()`).Scan(&killed))
	require.Equal(t, 1, killed)
	require.Eventually(t, func() bool { return alive(ctx) != nil }, 5*time.Second, 10*time.Millisecond)
	unlock()
	// The lock went with it: another replica takes it.
	unlock, _, ok, err = locker.TryLockChecked(ctx, vault)
	require.NoError(t, err)
	require.True(t, ok)
	unlock()
}

// D8, D17: a path with a ".git" segment, in any case, is refused. GitHub
// does not accept a push that has one, so a branch that shows one is not
// trusted at all: nothing of it is merged, nothing is written, the vault's
// own history keeps working, and the pull goes through once the path is gone.
func TestRemoteTreeWithADotGitPathIsNotMerged(t *testing.T) {
	for _, bad := range []string{"notes/.git/config", ".GIT/hooks/pre-commit", "a/b/.Git"} {
		t.Run(bad, func(t *testing.T) {
			pe := newPullEnv(t)
			l := pe.connect()
			pe.seed(l, map[string]string{"a.md": "one"})
			head := pe.head(l.vault)
			theirs := pe.remote(l, map[string]*githubapptest.Entry{bad: text("[core]\n\tfsmonitor = x\n"), "fine.md": text("fine")})
			pe.pullNow(l)

			require.Equal(t, map[string]string{"a.md": "one", ".gitignore": vaultgit.GitIgnore}, pe.disk(l.vault))
			require.Equal(t, head, pe.head(l.vault))
			require.Empty(t, pe.held(l.vault))
			c := pe.row(l.vault)
			require.Equal(t, githubsync.StateActive, c.State)
			require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode)
			require.Equal(t, theirs, pe.tip(l))

			// The vault still saves and commits.
			pe.upload(l.vault, "a.md", "two")
			pe.listen(nil)
			require.NotEqual(t, head, pe.flush(l.vault))
			pe.listen(pe.syncer.OnCommit)

			// The path goes away on GitHub: the next try merges.
			pe.remote(l, map[string]*githubapptest.Entry{bad: nil})
			pe.clock.advance(time.Hour)
			pe.pullNow(l)
			pe.synced(l)
			require.Equal(t, "fine", pe.disk(l.vault)["fine.md"])
			require.Equal(t, "two", string(pe.remoteEntries(l)["a.md"].Data))
			require.Nil(t, pe.row(l.vault).ErrorCode)
			require.Zero(t, pe.gh.GitForced())
		})
	}
}
