package main

import (
	"context"
	"log/slog"
	"lumina/internal/config"
	"lumina/internal/diskguard"
	"lumina/internal/jobs"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultpaths"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// vaultHistory is the running committer and its commit hook. The hook is set
// after the committer exists (GitHub sync is built later and needs the
// committer itself), so it sits behind a pointer the committer reads on every
// commit.
type vaultHistory struct {
	committer *vaultgit.Committer
	hook      atomic.Pointer[func(tenant uuid.UUID, sha string)]
}

// onCommit sets who hears about each new commit (vaultgit.Options.OnCommit).
func (h *vaultHistory) onCommit(fn func(tenant uuid.UUID, sha string)) { h.hook.Store(&fn) }

// vaultGit starts per-tenant vault history (backlog E4) when VAULT_GIT_ENABLED
// is true: the committer behind vaultgit.Touch, plus VaultGitSweep and
// VaultGitGC (which run only when LV_GO_JOBS names them, like every job). stop
// flushes pending commits; call it on shutdown. Off, it registers nothing,
// history is nil and Touch stays a no-op.
func vaultGit(pool *pgxpool.Pool, cfg config.Config, paths vaultpaths.Paths, disk *diskguard.Guard) (history *vaultHistory, registered []jobs.Job, stop func()) {
	if !swiftBoolDefaultFalse(cfg.VaultGitEnabled) {
		return nil, nil, func() {}
	}
	history = &vaultHistory{}
	opts := vaultgit.Options{
		RawRoot: paths.Raw,
		Logger:  slog.Default().With("label", "lv.vault.git"),
		OnCommit: func(tenant uuid.UUID, sha string) {
			if fn := history.hook.Load(); fn != nil {
				(*fn)(tenant, sha)
			}
		},
	}
	if pool != nil {
		opts.Lock = vaultgit.PGLocker{Pool: pool}
	}
	if disk != nil {
		opts.Space = disk.Check
	}
	c := vaultgit.NewCommitter(opts)
	history.committer = c
	vaultgit.Start(c)
	tenants := filepath.Join(paths.Root, "tenants")
	return history, []jobs.Job{c.SweepJob(tenants), c.GCJob(tenants)}, func() {
		vaultgit.Start(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c.Close(ctx)
	}
}
