package workers

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/jobs"
	"lumina/internal/vaultfiles"
	"time"

	"github.com/google/uuid"
)

// VaultPropertiesBackfill indexes the frontmatter of Markdown notes written
// before VAULT_PROPERTIES_ENABLED was turned on (backlog E8.2, spec D2). It is
// Go-only and does nothing while the flag is off. Once every note is indexed
// a run finds no rows and costs one indexed query.
type VaultPropertiesBackfill struct {
	Store  *vaultfiles.Store
	Logger *slog.Logger
	// Pause between batches, so a large backfill doesn't hold the disk.
	Pause time.Duration
}

const propertiesBackfillBatch = 200

// Run indexes every unindexed note once, walking ids upward so a note that
// fails is skipped this run and retried by the next.
func (j *VaultPropertiesBackfill) Run(ctx context.Context) (indexed, failed int, err error) {
	if !vaultfiles.PropertiesEnabled() {
		return 0, 0, nil
	}
	after := uuid.Nil
	for {
		batch, err := j.Store.UnindexedNotes(ctx, after, propertiesBackfillBatch)
		if err != nil {
			return indexed, failed, err
		}
		for _, id := range batch {
			if ctx.Err() != nil {
				return indexed, failed, ctx.Err()
			}
			after = id
			if err := j.Store.Reindex(ctx, id); err != nil {
				failed++
				j.logger().WarnContext(ctx, "vault properties backfill: note not indexed", "id", id, "error", err)
				continue
			}
			indexed++
		}
		if len(batch) < propertiesBackfillBatch {
			return indexed, failed, nil
		}
		if j.Pause > 0 {
			select {
			case <-ctx.Done():
				return indexed, failed, ctx.Err()
			case <-time.After(j.Pause):
			}
		}
	}
}

func (j *VaultPropertiesBackfill) logger() *slog.Logger {
	if j.Logger == nil {
		return slog.Default()
	}
	return j.Logger
}

// Job runs at boot after up to ten minutes, then hourly.
func (j *VaultPropertiesBackfill) Job() jobs.Job {
	return jobs.Job{
		Name:     "VaultPropertiesBackfill",
		Schedule: jobs.Every(time.Hour, 10*time.Minute),
		Run: func(ctx context.Context) error {
			indexed, failed, err := j.Run(ctx)
			if indexed > 0 || failed > 0 {
				j.logger().InfoContext(ctx, "vault properties backfill", "indexed", indexed, "failed", failed)
			}
			if err != nil {
				return err
			}
			if failed > 0 {
				return fmt.Errorf("vault properties backfill: %d failed", failed)
			}
			return nil
		},
	}
}
