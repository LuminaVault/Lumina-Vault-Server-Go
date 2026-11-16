package vaultgit

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/jobs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/google/uuid"
)

// tenantsWithRaw lists the tenants under <root>/tenants that have a raw directory.
func tenantsWithRaw(tenantsDir string) ([]uuid.UUID, error) {
	entries, err := os.ReadDir(tenantsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := uuid.Parse(e.Name())
		if err != nil {
			continue
		}
		if info, err := os.Stat(filepath.Join(tenantsDir, e.Name(), "raw")); err == nil && info.IsDir() {
			out = append(out, id)
		}
	}
	return out, nil
}

// Sweep commits every tenant whose tree has changes no Touch reported, and
// creates the repository for tenants that have none yet (spec D5: this is also
// the init/backfill, folded into one job). Tenants locked by another replica
// are left for the next run.
func (c *Committer) Sweep(ctx context.Context, tenantsDir string) (committed, failed int, err error) {
	tenants, err := tenantsWithRaw(tenantsDir)
	if err != nil {
		return 0, 0, err
	}
	for _, t := range tenants {
		if ctx.Err() != nil {
			return committed, failed, ctx.Err()
		}
		sha, err := c.Flush(ctx, t, Message{Reason: "sweep"})
		switch {
		case errors.Is(err, ErrLocked):
		case err != nil:
			failed++
			c.opts.Logger.WarnContext(ctx, "vault git sweep failed", "tenant", strings.ToUpper(t.String()), "error", err)
		case sha != "":
			committed++
		}
	}
	return committed, failed, nil
}

// GC repacks every tenant repository (spec D5), under the tenant's lock so it
// never overlaps a commit. Loose objects older than an hour are packed and
// removed; packs younger than that are left for the next run.
func (c *Committer) GC(ctx context.Context, tenantsDir string) (repacked, failed int, err error) {
	tenants, err := tenantsWithRaw(tenantsDir)
	if err != nil {
		return 0, 0, err
	}
	for _, t := range tenants {
		if ctx.Err() != nil {
			return repacked, failed, ctx.Err()
		}
		switch err := c.gcTenant(ctx, t); {
		case errors.Is(err, ErrNoRepo), errors.Is(err, ErrLocked):
		case err != nil:
			failed++
			c.opts.Logger.WarnContext(ctx, "vault git gc failed", "tenant", strings.ToUpper(t.String()), "error", err)
		default:
			repacked++
		}
	}
	return repacked, failed, nil
}

func (c *Committer) gcTenant(ctx context.Context, tenant uuid.UUID) error {
	l := c.tenantLock(tenant)
	l.Lock()
	defer l.Unlock()
	if c.opts.Lock != nil {
		unlock, ok, err := c.opts.Lock.TryLock(ctx, tenant)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLocked
		}
		defer unlock()
	}
	r, err := Open(c.opts.RawRoot(tenant))
	if err != nil {
		return err
	}
	return r.repo.RepackObjects(&git.RepackConfig{OnlyDeletePacksOlderThan: c.opts.Now().Add(-time.Hour)})
}

// SweepJob runs Sweep every 10 minutes.
func (c *Committer) SweepJob(tenantsDir string) jobs.Job {
	return jobs.Job{
		Name:     "VaultGitSweep",
		Schedule: jobs.Every(10*time.Minute, time.Minute),
		Run: func(ctx context.Context) error {
			committed, failed, err := c.Sweep(ctx, tenantsDir)
			c.opts.Logger.InfoContext(ctx, "vault git sweep", "committed", committed, "failed", failed)
			if err != nil {
				return err
			}
			if failed > 0 {
				return fmt.Errorf("vault git sweep: %d failed", failed)
			}
			return nil
		},
	}
}

// GCJob runs GC daily at 06:00 UTC, after VaultTrashPurge (05:00).
func (c *Committer) GCJob(tenantsDir string) jobs.Job {
	return jobs.Job{
		Name:     "VaultGitGC",
		Schedule: jobs.DailyUTC(6),
		Run: func(ctx context.Context) error {
			repacked, failed, err := c.GC(ctx, tenantsDir)
			c.opts.Logger.InfoContext(ctx, "vault git gc", "repacked", repacked, "failed", failed)
			if err != nil {
				return err
			}
			if failed > 0 {
				return fmt.Errorf("vault git gc: %d failed", failed)
			}
			return nil
		},
	}
}
