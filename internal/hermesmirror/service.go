// Package hermesmirror is HermesMirrorService: every skill, cron job, KB vault
// file and past session on the tenant's Hermes, mirrored into LuminaVault.
// The routes and HermesMirrorRefreshWorker (worker.go) both live here; the
// worker runs only when LV_GO_JOBS names it.
package hermesmirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"lumina/internal/hermescaps"
	"lumina/internal/hermesdash"
	"lumina/internal/ids"
	"lumina/internal/modules/imports"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Names and markers (HermesMirrorService statics).
const (
	CompileJobName     = "luminavault-nightly-compile"
	CompileJobSchedule = "0 3 * * *"
	CompileSkillName   = "kb-compile"
	VaultSpaceName     = "Hermes Vault"
	SessionsSpaceName  = "Hermes Sessions"
	JobsSpaceName      = "Hermes Jobs"
	VaultProvenance    = "hermes-mirror"
	SessionProvenance  = "hermes-session"
	JobProvenance      = "hermes-job"
)

var skippedDirectories = map[string]bool{".obsidian": true, ".trash": true, ".kb": true, ".git": true, "node_modules": true}

// Limits are HermesMirrorService.Limits.
type Limits struct {
	MaxVaultFiles    int
	MaxFileBytes     int
	IngestBatchSize  int
	VaultFilesPerRun int
	ReadPause        time.Duration
	MaxSessions      int
	MaxSessionBytes  int
	SessionsPerRun   int
	SessionPageSize  int
}

// DefaultLimits are Swift's defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxVaultFiles: 5000, MaxFileBytes: hermesdash.FileBodyCap, IngestBatchSize: 50, VaultFilesPerRun: 200,
		ReadPause: 50 * time.Millisecond, MaxSessions: 2000, MaxSessionBytes: 1024 * 1024,
		SessionsPerRun: 100, SessionPageSize: 50,
	}
}

// Ingester is VaultIngestService.ingestBatch(..., provenance:).
type Ingester interface {
	IngestBatchWithProvenance(ctx context.Context, tenant uuid.UUID, spaceName string, files []imports.FileInput, provenance *string) (imports.IngestResult, error)
}

// Compiler is HermesMirrorCompileRunning.
type Compiler interface {
	Compile(ctx context.Context, tenant uuid.UUID, vaultFileIDs []uuid.UUID) error
}

// Capabilities is the slice of HermesRemoteCapabilitiesService status reads.
type Capabilities interface {
	Capabilities(ctx context.Context, tenant uuid.UUID, force bool) hermescaps.Response
}

// Service is HermesMirrorService.
type Service struct {
	Pool         *pgxpool.Pool
	Transports   hermesdash.Provider
	Capabilities Capabilities // optional
	Ingest       Ingester
	Compile      Compiler // optional
	// BundledSkills is the HermesSkills directory (kb-*/SKILL.md).
	BundledSkills fs.FS
	Limits        Limits
	Now           func() time.Time
	Sleep         func(context.Context, time.Duration) error
	Log           *slog.Logger

	mu       sync.Mutex
	inFlight map[uuid.UUID]bool
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var errBusy = wire.NewError(http.StatusConflict, "hermes_mirror_busy")

// beginExclusive is the per-tenant latch for imports and collects.
func (s *Service) beginExclusive(tenant uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight == nil {
		s.inFlight = map[uuid.UUID]bool{}
	}
	if s.inFlight[tenant] {
		return errBusy
	}
	s.inFlight[tenant] = true
	return nil
}

func (s *Service) endExclusive(tenant uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, tenant)
}

// Describe is HermesMirrorService.describe.
func Describe(err error) string {
	var te *hermesdash.Error
	if errors.As(err, &te) {
		return te.Error()
	}
	var we *wire.Error
	if errors.As(err, &we) {
		return fmt.Sprintf("HTTPError(status: %d, message: %s)", we.Status, we.Message)
	}
	return err.Error()
}

// MARK: - Status

var (
	syncStatuses = map[string]bool{"never": true, "ok": true, "partial": true, "failed": true}
	vaultStates  = map[string]bool{"absent": true, "detected": true, "created": true, "imported": true}
)

// Status is status(tenantID:).
func (s *Service) Status(ctx context.Context, tenant uuid.UUID) (StatusDTO, error) {
	st, err := s.loadState(ctx, tenant)
	if err != nil {
		return StatusDTO{}, err
	}
	kind := s.Transports.Kind(ctx, tenant)
	var dashboard *hermescaps.Dashboard
	if s.Capabilities != nil {
		dashboard = s.Capabilities.Capabilities(ctx, tenant, false).Capabilities.Dashboard
	}
	return statusDTO(st, kind, dashboard), nil
}

func statusDTO(st *mirrorState, kind hermesdash.TransportKind, dashboard *hermescaps.Dashboard) StatusDTO {
	dto := StatusDTO{Transport: string(kind), LastStatus: "never", VaultState: "absent", Dashboard: dashboard}
	if st == nil {
		return dto
	}
	dto.LastSyncAt = wtime(st.LastSyncAt)
	if syncStatuses[st.LastStatus] {
		dto.LastStatus = st.LastStatus
	}
	dto.LastError = st.LastError
	dto.SkillsCount, dto.JobsCount, dto.VaultFilesCount = st.SkillsCount, st.JobsCount, st.VaultFilesCount
	dto.VaultPath = st.VaultPath
	if vaultStates[st.VaultState] {
		dto.VaultState = st.VaultState
	}
	dto.SessionsImported = st.SessionsImported
	dto.CompileJobID = st.CompileJobID
	return dto
}

// Sync scopes.
const (
	ScopeSkills    = "skills"
	ScopeJobs      = "jobs"
	ScopeVault     = "vault"
	ScopeArtifacts = "artifacts"
)

// AllScopes is HermesMirrorSyncScope.allCases.
var AllScopes = []string{ScopeSkills, ScopeJobs, ScopeVault, ScopeArtifacts}

// Sync is sync(tenantID:scopes:): each scope independently; upstream failures
// land in lastStatus/lastError, never in the response status.
func (s *Service) Sync(ctx context.Context, tenant uuid.UUID, scopes map[string]bool) (StatusDTO, error) {
	if len(scopes) == 0 {
		scopes = map[string]bool{}
		for _, sc := range AllScopes {
			scopes[sc] = true
		}
	}
	st, err := s.ensureState(ctx, tenant)
	if err != nil {
		return StatusDTO{}, err
	}
	var errs []string
	succeeded := 0
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		msg := Describe(err)
		if err := s.record(ctx, st, "failed", &msg); err != nil {
			return StatusDTO{}, err
		}
		return s.Status(ctx, tenant)
	}
	if scopes[ScopeSkills] {
		if err := s.syncSkills(ctx, tenant, transport, st); err != nil {
			errs = append(errs, "skills: "+Describe(err))
		} else {
			succeeded++
		}
	}
	if scopes[ScopeJobs] {
		if err := s.syncJobs(ctx, tenant, transport, st); err != nil {
			errs = append(errs, "jobs: "+Describe(err))
		} else {
			succeeded++
		}
	}
	if scopes[ScopeVault] {
		detected, err := s.detectVault(ctx, transport, st.VaultPath)
		if err != nil {
			errs = append(errs, "vault: "+Describe(err))
		} else {
			if detected != nil {
				st.VaultPath = detected
				if st.VaultState == "absent" {
					st.VaultState = "detected"
				}
			} else if st.VaultState != "created" {
				st.VaultState = "absent"
				st.VaultPath = nil
			}
			succeeded++
		}
	}
	if scopes[ScopeArtifacts] {
		if _, err := s.CollectArtifacts(ctx, tenant); err != nil {
			errs = append(errs, "artifacts: "+Describe(err))
		} else {
			succeeded++
		}
	}
	status := "ok"
	var errText *string
	if len(errs) > 0 {
		status = "failed"
		if succeeded > 0 {
			status = "partial"
		}
		joined := strings.Join(errs, "; ")
		errText = &joined
	}
	if err := s.record(ctx, st, status, errText); err != nil {
		return StatusDTO{}, err
	}
	return s.Status(ctx, tenant)
}

func (s *Service) syncSkills(ctx context.Context, tenant uuid.UUID, transport hermesdash.Transport, st *mirrorState) error {
	skills, err := transport.ListSkills(ctx)
	if err != nil {
		return err
	}
	if err := s.replaceSkills(ctx, tenant, skills); err != nil {
		return err
	}
	st.SkillsCount = int64(len(skills))
	return nil
}

func (s *Service) syncJobs(ctx context.Context, tenant uuid.UUID, transport hermesdash.Transport, st *mirrorState) error {
	jobs, err := transport.ListJobs(ctx)
	if err != nil {
		return err
	}
	if err := s.replaceJobs(ctx, tenant, jobs); err != nil {
		return err
	}
	st.JobsCount = int64(len(jobs))
	if st.CompileJobID != nil {
		found := false
		for _, j := range jobs {
			if j.ID == *st.CompileJobID {
				found = true
				break
			}
		}
		if !found {
			st.CompileJobID = nil
			for _, j := range jobs {
				if j.Name != nil && *j.Name == CompileJobName {
					id := j.ID
					st.CompileJobID = &id
					break
				}
			}
		}
	}
	return nil
}

// MARK: - Skills

func skillDTO(row skillRow) SkillDTO {
	source := row.Source
	switch source {
	case "builtin", "hub", "custom":
	default:
		source = "custom"
	}
	return SkillDTO{Name: row.Name, Description: row.Description, Enabled: row.Enabled, Source: source, UpdatedAt: wtime(row.UpdatedAt)}
}

// MirroredSkills is mirroredSkills(tenantID:), sorted by name.
func (s *Service) MirroredSkills(ctx context.Context, tenant uuid.UUID) ([]SkillDTO, error) {
	rows, err := s.skillRows(ctx, tenant, true)
	if err != nil {
		return nil, err
	}
	out := make([]SkillDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, skillDTO(r))
	}
	return out, nil
}

// ToggleSkill toggles on Hermes, then mirrors the new flag.
func (s *Service) ToggleSkill(ctx context.Context, tenant uuid.UUID, name string, enabled bool) (SkillDTO, error) {
	var row skillRow
	err := s.Pool.QueryRow(ctx, `SELECT id, name, description, enabled, source, content_hash, updated_at
		FROM hermes_mirrored_skills WHERE tenant_id = $1 AND name = $2 LIMIT 1`, tenant, name).Scan(
		&row.ID, &row.Name, &row.Description, &row.Enabled, &row.Source, &row.ContentHash, &row.UpdatedAt)
	if isNoRows(err) {
		return SkillDTO{}, wire.NewError(http.StatusNotFound, "hermes_skill_not_found")
	}
	if err != nil {
		return SkillDTO{}, err
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return SkillDTO{}, err
	}
	if err := transport.ToggleSkill(ctx, name, enabled); err != nil {
		return SkillDTO{}, err
	}
	now := s.now()
	if _, err := s.Pool.Exec(ctx, `UPDATE hermes_mirrored_skills SET enabled = $2, updated_at = $3 WHERE id = $1`, row.ID, enabled, now); err != nil {
		return SkillDTO{}, err
	}
	row.Enabled = enabled
	row.UpdatedAt = &now
	return skillDTO(row), nil
}

// MARK: - Jobs

func liveJobDTO(job hermesdash.Job) JobDTO {
	return JobDTO{
		HermesJobID: job.ID, Name: job.Name, Schedule: job.Schedule, Prompt: job.Prompt, Paused: job.Paused,
		LastRunAt: wtime(job.LastRunAt), NextRunAt: wtime(job.NextRunAt),
	}
}

func rowJobDTO(row jobRow) JobDTO {
	return JobDTO{
		HermesJobID: row.HermesJobID, Name: row.Name, Schedule: row.Schedule, Prompt: row.Prompt, Paused: row.Paused,
		LastRunAt: wtime(row.LastRunAt), NextRunAt: wtime(row.NextRunAt), UpdatedAt: wtime(row.UpdatedAt),
	}
}

// Jobs is jobs(tenantID:): live when reachable, else the mirrored snapshot.
func (s *Service) Jobs(ctx context.Context, tenant uuid.UUID) (JobsResponse, error) {
	live, err := func() ([]hermesdash.Job, error) {
		transport, err := s.Transports.Transport(ctx, tenant)
		if err != nil {
			return nil, err
		}
		jobs, err := transport.ListJobs(ctx)
		if err != nil {
			return nil, err
		}
		return jobs, s.replaceJobs(ctx, tenant, jobs)
	}()
	if err == nil {
		out := make([]JobDTO, 0, len(live))
		for _, j := range live {
			out = append(out, liveJobDTO(j))
		}
		return JobsResponse{Source: "live", Jobs: out}, nil
	}
	s.log().DebugContext(ctx, "hermes mirror live jobs failed; serving snapshot", "tenant", ids.Upper(tenant), "error", Describe(err))
	rows, err := s.jobRows(ctx, tenant, `ORDER BY hermes_job_id`)
	if err != nil {
		return JobsResponse{}, err
	}
	out := make([]JobDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowJobDTO(r))
	}
	return JobsResponse{Source: "snapshot", Jobs: out}, nil
}

// MARK: - Vault detection

// detectVault finds the KB root: the preferred path, the dashboard cwd's
// candidates (a kb-config.json pointer first), then obsidian-vault children.
func (s *Service) detectVault(ctx context.Context, transport hermesdash.Transport, preferred *string) (*string, error) {
	if preferred != nil && isVaultRoot(ctx, transport, *preferred) {
		p := *preferred
		return &p, nil
	}
	status, err := transport.Status(ctx)
	if err != nil {
		return nil, err
	}
	if status.DefaultCwd == nil {
		return nil, nil
	}
	base, err := hermesdash.ValidatePath(*status.DefaultCwd)
	if err != nil {
		return nil, nil
	}
	candidates := []string{base, hermesdash.JoinPath(base, "kb"), hermesdash.JoinPath(base, "vault"), hermesdash.JoinPath(base, "obsidian-vault")}
	if pointer := readKBConfigPointer(ctx, transport, base); pointer != nil {
		candidates = append([]string{*pointer}, candidates...)
	}
	for _, c := range candidates {
		if isVaultRoot(ctx, transport, c) {
			return &c, nil
		}
	}
	if entries, err := transport.ListFiles(ctx, hermesdash.JoinPath(base, "obsidian-vault")); err == nil {
		for _, e := range entries {
			if e.IsDirectory && !strings.HasPrefix(e.Name, ".") && isVaultRoot(ctx, transport, e.Path) {
				p := e.Path
				return &p, nil
			}
		}
	}
	return nil, nil
}

// isVaultRoot: a .kb/manifest.json, or a raw/ directory.
func isVaultRoot(ctx context.Context, transport hermesdash.Transport, path string) bool {
	entries, err := transport.ListFiles(ctx, path)
	if err != nil {
		return false
	}
	dirs := map[string]bool{}
	for _, e := range entries {
		if e.IsDirectory {
			dirs[e.Name] = true
		}
	}
	if dirs[".kb"] {
		if kb, err := transport.ListFiles(ctx, hermesdash.JoinPath(path, ".kb")); err == nil {
			for _, e := range kb {
				if e.Name == "manifest.json" && !e.IsDirectory {
					return true
				}
			}
		}
	}
	return dirs["raw"]
}

func readKBConfigPointer(ctx context.Context, transport hermesdash.Transport, base string) *string {
	text, err := transport.ReadText(ctx, hermesdash.JoinPath(base, "kb-config.json"))
	if err != nil {
		return nil
	}
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) != nil {
		return nil
	}
	var raw string
	found := false
	for _, key := range []string{"kb_path", "root", "path"} {
		if v, ok := object[key].(string); ok {
			raw, found = v, true
			break
		}
	}
	if !found {
		return nil
	}
	absolute := raw
	if !strings.HasPrefix(raw, "/") {
		absolute = hermesdash.JoinPath(base, raw)
	}
	p, err := hermesdash.ValidatePath(absolute)
	if err != nil {
		return nil
	}
	return &p
}

// MARK: - Vault import

type vaultCursor struct {
	VaultPath string `json:"vaultPath"`
	LastPath  string `json:"lastPath"`
	Scanned   int    `json:"scanned"`
}

// ImportVault is importVault(tenantID:requestedPath:): a capped, resumable
// depth-first walk ingesting *.md into the Hermes Vault space.
func (s *Service) ImportVault(ctx context.Context, tenant uuid.UUID, requestedPath *string) (VaultImportResultDTO, error) {
	if err := s.beginExclusive(tenant); err != nil {
		return VaultImportResultDTO{}, err
	}
	defer s.endExclusive(tenant)
	st, err := s.ensureState(ctx, tenant)
	if err != nil {
		return VaultImportResultDTO{}, err
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return VaultImportResultDTO{}, err
	}

	var vaultPath string
	switch {
	case requestedPath != nil:
		vaultPath, err = hermesdash.ValidatePath(*requestedPath)
		if err != nil {
			return VaultImportResultDTO{}, err
		}
		if _, err := transport.ListFiles(ctx, vaultPath); err != nil {
			return VaultImportResultDTO{}, wire.NewError(http.StatusBadRequest, "hermes_vault_path_not_found")
		}
	default:
		detected, err := s.detectVault(ctx, transport, st.VaultPath)
		if err != nil {
			return VaultImportResultDTO{}, err
		}
		if detected == nil {
			return VaultImportResultDTO{}, wire.NewError(http.StatusNotFound, "hermes_vault_not_found")
		}
		vaultPath = *detected
	}

	var cursor *vaultCursor
	if st.VaultCursor != nil {
		var c vaultCursor
		if json.Unmarshal([]byte(*st.VaultCursor), &c) == nil && c.VaultPath == vaultPath {
			cursor = &c
		}
	}
	scanned := 0
	var resumeFrom *string
	if cursor != nil {
		scanned = cursor.Scanned
		lp := cursor.LastPath
		resumeFrom = &lp
	}
	imported, skipped, failed, processed := 0, 0, 0, 0
	var batch []imports.FileInput
	var lastPath *string
	truncated := false

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		prov := VaultProvenance
		res, err := s.Ingest.IngestBatchWithProvenance(ctx, tenant, VaultSpaceName, batch, &prov)
		if err != nil {
			return err
		}
		imported += res.Imported
		skipped += res.Skipped
		failed += res.Failed
		batch = nil
		return nil
	}

	stack := []string{vaultPath}
walk:
	for len(stack) > 0 {
		directory := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := transport.ListFiles(ctx, directory)
		if err != nil {
			failed++
			continue
		}
		var subdirectories []string
		for _, entry := range entries {
			if entry.IsDirectory {
				if !skippedDirectories[entry.Name] && !strings.HasPrefix(entry.Name, ".") {
					subdirectories = append(subdirectories, entry.Path)
				}
				continue
			}
			if !strings.HasSuffix(strings.ToLower(entry.Name), ".md") {
				continue
			}
			if resumeFrom != nil {
				if entry.Path == *resumeFrom {
					resumeFrom = nil
				}
				continue
			}
			if scanned >= s.Limits.MaxVaultFiles || processed >= s.Limits.VaultFilesPerRun {
				truncated = true
				break walk
			}
			scanned++
			processed++
			p := entry.Path
			lastPath = &p
			content, err := transport.ReadText(ctx, entry.Path)
			if err != nil {
				var te *hermesdash.Error
				if !errors.As(err, &te) {
					return VaultImportResultDTO{}, err
				}
				if te.Kind == hermesdash.ErrBodyTooLarge || te.Kind == hermesdash.ErrInvalidResponse {
					skipped++
				} else {
					failed++
				}
			} else if len(content) > s.Limits.MaxFileBytes {
				skipped++
			} else {
				relative := strings.Trim(entry.Path[min(len(vaultPath), len(entry.Path)):], "/")
				batch = append(batch, imports.FileInput{Path: relative, Content: content})
			}
			if len(batch) >= s.Limits.IngestBatchSize {
				if err := flush(); err != nil {
					return VaultImportResultDTO{}, err
				}
			}
			if err := s.pause(ctx, s.Limits.ReadPause); err != nil {
				return VaultImportResultDTO{}, err
			}
		}
		sort.Strings(subdirectories)
		for i := len(subdirectories) - 1; i >= 0; i-- {
			stack = append(stack, subdirectories[i])
		}
	}
	if err := flush(); err != nil {
		return VaultImportResultDTO{}, err
	}

	if resumeFrom != nil && processed == 0 {
		st.VaultCursor = nil
		if err := s.saveState(ctx, st); err != nil {
			return VaultImportResultDTO{}, err
		}
		return VaultImportResultDTO{VaultPath: vaultPath, Scanned: scanned, Truncated: true}, nil
	}

	var count int64
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM vault_files WHERE tenant_id = $1 AND path LIKE $2`,
		tenant, imports.Slugify(VaultSpaceName)+"/%").Scan(&count); err != nil {
		return VaultImportResultDTO{}, err
	}
	st.VaultPath = &vaultPath
	st.VaultFilesCount = count
	st.VaultState = "imported"
	var encoded *string
	if truncated && lastPath != nil {
		encoded = encodeJSON(vaultCursor{VaultPath: vaultPath, LastPath: *lastPath, Scanned: scanned})
	}
	st.VaultCursor = encoded
	status := "ok"
	var errText *string
	if failed > 0 {
		status = "partial"
		msg := fmt.Sprintf("vault import: %d file(s) failed", failed)
		errText = &msg
	}
	if err := s.record(ctx, st, status, errText); err != nil {
		return VaultImportResultDTO{}, err
	}
	s.log().InfoContext(ctx, "hermes mirror vault import", "tenant", ids.Upper(tenant), "imported", imported,
		"skipped", skipped, "failed", failed, "truncated", truncated)
	return VaultImportResultDTO{
		VaultPath: vaultPath, Scanned: scanned, Imported: imported, Skipped: skipped, Failed: failed,
		Truncated: truncated, Cursor: encoded,
	}, nil
}

// MARK: - Vault create

// CreateVault builds the KB skeleton on the tenant's Hermes and installs the
// missing bundled kb-* skills. An existing vault is reported, not touched.
func (s *Service) CreateVault(ctx context.Context, tenant uuid.UUID) (VaultCreateResultDTO, error) {
	if err := s.beginExclusive(tenant); err != nil {
		return VaultCreateResultDTO{}, err
	}
	defer s.endExclusive(tenant)
	st, err := s.ensureState(ctx, tenant)
	if err != nil {
		return VaultCreateResultDTO{}, err
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return VaultCreateResultDTO{}, err
	}
	existing, err := s.detectVault(ctx, transport, st.VaultPath)
	if err != nil {
		return VaultCreateResultDTO{}, err
	}
	if existing != nil {
		st.VaultPath = existing
		if st.VaultState == "absent" {
			st.VaultState = "detected"
		}
		installed, err := s.installMissingKBSkills(ctx, transport)
		if err != nil {
			return VaultCreateResultDTO{}, err
		}
		if err := s.saveState(ctx, st); err != nil {
			return VaultCreateResultDTO{}, err
		}
		return VaultCreateResultDTO{VaultPath: *existing, AlreadyExisted: true, CreatedDirectories: []string{}, InstalledSkills: installed}, nil
	}
	status, err := transport.Status(ctx)
	if err != nil {
		return VaultCreateResultDTO{}, err
	}
	if status.DefaultCwd == nil {
		return VaultCreateResultDTO{}, wire.NewError(http.StatusBadGateway, "hermes_vault_root_unknown")
	}
	cwd, err := hermesdash.ValidatePath(*status.DefaultCwd)
	if err != nil {
		return VaultCreateResultDTO{}, err
	}
	root := hermesdash.JoinPath(cwd, "kb")
	created := []string{}
	for _, dir := range []string{root, hermesdash.JoinPath(root, "raw"), hermesdash.JoinPath(root, "wiki"), hermesdash.JoinPath(root, ".kb")} {
		if err := transport.Mkdir(ctx, dir); err != nil {
			return VaultCreateResultDTO{}, err
		}
		created = append(created, dir)
	}
	if err := transport.WriteText(ctx, hermesdash.JoinPath(root, ".kb/manifest.json"), ManifestJSON(s.now())); err != nil {
		return VaultCreateResultDTO{}, err
	}
	if err := transport.WriteText(ctx, hermesdash.JoinPath(root, "README.md"), Readme(root)); err != nil {
		return VaultCreateResultDTO{}, err
	}
	installed, err := s.installMissingKBSkills(ctx, transport)
	if err != nil {
		return VaultCreateResultDTO{}, err
	}
	st.VaultPath = &root
	st.VaultState = "created"
	if err := s.record(ctx, st, "ok", nil); err != nil {
		return VaultCreateResultDTO{}, err
	}
	return VaultCreateResultDTO{VaultPath: root, CreatedDirectories: created, InstalledSkills: installed}, nil
}

// BundledSkill is HermesBundledSkills.Skill.
type BundledSkill struct{ Name, Content string }

// KBSkills is HermesBundledSkills.kbSkills: every kb-*/SKILL.md, by name.
func KBSkills(root fs.FS) ([]BundledSkill, error) {
	if root == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil, nil //nolint:nilerr // a missing directory is no skills, as in Swift
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "kb-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []BundledSkill
	for _, name := range names {
		data, err := fs.ReadFile(root, name+"/SKILL.md")
		if err != nil {
			continue
		}
		out = append(out, BundledSkill{Name: name, Content: string(data)})
	}
	return out, nil
}

func (s *Service) installMissingKBSkills(ctx context.Context, transport hermesdash.Transport) ([]string, error) {
	present := map[string]bool{}
	if skills, err := transport.ListSkills(ctx); err == nil {
		for _, sk := range skills {
			present[sk.Name] = true
		}
	}
	bundled, err := KBSkills(s.BundledSkills)
	if err != nil {
		return nil, err
	}
	installed := []string{}
	for _, skill := range bundled {
		if present[skill.Name] {
			continue
		}
		if err := transport.CreateSkill(ctx, skill.Name, skill.Content); err != nil {
			return nil, err
		}
		installed = append(installed, skill.Name)
	}
	return installed, nil
}

// ManifestJSON is manifestJSON(now:): JSONEncoder [.prettyPrinted, .sortedKeys].
func ManifestJSON(now time.Time) string {
	created, _ := json.Marshal(hermesdash.ISO(now))
	return "{\n" +
		"  \"articles\" : [\n\n  ],\n" +
		"  \"created_at\" : " + strings.ReplaceAll(string(created), "/", `\/`) + ",\n" +
		"  \"created_by\" : \"luminavault\",\n" +
		"  \"sources\" : [\n\n  ],\n" +
		"  \"version\" : 1\n" +
		"}\n"
}

// Readme is readme(root:).
func Readme(root string) string {
	return "# LuminaVault knowledge base\n\n" +
		"Created by LuminaVault at `" + root + "`.\n\n" +
		"- `raw/` — sources you ingest (`/kb-ingest`, `/kb-import`).\n" +
		"- `wiki/` — compiled concept articles with backlinks (`/kb-compile`).\n" +
		"- `.kb/manifest.json` — compile bookkeeping.\n\n" +
		"LuminaVault mirrors this vault and runs the nightly `" + CompileJobName + "` job.\n"
}

// MARK: - Sessions import

type sessionsCursor struct {
	Offset       int          `json:"offset"`
	HighWater    *wire.Double `json:"highWater,omitempty"`
	RunHighWater *wire.Double `json:"runHighWater,omitempty"`
}

func seconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// ImportSessions is importSessions(tenantID:): newest-first, resumable, stops
// at already-imported history; triggers a memory compile over what it wrote.
func (s *Service) ImportSessions(ctx context.Context, tenant uuid.UUID) (SessionsImportResultDTO, error) {
	if err := s.beginExclusive(tenant); err != nil {
		return SessionsImportResultDTO{}, err
	}
	defer s.endExclusive(tenant)
	st, err := s.ensureState(ctx, tenant)
	if err != nil {
		return SessionsImportResultDTO{}, err
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return SessionsImportResultDTO{}, err
	}
	cursor := sessionsCursor{}
	if st.SessionsCursor != nil {
		var c sessionsCursor
		if json.Unmarshal([]byte(*st.SessionsCursor), &c) == nil {
			cursor = c
		}
	}
	imported, skipped, processed := 0, 0, 0
	var files []imports.FileInput
	var written []uuid.UUID
	reachedHistory, exhausted := false, false
	flush := func() error {
		if len(files) == 0 {
			return nil
		}
		prov := SessionProvenance
		res, err := s.Ingest.IngestBatchWithProvenance(ctx, tenant, SessionsSpaceName, files, &prov)
		if err != nil {
			return err
		}
		written = append(written, res.VaultFileIDs...)
		files = nil
		return nil
	}

	for processed < s.Limits.SessionsPerRun && cursor.Offset < s.Limits.MaxSessions && !reachedHistory && !exhausted {
		page, err := transport.ListSessions(ctx, cursor.Offset, s.Limits.SessionPageSize)
		if err != nil {
			return SessionsImportResultDTO{}, err
		}
		if len(page.Sessions) == 0 {
			exhausted = true
			break
		}
		for _, session := range page.Sessions {
			if processed >= s.Limits.SessionsPerRun || cursor.Offset >= s.Limits.MaxSessions {
				break
			}
			cursor.Offset++
			var lastActive *float64
			if session.LastActiveAt != nil {
				v := seconds(*session.LastActiveAt)
				lastActive = &v
			} else if session.StartedAt != nil {
				v := seconds(*session.StartedAt)
				lastActive = &v
			}
			if cursor.HighWater != nil && lastActive != nil && *lastActive <= float64(*cursor.HighWater) {
				reachedHistory = true
				break
			}
			processed++
			if session.MessageCount <= 0 {
				skipped++
				continue
			}
			messages, err := transport.SessionMessages(ctx, session.ID)
			if err != nil {
				skipped++
				continue
			}
			markdown := SessionMarkdown(session, messages, s.Limits.MaxSessionBytes)
			if markdown == nil {
				skipped++
				continue
			}
			files = append(files, imports.FileInput{Path: SessionFileName(session), Content: *markdown})
			imported++
			if lastActive != nil {
				prev := 0.0
				if cursor.RunHighWater != nil {
					prev = float64(*cursor.RunHighWater)
				}
				v := wire.Double(math.Max(prev, *lastActive))
				cursor.RunHighWater = &v
			}
			if len(files) >= s.Limits.IngestBatchSize {
				if err := flush(); err != nil {
					return SessionsImportResultDTO{}, err
				}
			}
			if err := s.pause(ctx, s.Limits.ReadPause); err != nil {
				return SessionsImportResultDTO{}, err
			}
		}
		if len(page.Sessions) < s.Limits.SessionPageSize {
			exhausted = true
		}
	}
	if err := flush(); err != nil {
		return SessionsImportResultDTO{}, err
	}

	complete := reachedHistory || exhausted || cursor.Offset >= s.Limits.MaxSessions
	var encoded *string
	if complete {
		hw, run := 0.0, 0.0
		if cursor.HighWater != nil {
			hw = float64(*cursor.HighWater)
		}
		if cursor.RunHighWater != nil {
			run = float64(*cursor.RunHighWater)
		}
		v := wire.Double(math.Max(hw, run))
		cursor.HighWater = &v
		cursor.RunHighWater = nil
		cursor.Offset = 0
		st.SessionsCursor = encodeJSON(cursor)
	} else {
		encoded = encodeJSON(cursor)
		st.SessionsCursor = encoded
	}
	st.SessionsImported += int64(imported)

	compileTriggered := false
	if s.Compile != nil && len(written) > 0 {
		if err := s.Compile.Compile(ctx, tenant, written); err != nil {
			s.log().WarnContext(ctx, "hermes mirror session compile failed", "tenant", ids.Upper(tenant), "error", Describe(err))
		} else {
			compileTriggered = true
		}
	}
	if err := s.record(ctx, st, "ok", nil); err != nil {
		return SessionsImportResultDTO{}, err
	}
	s.log().InfoContext(ctx, "hermes mirror sessions import", "tenant", ids.Upper(tenant), "imported", imported,
		"skipped", skipped, "files", len(written), "truncated", !complete)
	return SessionsImportResultDTO{
		SessionsImported: imported, SessionsSkipped: skipped, FilesWritten: len(written),
		Truncated: !complete, Cursor: encoded, CompileTriggered: compileTriggered,
	}, nil
}

// SessionFileName is sessionFileName(_:): sessions/<yyyy-MM-dd>-<safe id>.md.
func SessionFileName(session hermesdash.Session) string {
	date := time.Unix(0, 0).UTC()
	if session.StartedAt != nil {
		date = *session.StartedAt
	} else if session.LastActiveAt != nil {
		date = *session.LastActiveAt
	}
	var safe strings.Builder
	for _, c := range swiftstr.Characters(session.ID) {
		if c == "-" || c == "_" || swiftstr.IsLetterOrNumber(c) {
			safe.WriteString(c)
		}
	}
	id := safe.String()
	if id == "" {
		id = "session"
	}
	return "sessions/" + date.UTC().Format("2006-01-02") + "-" + id + ".md"
}

// SessionMarkdown is sessionMarkdown(session:messages:maxBytes:): nil when no
// user or assistant text survives; truncated with a note past maxBytes.
func SessionMarkdown(session hermesdash.Session, messages []hermesdash.Message, maxBytes int) *string {
	var textual []hermesdash.Message
	for _, m := range messages {
		if swiftstr.TrimWhitespacesAndNewlines(m.Content) != "" && (m.Role == "user" || m.Role == "assistant") {
			textual = append(textual, m)
		}
	}
	if len(textual) == 0 {
		return nil
	}
	title := ""
	if session.Title != nil {
		title = swiftstr.TrimWhitespacesAndNewlines(*session.Title)
	}
	if title == "" {
		title = "Hermes session " + session.ID
	}
	lines := []string{"# " + title, "", "- session: `" + session.ID + "`"}
	if session.Source != nil {
		lines = append(lines, "- source: "+*session.Source)
	}
	if session.StartedAt != nil {
		lines = append(lines, "- started: "+hermesdash.ISO(*session.StartedAt))
	}
	if session.LastActiveAt != nil {
		lines = append(lines, "- last active: "+hermesdash.ISO(*session.LastActiveAt))
	}
	lines = append(lines, fmt.Sprintf("- messages: %d", len(textual)), "")
	body := strings.Join(lines, "\n") + "\n"
	truncated := false
	for _, m := range textual {
		heading := "## Assistant"
		if m.Role == "user" {
			heading = "## User"
		}
		stamp := ""
		if m.Timestamp != nil {
			stamp = " (" + hermesdash.ISO(*m.Timestamp) + ")"
		}
		block := heading + stamp + "\n\n" + swiftstr.TrimWhitespacesAndNewlines(m.Content) + "\n\n"
		if len(body)+len(block) > maxBytes {
			truncated = true
			break
		}
		body += block
	}
	if truncated {
		body += fmt.Sprintf("_Transcript truncated at %d bytes._\n", maxBytes)
	}
	return &body
}

// MARK: - Compile cron

// InstallCompileJob creates (once, by name) the nightly kb-compile job.
func (s *Service) InstallCompileJob(ctx context.Context, tenant uuid.UUID) (CompileJobInstallResultDTO, error) {
	st, err := s.ensureState(ctx, tenant)
	if err != nil {
		return CompileJobInstallResultDTO{}, err
	}
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return CompileJobInstallResultDTO{}, err
	}
	jobs, err := transport.ListJobs(ctx)
	if err != nil {
		return CompileJobInstallResultDTO{}, err
	}
	for _, job := range jobs {
		if job.Name != nil && *job.Name == CompileJobName {
			id := job.ID
			st.CompileJobID = &id
			if err := s.saveState(ctx, st); err != nil {
				return CompileJobInstallResultDTO{}, err
			}
			return CompileJobInstallResultDTO{JobID: job.ID, Schedule: orDefault(job.Schedule, CompileJobSchedule)}, nil
		}
	}
	vaultPath := st.VaultPath
	if vaultPath == nil {
		vaultPath, err = s.detectVault(ctx, transport, nil)
		if err != nil {
			return CompileJobInstallResultDTO{}, err
		}
	}
	created, err := transport.CreateJob(ctx, hermesdash.JobSpec{
		Name: CompileJobName, Schedule: CompileJobSchedule, Prompt: CompilePrompt(vaultPath),
		Deliver: "origin", Skills: []string{CompileSkillName},
	})
	if err != nil {
		return CompileJobInstallResultDTO{}, err
	}
	id := created.ID
	st.CompileJobID = &id
	if vaultPath != nil {
		st.VaultPath = vaultPath
	}
	if err := s.saveState(ctx, st); err != nil {
		return CompileJobInstallResultDTO{}, err
	}
	return CompileJobInstallResultDTO{JobID: created.ID, Schedule: orDefault(created.Schedule, CompileJobSchedule), Created: true}, nil
}

// CompilePrompt is compilePrompt(vaultPath:).
func CompilePrompt(vaultPath *string) string {
	location := "Locate the knowledge base (the directory holding `raw/`, `wiki/` and `.kb/manifest.json`)."
	if vaultPath != nil {
		location = "The knowledge base lives at `" + *vaultPath + "`."
	}
	return "Run the /kb-compile skill. " + location + " Compile every uncompiled file under raw/ into wiki/ concept articles with Obsidian backlinks, update the index, and report how many sources and articles were written. Do not ask questions; skip anything that cannot be compiled and list it at the end."
}

// SupportsAdminConfig is supportsAdminConfig(tenantID:): any failure is "no".
func (s *Service) SupportsAdminConfig(ctx context.Context, tenant uuid.UUID) bool {
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return false
	}
	status, err := transport.Status(ctx)
	if err != nil {
		return false
	}
	return status.AdminConfigRW != nil && *status.AdminConfigRW
}

func orDefault(p *string, fallback string) string {
	if p != nil {
		return *p
	}
	return fallback
}

func encodeJSON(v any) *string {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out := string(data)
	return &out
}

func prefixChars(s string, n int) string {
	return swiftstr.Prefix(s, n)
}
