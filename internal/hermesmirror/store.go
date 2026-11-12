package hermesmirror

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/hermesdash"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// mirrorState is a hermes_mirror_state row (HermesMirrorState).
type mirrorState struct {
	ID               uuid.UUID
	Tenant           uuid.UUID
	LastSyncAt       *time.Time
	LastStatus       string
	LastError        *string
	SkillsCount      int64
	JobsCount        int64
	VaultFilesCount  int64
	VaultPath        *string
	VaultState       string
	VaultCursor      *string
	SessionsCursor   *string
	SessionsImported int64
	CompileJobID     *string
}

const stateColumns = `id, tenant_id, last_sync_at, last_status, last_error, skills_count, jobs_count,
	vault_files_count, vault_path, vault_state, vault_cursor, sessions_cursor, sessions_imported, compile_job_id`

// LoadState is loadState: nil when the tenant has never synced.
func (s *Service) loadState(ctx context.Context, tenant uuid.UUID) (*mirrorState, error) {
	var st mirrorState
	err := s.Pool.QueryRow(ctx, `SELECT `+stateColumns+` FROM hermes_mirror_state WHERE tenant_id = $1 LIMIT 1`, tenant).Scan(
		&st.ID, &st.Tenant, &st.LastSyncAt, &st.LastStatus, &st.LastError, &st.SkillsCount, &st.JobsCount,
		&st.VaultFilesCount, &st.VaultPath, &st.VaultState, &st.VaultCursor, &st.SessionsCursor, &st.SessionsImported, &st.CompileJobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Service) ensureState(ctx context.Context, tenant uuid.UUID) (*mirrorState, error) {
	existing, err := s.loadState(ctx, tenant)
	if err != nil || existing != nil {
		return existing, err
	}
	st := &mirrorState{ID: uuid.New(), Tenant: tenant, LastStatus: "never", VaultState: "absent"}
	now := s.now()
	_, err = s.Pool.Exec(ctx, `INSERT INTO hermes_mirror_state (`+stateColumns+`, created_at, updated_at)
		VALUES ($1, $2, NULL, 'never', NULL, 0, 0, 0, NULL, 'absent', NULL, NULL, 0, NULL, $3, $3)`, st.ID, tenant, now)
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Service) saveState(ctx context.Context, st *mirrorState) error {
	_, err := s.Pool.Exec(ctx, `UPDATE hermes_mirror_state SET last_sync_at = $2, last_status = $3, last_error = $4,
		skills_count = $5, jobs_count = $6, vault_files_count = $7, vault_path = $8, vault_state = $9,
		vault_cursor = $10, sessions_cursor = $11, sessions_imported = $12, compile_job_id = $13, updated_at = $14
		WHERE id = $1`,
		st.ID, st.LastSyncAt, st.LastStatus, st.LastError, st.SkillsCount, st.JobsCount, st.VaultFilesCount,
		st.VaultPath, st.VaultState, st.VaultCursor, st.SessionsCursor, st.SessionsImported, st.CompileJobID, s.now())
	return err
}

// record stamps the sync outcome; the error is capped at 1000 characters.
func (s *Service) record(ctx context.Context, st *mirrorState, status string, errText *string) error {
	now := s.now()
	st.LastSyncAt = &now
	st.LastStatus = status
	if errText != nil {
		capped := prefixChars(*errText, 1000)
		errText = &capped
	}
	st.LastError = errText
	return s.saveState(ctx, st)
}

// skillRow is a hermes_mirrored_skills row.
type skillRow struct {
	ID          uuid.UUID
	Name        string
	Description string
	Enabled     bool
	Source      string
	ContentHash *string
	UpdatedAt   *time.Time
}

func (s *Service) skillRows(ctx context.Context, tenant uuid.UUID, orderByName bool) ([]skillRow, error) {
	q := `SELECT id, name, description, enabled, source, content_hash, updated_at FROM hermes_mirrored_skills WHERE tenant_id = $1`
	if orderByName {
		q += ` ORDER BY name`
	}
	rows, err := s.Pool.Query(ctx, q, tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (skillRow, error) {
		var row skillRow
		err := r.Scan(&row.ID, &row.Name, &row.Description, &row.Enabled, &row.Source, &row.ContentHash, &row.UpdatedAt)
		return row, err
	})
}

func (s *Service) replaceSkills(ctx context.Context, tenant uuid.UUID, skills []hermesdash.Skill) error {
	existing, err := s.skillRows(ctx, tenant, false)
	if err != nil {
		return err
	}
	byName := map[string]skillRow{}
	for _, row := range existing {
		if _, ok := byName[row.Name]; !ok {
			byName[row.Name] = row
		}
	}
	for _, skill := range skills {
		row, ok := byName[skill.Name]
		if ok {
			delete(byName, skill.Name)
			changed := row.Description != skill.Description || row.Enabled != skill.Enabled ||
				row.Source != skill.Source || !equalPtr(row.ContentHash, skill.ContentHash)
			if changed {
				if _, err := s.Pool.Exec(ctx, `UPDATE hermes_mirrored_skills SET description = $2, enabled = $3, source = $4,
					content_hash = $5, updated_at = $6 WHERE id = $1`,
					row.ID, skill.Description, skill.Enabled, skill.Source, skill.ContentHash, s.now()); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO hermes_mirrored_skills (id, tenant_id, name, description, enabled, source, content_hash, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			uuid.New(), tenant, skill.Name, skill.Description, skill.Enabled, skill.Source, skill.ContentHash, s.now()); err != nil {
			return err
		}
	}
	for _, stale := range byName {
		if _, err := s.Pool.Exec(ctx, `DELETE FROM hermes_mirrored_skills WHERE id = $1`, stale.ID); err != nil {
			return err
		}
	}
	return nil
}

// jobRow is a hermes_mirrored_jobs row.
type jobRow struct {
	ID              uuid.UUID
	Exists          bool
	HermesJobID     string
	Name            *string
	Schedule        *string
	Prompt          *string
	Paused          bool
	LastRunAt       *time.Time
	NextRunAt       *time.Time
	Raw             []byte
	RunsHighWaterAt *time.Time
	RunsCollectedAt *time.Time
	UpdatedAt       *time.Time
}

const jobColumns = `id, hermes_job_id, name, schedule, prompt, paused, last_run_at, next_run_at, runs_high_water_at, runs_collected_at, updated_at`

func scanJob(r pgx.CollectableRow) (jobRow, error) {
	row := jobRow{Exists: true}
	err := r.Scan(&row.ID, &row.HermesJobID, &row.Name, &row.Schedule, &row.Prompt, &row.Paused, &row.LastRunAt,
		&row.NextRunAt, &row.RunsHighWaterAt, &row.RunsCollectedAt, &row.UpdatedAt)
	return row, err
}

func (s *Service) jobRows(ctx context.Context, tenant uuid.UUID, suffix string, args ...any) ([]jobRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+jobColumns+` FROM hermes_mirrored_jobs WHERE tenant_id = $1 `+suffix, append([]any{tenant}, args...)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanJob)
}

// mirroredJob is mirroredJob(tenantID:jobID:).
func (s *Service) mirroredJob(ctx context.Context, tenant uuid.UUID, jobID string) (*jobRow, error) {
	rows, err := s.jobRows(ctx, tenant, `AND hermes_job_id = $2 LIMIT 1`, jobID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// apply is HermesMirroredJob.apply.
func (row *jobRow) apply(job hermesdash.Job) {
	row.Name, row.Schedule, row.Prompt = job.Name, job.Schedule, job.Prompt
	row.Paused = job.Paused
	row.LastRunAt, row.NextRunAt = job.LastRunAt, job.NextRunAt
	row.Raw = job.Raw
}

func newJobRow(job hermesdash.Job) *jobRow {
	row := &jobRow{ID: uuid.New(), HermesJobID: job.ID}
	row.apply(job)
	return row
}

// saveJob inserts or updates the row's mirrored columns (never the collect columns).
func (s *Service) saveJob(ctx context.Context, tenant uuid.UUID, row *jobRow) error {
	now := s.now()
	row.UpdatedAt = &now
	raw := row.Raw
	if len(raw) == 0 || !json.Valid(raw) {
		raw = []byte("{}")
	}
	if row.Exists {
		_, err := s.Pool.Exec(ctx, `UPDATE hermes_mirrored_jobs SET name = $2, schedule = $3, prompt = $4, paused = $5,
			last_run_at = $6, next_run_at = $7, raw = $8::jsonb, updated_at = $9 WHERE id = $1`,
			row.ID, row.Name, row.Schedule, row.Prompt, row.Paused, row.LastRunAt, row.NextRunAt, string(raw), now)
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO hermes_mirrored_jobs (id, tenant_id, hermes_job_id, name, schedule, prompt, paused,
		last_run_at, next_run_at, raw, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11)`,
		row.ID, tenant, row.HermesJobID, row.Name, row.Schedule, row.Prompt, row.Paused, row.LastRunAt, row.NextRunAt, string(raw), now)
	if err == nil {
		row.Exists = true
	}
	return err
}

func (s *Service) replaceJobs(ctx context.Context, tenant uuid.UUID, jobs []hermesdash.Job) error {
	existing, err := s.jobRows(ctx, tenant, ``)
	if err != nil {
		return err
	}
	byID := map[string]jobRow{}
	for _, row := range existing {
		if _, ok := byID[row.HermesJobID]; !ok {
			byID[row.HermesJobID] = row
		}
	}
	for _, job := range jobs {
		row, ok := byID[job.ID]
		if ok {
			delete(byID, job.ID)
			row.apply(job)
			if err := s.saveJob(ctx, tenant, &row); err != nil {
				return err
			}
			continue
		}
		if err := s.saveJob(ctx, tenant, newJobRow(job)); err != nil {
			return err
		}
	}
	for _, stale := range byID {
		if _, err := s.Pool.Exec(ctx, `DELETE FROM hermes_mirrored_jobs WHERE id = $1`, stale.ID); err != nil {
			return err
		}
	}
	return nil
}

func equalPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
