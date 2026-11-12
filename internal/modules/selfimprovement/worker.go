package selfimprovement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/jobs"
	"lumina/internal/llm"
	"lumina/internal/modules/selfimprovement/db"
	"lumina/internal/selfimprove"
	"lumina/internal/skills"
	"lumina/internal/soul"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SchedulerInterval is SelfImprovementScheduler's interval.
const SchedulerInterval = 15 * time.Second

const (
	reviewLease          = 600 * time.Second
	dueBatch             = 10
	archiveAfterDays     = 90
	staleAfterDays       = 30
	consolidateMax       = 40
	consolidateActions   = 20
	skillDocumentChars   = 16000
	promptDocumentsChars = 60000
	historyChars         = 60000
	reviewTemperature    = 0.1
)

// ReviewLLM is HermesLLMService.chat for one user prompt: DefaultHermesLLMService
// (llm.DirectHermes) in production. It returns the upstream model and the reply.
type ReviewLLM interface {
	Chat(ctx context.Context, sessionKey, sessionID, model, prompt string, temperature *float64) (string, llm.CompletionMessage, error)
}

// WorkerDeps are the scheduler's collaborators beyond the module's own.
type WorkerDeps struct {
	LLM       ReviewLLM
	MainModel string // HERMES_DEFAULT_MODEL
	Log       *slog.Logger
}

// Worker is SelfImprovementScheduler driving SelfImprovementService.tick.
type Worker struct {
	m   *Module
	d   WorkerDeps
	log *slog.Logger
}

// NewWorker builds the scheduler over the module's settings, catalog and paths.
func (m *Module) NewWorker(d WorkerDeps) *Worker {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Worker{m: m, d: d, log: log}
}

// Job is SelfImprovementScheduler: one tick every 15 s.
func (w *Worker) Job() jobs.Job {
	return jobs.Job{
		Name:     "SelfImprovementScheduler",
		Schedule: jobs.Every(SchedulerInterval, 0),
		Run: func(ctx context.Context) error {
			w.Tick(ctx, w.m.now())
			return nil
		},
	}
}

// httpError is a Hummingbird HTTPError the service throws; Error() is its
// String(describing:), which is what failure_reason stores.
type httpError struct {
	status  int
	message string
}

// reasonPhrases are swift-http-types' reason phrases for the statuses thrown here.
var reasonPhrases = map[int]string{
	http.StatusNotFound:            "Not Found",
	http.StatusConflict:            "Conflict",
	http.StatusUnprocessableEntity: "Unprocessable Content",
	http.StatusBadGateway:          "Bad Gateway",
	http.StatusServiceUnavailable:  "Service Unavailable",
}

func (e *httpError) Error() string {
	phrase, ok := reasonPhrases[e.status]
	if !ok {
		phrase = http.StatusText(e.status)
	}
	return "HTTPError: " + phrase + ", " + e.message
}

// describe is String(describing: error): a route error becomes the HTTPError it
// ports.
func describe(err error) string {
	var werr *wire.Error
	if errors.As(err, &werr) {
		return (&httpError{werr.Status, werr.Message}).Error()
	}
	return err.Error()
}

// Tick is SelfImprovementService.tick(now:): queue due weekly reviews, then claim
// and process at most one queued run. Any failure is only logged.
func (w *Worker) Tick(ctx context.Context, now time.Time) {
	if err := w.tick(ctx, now); err != nil {
		w.log.ErrorContext(ctx, "self_improvement.tick_failed error="+describe(err))
	}
}

func (w *Worker) tick(ctx context.Context, now time.Time) error {
	if err := w.enqueueDueReviews(ctx, now); err != nil {
		return err
	}
	run, ok, err := w.claimQueuedRun(ctx, now)
	if err != nil || !ok {
		return err
	}
	return w.process(ctx, run)
}

func (w *Worker) enqueueDueReviews(ctx context.Context, now time.Time) error {
	if !w.m.enabled {
		return nil
	}
	// The migration seeds existing tenants; this keeps the default-on contract for
	// accounts created since, even if they never open settings.
	if _, err := w.m.pool.Exec(ctx, `
		INSERT INTO self_improvement_settings (id, tenant_id, next_review_at)
		SELECT gen_random_uuid(), id, $1 FROM users
		ON CONFLICT (tenant_id) DO NOTHING`, now.Add(firstReviewDelay)); err != nil {
		return err
	}
	rows, err := w.m.pool.Query(ctx, `
		UPDATE self_improvement_settings
		SET lease_until = $2,
		    next_review_at = $1 + (interval_hours || ' hours')::interval,
		    updated_at = $1
		WHERE id IN (
		  SELECT id FROM self_improvement_settings
		  WHERE enabled = TRUE
		    AND next_review_at <= $1
		    AND (lease_until IS NULL OR lease_until < $1)
		    AND (last_activity_at IS NULL OR last_activity_at <= $1 - (minimum_idle_hours || ' hours')::interval)
		  ORDER BY next_review_at ASC
		  FOR UPDATE SKIP LOCKED
		  LIMIT 10
		)
		RETURNING tenant_id, curator_enabled, soul_review_enabled`, now, now.Add(reviewLease))
	if err != nil {
		return err
	}
	type due struct {
		tenant        uuid.UUID
		curator, soul bool
	}
	var dueRows []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.tenant, &d.curator, &d.soul); err != nil {
			rows.Close()
			return err
		}
		dueRows = append(dueRows, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range dueRows {
		if d.curator {
			if err := w.enqueueWeekly(ctx, d.tenant, Curator); err != nil {
				return err
			}
		}
		if d.soul {
			if err := w.enqueueWeekly(ctx, d.tenant, Soul); err != nil {
				return err
			}
		}
	}
	return nil
}

// enqueueWeekly is enqueue(tenantID:kind:trigger: .weekly, dryRun: false).
func (w *Worker) enqueueWeekly(ctx context.Context, tenant uuid.UUID, kind ImprovementChangeKind) error {
	if err := w.m.requireManaged(ctx, tenant); err != nil {
		return err
	}
	active, err := w.m.q.ActiveRun(ctx, db.ActiveRunParams{TenantID: tenant, Kind: string(kind)})
	switch {
	case err == nil && !active.DryRun:
		return nil
	case err == nil:
		return errRunActive
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	_, err = w.m.q.InsertRun(ctx, db.InsertRunParams{
		ID: uuid.New(), TenantID: tenant, Kind: string(kind), Trigger: string(Weekly), DryRun: false, CreatedAt: w.m.now(),
	})
	return err
}

func (w *Worker) claimQueuedRun(ctx context.Context, now time.Time) (db.SelfImprovementRun, bool, error) {
	var run db.SelfImprovementRun
	err := w.m.pool.QueryRow(ctx, `
		UPDATE self_improvement_runs
		SET status = 'running', started_at = $1
		WHERE id = (
		  SELECT id FROM self_improvement_runs
		  WHERE status = 'queued'
		  ORDER BY created_at ASC
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1
		)
		RETURNING id, tenant_id, kind, trigger, dry_run`, now).Scan(&run.ID, &run.TenantID, &run.Kind, &run.Trigger, &run.DryRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return run, false, nil
	}
	return run, err == nil, err
}

// runOutcome is what processing sets on the ImprovementRun before process() saves
// it: Swift mutates the model as it goes, so fields set before a failure are saved
// with it.
type runOutcome struct {
	modelUsed, report, snapshotJSON *string
	applied, skipped                *int
}

func (w *Worker) process(ctx context.Context, run db.SelfImprovementRun) error {
	var out runOutcome
	var err error
	if ImprovementChangeKind(run.Kind) == Soul {
		err = w.processSoulReview(ctx, run, &out)
	} else {
		err = w.processCurator(ctx, run, &out)
	}
	status := "succeeded"
	var failure *string
	if err != nil {
		status = "failed"
		reason := describe(err)
		failure = &reason
		w.log.WarnContext(ctx, fmt.Sprintf("self_improvement.run_failed id=%s error=%s", ids.Upper(run.ID), reason))
	}
	_, saveErr := w.m.pool.Exec(ctx, `
		UPDATE self_improvement_runs SET status = $2, failure_reason = COALESCE($3, failure_reason), ended_at = $4,
			model_used = COALESCE($5, model_used), report_markdown = COALESCE($6, report_markdown),
			snapshot_json = COALESCE($7, snapshot_json),
			actions_applied = COALESCE($8, actions_applied), actions_skipped = COALESCE($9, actions_skipped)
		WHERE id = $1`,
		run.ID, status, failure, w.m.now(), out.modelUsed, out.report, out.snapshotJSON, out.applied, out.skipped)
	return saveErr
}

// MARK: - Curator

// snapshot is ResourceSnapshot, encoded in Swift's declaration order with nil
// optionals omitted.
type snapshot struct {
	Name         string  `json:"name"`
	Markdown     string  `json:"markdown"`
	State        string  `json:"state"`
	AfterSHA     *string `json:"afterSHA,omitempty"`
	ArchivedPath *string `json:"archivedPath,omitempty"`
}

// skillState is the SkillsState row state(tenantID:name:create:) returns. A loaded
// row saves as Fluent saves it: by its @ID, which is tenant_id alone, so the update
// reaches every skills_state row of the tenant. A new row inserts.
type skillState struct {
	exists           bool
	name             string
	pinned           bool
	curatorState     string
	lastActivityAt   *time.Time
	curatorArchivedA *time.Time
}

func (w *Worker) state(ctx context.Context, tenant uuid.UUID, name string) (*skillState, error) {
	s := &skillState{name: name, curatorState: string(ImprovementResourceStateActive)}
	err := w.m.pool.QueryRow(ctx, `SELECT curator_pinned, curator_state, curator_last_activity_at, curator_archived_at
		FROM skills_state WHERE tenant_id = $1 AND source = 'vault' AND name = $2 LIMIT 1`, tenant, name).
		Scan(&s.pinned, &s.curatorState, &s.lastActivityAt, &s.curatorArchivedA)
	switch {
	case err == nil:
		s.exists = true
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	return s, nil
}

// stateChange is the fields a curator step set before save().
type stateChange struct {
	lastActivity *time.Time
	curatorState *string
	archivedAt   *time.Time
}

func (w *Worker) saveState(ctx context.Context, tenant uuid.UUID, s *skillState, c stateChange) error {
	if !s.exists {
		state := s.curatorState
		if c.curatorState != nil {
			state = *c.curatorState
		}
		_, err := w.m.pool.Exec(ctx, `INSERT INTO skills_state
			(tenant_id, source, name, enabled, schedule_override, run_at, last_run_at, last_status, last_error, apns_category,
			 curator_pinned, curator_state, curator_last_activity_at, curator_archived_at)
			VALUES ($1, 'vault', $2, TRUE, NULL, NULL, NULL, NULL, NULL, NULL, FALSE, $3, $4, $5)`,
			tenant, s.name, state, c.lastActivity, c.archivedAt)
		if err == nil {
			s.exists = true
		}
		return err
	}
	sets := []string{}
	args := []any{tenant}
	add := func(column string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if c.lastActivity != nil {
		add("curator_last_activity_at", *c.lastActivity)
	}
	if c.curatorState != nil {
		add("curator_state", *c.curatorState)
	}
	if c.archivedAt != nil {
		add("curator_archived_at", *c.archivedAt)
	}
	if len(sets) == 0 {
		return nil
	}
	_, err := w.m.pool.Exec(ctx, `UPDATE skills_state SET `+strings.Join(sets, ", ")+` WHERE tenant_id = $1`, args...)
	return err
}

func (w *Worker) lastActivity(ctx context.Context, tenant uuid.UUID, name string) (time.Time, error) {
	var last *time.Time
	if err := w.m.pool.QueryRow(ctx, `SELECT MAX(started_at) AS last_run_at FROM skill_run_log WHERE tenant_id = $1 AND name = $2`,
		tenant, name).Scan(&last); err != nil {
		return time.Time{}, err
	}
	if last != nil {
		return *last, nil
	}
	if info, err := os.Stat(w.m.skillFile(tenant, name)); err == nil {
		return info.ModTime(), nil
	}
	return time.Now(), nil
}

func (w *Worker) snapshotOf(tenant uuid.UUID, name, state string) (snapshot, error) {
	data, err := os.ReadFile(w.m.skillFile(tenant, name))
	if err != nil {
		return snapshot{}, err
	}
	text, ok := vaultfiles.DecodeUTF8(data)
	if !ok {
		return snapshot{}, fmt.Errorf("%s is not UTF-8", w.m.skillFile(tenant, name))
	}
	return snapshot{Name: name, Markdown: text, State: state}, nil
}

// archive moves the skill's directory to skills/.archive/<name>-<RUN-ID>.
func (w *Worker) archive(tenant uuid.UUID, name string, run uuid.UUID) (string, error) {
	source := filepath.Dir(w.m.skillFile(tenant, name))
	target := filepath.Join(w.m.paths.TenantRoot(tenant), "skills", ".archive", name+"-"+ids.Upper(run))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // Foundation's createDirectory default, as Swift creates it
		return "", err
	}
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("archive target exists: %s", target)
	}
	if err := os.Rename(source, target); err != nil {
		return "", err
	}
	return target, nil
}

func vaultManifests(c *skills.Catalog, tenant uuid.UUID) []skills.Manifest {
	var out []skills.Manifest
	for _, mf := range c.Manifests(tenant) {
		if mf.Source == skills.SourceVault {
			out = append(out, mf)
		}
	}
	return out
}

func intPtr(n int) *int { return &n }

func (w *Worker) processCurator(ctx context.Context, run db.SelfImprovementRun, out *runOutcome) error {
	tenant := run.TenantID
	if err := w.m.requireManaged(ctx, tenant); err != nil {
		return err
	}
	settings, err := w.m.settings(ctx, tenant)
	if err != nil {
		return err
	}
	if !run.DryRun && (!w.m.enabled || !settings.Enabled || !settings.CuratorEnabled) {
		return &httpError{http.StatusServiceUnavailable, "curator_disabled"}
	}
	manifests := vaultManifests(w.m.catalog, tenant)
	mode := "live"
	if run.DryRun {
		mode = "dry-run"
	}
	report := []string{
		"# Curator Report", "",
		"- Run: " + ids.Upper(run.ID),
		"- Mode: " + mode,
		fmt.Sprintf("- Candidates: %d", len(manifests)), "",
	}
	var snapshots []snapshot
	applied, skipped := 0, 0
	now := w.m.now()

	for _, mf := range manifests {
		state, err := w.state(ctx, tenant, mf.Name)
		if err != nil {
			return err
		}
		last, err := w.lastActivity(ctx, tenant, mf.Name)
		if err != nil {
			return err
		}
		change := stateChange{lastActivity: &last}
		if state.pinned {
			skipped++
			report = append(report, "- Skipped `"+mf.Name+"` — pinned.")
			if !run.DryRun {
				if err := w.saveState(ctx, tenant, state, change); err != nil {
					return err
				}
			}
			continue
		}
		idleDays := now.Sub(last).Seconds() / 86400
		switch {
		case idleDays >= archiveAfterDays:
			verb := "Archived"
			if run.DryRun {
				verb = "Would archive"
			}
			report = append(report, fmt.Sprintf("- %s `%s` after %d idle days.", verb, mf.Name, int(idleDays)))
			if !run.DryRun {
				snap, err := w.snapshotOf(tenant, mf.Name, state.curatorState)
				if err != nil {
					return err
				}
				snapshots = append(snapshots, snap)
				archived, err := w.archive(tenant, mf.Name, run.ID)
				if err != nil {
					return err
				}
				snapshots[len(snapshots)-1].ArchivedPath = &archived
				archivedState := string(ImprovementResourceStateArchived)
				change.curatorState, change.archivedAt = &archivedState, &now
				if err := w.saveState(ctx, tenant, state, change); err != nil {
					return err
				}
				applied++
			}
		case idleDays >= staleAfterDays && state.curatorState == string(ImprovementResourceStateActive):
			verb := "Marked"
			if run.DryRun {
				verb = "Would mark"
			}
			report = append(report, fmt.Sprintf("- %s `%s` stale after %d idle days.", verb, mf.Name, int(idleDays)))
			if !run.DryRun {
				snap, err := w.snapshotOf(tenant, mf.Name, state.curatorState)
				if err != nil {
					return err
				}
				snapshots = append(snapshots, snap)
				stale := string(ImprovementResourceStateStale)
				change.curatorState = &stale
				if err := w.saveState(ctx, tenant, state, change); err != nil {
					return err
				}
				applied++
			}
		case !run.DryRun:
			if err := w.saveState(ctx, tenant, state, change); err != nil {
				return err
			}
		}
	}

	var unpinned []skills.Manifest
	for _, mf := range vaultManifests(w.m.catalog, tenant) {
		if manifestKind(mf) != ImprovementResourceKindSkill {
			continue
		}
		state, err := w.state(ctx, tenant, mf.Name)
		if err != nil {
			return err
		}
		if !state.pinned && state.curatorState != string(ImprovementResourceStateArchived) {
			unpinned = append(unpinned, mf)
		}
	}
	if settings.Consolidate && len(unpinned) >= 2 {
		candidates := unpinned[:min(len(unpinned), consolidateMax)]
		result, err := w.consolidate(ctx, tenant, candidates, settings, run.DryRun, run.ID, &snapshots)
		if err != nil {
			return err
		}
		out.modelUsed = &result.model
		applied += result.applied
		skipped += result.skipped
		report = append(report, result.report...)
	} else {
		report = append(report, "- Consolidation skipped — fewer than two eligible skills or disabled.")
	}

	out.applied, out.skipped = intPtr(applied), intPtr(skipped)
	joined := strings.Join(report, "\n")
	out.report = &joined
	if !run.DryRun && len(snapshots) > 0 {
		encoded, err := wire.MarshalFoundation(snapshots)
		if err != nil {
			return err
		}
		s := string(encoded)
		out.snapshotJSON = &s
		if err := w.pruneSnapshots(ctx, tenant, int(settings.BackupKeep)); err != nil {
			return err
		}
	}
	if !run.DryRun {
		if _, err := w.m.pool.Exec(ctx, `UPDATE self_improvement_settings
			SET last_curator_review_at = $2, lease_until = NULL, updated_at = NOW() WHERE id = $1`, settings.ID, now); err != nil {
			return err
		}
		w.writeReportBestEffort(ctx, joined, tenant, run.ID)
	}
	return nil
}

type consolidation struct {
	model            string
	applied, skipped int
	report           []string
}

// consolidationAction is ConsolidationEnvelope.Action.
type consolidationAction struct {
	Action              string
	Name                string
	Target              *string
	Reason              string
	ReplacementMarkdown *string
}

// decodeEnvelope is JSONDecoder on a Decodable with the given required keys: keys
// match exactly (Go's are case-insensitive), required ones must be present and
// typed, optional ones may be null.
func decodeObject(raw json.RawMessage, required, optional map[string]any) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for key, target := range required {
		v, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, target) != nil {
			return false
		}
	}
	for key, target := range optional {
		if v, ok := fields[key]; ok && json.Unmarshal(v, target) != nil {
			return false
		}
	}
	return true
}

var errInvalidReview = &httpError{http.StatusBadGateway, "review_output_invalid_json"}

func decodeConsolidation(content string) ([]consolidationAction, error) {
	raw, err := selfimprove.DecodeJSON[json.RawMessage](content)
	if err != nil {
		return nil, err
	}
	var list []json.RawMessage
	if !decodeObject(raw, map[string]any{"actions": &list}, nil) {
		return nil, errInvalidReview
	}
	actions := make([]consolidationAction, 0, len(list))
	for _, item := range list {
		var a consolidationAction
		if !decodeObject(item, map[string]any{"action": &a.Action, "name": &a.Name, "reason": &a.Reason},
			map[string]any{"target": &a.Target, "replacement_markdown": &a.ReplacementMarkdown}) {
			return nil, errInvalidReview
		}
		actions = append(actions, a)
	}
	return actions, nil
}

func (w *Worker) reviewModel(settings db.SelfImprovementSetting) (string, error) {
	model := w.d.MainModel
	if settings.ModelMode == string(Economy) {
		model = w.m.economy
	}
	if model == "" {
		return "", &httpError{http.StatusServiceUnavailable, "economy_model_unavailable"}
	}
	return model, nil
}

func capitalized(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func isSubset(sub, of []string) bool {
	for _, s := range sub {
		if !slices.Contains(of, s) {
			return false
		}
	}
	return true
}

func (w *Worker) consolidate(ctx context.Context, tenant uuid.UUID, candidates []skills.Manifest, settings db.SelfImprovementSetting,
	dryRun bool, runID uuid.UUID, snapshots *[]snapshot,
) (consolidation, error) {
	var documents []string
	for _, mf := range candidates {
		data, err := os.ReadFile(w.m.skillFile(tenant, mf.Name))
		if err != nil {
			continue
		}
		text, ok := vaultfiles.DecodeUTF8(data)
		if !ok {
			continue
		}
		documents = append(documents, "<skill name=\""+mf.Name+"\">\n"+swiftstr.Prefix(text, skillDocumentChars)+"\n</skill>")
	}
	model, err := w.reviewModel(settings)
	if err != nil {
		return consolidation{}, err
	}
	prompt := `Review these LuminaVault skills conservatively. Find only clear duplication or drift.
Return JSON only: {"actions":[{"action":"keep|patch|merge","name":"skill","target":"optional-target","reason":"short","replacement_markdown":"full SKILL.md or null"}]}.
Never change schedules, add tools, edit jobs, or invent capabilities. Prefer keep. A merge replaces target and archives name.

` + swiftstr.Prefix(strings.Join(documents, "\n\n"), promptDocumentsChars)
	temperature := reviewTemperature
	responseModel, message, err := w.d.LLM.Chat(ctx, ids.Upper(tenant), "", model, prompt, &temperature)
	if err != nil {
		return consolidation{}, err
	}
	actions, err := decodeConsolidation(message.Content)
	if err != nil {
		return consolidation{}, err
	}
	byName := map[string]skills.Manifest{}
	for _, mf := range candidates {
		byName[mf.Name] = mf
	}
	result := consolidation{model: responseModel, report: []string{"", "## Consolidation", ""}}
	for _, action := range actions[:min(len(actions), consolidateActions)] {
		if action.Action != "keep" && action.Action != "patch" && action.Action != "merge" {
			result.skipped++
			result.report = append(result.report, "- Rejected unknown action for `"+action.Name+"`.")
			continue
		}
		if action.Action == "keep" {
			result.report = append(result.report, "- Kept `"+action.Name+"`: "+action.Reason)
			continue
		}
		original, ok := byName[action.Name]
		if !ok || action.ReplacementMarkdown == nil {
			result.skipped++
			result.report = append(result.report, "- Skipped invalid action for `"+action.Name+"`.")
			continue
		}
		targetName := &action.Name
		if action.Action == "merge" {
			targetName = action.Target
		}
		unsafe := func() bool {
			if targetName == nil {
				return true
			}
			target, ok := byName[*targetName]
			if !ok || target.Schedule != nil {
				return true
			}
			parsed, err := skills.Parse(skills.SourceVault, *action.ReplacementMarkdown)
			if err != nil || parsed.Name != *targetName || parsed.Schedule != nil {
				return true
			}
			return !isSubset(parsed.AllowedTools, append(slices.Clone(original.AllowedTools), target.AllowedTools...))
		}
		if unsafe() {
			result.skipped++
			result.report = append(result.report, "- Rejected unsafe `"+action.Action+"` for `"+action.Name+"`.")
			continue
		}
		target := byName[*targetName]
		verb := capitalized(action.Action)
		if dryRun {
			verb = "Would " + action.Action
		}
		into := ""
		if *targetName != action.Name {
			into = " into `" + *targetName + "`"
		}
		result.report = append(result.report, "- "+verb+" `"+action.Name+"`"+into+": "+action.Reason)
		if dryRun {
			continue
		}
		targetState, err := w.state(ctx, tenant, *targetName)
		if err != nil {
			return consolidation{}, err
		}
		if targetState.pinned {
			result.skipped++
			continue
		}
		snap, err := w.snapshotOf(tenant, target.Name, targetState.curatorState)
		if err != nil {
			return consolidation{}, err
		}
		*snapshots = append(*snapshots, snap)
		if err := writeAtomic(w.m.skillFile(tenant, *targetName), []byte(*action.ReplacementMarkdown)); err != nil {
			return consolidation{}, err
		}
		sha := selfimprove.SHA256(*action.ReplacementMarkdown)
		(*snapshots)[len(*snapshots)-1].AfterSHA = &sha
		if action.Action == "merge" && action.Name != *targetName {
			sourceState, err := w.state(ctx, tenant, action.Name)
			if err != nil {
				return consolidation{}, err
			}
			if !sourceState.pinned {
				snap, err := w.snapshotOf(tenant, original.Name, sourceState.curatorState)
				if err != nil {
					return consolidation{}, err
				}
				*snapshots = append(*snapshots, snap)
				archived, err := w.archive(tenant, action.Name, runID)
				if err != nil {
					return consolidation{}, err
				}
				(*snapshots)[len(*snapshots)-1].ArchivedPath = &archived
				archivedState, at := string(ImprovementResourceStateArchived), w.m.now()
				if err := w.saveState(ctx, tenant, sourceState, stateChange{curatorState: &archivedState, archivedAt: &at}); err != nil {
					return consolidation{}, err
				}
			}
		}
		result.applied++
	}
	return result, nil
}

// pruneSnapshots keeps the snapshot of the newest max(1, keep) backed-up live
// curator runs (the current one isn't saved yet, so it isn't counted).
func (w *Worker) pruneSnapshots(ctx context.Context, tenant uuid.UUID, keep int) error {
	rows, err := w.m.pool.Query(ctx, `SELECT id FROM self_improvement_runs
		WHERE tenant_id = $1 AND dry_run = FALSE AND kind = 'curator' AND snapshot_json IS NOT NULL
		ORDER BY created_at DESC`, tenant)
	if err != nil {
		return err
	}
	var backedUp []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		backedUp = append(backedUp, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range backedUp[min(len(backedUp), max(1, keep)):] {
		if _, err := w.m.pool.Exec(ctx, `UPDATE self_improvement_runs SET snapshot_json = NULL WHERE id = $1`, id); err != nil {
			return err
		}
	}
	return nil
}

// writeReportBestEffort mirrors the report to reports/self-improvement/<RUN>/REPORT.md.
// The database copy is authoritative, so a failure is only logged.
func (w *Worker) writeReportBestEffort(ctx context.Context, markdown string, tenant, run uuid.UUID) {
	dir := filepath.Join(w.m.paths.TenantRoot(tenant), "reports", "self-improvement", ids.Upper(run))
	err := os.MkdirAll(dir, 0o755) //nolint:gosec // Foundation's createDirectory default, as Swift creates it
	if err == nil {
		err = writeAtomic(filepath.Join(dir, "REPORT.md"), []byte(markdown))
	}
	if err != nil {
		w.log.WarnContext(ctx, fmt.Sprintf("self_improvement.report_mirror_failed run=%s error=%v", ids.Upper(run), err))
	}
}

// writeAtomic is Data.write(to:options: .atomic): a temp file renamed into place.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // Data.write's 0644, which Hermes and the routes read
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// MARK: - SOUL reviewer

func (w *Worker) processSoulReview(ctx context.Context, run db.SelfImprovementRun, out *runOutcome) error {
	tenant := run.TenantID
	settings, err := w.m.settings(ctx, tenant)
	if err != nil {
		return err
	}
	if !w.m.enabled || !settings.Enabled || !settings.SoulReviewEnabled {
		return &httpError{http.StatusServiceUnavailable, "soul_review_disabled"}
	}
	var username string
	err = w.m.pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, tenant).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		return &httpError{http.StatusNotFound, "user_not_found"}
	}
	if err != nil {
		return err
	}
	if w.m.isUserOverride(ctx, tenant) {
		return &httpError{http.StatusConflict, "soul_review_unsupported_on_byo_hermes"}
	}
	current := w.m.soul.Read(tenant, username, w.m.now())
	history, err := w.recentConversationText(ctx, tenant, int(settings.SoulReviewWindowDays))
	if err != nil {
		return err
	}
	model, err := w.reviewModel(settings)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf(`Review the last %d days of sessions and the current SOUL.md. Identify drift in mission, voice, priorities, or constraints. Propose a precise, minimal patch only if needed. Be conservative. Never remove or weaken locked safety constraints.
Return JSON only: {"needed":true|false,"summary":"short reason","proposed_markdown":"complete SOUL.md or null"}.

<current_soul>
%s
</current_soul>
<recent_sessions>
%s
</recent_sessions>`, settings.SoulReviewWindowDays, current, swiftstr.Prefix(history, historyChars))
	temperature := reviewTemperature
	responseModel, message, err := w.d.LLM.Chat(ctx, ids.Upper(tenant), "", model, prompt, &temperature)
	if err != nil {
		return err
	}
	raw, err := selfimprove.DecodeJSON[json.RawMessage](message.Content)
	if err != nil {
		return err
	}
	var needed bool
	var summary string
	var proposed *string
	if !decodeObject(raw, map[string]any{"needed": &needed, "summary": &summary}, map[string]any{"proposed_markdown": &proposed}) {
		return errInvalidReview
	}
	out.modelUsed = &responseModel
	report := []string{"# SOUL Review", "", summary}
	if needed && proposed != nil {
		enforced := soul.Inject(*proposed)
		if len(enforced) > soul.MaxSizeBytes || !selfimprove.IsConservativeChange(current, enforced) {
			return &httpError{http.StatusUnprocessableEntity, "soul_review_patch_too_broad"}
		}
		joined := strings.Join(report, "\n")
		if _, err := w.m.pool.Exec(ctx, `INSERT INTO self_improvement_changes
			(id, tenant_id, run_id, kind, state, trigger, title, summary, patch, proposed_markdown, base_sha256, report_markdown, created_at)
			VALUES ($1, $2, $3, 'soul', 'pending', $4, 'SOUL.md review', $5, $6, $7, $8, $9, $10)`,
			uuid.New(), tenant, run.ID, run.Trigger, summary, simpleDiff(current, enforced), enforced,
			selfimprove.SHA256(current), joined, w.m.now()); err != nil {
			return err
		}
		report = append(report, "\nA conservative patch is waiting for approval.")
	} else {
		report = append(report, "\nNo SOUL.md change is needed.")
	}
	joined := strings.Join(report, "\n")
	out.report = &joined
	if _, err := w.m.pool.Exec(ctx, `UPDATE self_improvement_settings
		SET last_soul_review_at = $2, lease_until = NULL, updated_at = NOW() WHERE id = $1`, settings.ID, w.m.now()); err != nil {
		return err
	}
	w.writeReportBestEffort(ctx, joined, tenant, run.ID)
	return nil
}

func (w *Worker) recentConversationText(ctx context.Context, tenant uuid.UUID, days int) (string, error) {
	since := w.m.now().Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := w.m.pool.Query(ctx, `
		SELECT m.role, m.content, m.created_at
		FROM conversation_messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE c.tenant_id = $1 AND m.created_at >= $2
		ORDER BY m.created_at ASC
		LIMIT 500`, tenant, since)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var role, content string
		var at time.Time
		if err := rows.Scan(&role, &content, &at); err != nil {
			return "", err
		}
		lines = append(lines, "["+at.UTC().Format("2006-01-02T15:04:05Z")+"] "+role+": "+content)
	}
	return strings.Join(lines, "\n"), rows.Err()
}

// simpleDiff lists removed then added lines, by exact membership.
func simpleDiff(old, proposed string) string {
	oldLines, newLines := strings.Split(old, "\n"), strings.Split(proposed, "\n")
	lines := []string{"--- SOUL.md", "+++ SOUL.md (proposed)"}
	for _, l := range oldLines {
		if !slices.Contains(newLines, l) {
			lines = append(lines, "-"+l)
		}
	}
	for _, l := range newLines {
		if !slices.Contains(oldLines, l) {
			lines = append(lines, "+"+l)
		}
	}
	return strings.Join(lines, "\n")
}
