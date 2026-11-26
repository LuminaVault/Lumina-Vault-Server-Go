package githubsync_test

import (
	"context"
	"crypto/rand"
	"errors"
	"lumina/internal/githubapp/githubapptest"
	"lumina/internal/githubsync"
	"lumina/internal/modules/vault"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The tests of the E4.9 review's findings and of the owner's two decisions
// of 2026-11-26.

// healthy: the connection is at work and has nothing to report as an error.
func (pe *pullEnv) healthy(l link) {
	pe.t.Helper()
	c := pe.row(l.vault)
	code := ""
	if c.ErrorCode != nil {
		code = *c.ErrorCode
	}
	require.Equal(pe.t, githubsync.StateActive, c.State, code)
	require.Empty(pe.t, code)
}

// quietly commits what the vault has without starting a sync, so that the
// next pull finds both sides moved.
func (pe *pullEnv) quietly(l link) {
	pe.t.Helper()
	pe.listen(nil)
	pe.flush(l.vault)
	pe.listen(pe.syncer.OnCommit)
}

// foreign is the commits the vault's repository has on record as pulled
// from GitHub, not made by this server (review item 13).
func (pe *pullEnv) foreign(l link) []string {
	pe.t.Helper()
	b, err := os.ReadFile(filepath.Join(pe.raw(l.vault), ".git", "lv-foreign"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(pe.t, err)
	return strings.Fields(string(b))
}

// named is the one vault file with that body.
func named(t *testing.T, disk map[string]string, body string) string {
	t.Helper()
	var found []string
	for p, b := range disk {
		if b == body {
			found = append(found, p)
		}
	}
	require.Len(t, found, 1, "one file with %q in %v", body, disk)
	return found[0]
}

// Review item 1: the remote turns a folder into an entry the vault cannot
// hold (a link). The folder's files leave, one the vault added there steps
// aside as a conflict copy, nothing can be saved under the name any more
// (409 path_held), and every later commit and push goes on working, also
// after a writer that does not ask first put a file there.
func TestFolderBecomesAHeldEntry(t *testing.T) {
	pe := newPullEnv(t)
	ctx := context.Background()
	l := pe.connect()
	pe.seed(l, map[string]string{"dir/a.md": "a", "keep.md": "k"})
	link := &githubapptest.Entry{Mode: "120000", Data: []byte("keep.md")}
	pe.remote(l, map[string]*githubapptest.Entry{"dir/a.md": nil, "dir": link})
	pe.upload(l.vault, "dir/b.md", "local new")
	pe.quietly(l)
	pe.pullNow(l)
	pe.synced(l)
	pe.healthy(l)

	require.Equal(t, []string{"dir"}, pe.held(l.vault))
	disk := pe.disk(l.vault)
	require.NotContains(t, disk, "dir/a.md")
	require.NotContains(t, disk, "dir/b.md")
	aside := named(t, disk, "local new")
	require.False(t, strings.HasPrefix(aside, "dir/"), "the copy is beside the held name, not under it: %s", aside)
	require.NotNil(t, pe.fileRow(l.vault, aside))
	got := pe.remoteEntries(l)
	require.Equal(t, githubapptest.Entry{Mode: "120000", Data: []byte("keep.md")}, got["dir"])
	require.Equal(t, "local new", string(got[aside].Data))

	// Nothing is saved under the held name, or moved there.
	for _, p := range []string{"dir/new.md", "dir/deep/new.md"} {
		_, err := pe.files.Files.Write(ctx, vault.WriteInput{Vault: l.vault, Actor: l.vault, Path: p, Data: []byte("x")})
		var held *vault.HeldPathError
		require.ErrorAs(t, err, &held, p)
		require.Equal(t, "dir", held.Held)
		require.Equal(t, http.StatusConflict, held.StatusCode())
	}
	require.NotContains(t, pe.disk(l.vault), "dir/new.md")
	require.Nil(t, pe.fileRow(l.vault, "dir/new.md"))

	// A writer that does not go through the vault's pipeline puts a file
	// there all the same. It is left out of the history; saves after it are
	// committed and pushed as ever.
	pe.write(l.vault, "dir/raw.md", "written straight to raw/")
	for _, p := range []string{"later.md", "later2.md"} {
		pe.upload(l.vault, p, p)
		pe.flush(l.vault)
		pe.synced(l)
	}
	pe.healthy(l)
	got = pe.remoteEntries(l)
	require.Equal(t, "120000", got["dir"].Mode)
	require.NotContains(t, got, "dir/raw.md")
	require.Contains(t, got, "later2.md")
	require.Equal(t, "written straight to raw/", pe.disk(l.vault)["dir/raw.md"], "the file itself is not touched")

	// The link becomes a folder again: its files are written by the pull
	// that sees it, and saves there are taken again.
	pe.remote(l, map[string]*githubapptest.Entry{"dir": nil, "dir/c.md": text("back")})
	pe.pullNow(l)
	pe.healthy(l)
	require.Empty(t, pe.held(l.vault))
	require.Equal(t, "back", pe.disk(l.vault)["dir/c.md"])
	pe.upload(l.vault, "dir/new.md", "saved")
	pe.flush(l.vault)
	pe.synced(l)
	require.Equal(t, "saved", string(pe.remoteEntries(l)["dir/new.md"].Data))
}

// Review items 1 and 3, the other way round: the remote turns a file into a
// folder. What leaves the name goes first (to the trash, or aside when it
// was edited), then the folder's files are written. And a file the vault
// cannot take because a held entry lies under its name is refused at save.
func TestFileBecomesAFolder(t *testing.T) {
	pe := newPullEnv(t)
	ctx := context.Background()
	l := pe.connect()
	pe.seed(l, map[string]string{"a.md": "v1", "b.md": "v1", "old/name.md": "moved", "keep.md": "k"})
	pe.remote(l, map[string]*githubapptest.Entry{
		"a.md": nil, "a.md/x.md": text("x"),
		"b.md": nil, "b.md/y.md": text("y"),
		// A rename into what was a file, and a held entry under a new name.
		"old/name.md": nil, "keep.md": nil, "keep.md/name.md": text("moved"),
		"c.md/.cache": text("{}"),
	})
	pe.upload(l.vault, "b.md", "local edit")
	pe.quietly(l)
	pe.pullNow(l)
	pe.synced(l)
	pe.healthy(l)

	disk := pe.disk(l.vault)
	require.Equal(t, "x", disk["a.md/x.md"])
	require.Equal(t, "y", disk["b.md/y.md"])
	require.Equal(t, "moved", disk["keep.md/name.md"])
	require.NotContains(t, disk, "old/name.md")
	aside := named(t, disk, "local edit")
	require.False(t, strings.HasPrefix(aside, "b.md/"), aside)
	require.Equal(t, "local edit", string(pe.remoteEntries(l)[aside].Data))
	for _, p := range []string{"a.md/x.md", "b.md/y.md", "keep.md/name.md"} {
		require.NotNil(t, pe.fileRow(l.vault, p), p)
	}
	require.Nil(t, pe.fileRow(l.vault, "a.md"))
	require.Equal(t, []string{"c.md/.cache"}, pe.held(l.vault))

	// c.md cannot be a file while GitHub has something inside that name.
	_, err := pe.files.Files.Write(ctx, vault.WriteInput{Vault: l.vault, Actor: l.vault, Path: "c.md", Data: []byte("x")})
	var held *vault.HeldPathError
	require.ErrorAs(t, err, &held)
	require.Equal(t, "c.md/.cache", held.Held)
	_, err = pe.files.MoveIfMatch(ctx, l.vault, "a.md/x.md", "c.md", "")
	require.ErrorAs(t, err, &held, "a move is a save at its target")
	// A folder beside it is no concern of the entry.
	pe.upload(l.vault, "c.md.md", "beside")

	// And back: a folder becomes a file again. Its files leave first, then
	// the file is written where the folder was.
	pe.remote(l, map[string]*githubapptest.Entry{"a.md/x.md": nil, "a.md": text("a file again"), "b.md/y.md": nil, "b.md": text("b again")})
	pe.upload(l.vault, "b.md/y.md", "local edit of y")
	pe.quietly(l)
	pe.pullNow(l)
	pe.synced(l)
	pe.healthy(l)
	disk = pe.disk(l.vault)
	require.Equal(t, "a file again", disk["a.md"])
	require.NotContains(t, disk, "a.md/x.md")
	require.NotNil(t, pe.fileRow(l.vault, "a.md"))
	require.Nil(t, pe.fileRow(l.vault, "a.md/x.md"))
	// The edited file in the folder stepped aside, out of the folder.
	require.Equal(t, "b again", disk["b.md"])
	asideY := named(t, disk, "local edit of y")
	require.False(t, strings.HasPrefix(asideY, "b.md/"), asideY)
	require.Equal(t, []string{"c.md/.cache"}, pe.held(l.vault), "nothing was held for it")
	got := pe.remoteEntries(l)
	require.Equal(t, "a file again", string(got["a.md"].Data))
	require.Equal(t, "b again", string(got["b.md"].Data))
	require.Equal(t, "local edit of y", string(got[asideY].Data))
	pe.clock.advance(11 * time.Minute)
	pe.pullNow(l)
	pe.healthy(l)
}

// Review item 2: a name too long for a file, on the remote or made by the
// vault's own suffixes (a conflict copy, the trash), holds that one entry or
// keeps that one file. It never fails the pull, this time or the next.
func TestNamesTooLongForTheVault(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	fits := strings.Repeat("f", githubsync.MaxSegmentBytes-3) + ".md"
	over := strings.Repeat("o", githubsync.MaxSegmentBytes-2) + ".md"
	long := strings.Repeat("n", 300) + ".md"
	// A file the vault already had under a name longer than the limit for
	// new ones: it stays a vault file.
	had := strings.Repeat("h", 215) + ".md"
	pe.seed(l, map[string]string{"keep.md": "k", had: "v1", "gone-" + had: "v1"})

	theirs := pe.remote(l, map[string]*githubapptest.Entry{
		long: text("long"), over: text("over"), "d/" + over: text("deep"), over[:100] + "/in.md": text("a long folder that fits"),
		fits: text("fits"), "ok.md": text("ok"),
		had: text("remote edit"), "gone-" + had: nil,
	})
	pe.upload(l.vault, had, "local edit")
	pe.quietly(l)
	pe.pullNow(l)
	pe.synced(l)
	pe.healthy(l)

	disk := pe.disk(l.vault)
	require.Equal(t, "ok", disk["ok.md"])
	require.Equal(t, "fits", disk[fits])
	require.Equal(t, "a long folder that fits", disk[over[:100]+"/in.md"])
	require.Equal(t, []string{"d/" + over, had, long, over}, pe.held(l.vault))
	report := pe.row(l.vault).Report
	require.Equal(t, 4, report.HeldCount)
	for _, h := range report.Held {
		require.Equal(t, githubsync.HeldBadName, h.Reason, h.Path)
	}
	got := pe.remoteEntries(l)
	require.Equal(t, "long", string(got[long].Data), "GitHub keeps what the vault cannot hold")
	require.Equal(t, "over", string(got[over].Data))
	// The edit on both sides of the long-named file: its conflict copy
	// would have a name no file can have, so the path is held from here on.
	// GitHub keeps its version, the vault keeps its own, neither is lost.
	require.Equal(t, "local edit", disk[had])
	require.Equal(t, "remote edit", string(got[had].Data))
	require.NotEmpty(t, theirs)
	// Its neighbour, deleted on GitHub, goes to the trash, which cuts the
	// name it keeps.
	require.NotContains(t, disk, "gone-"+had)
	require.NotContains(t, got, "gone-"+had)
	require.Empty(t, report.Kept)

	// A name at the limit takes the suffixes: a conflict copy, the trash.
	edited := pe.remote(l, map[string]*githubapptest.Entry{fits: text("their edit"), "ok.md": nil})
	pe.upload(l.vault, fits, "our edit")
	pe.quietly(l)
	pe.pullNow(l)
	pe.synced(l)
	pe.healthy(l)
	disk = pe.disk(l.vault)
	require.Equal(t, "our edit", disk[fits])
	require.Equal(t, "their edit", disk[pe.conflictName(l, fits, edited)])
	pe.remote(l, map[string]*githubapptest.Entry{fits: nil})
	pe.pullNow(l)
	pe.healthy(l)
	require.NotContains(t, pe.disk(l.vault), fits)
	trash, err := pe.trash.ListTrash(context.Background(), l.vault)
	require.NoError(t, err)
	paths := map[string]bool{}
	for _, f := range trash {
		paths[f.OriginalPath] = true
	}
	require.True(t, paths[fits], "a file named at the limit fits in the trash")

	// And the pulls after are as quiet.
	pe.clock.advance(11 * time.Minute)
	pe.pullNow(l)
	pe.healthy(l)
}

// Review item 5: an apply holds no lock, so the owner can disconnect in the
// middle of it. It stops within a few files, records nothing and moves no
// ref; and a reconnect, which is a new connection even to the same
// repository, is not finished by the old one's pull.
func TestConnectionChangedDuringApply(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		pe := newPullEnv(t, func(o *githubsync.PullOptions) { o.Parallel = 1 })
		ctx := context.Background()
		l := pe.connect()
		before := pe.seed(l, map[string]string{"a.md": "one"})
		entries := map[string]*githubapptest.Entry{}
		for i := range 130 {
			entries["n/"+strings.Repeat("x", 1+i%7)+"-"+string(rune('a'+i%26))+string(rune('a'+i/26))+".md"] = text("remote")
		}
		require.Len(t, entries, 130)
		theirs := pe.remote(l, entries)

		var once sync.Once
		pe.files.hook(func(vault.WriteInput) {
			once.Do(func() {
				old, err := pe.store.Delete(ctx, l.vault)
				if err != nil || old == nil {
					t.Error("the connection was not there to remove")
					return
				}
				if reconnect {
					fresh := *old
					fresh.Report = githubsync.Report{}
					if err := pe.store.Insert(ctx, fresh); err != nil {
						t.Error(err)
					}
				}
			})
		})
		pe.syncer.NotifyPull(l.vault)
		pe.idle(l.vault)
		pe.files.hook(nil)

		written := 0
		for p := range pe.disk(l.vault) {
			if strings.HasPrefix(p, "n/") {
				written++
			}
		}
		require.Positive(t, written)
		require.LessOrEqual(t, written, 51, "it looked at the connection again within 50 files (reconnect: %v)", reconnect)
		require.Equal(t, before, pe.head(l.vault), "no ref moved")
		require.Equal(t, theirs, pe.tip(l), "and nothing was pushed")
		c, err := pe.store.Get(ctx, l.vault)
		require.NoError(t, err)
		if !reconnect {
			require.Nil(t, c)
			continue
		}
		require.NotNil(t, c)
		require.Equal(t, githubsync.StateActive, c.State)
		require.Nil(t, c.ErrorCode, "the old pull wrote no failure on the new connection")
		require.Nil(t, c.LastPullAt, "nor a success")
		require.Nil(t, c.RemoteSHA)
		require.Zero(t, c.RetryCount)
		require.Zero(t, c.Report.HeldCount)

		// The new connection's own pull finds the files already written
		// equal to the remote, and finishes.
		pe.pullNow(l)
		pe.synced(l)
		pe.healthy(l)
		require.Len(t, pe.disk(l.vault), 130+2) // and a.md, .gitignore
	}
}

// Review item 5: the finish looks at the connection again under the lock.
// Here the pull has one file, so nothing in the apply looks after the write
// during which the owner disconnects (or reconnects): only the finish stands
// between that pull and a moved ref, a report and a push.
func TestConnectionChangedBeforeFinish(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		pe := newPullEnv(t)
		ctx := context.Background()
		l := pe.connect()
		before := pe.seed(l, map[string]string{"a.md": "one"})
		theirs := pe.remote(l, map[string]*githubapptest.Entry{"only.md": text("remote")})
		pe.files.hook(func(vault.WriteInput) {
			old, err := pe.store.Delete(ctx, l.vault)
			if err != nil || old == nil {
				t.Error("the connection was not there to remove")
				return
			}
			if reconnect {
				fresh := *old
				fresh.Report = githubsync.Report{}
				if err := pe.store.Insert(ctx, fresh); err != nil {
					t.Error(err)
				}
			}
		})
		pe.syncer.NotifyPull(l.vault)
		pe.idle(l.vault)
		pe.files.hook(nil)

		require.Equal(t, "remote", pe.disk(l.vault)["only.md"], "the write that was under way ended")
		require.Equal(t, before, pe.head(l.vault), "no ref moved (reconnect: %v)", reconnect)
		require.Equal(t, theirs, pe.tip(l))
		require.Empty(t, pe.held(l.vault))
		c, err := pe.store.Get(ctx, l.vault)
		require.NoError(t, err)
		if !reconnect {
			require.Nil(t, c)
			continue
		}
		require.NotNil(t, c)
		require.Nil(t, c.ErrorCode)
		require.Nil(t, c.LastPullAt, "the old connection's pull is not the new one's")
		require.Nil(t, c.RemoteSHA)
		require.Zero(t, c.RetryCount)
	}
}

// stall keeps every fetch of one repository waiting until it is released,
// as a remote that stopped answering does.
func (v *syncEnv) stall(name string) (arrived <-chan struct{}, release func()) {
	seen, open := make(chan struct{}, 64), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(open) }) }
	v.gh.Lock()
	v.gh.Intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/"+name+".git/git-upload-pack" {
			return false
		}
		select {
		case <-open:
			return false
		default:
		}
		seen <- struct{}{}
		<-open
		// Its client gave up long ago.
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return true
	}
	v.gh.Unlock()
	v.t.Cleanup(release)
	return seen, release
}

// Review item 14: the fetch has a deadline of its own. A remote that stops
// answering costs that long, is a transient failure, and the next attempt
// brings the commit.
func TestFetchDeadline(t *testing.T) {
	pe := newPullEnv(t, func(o *githubsync.PullOptions) { o.FetchTimeout = 2 * time.Second })
	l := pe.connect()
	before := pe.seed(l, map[string]string{"a.md": "one"})
	late := pe.remote(l, map[string]*githubapptest.Entry{"late.md": text("remote")})
	arrived, release := pe.stall(l.name)

	began := time.Now()
	pe.syncer.NotifyPull(l.vault)
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("no fetch arrived")
	}
	pe.idle(l.vault)
	require.Less(t, time.Since(began), 18*time.Second, "the deadline ended it, not the remote")
	c := pe.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.NotNil(t, c.ErrorCode)
	require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode)
	require.NotNil(t, c.NextRetryAt, "to be tried again")
	require.NotContains(t, pe.disk(l.vault), "late.md")
	require.Equal(t, before, pe.head(l.vault))

	release()
	pe.clock.advance(11 * time.Minute)
	pe.pullNow(l)
	pe.synced(l)
	pe.healthy(l)
	require.Equal(t, "remote", pe.disk(l.vault)["late.md"])
	// Review item 13: the commit a fast-forward brought is on record as
	// GitHub's, and the server's own are not.
	require.Equal(t, []string{late}, pe.foreign(l))
}

// Review item 14: so has the apply. When it runs out the ref is not moved
// and the failure is a transient one; the files it had written are found
// equal to the remote by the next pull, which finishes.
func TestApplyDeadline(t *testing.T) {
	pe := newPullEnv(t, func(o *githubsync.PullOptions) {
		o.ApplyTimeout = 3 * time.Second
		o.Parallel = 1
	})
	l := pe.connect()
	before := pe.seed(l, map[string]string{"a.md": "one"})
	theirs := pe.remote(l, map[string]*githubapptest.Entry{"one.md": text("1"), "two.md": text("2"), "three.md": text("3"), "four.md": text("4")})
	var writes atomic.Int32
	pe.files.hook(func(vault.WriteInput) {
		if writes.Add(1) == 2 {
			time.Sleep(4 * time.Second)
		}
	})
	pe.pullNow(l)
	pe.files.hook(nil)

	c := pe.row(l.vault)
	require.Equal(t, githubsync.StateActive, c.State)
	require.NotNil(t, c.ErrorCode)
	require.Equal(t, githubsync.CodeSyncFailed, *c.ErrorCode)
	require.NotNil(t, c.NextRetryAt)
	require.Nil(t, c.LastPullAt)
	require.Equal(t, before, pe.head(l.vault), "the ref is not moved by half an apply")
	require.Equal(t, theirs, pe.tip(l))
	done := 0
	for _, p := range []string{"one.md", "two.md", "three.md", "four.md"} {
		if _, ok := pe.disk(l.vault)[p]; ok {
			done++
		}
	}
	require.Less(t, done, 4, "the deadline stopped it")
	_, applying := pullOf(pe).Progress(l.vault)
	require.False(t, applying)

	pe.clock.advance(11 * time.Minute)
	pe.pullNow(l)
	pe.synced(l)
	pe.healthy(l)
	for _, p := range []string{"one.md", "two.md", "three.md", "four.md"} {
		require.Contains(t, pe.disk(l.vault), p)
		require.NotNil(t, pe.fileRow(l.vault, p), p)
	}
}

// Review item 7: the disk guard is asked about the size GitHub reports, not
// about nothing; a fetch that reads more than the limit ends there, whatever
// size was reported; and a tree with more files than the limit is not
// walked to its end.
func TestPullSizeGuards(t *testing.T) {
	var asked atomic.Int64
	asked.Store(-1)
	pe := newPullEnv(t, func(o *githubsync.PullOptions) {
		o.Space = func(incoming int64) error {
			asked.Store(incoming)
			if incoming > 1<<30 {
				return errors.New("no room")
			}
			return nil
		}
	})
	l := pe.connect()
	before := pe.seed(l, map[string]string{"a.md": "one"})
	// The limit is far away; the disk is what is short.
	roomy := pe.newSyncer(pe.committer, pe.realPull(), withMaxRepoBytes(8<<30))
	pull := func(s *githubsync.Syncer, l link) {
		s.NotifyPull(l.vault)
		require.Eventually(t, func() bool { return !s.Syncing(l.vault) }, 20*time.Second, 5*time.Millisecond)
	}
	size := func(kib int64) {
		pe.gh.Lock()
		r := pe.gh.Repos[l.repo]
		r.Size = kib
		pe.gh.Repos[l.repo] = r
		pe.gh.Unlock()
	}

	size(1234)
	pe.remote(l, map[string]*githubapptest.Entry{"b.md": text("2")})
	pull(roomy, l)
	pe.healthy(l)
	require.EqualValues(t, 1234<<10, asked.Load(), "the size GitHub reports, in bytes")
	require.Equal(t, "2", pe.disk(l.vault)["b.md"])

	// More than the disk has room for: nothing is fetched.
	size(2 << 20)
	pe.remote(l, map[string]*githubapptest.Entry{"c.md": text("3")})
	pull(roomy, l)
	require.EqualValues(t, 2<<30, asked.Load())
	require.NotContains(t, pe.disk(l.vault), "c.md")
	require.NotEqual(t, before, pe.tip(l))
	require.Equal(t, githubsync.StateActive, pe.row(l.vault).State, "a full disk is the server's trouble, not the owner's")

	// A repository that reports a small size and sends a large pack.
	s := pe.connect()
	pe.seed(s, map[string]string{"a.md": "one"})
	tight := pe.newSyncer(pe.committer, pe.realPull(), withMaxRepoBytes(256<<10))
	noise := make([]byte, 1<<20)
	_, err := rand.Read(noise)
	require.NoError(t, err)
	pe.remote(s, map[string]*githubapptest.Entry{"big.bin": {Data: noise}, "b.md": text("2")})
	pull(tight, s)
	c := pe.row(s.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State)
	require.Equal(t, githubsync.CodeRepoTooLarge, *c.ErrorCode)
	require.NotContains(t, pe.disk(s.vault), "b.md")
	require.NotContains(t, pe.disk(s.vault), "big.bin")
}

// Owner decision 2026-11-26, end to end: a .gitignore is the user's file and
// is synced like any other, but its lines decide nothing. A vault file a
// user's rule names reaches GitHub; the trash never does, though a rule asks
// for it; and a rule written on GitHub hides no later save.
func TestUserIgnoreRulesDecideNothing(t *testing.T) {
	pe := newPullEnv(t)
	l := pe.connect()
	pe.seed(l, map[string]string{"a.md": "one", "gone.md": "to be deleted"})
	rules := "secret.md\ndrafts/\n!_deleted_*\n!*.tmp\n"
	pe.write(l.vault, ".gitignore", rules)
	pe.upload(l.vault, "secret.md", "a secret")
	pe.upload(l.vault, "drafts/d.md", "a draft")
	pe.remove(l.vault, "gone.md")
	pe.write(l.vault, "half.tmp", "half a write")
	pe.flush(l.vault)
	pe.synced(l)

	got := pe.remoteEntries(l)
	require.Equal(t, "a secret", string(got["secret.md"].Data))
	require.Equal(t, "a draft", string(got["drafts/d.md"].Data))
	ignore := string(got[".gitignore"].Data)
	require.True(t, strings.HasPrefix(ignore, rules), "the user's lines, byte for byte, with the managed block after them: %q", ignore)
	require.Equal(t, ignore, pe.disk(l.vault)[".gitignore"])
	require.NotContains(t, got, "gone.md")
	require.NotContains(t, got, "half.tmp")
	for p := range got {
		require.NotContains(t, p, "_deleted_", "the trash stays out of the history")
	}

	// GitHub's side writes rules of its own.
	theirs := "*.md\n!_deleted_*\n"
	pe.remote(l, map[string]*githubapptest.Entry{".gitignore": text(theirs)})
	pe.pullNow(l)
	pe.healthy(l)
	require.True(t, strings.HasPrefix(pe.disk(l.vault)[".gitignore"], theirs))
	pe.upload(l.vault, "after.md", "saved after their rule")
	pe.remove(l.vault, "secret.md")
	pe.flush(l.vault)
	pe.synced(l)
	got = pe.remoteEntries(l)
	require.Equal(t, "saved after their rule", string(got["after.md"].Data))
	require.NotContains(t, got, "secret.md")
	for p := range got {
		require.NotContains(t, p, "_deleted_")
	}
}

// Owner decision 2026-11-26: a connection stopped at remote_not_empty is
// put back to work by the owner's consent, recorded on the connection it
// has. No disconnect, no reconnect.
func TestMergeConsentOnAnExistingConnection(t *testing.T) {
	pe := newPullEnv(t)
	ctx := context.Background()
	l := pe.connect()
	theirs := pe.remote(l, map[string]*githubapptest.Entry{"theirs.md": text("from GitHub"), "shared.md": text("their words")})
	pe.upload(l.vault, "ours.md", "from the vault")
	pe.upload(l.vault, "shared.md", "our words")
	ours := pe.flush(l.vault)
	pe.idle(l.vault)
	c := pe.row(l.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State)
	require.Equal(t, githubsync.CodeRemoteNotEmpty, *c.ErrorCode)
	require.False(t, c.MergeExisting)
	created := c.CreatedAt

	ok, err := pe.syncer.MergeExisting(ctx, l.vault)
	require.NoError(t, err)
	require.True(t, ok)
	merge := pe.synced(l)
	pe.healthy(l)
	c = pe.row(l.vault)
	require.True(t, c.MergeExisting)
	require.True(t, created.Equal(c.CreatedAt), "the same connection")
	copyName := pe.conflictName(l, "shared.md", theirs)
	disk := pe.disk(l.vault)
	delete(disk, ".gitignore")
	require.Equal(t, map[string]string{
		"theirs.md": "from GitHub", "ours.md": "from the vault", "shared.md": "our words", copyName: "their words",
	}, disk)
	require.Equal(t, []string{ours, theirs}, pe.gh.Git.Parents(l.repo, merge))
	require.Zero(t, pe.gh.GitForced())
	// Review item 13: what the merge brought is on record as GitHub's.
	require.Equal(t, []string{theirs}, pe.foreign(l))

	// Asked for again, or for a connection stopped for another reason, or
	// for a vault with none: nothing to consent to, and nothing changes.
	ok, err = pe.syncer.MergeExisting(ctx, l.vault)
	require.NoError(t, err)
	require.False(t, ok)
	other := pe.connect()
	pe.seed(other, map[string]string{"a.md": "one"})
	_, err = pe.pool.Exec(ctx, `UPDATE github_sync_connections SET state = 'needs_attention', error_code = 'push_rejected', error_at = now() WHERE vault_id = $1`, other.vault)
	require.NoError(t, err)
	ok, err = pe.syncer.MergeExisting(ctx, other.vault)
	require.NoError(t, err)
	require.False(t, ok)
	c = pe.row(other.vault)
	require.Equal(t, githubsync.StateNeedsAttention, c.State)
	require.False(t, c.MergeExisting)
	ok, err = pe.syncer.MergeExisting(ctx, user(t, pe.pool))
	require.NoError(t, err)
	require.False(t, ok)
}

// Review items 9 and 10: an installation event is a hint, settled against
// what GitHub says now. An old "deleted" or "suspend" delivered again (a
// webhook secret does not expire, and a signed delivery can be replayed)
// stops nothing while the installation is there; and an installation event
// whose nudge was lost, or whose handling failed, is run again by the sweep,
// as a push delivery is.
func TestInstallationEventsAreSettledAndRedelivered(t *testing.T) {
	pe := newPullEnv(t)
	ctx := context.Background()
	a, b := pe.connect(), pe.connect()
	pe.seed(a, map[string]string{"a.md": "one"})
	pe.seed(b, map[string]string{"b.md": "one"})
	state := func(l link) (string, string) {
		c := pe.row(l.vault)
		if c.ErrorCode == nil {
			return c.State, ""
		}
		return c.State, *c.ErrorCode
	}
	insert := func(d githubsync.Delivery) {
		inserted, err := pe.store.InsertDelivery(ctx, d, pe.clock.now())
		require.NoError(t, err)
		require.True(t, inserted)
	}
	statusOf := func(id string) string {
		s, err := pe.store.DeliveryStatus(ctx, id)
		require.NoError(t, err)
		return s
	}

	// Replays: GitHub still has the installation, and it is not suspended.
	for i, action := range []string{"deleted", "suspend"} {
		d := githubsync.Delivery{ID: "replay-" + action, Event: "installation", Action: action, InstallationID: a.installation}
		insert(d)
		require.Equal(t, githubsync.DeliveryDone, pe.syncer.Deliver(ctx, d), "%d", i)
		pe.idle(a.vault)
		st, code := state(a)
		require.Equal(t, githubsync.StateActive, st, action)
		require.Empty(t, code, action)
	}

	// The real ones, never handled: the 202 was sent and the process ended.
	pe.gh.Lock()
	pe.gh.Suspended[a.installation] = true
	delete(pe.gh.InstallationRepos, b.installation)
	pe.gh.Unlock()
	insert(githubsync.Delivery{ID: "lost-suspend", Event: "installation", Action: "suspend", InstallationID: a.installation})
	insert(githubsync.Delivery{ID: "lost-deleted", Event: "installation", Action: "deleted", InstallationID: b.installation})
	pe.sweep()
	st, _ := state(a)
	require.Equal(t, githubsync.StateActive, st, "a fresh delivery is its handler's")
	require.Equal(t, githubsync.DeliveryQueued, statusOf("lost-suspend"))
	pe.clock.advance(61 * time.Second)
	pe.sweep()
	st, code := state(a)
	require.Equal(t, githubsync.StateNeedsAttention, st)
	require.Equal(t, githubsync.CodeInstallationSuspended, code)
	st, code = state(b)
	require.Equal(t, githubsync.StateNeedsAttention, st)
	require.Equal(t, githubsync.CodeInstallationRemoved, code)
	require.Equal(t, githubsync.DeliveryDone, statusOf("lost-suspend"))
	require.Equal(t, githubsync.DeliveryDone, statusOf("lost-deleted"))

	// One that failed (GitHub could not be asked) is run again too, and one
	// that is done is not.
	pe.gh.Lock()
	pe.gh.Suspended[a.installation] = false
	pe.gh.Unlock()
	insert(githubsync.Delivery{ID: "failed-unsuspend", Event: "installation", Action: "unsuspend", InstallationID: a.installation})
	_, err := pe.pool.Exec(ctx, `UPDATE github_webhook_deliveries SET status = 'failed' WHERE delivery_id = 'failed-unsuspend'`)
	require.NoError(t, err)
	pe.clock.advance(61 * time.Second)
	pe.sweep()
	pe.idle(a.vault)
	st, code = state(a)
	require.Equal(t, githubsync.StateActive, st)
	require.Empty(t, code)
	require.Equal(t, githubsync.DeliveryDone, statusOf("failed-unsuspend"))
	st, _ = state(b)
	require.Equal(t, githubsync.StateNeedsAttention, st, "another installation's event")

	// A failed push delivery is pulled by the sweep as a queued one is.
	tip := pe.remote(a, map[string]*githubapptest.Entry{"pushed.md": text("x")})
	insert(githubsync.Delivery{ID: "failed-push", Event: "push", RepoID: a.repo, InstallationID: a.installation, Ref: "refs/heads/main", After: tip})
	_, err = pe.pool.Exec(ctx, `UPDATE github_webhook_deliveries SET status = 'failed' WHERE delivery_id = 'failed-push'`)
	require.NoError(t, err)
	pe.clock.advance(61 * time.Second)
	pe.sweep()
	pe.idle(a.vault)
	require.Equal(t, "x", pe.disk(a.vault)["pushed.md"])
	require.Equal(t, githubsync.DeliveryDone, statusOf("failed-push"))

	// "Repositories removed", run again: the list of them was in the
	// payload, which is not kept. Each connection of the installation asks
	// GitHub itself, and the one whose repository went learns it.
	c := pe.connect()
	pe.seed(c, map[string]string{"c.md": "one"})
	pe.gh.Lock()
	pe.gh.InstallationRepos[c.installation] = []int64{}
	pe.gh.Unlock()
	insert(githubsync.Delivery{ID: "lost-removed", Event: "installation_repositories", Action: "removed", InstallationID: c.installation})
	pe.clock.advance(61 * time.Second)
	pe.sweep()
	pe.idle(c.vault)
	st, code = state(c)
	require.Equal(t, githubsync.StateNeedsAttention, st)
	require.Equal(t, githubsync.CodeRepoAccessRemoved, code)
	require.Equal(t, githubsync.DeliveryDone, statusOf("lost-removed"))
	st, _ = state(a)
	require.Equal(t, githubsync.StateActive, st)
}
