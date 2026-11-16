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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// vaultGit starts per-tenant vault history (backlog E4) when VAULT_GIT_ENABLED
// is true: the committer behind vaultgit.Touch, plus VaultGitSweep and
// VaultGitGC (which run only when LV_GO_JOBS names them, like every job). stop
// flushes pending commits; call it on shutdown. Off, it registers nothing and
// Touch stays a no-op.
func vaultGit(pool *pgxpool.Pool, cfg config.Config, paths vaultpaths.Paths, disk *diskguard.Guard) (registered []jobs.Job, stop func()) {
	if !swiftBoolDefaultFalse(cfg.VaultGitEnabled) {
		return nil, func() {}
	}
	opts := vaultgit.Options{
		RawRoot: paths.Raw,
		Logger:  slog.Default().With("label", "lv.vault.git"),
	}
	if pool != nil {
		opts.Lock = vaultgit.PGLocker{Pool: pool}
	}
	if disk != nil {
		opts.Space = disk.Check
	}
	c := vaultgit.NewCommitter(opts)
	vaultgit.Start(c)
	tenants := filepath.Join(paths.Root, "tenants")
	return []jobs.Job{c.SweepJob(tenants), c.GCJob(tenants)}, func() {
		vaultgit.Start(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c.Close(ctx)
	}
}
