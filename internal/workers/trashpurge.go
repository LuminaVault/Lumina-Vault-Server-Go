package workers

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/jobs"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"time"
)

// VaultTrashPurge removes vault_trash entries older than Retention: the file
// under the tenant's raw root and then the row (backlog E3.2). It is Go-only;
// Swift never purged per-file trash.
type VaultTrashPurge struct {
	Store     *vaultfiles.Store
	Paths     vaultpaths.Paths
	Retention time.Duration
	Logger    *slog.Logger
}

// trashPurgeBatch bounds one query; a run keeps going until a batch comes back short.
const trashPurgeBatch = 500

// NewVaultTrashPurge clamps retention to at least one day, as the account purge does.
func NewVaultTrashPurge(store *vaultfiles.Store, paths vaultpaths.Paths, retentionDays int, logger *slog.Logger) *VaultTrashPurge {
	if logger == nil {
		logger = slog.Default()
	}
	return &VaultTrashPurge{
		Store: store, Paths: paths, Logger: logger,
		Retention: time.Duration(max(1, retentionDays)) * 24 * time.Hour,
	}
}

// Run purges everything deleted before now minus Retention. A file that fails
// to go keeps its row, so the next run tries again.
func (j *VaultTrashPurge) Run(ctx context.Context, now time.Time) (purged, failed int, err error) {
	cutoff := now.Add(-j.Retention)
	for {
		batch, err := j.Store.ExpiredTrash(ctx, cutoff, trashPurgeBatch)
		if err != nil {
			return purged, failed, err
		}
		progressed := false
		for _, e := range batch {
			if ctx.Err() != nil {
				return purged, failed, ctx.Err()
			}
			if err := vaultfiles.RemoveTrashedFile(j.Paths.Raw(e.TenantID), e.TrashName); err != nil {
				failed++
				j.Logger.WarnContext(ctx, "vault trash purge: file not removed", "trash_id", e.ID, "error", err)
				continue
			}
			if err := j.Store.DeleteTrashRow(ctx, e.ID); err != nil {
				return purged, failed, err
			}
			purged++
			progressed = true
		}
		if len(batch) < trashPurgeBatch || !progressed {
			return purged, failed, nil
		}
	}
}

// Job runs once a day at 05:00 UTC, an hour after DeletedDataPurgeService.
func (j *VaultTrashPurge) Job() jobs.Job {
	return jobs.Job{
		Name:     "VaultTrashPurge",
		Schedule: jobs.DailyUTC(5),
		Run: func(ctx context.Context) error {
			purged, failed, err := j.Run(ctx, time.Now())
			j.Logger.InfoContext(ctx, "vault trash purge", "purged", purged, "failed", failed)
			if err != nil {
				return err
			}
			if failed > 0 {
				return fmt.Errorf("vault trash purge: %d failed", failed)
			}
			return nil
		},
	}
}
