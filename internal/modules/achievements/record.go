package achievements

import (
	"context"
	"errors"
	"lumina/internal/modules/achievements/db"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Recorder is the write half of AchievementsService (record(tenantID:event:)).
// Producers don't call it directly: a handler enqueues on
// internal/achievementsink, whose worker calls this and pushes the unlocks.
type Recorder struct {
	q       *db.Queries
	catalog Catalog
	now     func() time.Time
}

func NewRecorder(pool *pgxpool.Pool) *Recorder {
	return &Recorder{q: db.New(pool), catalog: Current, now: time.Now}
}

// Record increments every sub whose event matches and returns the subs that
// crossed their threshold in this call. unlocked_at is sticky once set. Like
// Swift, increments are read-then-write, not transactional.
func (r *Recorder) Record(ctx context.Context, tenantID uuid.UUID, e Event) ([]Sub, error) {
	var unlocked []Sub
	for _, sub := range r.catalog.SubsMatching(e) {
		row, err := r.q.GetProgress(ctx, db.GetProgressParams{TenantID: tenantID, AchievementKey: sub.Key})
		isNew := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !isNew {
			return nil, err
		}
		wasUnlocked := row.UnlockedAt != nil
		row.ProgressCount++
		now := r.now()
		if !wasUnlocked && row.ProgressCount >= sub.Target {
			row.UnlockedAt = &now
			unlocked = append(unlocked, sub)
		}
		if isNew {
			err = r.q.InsertProgress(ctx, db.InsertProgressParams{
				ID: uuid.New(), TenantID: tenantID, AchievementKey: sub.Key,
				ProgressCount: row.ProgressCount, UnlockedAt: row.UnlockedAt, CreatedAt: now,
			})
		} else {
			err = r.q.UpdateProgress(ctx, db.UpdateProgressParams{
				ID: row.ID, ProgressCount: row.ProgressCount, UnlockedAt: row.UnlockedAt, UpdatedAt: now,
			})
		}
		if err != nil {
			return nil, err
		}
	}
	return unlocked, nil
}
