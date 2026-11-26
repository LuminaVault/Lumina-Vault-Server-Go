package githubsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"lumina/internal/modules/vault"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultgit"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file is the pull of spec D10: fetch, diff, apply through the vault's
// write pipeline, then move the ref. No git command ever writes the working
// tree; raw/ changes only through Files.
//
// A pull has three phases. Plan and Finish run under the vault's lock
// (Committer.WithRepo); Apply runs without it, so a long first import never
// keeps commits, the sweep or gc waiting for longer than a fetch.
//
// Nothing here is kept in memory between calls except the Plan the caller
// carries from one phase to the next, so it is safe with any number of
// replicas: two that plan the same remote tip apply the same operations,
// every operation is a no-op once its result is on disk, and the second
// Finish finds the tip already merged.

var (
	// ErrRepoTooLarge: the remote tree has more files the vault would hold
	// than the limit (D11).
	ErrRepoTooLarge = errors.New("githubsync: remote has too many files")
	// ErrMergeConsent: the remote branch has files and a history the vault
	// shares nothing with, the connection never synced, and its owner did not
	// agree to a merge (D11).
	ErrMergeConsent = errors.New("githubsync: merging the remote needs the owner's consent")
	// ErrUnsafeTree: the remote tree has a path with a ".git" segment, in
	// any case. GitHub refuses a push that has one, so this is never an
	// honest repository. Such a path cannot even be carried in the index:
	// on a case-insensitive disk it names the repository's own directory.
	// Nothing of that commit is merged.
	ErrUnsafeTree = errors.New("githubsync: the remote tree has a .git path")
)

// Files is what a pull needs from the vault's write pipeline: vault.Files,
// the module's own instance, so its path locks are the ones every save takes.
type Files interface {
	Write(ctx context.Context, in vault.WriteInput) (*vault.WriteResult, error)
	Find(ctx context.Context, vaultID uuid.UUID, path string) (*vaultfiles.VaultFile, error)
	MoveIfMatch(ctx context.Context, vaultID uuid.UUID, path, newPath, ifMatch string) (*vaultfiles.VaultFile, error)
	DeleteIfMatch(ctx context.Context, vaultID, actor uuid.UUID, path, ifMatch string) error
	ReresolvePending(ctx context.Context, vaultID uuid.UUID)
}

// Puller is a Syncer's pull: what brings the remote branch into the vault.
// Plan and Finish are called inside the vault's lock, Apply outside it.
type Puller interface {
	// Plan commits what is pending, fetches the branch and works out what to
	// do. A nil plan means there is nothing to pull.
	Plan(ctx context.Context, in PullInput) (*Plan, error)
	// Apply runs the plan's operations through the vault's write pipeline.
	Apply(ctx context.Context, plan *Plan) error
	// Finish writes the held list, moves the ref and stores the report.
	Finish(ctx context.Context, in PullInput, plan *Plan) error
}

const (
	defaultFetchTimeout = 120 * time.Second
	defaultApplyTimeout = 30 * time.Minute
	defaultParallel     = 4
	reportSample        = 20
)

// PullOptions configure a Pull. Files, Store and RawRoot are required.
type PullOptions struct {
	Files   Files
	Store   *Store
	RawRoot func(vault uuid.UUID) string
	// MaxFileSize is the tenant's history limit: a larger remote file is held
	// (D8). Nil is vaultgit.DefaultMaxFileSize.
	MaxFileSize func(vault uuid.UUID) int64
	// MaxFiles is GITHUB_SYNC_MAX_FILES (D11). Zero is no limit.
	MaxFiles int
	// Space is the disk guard, checked before a fetch (D11). Nil: no check.
	Space func(incoming int64) error
	// FetchTimeout bounds the fetch (default 120 s); ApplyTimeout the whole
	// apply phase (default 30 min). Parallel is how many files of one vault
	// are written at once (default 4).
	FetchTimeout time.Duration
	ApplyTimeout time.Duration
	Parallel     int
	Now          func() time.Time
	Log          *slog.Logger
}

// Pull is the Puller.
type Pull struct {
	opts PullOptions
	log  *slog.Logger

	mu       sync.Mutex
	progress map[uuid.UUID]*Progress
}

// Progress is how far an apply is, for the status route.
type Progress struct{ Done, Total int }

func NewPull(opts PullOptions) *Pull {
	if opts.MaxFileSize == nil {
		opts.MaxFileSize = func(uuid.UUID) int64 { return vaultgit.DefaultMaxFileSize }
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = defaultFetchTimeout
	}
	if opts.ApplyTimeout <= 0 {
		opts.ApplyTimeout = defaultApplyTimeout
	}
	if opts.Parallel <= 0 {
		opts.Parallel = defaultParallel
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Pull{opts: opts, log: log, progress: map[uuid.UUID]*Progress{}}
}

// Progress reports an apply running for the vault in this process.
func (p *Pull) Progress(vault uuid.UUID) (Progress, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr := p.progress[vault]; pr != nil {
		return *pr, true
	}
	return Progress{}, false
}

// MARK: the plan

type opKind int

const (
	// opWrite takes the remote version at Path. Local is the blob the plan saw
	// there ("" = none): the write only replaces that.
	opWrite opKind = iota
	// opDelete trashes Path, which the remote deleted and the vault left alone.
	opDelete
	// opMove renames Path to To: the remote moved identical bytes.
	opMove
	// opConflict keeps both: the local file stays, the remote version goes
	// beside it as a conflict copy.
	opConflict
	// opStepAside clears Path for a remote entry the vault cannot hold (D8):
	// an unchanged local file goes to the trash, an edited one becomes a
	// conflict copy first.
	opStepAside
)

type op struct {
	Kind   opKind
	Path   string
	To     string // opMove
	Remote string // blob id of the remote version
	Local  string // blob id the plan saw in the local tree, "" = none
	Edited bool   // opStepAside: the local file changed since the base
}

// Plan is one pull between its phases.
type Plan struct {
	conn   Connection
	local  string // L: the local tip the plan was made from
	remote string // R: the fetched tip
	stamp  time.Time
	ops    []op
	// held is every path of R's tree that stays in git only, and local git-only
	// entries the remote never had (carry), which a merge keeps.
	held  []Held
	carry []string
	// ignore is the remote .gitignore's blob, "" when it has none to take.
	ignore string
	// written are the paths Apply was to put on disk. One that is missing at
	// Finish is held instead: no path of R may be left neither on disk nor
	// listed, or the next commit would delete it on GitHub.
	written map[string]bool

	mu        sync.Mutex
	conflicts []Conflict
	kept      []string
	refused   []Held
}

// Remote is the tip the plan merges.
func (p *Plan) Remote() string { return p.remote }

func same(a vaultgit.TreeEntry, inA bool, b vaultgit.TreeEntry, inB bool) bool {
	return inA == inB && (!inA || (a.ID == b.ID && a.Mode == b.Mode))
}

// Plan is phase 1 (D10 steps 1 to 5), under the vault's lock.
func (p *Pull) Plan(ctx context.Context, in PullInput) (*Plan, error) {
	conn, repo, vaultID := in.Connection, in.Repo, in.Connection.VaultID
	maxSize := p.opts.MaxFileSize(vaultID)
	if p.opts.Space != nil {
		if err := p.opts.Space(0); err != nil {
			return nil, fmt.Errorf("githubsync: disk guard: %w", err)
		}
	}
	// Step 1: whatever is pending becomes a commit, so the local tree is what
	// raw/ holds. Not Committer.Flush: it takes this same lock.
	if _, _, err := repo.Commit(vaultgit.Message{Reason: "sweep"}, maxSize, p.opts.Now()); err != nil {
		return nil, err
	}
	local, err := repo.Head()
	if err != nil {
		return nil, err
	}
	fctx, cancel := context.WithTimeout(ctx, p.opts.FetchTimeout)
	remote, err := repo.Fetch(fctx, conn.Branch, in.Token)
	cancel()
	if err != nil {
		return nil, err
	}
	if remote == "" {
		return nil, nil // no such branch yet: the first push creates it
	}
	if local != "" {
		// Step 3 (and D12): the remote tip is already in the local history.
		if merged, err := repo.IsAncestor(remote, local); err != nil || merged {
			return nil, err
		}
	}
	base := ""
	if local != "" {
		if base, err = repo.MergeBase(local, remote); err != nil {
			return nil, err
		}
	}
	tb, err := repo.Tree(base)
	if err != nil {
		return nil, err
	}
	tl, err := repo.Tree(local)
	if err != nil {
		return nil, err
	}
	tr, err := repo.Tree(remote)
	if err != nil {
		return nil, err
	}
	for name := range tr {
		for seg := range strings.SplitSeq(name, "/") {
			if strings.EqualFold(seg, ".git") {
				return nil, ErrUnsafeTree
			}
		}
	}
	if base == "" && conn.RemoteSHA == nil && !conn.MergeExisting && len(tr) > 0 {
		return nil, ErrMergeConsent
	}
	oldHeld, err := repo.Held()
	if err != nil {
		return nil, err
	}
	gitOnly := map[string]bool{}
	for _, h := range oldHeld {
		gitOnly[h] = true
	}
	stamp, err := repo.CommitTime(remote)
	if err != nil {
		return nil, err
	}
	plan := &Plan{conn: conn, local: local, remote: remote, stamp: stamp, written: map[string]bool{}}
	classify := func(e vaultgit.TreeEntry) HeldReason { return Classify(e.Path, e.Mode, e.Size, maxSize) }

	paths := map[string]struct{}{}
	for _, t := range []map[string]vaultgit.TreeEntry{tb, tl, tr} {
		for k := range t {
			paths[k] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(paths))
	for k := range paths {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	materialized := 0
	var deletes, adds []int // indexes into plan.ops, for rename detection
	for _, name := range sorted {
		r, inR := tr[name]
		l, inL := tl[name]
		b, inB := tb[name]
		remoteChanged := !same(r, inR, b, inB)
		localChanged := !same(l, inL, b, inB)
		// A local entry the vault does not hold as a file: listed as held, or
		// of a kind it cannot hold.
		localGitOnly := inL && (gitOnly[name] || classify(l) != Materialized)

		if name == IgnorePath {
			if inR && classify(r) == IgnoreFile {
				plan.ignore = r.ID
			} else if inR {
				plan.held = append(plan.held, Held{Path: name, Reason: classify(r)})
			}
			continue
		}
		if !inR {
			switch {
			case !inL:
			case localGitOnly && remoteChanged: // the remote deleted an entry it alone holds
			case localGitOnly && gitOnly[name] && classify(l) == Materialized:
				// Held once, holdable now, and the remote never had it: it
				// becomes a vault file.
				plan.add(op{Kind: opWrite, Path: name, Remote: l.ID})
			case localGitOnly:
				reason := classify(l)
				if reason == Materialized || reason == IgnoreFile {
					reason = HeldHidden
				}
				plan.held = append(plan.held, Held{Path: name, Reason: reason})
				plan.carry = append(plan.carry, name)
			case remoteChanged && !localChanged:
				deletes = append(deletes, plan.add(op{Kind: opDelete, Path: name, Local: l.ID}))
			}
			continue
		}
		if reason := classify(r); reason != Materialized {
			plan.held = append(plan.held, Held{Path: name, Reason: reason})
			if inL && !localGitOnly {
				// The path is a vault file and is held on the remote now: the
				// local file must not shadow the entry.
				plan.add(op{Kind: opStepAside, Path: name, Local: l.ID, Edited: localChanged})
			}
			continue
		}
		materialized++
		switch {
		case localGitOnly || (!inL && gitOnly[name]):
			// Held until now and holdable: each pull re-checks the list (D8).
			plan.add(op{Kind: opWrite, Path: name, Remote: r.ID})
		case !remoteChanged:
			// Only the vault changed, or nobody did: nothing to take.
		case same(l, inL, r, inR):
			// Both sides made the same change. No bytes to take, but the
			// file may be what a stopped pull left behind, bytes with no
			// row: the apply looks, and completes it.
			plan.add(op{Kind: opWrite, Path: name, Remote: r.ID, Local: l.ID})
		case !inL && !localChanged:
			adds = append(adds, plan.add(op{Kind: opWrite, Path: name, Remote: r.ID}))
		case !inL:
			// Deleted here, modified there: the remote version is restored.
			plan.add(op{Kind: opWrite, Path: name, Remote: r.ID})
		case !localChanged:
			plan.add(op{Kind: opWrite, Path: name, Remote: r.ID, Local: l.ID})
		default:
			plan.add(op{Kind: opConflict, Path: name, Remote: r.ID, Local: l.ID})
		}
	}
	if p.opts.MaxFiles > 0 && materialized > p.opts.MaxFiles {
		return nil, ErrRepoTooLarge
	}
	plan.renames(deletes, adds)
	for _, o := range plan.ops {
		switch o.Kind {
		case opWrite:
			plan.written[o.Path] = true
		case opMove:
			plan.written[o.To] = true
		case opDelete, opConflict, opStepAside:
		}
	}
	// A dead lock session means another replica may be in this repository:
	// nothing is applied on the strength of a lock that is gone.
	if err := repo.StillLocked(ctx); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *Plan) add(o op) int {
	p.ops = append(p.ops, o)
	return len(p.ops) - 1
}

// renames turns one deleted path and one added path with the identical blob
// into a move (D10). Anything less certain stays a delete plus an add.
func (p *Plan) renames(deletes, adds []int) {
	byBlob := func(idx []int, id func(op) string) map[string][]int {
		m := map[string][]int{}
		for _, i := range idx {
			m[id(p.ops[i])] = append(m[id(p.ops[i])], i)
		}
		return m
	}
	gone := byBlob(deletes, func(o op) string { return o.Local })
	came := byBlob(adds, func(o op) string { return o.Remote })
	drop := map[int]bool{}
	for blob, from := range gone {
		to := came[blob]
		if len(from) != 1 || len(to) != 1 {
			continue
		}
		p.ops[from[0]] = op{Kind: opMove, Path: p.ops[from[0]].Path, To: p.ops[to[0]].Path, Remote: blob, Local: blob}
		drop[to[0]] = true
	}
	if len(drop) == 0 {
		return
	}
	kept := p.ops[:0]
	for i, o := range p.ops {
		if !drop[i] {
			kept = append(kept, o)
		}
	}
	p.ops = kept
}

// MARK: apply

type applier struct {
	p     *Pull
	plan  *Plan
	repo  *vaultgit.Repo
	vault uuid.UUID
	actor uuid.UUID
	root  string
	// read serialises object reads: go-git's pack reader is not safe for
	// concurrent use, and answers "not found" when two readers meet.
	read sync.Mutex
}

// blob reads a remote blob from the object store.
func (a *applier) blob(id string) ([]byte, error) {
	a.read.Lock()
	defer a.read.Unlock()
	return a.repo.BlobByID(id)
}

// Apply is phase 2 (D10 steps 6 and 7), with no lock held. It stops at the
// first operation that fails for a reason of the server's (disk, database):
// the ref is then not moved, and files already written are ordinary local
// edits the next pull finds equal to the remote.
func (p *Pull) Apply(ctx context.Context, plan *Plan) error {
	vaultID := plan.conn.VaultID
	ctx, cancel := context.WithTimeout(ctx, p.opts.ApplyTimeout)
	defer cancel()
	owner, err := p.opts.Store.VaultOwner(ctx, vaultID)
	if err != nil {
		return err
	}
	if owner == uuid.Nil {
		return errors.New("githubsync: the vault has no owner")
	}
	root := p.opts.RawRoot(vaultID)
	// Objects are read through a handle of this phase's own: the plan's
	// belongs to the lock that was let go.
	repo, err := vaultgit.Open(root)
	if err != nil {
		return err
	}
	a := &applier{p: p, plan: plan, repo: repo, vault: vaultID, actor: owner, root: root}

	p.mu.Lock()
	pr := &Progress{Total: len(plan.ops)}
	p.progress[vaultID] = pr
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.progress[vaultID] == pr {
			delete(p.progress, vaultID)
		}
		p.mu.Unlock()
	}()

	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	work := make(chan op)
	var wg sync.WaitGroup
	for range min(p.opts.Parallel, max(len(plan.ops), 1)) {
		wg.Go(func() {
			for o := range work {
				if ctx.Err() != nil {
					continue
				}
				if err := a.run(ctx, o); err != nil {
					stop(fmt.Errorf("githubsync: pull %s: %w", o.Path, err))
					continue
				}
				p.mu.Lock()
				pr.Done++
				p.mu.Unlock()
			}
		})
	}
	for _, o := range plan.ops {
		work <- o
	}
	close(work)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if len(plan.ops) > 0 {
		p.opts.Files.ReresolvePending(ctx, vaultID)
	}
	return nil
}

// run does one operation. Something that is not a file sitting where one
// would go (a folder named like the note) is a path the vault cannot take:
// it is held, not a failure.
func (a *applier) run(ctx context.Context, o op) error {
	err := a.do(ctx, o)
	if errors.Is(err, errNotAFile) {
		return a.refuse(o.Path, err)
	}
	return err
}

func (a *applier) do(ctx context.Context, o op) error {
	switch o.Kind {
	case opWrite:
		return a.write(ctx, o.Path, o.Remote, o.Local)
	case opDelete:
		return a.remove(ctx, o.Path, o.Local)
	case opMove:
		return a.move(ctx, o)
	case opConflict:
		return a.conflict(ctx, o.Path, o.Remote)
	case opStepAside:
		return a.stepAside(ctx, o)
	}
	return nil
}

// status is the HTTP status the write pipeline's error stands for, 0 for any
// other error.
func status(err error) int {
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) {
		return coded.StatusCode()
	}
	return 0
}

// refusal reports an error that says the vault will not hold this file at
// all (a name, a type or a size it rejects), as opposed to one that says the
// server could not do it now.
func refusal(err error) bool {
	switch status(err) {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// raced reports a write that lost to another writer of the same path: the
// pipeline's own answer, or the database's when two creates of one path met
// (another replica applying the same pull). Either way: look again.
func raced(err error) bool {
	if errors.Is(err, vault.ErrPreconditionFailed) {
		return true
	}
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// errNotAFile: a folder, a link or a device sits where a vault file would go.
var errNotAFile = errors.New("githubsync: not a regular file")

// disk reads the vault file at name. ok is false when there is none.
func (a *applier) disk(name string) (data []byte, ok bool, err error) {
	target, err := vaultfiles.ResolveInside(a.root, name)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errNotAFile
	}
	data, err = os.ReadFile(target) //nolint:gosec // target comes from ResolveInside
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (a *applier) hasRow(ctx context.Context, name string) (bool, error) {
	_, err := a.p.opts.Files.Find(ctx, a.vault, name)
	if status(err) == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func (a *applier) input(name string, data []byte) vault.WriteInput {
	return vault.WriteInput{
		Vault: a.vault, Actor: a.actor, Path: name, Data: data,
		Reason: "pull", Source: "github", Quiet: true, DeferReresolve: true,
		// Processed stays false: the bytes are new to the vault, so the note
		// counts as not compiled yet, exactly as after an upload. Compiling
		// is something a user or agent asks for; a pull starts none.
	}
}

// write puts the remote blob at name. local is the blob the plan saw there:
// the write replaces only that, and creates only where nothing is. A file
// that changed since the plan (a save landed mid-pull) is never overwritten:
// the remote version becomes a conflict copy. A file that already has the
// remote bytes is left alone, so the operation can run twice.
func (a *applier) write(ctx context.Context, name, remote, local string) error {
	data, err := a.blob(remote)
	if err != nil {
		return err
	}
	for range 3 {
		current, exists, err := a.disk(name)
		if err != nil {
			return a.refuse(name, err)
		}
		in := a.input(name, data)
		switch {
		case !exists:
			in.IfAbsent = true
		case BlobID(current) == remote:
			return a.ensureRow(ctx, name, current)
		case local == "" || BlobID(current) != local:
			return a.conflictCopy(ctx, name, data)
		default:
			in.IfMatch = sha256Hex(current)
		}
		_, err = a.p.opts.Files.Write(ctx, in)
		switch {
		case err == nil:
			return nil
		case raced(err):
			continue // the file moved under us: look again
		case refusal(err):
			return a.refuse(name, err)
		default:
			return err
		}
	}
	return fmt.Errorf("%s kept changing", name)
}

// ensureRow gives the file at name, which has the bytes current, its row if
// it has none: a writer that was stopped between the bytes and the row (a
// cancelled pull, a crash) left it so, and nobody else will come back for it.
func (a *applier) ensureRow(ctx context.Context, name string, current []byte) error {
	var err error
	for range 3 {
		var has bool
		if has, err = a.hasRow(ctx, name); err != nil || has {
			return err
		}
		in := a.input(name, current)
		in.IfMatch = sha256Hex(current)
		if _, err = a.p.opts.Files.Write(ctx, in); err == nil {
			return nil
		}
		if !raced(err) {
			return err
		}
		// Another writer is at it. If the bytes changed, the file is no
		// longer this pull's to complete.
		got, ok, derr := a.disk(name)
		if derr != nil || !ok || BlobID(got) != BlobID(current) {
			return derr
		}
	}
	return err
}

// refuse records a remote path the vault would not take. It stays in git
// only, like any held path: skipping it silently would delete it on GitHub
// with the next commit, and failing would stop every pull for ever.
func (a *applier) refuse(name string, cause error) error {
	if !refusal(cause) && !errors.Is(cause, errNotAFile) {
		return cause
	}
	a.p.log.WarnContext(context.Background(), "github sync: the vault refused a remote file; it stays on GitHub only",
		"vault", a.vault.String(), "status", status(cause))
	a.plan.mu.Lock()
	a.plan.refused = append(a.plan.refused, Held{Path: name, Reason: HeldBadName})
	a.plan.mu.Unlock()
	return nil
}

// ConflictName is the name a conflict copy of name gets: <stem>.conflict-
// <yyyymmdd-hhmmss>.<ext> beside it, and -2, -3 ... on a clash (D10).
func ConflictName(name string, at time.Time, n int) string {
	dir, file := path.Split(name)
	ext := path.Ext(file)
	stem := strings.TrimSuffix(file, ext)
	suffix := ".conflict-" + at.UTC().Format("20060102-150405")
	if n > 1 {
		suffix += "-" + strconv.Itoa(n)
	}
	return dir + stem + suffix + ext
}

// existingCopy finds a conflict copy of name that already has these bytes: a
// pull that ran before, or runs beside this one, made it.
func (a *applier) existingCopy(name string, data []byte) (string, bool) {
	dir, file := path.Split(name)
	ext := path.Ext(file)
	prefix := strings.TrimSuffix(file, ext) + ".conflict-"
	target, err := vaultfiles.ResolveInside(a.root, name)
	if err != nil {
		return "", false
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || path.Ext(e.Name()) != ext || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() != int64(len(data)) {
			continue
		}
		got, ok, err := a.disk(dir + e.Name())
		if err == nil && ok && BlobID(got) == BlobID(data) {
			return dir + e.Name(), true
		}
	}
	return "", false
}

// conflictCopy writes data beside name and reports the pair. The name comes
// from the remote commit's time, not the clock, so a retry or another
// replica writes the same file rather than one more.
func (a *applier) conflictCopy(ctx context.Context, name string, data []byte) error {
	copyName, found := a.existingCopy(name, data)
	if found {
		if err := a.ensureRow(ctx, copyName, data); err != nil {
			return err
		}
	}
	for n := 1; !found; n++ {
		if n > 50 {
			return fmt.Errorf("no free conflict name for %s", name)
		}
		copyName = ConflictName(name, a.plan.stamp, n)
		in := a.input(copyName, data)
		in.IfAbsent = true
		_, err := a.p.opts.Files.Write(ctx, in)
		switch {
		case err == nil:
			found = true
		case raced(err):
			if got, ok, derr := a.disk(copyName); derr == nil && ok && BlobID(got) == BlobID(data) {
				if err := a.ensureRow(ctx, copyName, data); err != nil {
					return err
				}
				found = true
			}
		case refusal(err):
			return a.refuse(name, err)
		default:
			return err
		}
	}
	a.plan.mu.Lock()
	a.plan.conflicts = append(a.plan.conflicts, Conflict{Path: name, ConflictPath: copyName, At: a.p.opts.Now().UTC()})
	a.plan.mu.Unlock()
	return nil
}

// conflict is both sides changed: the local file stays, the remote version
// goes beside it. If the local file meanwhile has the remote bytes there is
// nothing to keep twice, and if it is gone the remote version takes the path.
func (a *applier) conflict(ctx context.Context, name, remote string) error {
	current, exists, err := a.disk(name)
	if err != nil {
		return err
	}
	if !exists {
		return a.write(ctx, name, remote, "")
	}
	if BlobID(current) == remote {
		return nil
	}
	data, err := a.blob(remote)
	if err != nil {
		return err
	}
	return a.conflictCopy(ctx, name, data)
}

func (a *applier) keep(name string) {
	a.plan.mu.Lock()
	a.plan.kept = append(a.plan.kept, name)
	a.plan.mu.Unlock()
}

// remove trashes a file the remote deleted, unless it changed since the plan
// (then the local file stays and goes back to GitHub). A file with no row
// cannot be trashed: it is kept and reported (D7).
func (a *applier) remove(ctx context.Context, name, local string) error {
	current, exists, err := a.disk(name)
	if err != nil || !exists {
		return err
	}
	if BlobID(current) != local {
		return nil
	}
	err = a.p.opts.Files.DeleteIfMatch(ctx, a.vault, a.actor, name, sha256Hex(current))
	switch {
	case err == nil, errors.Is(err, vault.ErrPreconditionFailed):
		return nil
	case status(err) == http.StatusNotFound:
		if _, still, _ := a.disk(name); still {
			a.keep(name)
		}
		return nil
	}
	return err
}

// move renames a file the remote renamed with identical bytes, which keeps
// its row, Space, memory and chunks. When that cannot be done as a move it
// is an add of the new path, and the old one is deleted if it still can be.
func (a *applier) move(ctx context.Context, o op) error {
	current, exists, err := a.disk(o.Path)
	if err != nil {
		return err
	}
	_, taken, err := a.disk(o.To)
	if err != nil {
		return err
	}
	if exists && !taken && BlobID(current) == o.Local {
		_, err := a.p.opts.Files.MoveIfMatch(ctx, a.vault, o.Path, o.To, sha256Hex(current))
		switch {
		case err == nil:
			return nil
		case errors.Is(err, vault.ErrPreconditionFailed), status(err) == http.StatusNotFound, status(err) == http.StatusConflict:
		case refusal(err):
			return a.refuse(o.To, err)
		default:
			return err
		}
	}
	if err := a.write(ctx, o.To, o.Remote, ""); err != nil {
		return err
	}
	return a.remove(ctx, o.Path, o.Local)
}

// stepAside clears a path the remote now has as something the vault cannot
// hold. An unchanged file goes to the trash; an edited one is first renamed
// to a conflict copy, so its text stays in the vault.
func (a *applier) stepAside(ctx context.Context, o op) error {
	current, exists, err := a.disk(o.Path)
	if err != nil || !exists {
		return err
	}
	if !o.Edited && BlobID(current) == o.Local {
		err := a.p.opts.Files.DeleteIfMatch(ctx, a.vault, a.actor, o.Path, sha256Hex(current))
		if err == nil || status(err) == http.StatusNotFound {
			return nil
		}
		if !errors.Is(err, vault.ErrPreconditionFailed) {
			return err
		}
		if current, exists, err = a.disk(o.Path); err != nil || !exists {
			return err
		}
	}
	for n := 1; n <= 50; n++ {
		copyName := ConflictName(o.Path, a.plan.stamp, n)
		if _, taken, err := a.disk(copyName); err != nil || taken {
			if err != nil {
				return err
			}
			continue
		}
		_, err := a.p.opts.Files.MoveIfMatch(ctx, a.vault, o.Path, copyName, sha256Hex(current))
		switch {
		case err == nil:
			a.plan.mu.Lock()
			a.plan.conflicts = append(a.plan.conflicts, Conflict{Path: o.Path, ConflictPath: copyName, At: a.p.opts.Now().UTC()})
			a.plan.mu.Unlock()
			return nil
		case status(err) == http.StatusNotFound:
			return nil // no row: the file stays, and the held entry is not shadowed (the held list wins)
		case errors.Is(err, vault.ErrPreconditionFailed), status(err) == http.StatusConflict:
			if current, exists, err = a.disk(o.Path); err != nil || !exists {
				return err
			}
		default:
			return err
		}
	}
	return fmt.Errorf("no free conflict name for %s", o.Path)
}

// MARK: finish

// Finish is phase 3 (D10 steps 8 to 10), under the vault's lock again.
func (p *Pull) Finish(ctx context.Context, in PullInput, plan *Plan) error {
	repo, conn, vaultID := in.Repo, plan.conn, plan.conn.VaultID
	now := p.opts.Now()
	// Another replica finished the same pull while this one applied.
	if merged, err := repo.Contains(plan.remote); err != nil || merged {
		return err
	}
	head, err := repo.Head()
	if err != nil {
		return err
	}
	held := append(append([]Held{}, plan.held...), plan.refused...)
	listed := map[string]bool{}
	for _, h := range held {
		listed[h.Path] = true
	}
	// The invariant that makes a pull unable to delete anything on GitHub:
	// every path the remote tree has is on disk or on the held list.
	for name := range plan.written {
		if listed[name] {
			continue
		}
		target, err := vaultfiles.ResolveInside(p.opts.RawRoot(vaultID), name)
		if err == nil {
			if _, err = os.Lstat(target); err == nil {
				continue
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		held = append(held, Held{Path: name, Reason: HeldBadName})
		listed[name] = true
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Path < held[j].Path })
	paths := make([]string, 0, len(held))
	for _, h := range held {
		paths = append(paths, h.Path)
	}
	if err := repo.StillLocked(ctx); err != nil {
		return err
	}
	// Step 8: the list first. Staging reads it; written later, every newly
	// held path would be staged as a deletion and pushed.
	if err := repo.SetHeld(paths); err != nil {
		return err
	}
	if plan.ignore != "" {
		data, err := repo.BlobByID(plan.ignore)
		if err != nil {
			return err
		}
		if _, err := repo.TakeIgnore(data); err != nil {
			return err
		}
	}
	// Step 9: move the ref, without touching a file.
	forward := head == ""
	if !forward {
		if forward, err = repo.IsAncestor(head, plan.remote); err != nil {
			return err
		}
	}
	plan.mu.Lock()
	conflicts := append([]Conflict{}, plan.conflicts...)
	kept := append([]string{}, plan.kept...)
	plan.mu.Unlock()
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	sort.Strings(kept)
	if forward {
		// Nothing local to keep: HEAD becomes the remote commit itself. No
		// commit is made, so there is nothing to push back (D12).
		if err := repo.FastForward(plan.remote); err != nil {
			return err
		}
	} else {
		msg := vaultgit.Message{
			Reason: "pull", Source: "github",
			Summary: fmt.Sprintf("pull: merge %s from GitHub (%d conflicts)", conn.Branch, len(conflicts)),
		}
		if _, err := repo.MergeCommit(plan.remote, plan.carry, msg, p.opts.MaxFileSize(vaultID), now); err != nil {
			return err
		}
	}
	// Whatever raw/ has beyond the new HEAD (a conflict copy after a
	// fast-forward, the managed .gitignore block) is for the committer.
	vaultgit.Touch(vaultID, "pull", "", "github")
	report := Report{HeldCount: len(held)}
	report.Conflicts = conflicts[:min(len(conflicts), reportSample)]
	report.Held = held[:min(len(held), reportSample)]
	report.Kept = kept[:min(len(kept), reportSample)]
	// Step 10. The store's context outlives a cancelled pull: the ref moved.
	if _, err := p.opts.Store.MarkPulled(context.WithoutCancel(ctx), conn.Key(), plan.remote, now, report); err != nil {
		return err
	}
	p.log.InfoContext(ctx, "github sync: pulled", "vault", vaultID.String(), "repo", conn.RepoID,
		"operations", len(plan.ops), "conflicts", len(conflicts), "held", len(held), "fast_forward", forward)
	return nil
}
