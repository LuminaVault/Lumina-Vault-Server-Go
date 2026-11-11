package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/jobs"
	"lumina/internal/modules/account"
	accountdb "lumina/internal/modules/account/db"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LapseSummary is LapseArchiverSummary.
type LapseSummary struct {
	Lapsed      int
	MovedToFree int
	Archived    int
	HardDeleted int
	Failures    []LapseFailure
}

// LapseFailure is LapseArchiverFailure.
type LapseFailure struct {
	UserID uuid.UUID
	Phase  string // lapse, archive, hard_delete
	Err    string
}

// LapseArchiver is LapseArchiverJob (Billing/LapseArchiverJob.swift):
//
//   - trial past expiry → free;
//   - pro or ultimate past expiry → lapsed;
//   - lapsed past 90 days → vault moved to <ColdStorageRoot>/<UUID upper>, archived;
//   - archived past 365 days → cold vault deleted, TenantDataEraser, user deleted.
//
// Users with an override are never touched. Like Swift it loads every user once
// and walks the three phases over that list, so one run can carry a user through
// several phases. The last two phases are irreversible, so DryRun (the default,
// LV_LAPSE_DRY_RUN) logs each would-be action and writes nothing
// (wave-6-8-plan.md "Lapse job").
type LapseArchiver struct {
	Pool                *pgxpool.Pool
	Paths               vaultpaths.Paths
	ColdStorageRoot     string
	DryRun              bool
	ArchiveGraceDays    int // 90 when zero
	HardDeleteGraceDays int // 365 when zero
	Logger              *slog.Logger
}

type lapseUser struct {
	id        uuid.UUID
	tier      string
	override  string
	expiresAt *time.Time
}

func (u *lapseUser) hasOverride() bool { return u.override == "pro" || u.override == "ultimate" }

// ErrColdStorageTargetExists is LapseArchiverStorageError.targetExists.
var ErrColdStorageTargetExists = errors.New("cold storage target already exists")

func (l *LapseArchiver) log() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default().With("label", "lv.billing.lapse-archiver")
}

// Run is LapseArchiverJob.run(now:).
func (l *LapseArchiver) Run(ctx context.Context, now time.Time) (LapseSummary, error) {
	var s LapseSummary
	rows, err := l.Pool.Query(ctx, `SELECT id, tier, tier_override, tier_expires_at FROM users`)
	if err != nil {
		return s, err
	}
	users, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*lapseUser, error) {
		var u lapseUser
		return &u, r.Scan(&u.id, &u.tier, &u.override, &u.expiresAt)
	})
	if err != nil {
		return s, err
	}
	archiveGrace, deleteGrace := l.ArchiveGraceDays, l.HardDeleteGraceDays
	if archiveGrace == 0 {
		archiveGrace = 90
	}
	if deleteGrace == 0 {
		deleteGrace = 365
	}
	fail := func(u *lapseUser, phase string, err error) {
		s.Failures = append(s.Failures, LapseFailure{UserID: u.id, Phase: phase, Err: err.Error()})
	}

	for _, u := range users {
		if u.hasOverride() || u.expiresAt == nil || !u.expiresAt.Before(now) ||
			(u.tier != "trial" && u.tier != "pro" && u.tier != "ultimate") {
			continue
		}
		to, event := "lapsed", "billing.user_lapsed"
		if u.tier == "trial" {
			to, event = "free", "billing.user_moved_to_free"
		}
		if err := l.setTier(ctx, u, to, now); err != nil {
			fail(u, "lapse", err)
			continue
		}
		l.log().InfoContext(ctx, event, "userID", ids.Upper(u.id), "dryRun", l.DryRun)
		if to == "free" {
			s.MovedToFree++
		} else {
			s.Lapsed++
		}
	}

	for _, u := range users {
		if u.tier != "lapsed" || u.hasOverride() || u.expiresAt == nil ||
			!u.expiresAt.Before(now.Add(-time.Duration(archiveGrace)*24*time.Hour)) {
			continue
		}
		if err := l.moveToColdStorage(u.id); err != nil {
			fail(u, "archive", err)
			continue
		}
		if err := l.setTier(ctx, u, "archived", now); err != nil {
			fail(u, "archive", err)
			continue
		}
		l.log().WarnContext(ctx, "billing.user_archived", "userID", ids.Upper(u.id), "dryRun", l.DryRun)
		s.Archived++
	}

	for _, u := range users {
		if u.tier != "archived" || u.hasOverride() || u.expiresAt == nil ||
			!u.expiresAt.Before(now.Add(-time.Duration(deleteGrace)*24*time.Hour)) {
			continue
		}
		if err := l.hardDelete(ctx, u.id); err != nil {
			fail(u, "hard_delete", err)
			continue
		}
		l.log().WarnContext(ctx, "billing.user_hard_deleted", "userID", ids.Upper(u.id), "dryRun", l.DryRun)
		s.HardDeleted++
	}

	l.log().InfoContext(ctx, fmt.Sprintf("billing.lapse_archiver lapsed=%d movedToFree=%d archived=%d hardDeleted=%d failures=%d",
		s.Lapsed, s.MovedToFree, s.Archived, s.HardDeleted, len(s.Failures)), "dryRun", l.DryRun)
	return s, nil
}

// setTier is user.tier = to; user.save. In a dry run only the in-memory row
// changes, so later phases see what Swift's would.
func (l *LapseArchiver) setTier(ctx context.Context, u *lapseUser, to string, now time.Time) error {
	if !l.DryRun {
		if _, err := l.Pool.Exec(ctx, `UPDATE users SET tier = $2, updated_at = $3 WHERE id = $1`, u.id, to, now); err != nil {
			return err
		}
	}
	u.tier = to
	return nil
}

func (l *LapseArchiver) coldPath(id uuid.UUID) string {
	return filepath.Join(l.ColdStorageRoot, ids.Upper(id))
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// moveToColdStorage is moveVaultToColdStorage.
func (l *LapseArchiver) moveToColdStorage(id uuid.UUID) error {
	source, target := l.Paths.TenantRoot(id), l.coldPath(id)
	if l.DryRun {
		if pathExists(source) && pathExists(target) {
			return fmt.Errorf("%w: %s", ErrColdStorageTargetExists, target)
		}
		l.log().Info("billing.lapse_archiver.dry_run would move vault", "from", source, "to", target)
		return nil
	}
	if err := os.MkdirAll(l.ColdStorageRoot, 0o755); err != nil { //nolint:gosec // FileManager.createDirectory default, as vaultpaths
		return err
	}
	if !pathExists(source) {
		return nil
	}
	if pathExists(target) {
		return fmt.Errorf("%w: %s", ErrColdStorageTargetExists, target)
	}
	return os.Rename(source, target)
}

// hardDelete is hardDeleteIfPastGrace's effect: the cold vault, then
// TenantDataEraser and the user row in one transaction.
func (l *LapseArchiver) hardDelete(ctx context.Context, id uuid.UUID) error {
	if l.DryRun {
		l.log().Info("billing.lapse_archiver.dry_run would hard delete", "userID", ids.Upper(id), "coldVault", l.coldPath(id))
		return nil
	}
	if err := os.RemoveAll(l.coldPath(id)); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, l.Pool, func(tx pgx.Tx) error {
		q := accountdb.New(tx)
		if err := account.Erase(ctx, q, id); err != nil {
			return err
		}
		return q.DeleteUser(ctx, id)
	})
}

// Job is LapseArchiverService: daily. Swift runs at 03:00 UTC; Go's dry run runs
// at 02:00, before Swift acts, so each day's log can be checked against what
// Swift then did. Off unless LV_GO_JOBS names it.
func (l *LapseArchiver) Job() jobs.Job {
	return jobs.Job{
		Name:     "LapseArchiverService",
		Schedule: jobs.DailyUTC(2),
		Run: func(ctx context.Context) error {
			_, err := l.Run(ctx, time.Now())
			return err
		},
	}
}
