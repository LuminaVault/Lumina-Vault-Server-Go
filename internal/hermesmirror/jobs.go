package hermesmirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/hermesdash"
	"lumina/internal/ids"
	"lumina/internal/modules/imports"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Job runs paging (HermesMirrorJobControl).
const (
	MaxJobRunsLimit     = 200
	DefaultJobRunsLimit = 50
	runsPerJobPerTick   = 50
	// MaxOutputBytes is HermesJobRun.maxOutputBytes (256 KiB).
	MaxOutputBytes = 256 * 1024
	// TruncationMarker is HermesJobRun.truncationMarker.
	TruncationMarker = "\n\n_[truncated by LuminaVault at 256 KiB]_\n"
)

var errJobNotFound = wire.NewError(http.StatusNotFound, "hermes_job_not_found")

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// MARK: - Create / mutate

// CreateJob is createJob(tenantID:request:).
func (s *Service) CreateJob(ctx context.Context, tenant uuid.UUID, req JobCreateRequest) (JobDTO, error) {
	if swiftstr.TrimWhitespacesAndNewlines(req.Name) == "" {
		return JobDTO{}, wire.NewError(http.StatusBadRequest, "hermes_job_name_required")
	}
	if swiftstr.TrimWhitespacesAndNewlines(req.Schedule) == "" {
		return JobDTO{}, wire.NewError(http.StatusBadRequest, "hermes_job_schedule_required")
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return JobDTO{}, err
	}
	created, err := transport.CreateJob(ctx, jobSpec(req))
	if err != nil {
		return JobDTO{}, err
	}
	row, err := s.mirroredJob(ctx, tenant, created.ID)
	if err != nil {
		return JobDTO{}, err
	}
	if row == nil {
		row = newJobRow(created)
	}
	row.apply(created)
	if err := s.saveJob(ctx, tenant, row); err != nil {
		return JobDTO{}, err
	}
	return rowJobDTO(*row), nil
}

// jobSpec is HermesMirrorJobSpec(_ request:).
func jobSpec(r JobCreateRequest) hermesdash.JobSpec {
	spec := hermesdash.JobSpec{
		Name: r.Name, Schedule: r.Schedule, Prompt: orDefault(r.Prompt, ""), Deliver: orDefault(r.Deliver, "origin"),
		Model: r.Model, Provider: r.Provider, BaseURL: r.BaseURL, Script: r.Script, Workdir: r.Workdir,
	}
	if r.Skills != nil {
		spec.Skills = *r.Skills
	}
	if r.ContextFrom != nil {
		spec.ContextFrom, spec.HasContextFrom = *r.ContextFrom, true
	}
	if r.EnabledToolsets != nil {
		spec.EnabledToolsets, spec.HasToolsets = *r.EnabledToolsets, true
	}
	if r.NoAgent != nil {
		spec.NoAgent = *r.NoAgent
	}
	return spec
}

func jobUpdate(r JobUpdateRequest) hermesdash.JobUpdate {
	return hermesdash.JobUpdate{
		Name: r.Name, Schedule: r.Schedule, Prompt: r.Prompt, Deliver: r.Deliver, Skills: r.Skills,
		Model: r.Model, Provider: r.Provider, BaseURL: r.BaseURL, Script: r.Script,
		ContextFrom: r.ContextFrom, EnabledToolsets: r.EnabledToolsets, Workdir: r.Workdir,
		NoAgent: r.NoAgent, Enabled: r.Enabled,
	}
}

// UpdateJob refuses an empty update rather than sending a no-op write.
func (s *Service) UpdateJob(ctx context.Context, tenant uuid.UUID, jobID string, req JobUpdateRequest) (JobDTO, error) {
	if req.IsEmpty() {
		return JobDTO{}, wire.NewError(http.StatusBadRequest, "hermes_job_update_empty")
	}
	return s.mutateJob(ctx, tenant, jobID, func(t hermesdash.Transport, id string) (hermesdash.Job, error) {
		return t.UpdateJob(ctx, id, jobUpdate(req))
	})
}

// PauseJob pauses on Hermes, then mirrors the answer.
func (s *Service) PauseJob(ctx context.Context, tenant uuid.UUID, jobID string) (JobDTO, error) {
	return s.mutateJob(ctx, tenant, jobID, func(t hermesdash.Transport, id string) (hermesdash.Job, error) { return t.PauseJob(ctx, id) })
}

// ResumeJob resumes on Hermes, then mirrors the answer.
func (s *Service) ResumeJob(ctx context.Context, tenant uuid.UUID, jobID string) (JobDTO, error) {
	return s.mutateJob(ctx, tenant, jobID, func(t hermesdash.Transport, id string) (hermesdash.Job, error) { return t.ResumeJob(ctx, id) })
}

// TriggerJob fires the job on Hermes' next tick.
func (s *Service) TriggerJob(ctx context.Context, tenant uuid.UUID, jobID string) (JobDTO, error) {
	return s.mutateJob(ctx, tenant, jobID, func(t hermesdash.Transport, id string) (hermesdash.Job, error) { return t.TriggerJob(ctx, id) })
}

// DeleteJob deletes on Hermes and drops the mirrored row; collected runs stay.
func (s *Service) DeleteJob(ctx context.Context, tenant uuid.UUID, jobID string) error {
	id, err := hermesdash.ValidateJobID(jobID)
	if err != nil {
		return err
	}
	row, err := s.mirroredJob(ctx, tenant, id)
	if err != nil {
		return err
	}
	if row == nil {
		return errJobNotFound
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return err
	}
	if err := transport.DeleteJob(ctx, id); err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `DELETE FROM hermes_mirrored_jobs WHERE id = $1`, row.ID)
	return err
}

func (s *Service) mutateJob(ctx context.Context, tenant uuid.UUID, jobID string, mutate func(hermesdash.Transport, string) (hermesdash.Job, error)) (JobDTO, error) {
	id, err := hermesdash.ValidateJobID(jobID)
	if err != nil {
		return JobDTO{}, err
	}
	row, err := s.mirroredJob(ctx, tenant, id)
	if err != nil {
		return JobDTO{}, err
	}
	if row == nil {
		return JobDTO{}, errJobNotFound
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return JobDTO{}, err
	}
	updated, err := mutate(transport, id)
	if err != nil {
		return JobDTO{}, err
	}
	row.apply(updated)
	if err := s.saveJob(ctx, tenant, row); err != nil {
		return JobDTO{}, err
	}
	return rowJobDTO(*row), nil
}

// MARK: - Runs

// JobRuns is jobRuns(tenantID:jobID:limit:): stored rows, newest first.
func (s *Service) JobRuns(ctx context.Context, tenant uuid.UUID, jobID string, limit int) (JobRunsResponse, error) {
	id, err := hermesdash.ValidateJobID(jobID)
	if err != nil {
		return JobRunsResponse{}, err
	}
	bounded := max(1, min(limit, MaxJobRunsLimit))
	rows, err := s.Pool.Query(ctx, `SELECT id, hermes_job_id, hermes_run_key, status, started_at, finished_at, output, error,
		tokens, vault_file_id, skill_run_log_id, collected_at
		FROM hermes_job_runs WHERE tenant_id = $1 AND hermes_job_id = $2 ORDER BY started_at DESC LIMIT $3`, tenant, id, bounded)
	if err != nil {
		return JobRunsResponse{}, err
	}
	type runRow struct {
		dto    JobRunDTO
		fileID *uuid.UUID
	}
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (runRow, error) {
		var (
			out                  runRow
			rid                  uuid.UUID
			started, collected   time.Time
			finished             *time.Time
			tokens               []byte
			logID                *uuid.UUID
			jobIDCol, key, state string
		)
		if err := r.Scan(&rid, &jobIDCol, &key, &state, &started, &finished, &out.dto.Output, &out.dto.Error,
			&tokens, &out.fileID, &logID, &collected); err != nil {
			return out, err
		}
		switch state {
		case hermesdash.RunRunning, hermesdash.RunOK, hermesdash.RunError:
		default:
			state = hermesdash.RunOK
		}
		out.dto.ID, out.dto.HermesJobID, out.dto.RunKey, out.dto.Status = wire.UUID{UUID: rid}, jobIDCol, key, state
		out.dto.StartedAt = wire.Time{Time: started}
		out.dto.FinishedAt = wtime(finished)
		out.dto.CollectedAt = &wire.Time{Time: collected}
		if len(tokens) > 0 && string(tokens) != "null" {
			var t TokensDTO
			if json.Unmarshal(tokens, &t) == nil {
				out.dto.Tokens = &t
			}
		}
		if logID != nil {
			out.dto.SkillRunLogID = &wire.UUID{UUID: *logID}
		}
		return out, nil
	})
	if err != nil {
		return JobRunsResponse{}, err
	}
	job, err := s.mirroredJob(ctx, tenant, id)
	if err != nil {
		return JobRunsResponse{}, err
	}
	var fileIDs []uuid.UUID
	for _, r := range runs {
		if r.fileID != nil {
			fileIDs = append(fileIDs, *r.fileID)
		}
	}
	paths := map[uuid.UUID]string{}
	if len(fileIDs) > 0 {
		prow, err := s.Pool.Query(ctx, `SELECT id, path FROM vault_files WHERE tenant_id = $1 AND id = ANY($2)`, tenant, fileIDs)
		if err != nil {
			return JobRunsResponse{}, err
		}
		for prow.Next() {
			var fid uuid.UUID
			var p string
			if err := prow.Scan(&fid, &p); err != nil {
				prow.Close()
				return JobRunsResponse{}, err
			}
			if _, ok := paths[fid]; !ok {
				paths[fid] = p
			}
		}
		prow.Close()
		if err := prow.Err(); err != nil {
			return JobRunsResponse{}, err
		}
	}
	out := JobRunsResponse{HermesJobID: id, Runs: make([]JobRunDTO, 0, len(runs))}
	for _, r := range runs {
		if r.fileID != nil {
			if p, ok := paths[*r.fileID]; ok {
				r.dto.VaultFilePath = &p
			}
		}
		out.Runs = append(out.Runs, r.dto)
	}
	if job != nil {
		out.CollectedAt = wtime(job.RunsCollectedAt)
	}
	return out, nil
}

// MARK: - Collect

// CollectJobRuns is collectJobRuns(tenantID:jobID:).
func (s *Service) CollectJobRuns(ctx context.Context, tenant uuid.UUID, jobID string) (CollectResultDTO, error) {
	id, err := hermesdash.ValidateJobID(jobID)
	if err != nil {
		return CollectResultDTO{}, err
	}
	if err := s.beginExclusive(tenant); err != nil {
		return CollectResultDTO{}, err
	}
	defer s.endExclusive(tenant)
	job, err := s.mirroredJob(ctx, tenant, id)
	if err != nil {
		return CollectResultDTO{}, err
	}
	if job == nil {
		return CollectResultDTO{}, errJobNotFound
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return CollectResultDTO{}, err
	}
	return s.collect(ctx, tenant, *job, transport)
}

// jobsPerTick is collectLimits.jobsPerTick.
const jobsPerTick = 50

// CollectSummary is HermesCollectSummary.
type CollectSummary struct {
	Jobs         int
	Inserted     int
	FilesWritten int
	Failed       int
	// Unavailable is the stable code of the tenant-wide error that stopped
	// the pass; not counted in Failed.
	Unavailable *string
}

// CollectAllJobRuns is collectAllJobRuns(tenantID:), the refresh worker's
// pass. The worker itself stays on Swift (O4); the pass is ported so its
// behaviour is pinned before the job moves.
func (s *Service) CollectAllJobRuns(ctx context.Context, tenant uuid.UUID) (CollectSummary, error) {
	if err := s.beginExclusive(tenant); err != nil {
		return CollectSummary{}, err
	}
	defer s.endExclusive(tenant)
	jobs, err := s.jobRows(ctx, tenant, `ORDER BY hermes_job_id LIMIT $2`, jobsPerTick)
	if err != nil || len(jobs) == 0 {
		return CollectSummary{}, err
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return CollectSummary{}, err
	}
	var summary CollectSummary
	for _, job := range jobs {
		result, err := s.collect(ctx, tenant, job, transport)
		var transportErr *hermesdash.Error
		switch {
		case err == nil:
			summary.Jobs++
			summary.Inserted += result.Inserted
			summary.FilesWritten += result.FilesWritten
		case errors.As(err, &transportErr) && transportErr.IsTenantWide():
			code := transportErr.Code()
			summary.Unavailable = &code
			s.log().Debug("hermes mirror collect unavailable", "tenant", tenant.String(), "error", err.Error())
			return summary, nil
		default:
			summary.Failed++
			s.log().Warn("hermes mirror collect failed", "tenant", tenant.String(), "job", job.HermesJobID, "error", err.Error())
		}
	}
	return summary, nil
}

func later(lhs *time.Time, rhs time.Time) *time.Time {
	if lhs == nil || rhs.After(*lhs) {
		return &rhs
	}
	return lhs
}

func (s *Service) collect(ctx context.Context, tenant uuid.UUID, job jobRow, transport hermesdash.Transport) (CollectResultDTO, error) {
	jobID := job.HermesJobID
	listed, err := transport.JobRuns(ctx, jobID, runsPerJobPerTick)
	if err != nil {
		return CollectResultDTO{}, err
	}
	now := s.now()
	result := CollectResultDTO{HermesJobID: jobID, Fetched: len(listed), Truncated: len(listed) >= runsPerJobPerTick}

	var newest *time.Time
	for _, r := range listed {
		newest = later(newest, r.StartedAt)
	}
	if job.RunsHighWaterAt != nil && newest != nil && !newest.After(*job.RunsHighWaterAt) {
		result.Skipped = len(listed)
		if err := s.markCollected(ctx, tenant, jobID, job.RunsHighWaterAt, now); err != nil {
			return CollectResultDTO{}, err
		}
		result.HighWaterMark = wtime(job.RunsHighWaterAt)
		return result, nil
	}

	highWater := job.RunsHighWaterAt
	sorted := append([]hermesdash.JobRun(nil), listed...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartedAt.Before(sorted[j].StartedAt) })
	for _, run := range sorted {
		if run.Status == hermesdash.RunRunning {
			result.Skipped++
			continue
		}
		var n int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM hermes_job_runs WHERE tenant_id = $1 AND hermes_run_key = $2`, tenant, run.Key).Scan(&n); err != nil {
			return CollectResultDTO{}, err
		}
		if n > 0 {
			result.Skipped++
			highWater = later(highWater, run.StartedAt)
			continue
		}
		wroteFile, err := s.store(ctx, tenant, job, run, transport, now)
		if err != nil {
			return CollectResultDTO{}, err
		}
		result.Inserted++
		if wroteFile {
			result.FilesWritten++
		}
		highWater = later(highWater, run.StartedAt)
		if err := s.pause(ctx, s.Limits.ReadPause); err != nil {
			return CollectResultDTO{}, err
		}
	}
	if err := s.markCollected(ctx, tenant, jobID, highWater, now); err != nil {
		return CollectResultDTO{}, err
	}
	if result.Inserted > 0 {
		s.log().InfoContext(ctx, "hermes mirror collect", "tenant", ids.Upper(tenant), "job", jobID,
			"inserted", result.Inserted, "files", result.FilesWritten)
	}
	result.HighWaterMark = wtime(highWater)
	return result, nil
}

func (s *Service) store(ctx context.Context, tenant uuid.UUID, job jobRow, run hermesdash.JobRun, transport hermesdash.Transport, now time.Time) (bool, error) {
	var markdown *string
	if run.Status == hermesdash.RunOK {
		if out, err := transport.JobRunOutput(ctx, job.HermesJobID, run.Key); err == nil {
			markdown = out
		}
	}
	var output *string
	if markdown != nil && swiftstr.TrimWhitespacesAndNewlines(*markdown) != "" {
		t := Truncate(*markdown)
		output = &t
	}
	var vaultFileID, spaceID *uuid.UUID
	if output != nil {
		prov := JobProvenance
		res, err := s.Ingest.IngestBatchWithProvenance(ctx, tenant, JobsSpaceName,
			[]imports.FileInput{{Path: JobOutputVaultPath(job.Name, job.HermesJobID, run.StartedAt), Content: *output}}, &prov)
		if err != nil {
			return false, err
		}
		if len(res.VaultFileIDs) > 0 {
			v := res.VaultFileIDs[0]
			vaultFileID = &v
		}
		sp := res.SpaceID
		spaceID = &sp
	}
	logID, err := s.recordSkillRun(ctx, tenant, job, run, output, spaceID)
	if err != nil {
		return false, err
	}
	var tokens any
	if run.TokensIn != nil || run.TokensOut != nil {
		data, _ := json.Marshal(TokensDTO{Input: run.TokensIn, Output: run.TokensOut})
		tokens = string(data)
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO hermes_job_runs (id, tenant_id, hermes_job_id, hermes_run_key, status, started_at,
		finished_at, output, error, tokens, vault_file_id, skill_run_log_id, collected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13)`,
		uuid.New(), tenant, job.HermesJobID, run.Key, run.Status, run.StartedAt, run.FinishedAt, output, run.Error,
		tokens, vaultFileID, logID, now)
	if err != nil {
		return false, err
	}
	return vaultFileID != nil, nil
}

// JobOutputVaultPath is jobOutputVaultPath(job:run:): raw/jobs/<slug>/<yyyy-MM-dd-HHmm>.md.
func JobOutputVaultPath(name *string, jobID string, startedAt time.Time) string {
	source := jobID
	if name != nil {
		if trimmed := swiftstr.TrimWhitespacesAndNewlines(*name); trimmed != "" {
			source = trimmed
		}
	}
	slug := imports.Slugify(source)
	if slug == "" {
		slug = jobID
	}
	return "raw/jobs/" + slug + "/" + Stamp(startedAt) + ".md"
}

// Stamp is yyyy-MM-dd-HHmm in UTC.
func Stamp(t time.Time) string {
	u := t.UTC()
	return fmt.Sprintf("%04d-%02d-%02d-%02d%02d", u.Year(), int(u.Month()), u.Day(), u.Hour(), u.Minute())
}

// Truncate is HermesJobRun.truncate: cut to MaxOutputBytes on a grapheme
// boundary and mark the cut.
func Truncate(markdown string) string {
	if len(markdown) <= MaxOutputBytes {
		return markdown
	}
	var b strings.Builder
	for _, c := range swiftstr.Characters(markdown) {
		if b.Len()+len(c) > MaxOutputBytes {
			break
		}
		b.WriteString(c)
	}
	return b.String() + TruncationMarker
}

func (s *Service) markCollected(ctx context.Context, tenant uuid.UUID, jobID string, highWater *time.Time, now time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE hermes_mirrored_jobs SET runs_high_water_at = $1, runs_collected_at = $2
		WHERE tenant_id = $3 AND hermes_job_id = $4`, highWater, now, tenant, jobID)
	return err
}

func (s *Service) recordSkillRun(ctx context.Context, tenant uuid.UUID, job jobRow, run hermesdash.JobRun, output *string, spaceID *uuid.UUID) (*uuid.UUID, error) {
	logID := uuid.New()
	name := job.HermesJobID
	if job.Name != nil {
		if trimmed := swiftstr.TrimWhitespacesAndNewlines(*job.Name); trimmed != "" {
			name = trimmed
		}
	}
	status := "success"
	if run.Status == hermesdash.RunError {
		status = "error"
	}
	ended := run.StartedAt
	if run.FinishedAt != nil {
		ended = *run.FinishedAt
	}
	tokIn, tokOut := 0, 0
	if run.TokensIn != nil {
		tokIn = *run.TokensIn
	}
	if run.TokensOut != nil {
		tokOut = *run.TokensOut
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO skill_run_log
		(id, tenant_id, source, name, started_at, ended_at, status, error, model_used, mtok_in, mtok_out, markdown, space_id)
		VALUES ($1, $2, 'hermes', $3, $4, $5, $6, $7, NULL, $8, $9, $10, $11)`,
		logID, tenant, name, run.StartedAt, ended, status, run.Error, tokIn, tokOut, output, spaceID)
	if err != nil {
		return nil, err
	}
	return &logID, nil
}
