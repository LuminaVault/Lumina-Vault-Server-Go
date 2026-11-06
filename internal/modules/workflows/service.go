package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/entitlement"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/workflows/db"
	"lumina/internal/sse"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	tierPro      = "pro"
	tierUltimate = "ultimate"
	tierLapsed   = "lapsed"
	tierFree     = "free"
	tierArchived = "archived"

	statusPending = "pending"

	managedUnavailablePrefix = "Managed provider unavailable"
	managedUnavailableScrub  = "Managed provider unavailable; retrying on a backup route."
)

// WorkflowServiceError. Routes without mapErrors in Swift let these through as
// plain errors, which render as a bare 500.
var (
	errNotFound         = errors.New("notFound")
	errRevisionConflict = errors.New("revisionConflict")
	errUnpublished      = errors.New("unpublished")
	errForbidden        = errors.New("forbidden")
	errActiveRunLimit   = errors.New("activeRunLimit")
	errNotRetryable     = invalidError{"run_not_retryable"}
	errBadAttachment    = invalidError{"invalid_memory_attachment"}
)

// invalidError is WorkflowServiceError.invalid(reason).
type invalidError struct{ reason string }

func (e invalidError) Error() string { return e.reason }

// mapErrors is WorkflowController.mapErrors. Anything else passes through.
func mapErrors(err error) error {
	switch {
	case errors.Is(err, errNotFound):
		return wire.NewError(http.StatusNotFound, "workflow_not_found")
	case errors.Is(err, errRevisionConflict):
		return wire.NewError(http.StatusConflict, "workflow_revision_conflict")
	case errors.Is(err, errUnpublished):
		return wire.NewError(http.StatusConflict, "workflow_not_published")
	}
	return mapSharedErrors(err)
}

// mapTemplateErrors is WorkflowTemplateController.mapErrors: it has its own 404, and
// no case for a revision conflict or an unpublished workflow.
func mapTemplateErrors(err error) error {
	if errors.Is(err, errNotFound) {
		return wire.NewError(http.StatusNotFound, "workflow_template_not_found")
	}
	return mapSharedErrors(err)
}

func mapSharedErrors(err error) error {
	var invalid invalidError
	switch {
	case errors.Is(err, errForbidden):
		return wire.NewError(http.StatusForbidden, "workflow_access_denied")
	case errors.Is(err, errActiveRunLimit):
		return wire.NewError(http.StatusTooManyRequests, "workflow_active_run_limit")
	case errors.As(err, &invalid):
		return wire.NewError(http.StatusBadRequest, "invalid_workflow:"+invalid.reason)
	}
	return err
}

// MARK: - Access

// effectiveTier is WorkflowService.effectiveTier.
func (m *Module) effectiveTier(ctx context.Context, tenant uuid.UUID) (string, error) {
	row, err := m.q.UserTier(ctx, tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound
	}
	if err != nil {
		return "", err
	}
	return entitlement.EffectiveTier(row.Tier, row.TierOverride), nil
}

func (m *Module) requireAuthorTier(ctx context.Context, tenant uuid.UUID) (string, error) {
	tier, err := m.effectiveTier(ctx, tenant)
	if err != nil {
		return "", err
	}
	if tier != tierPro && tier != tierUltimate {
		return "", errForbidden
	}
	return tier, nil
}

// requireReadAccess lets lapsed and free see what they have. trial and archived
// are refused, which is Swift's list as written.
func (m *Module) requireReadAccess(ctx context.Context, tenant uuid.UUID) error {
	tier, err := m.effectiveTier(ctx, tenant)
	if err != nil {
		return err
	}
	switch tier {
	case tierPro, tierUltimate, tierLapsed, tierFree:
		return nil
	}
	return errForbidden
}

func (m *Module) requireWorkflow(ctx context.Context, tenant, id uuid.UUID) (db.Workflow, error) {
	row, err := m.q.GetWorkflow(ctx, db.GetWorkflowParams{TenantID: tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errNotFound
	}
	return row, err
}

// validateForTier is WorkflowService.validate: every failure is .invalid.
func (m *Module) validateForTier(def WorkflowDefinitionDTO, tier string) error {
	err := validateDefinition(def)
	if err == nil {
		err = validateSchedule(def, policyFor(tier).minimumScheduleMinutes, m.now())
	}
	if err != nil {
		return invalidError{err.Error()}
	}
	return nil
}

// MARK: - Workflows

func (m *Module) listWorkflows(ctx context.Context, tenant uuid.UUID) (WorkflowListResponse, error) {
	out := WorkflowListResponse{Workflows: []WorkflowSummaryDTO{}}
	if err := m.requireReadAccess(ctx, tenant); err != nil {
		return out, err
	}
	rows, err := m.q.ListWorkflows(ctx, tenant)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		summary, _, err := m.summarize(ctx, row)
		if err != nil {
			return out, err
		}
		out.Workflows = append(out.Workflows, summary)
	}
	return out, nil
}

func (m *Module) workflowDetail(ctx context.Context, tenant, id uuid.UUID) (WorkflowDetailDTO, error) {
	if err := m.requireReadAccess(ctx, tenant); err != nil {
		return WorkflowDetailDTO{}, err
	}
	row, err := m.requireWorkflow(ctx, tenant, id)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	return m.detailOf(ctx, row)
}

func (m *Module) detailOf(ctx context.Context, row db.Workflow) (WorkflowDetailDTO, error) {
	summary, def, err := m.summarize(ctx, row)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	return WorkflowDetailDTO{Workflow: summary, Definition: def}, nil
}

// summarize is WorkflowService.summary. Decoding the definition here is why one bad
// stored row fails the whole list, as Fluent's decode does.
func (m *Module) summarize(ctx context.Context, row db.Workflow) (WorkflowSummaryDTO, WorkflowDefinitionDTO, error) {
	def, err := decodeDefinition(row.DraftDefinition)
	if err != nil {
		return WorkflowSummaryDTO{}, def, err
	}
	out := WorkflowSummaryDTO{
		Id: wire.UUID{UUID: row.ID}, Name: row.Name, DescriptionText: row.DescriptionText, Enabled: row.Enabled,
		Trigger: def.Trigger, DraftRevision: int(row.DraftRevision), IsLegacyJob: row.IsLegacyJob,
		CreatedAt: wire.Time{Time: row.CreatedAt}, UpdatedAt: wire.Time{Time: row.UpdatedAt},
	}
	if row.PublishedVersionID != nil {
		version, err := m.q.VersionNumberByID(ctx, *row.PublishedVersionID)
		switch {
		case err == nil:
			number := int(version)
			out.PublishedVersion = &number
		case !errors.Is(err, pgx.ErrNoRows):
			return out, def, err
		}
	}
	last, err := m.q.LatestRun(ctx, db.LatestRunParams{TenantID: row.TenantID, WorkflowID: row.ID})
	switch {
	case err == nil:
		if status, ok := knownRunStatus(last.Status); ok {
			out.LastRunStatus = &status
		}
		at := last.CreatedAt
		if last.StartedAt != nil {
			at = *last.StartedAt
		}
		out.LastRunAt = &wire.Time{Time: at}
	case !errors.Is(err, pgx.ErrNoRows):
		return out, def, err
	}
	pending, err := m.q.CountPendingApprovals(ctx, db.CountPendingApprovalsParams{TenantID: row.TenantID, WorkflowID: row.ID})
	if err != nil {
		return out, def, err
	}
	out.PendingApprovalCount = int(pending)
	return out, def, nil
}

// createWorkflow is WorkflowService.create. The name is trimmed and may end up
// empty; a duplicate of (tenant_id, name) is a constraint failure, so a bare 500.
func (m *Module) createWorkflow(ctx context.Context, tenant uuid.UUID, name string, description *string, def WorkflowDefinitionDTO) (WorkflowDetailDTO, error) {
	tier, err := m.requireAuthorTier(ctx, tenant)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	if err := m.validateForTier(def, tier); err != nil {
		return WorkflowDetailDTO{}, err
	}
	encoded, err := json.Marshal(def)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	now := m.now()
	row := db.Workflow{
		ID: uuid.New(), TenantID: tenant, Name: swiftstr.TrimWhitespacesAndNewlines(name), DescriptionText: description,
		Enabled: true, DraftDefinition: encoded, DraftRevision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := m.q.InsertWorkflow(ctx, db.InsertWorkflowParams{
		ID: row.ID, TenantID: tenant, Name: row.Name, DescriptionText: description, DraftDefinition: encoded, Now: now,
	}); err != nil {
		return WorkflowDetailDTO{}, err
	}
	return m.detailOf(ctx, row)
}

func (m *Module) updateDraft(ctx context.Context, tenant, id uuid.UUID, body draftUpdateRequest) (WorkflowDetailDTO, error) {
	tier, err := m.requireAuthorTier(ctx, tenant)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	def := body.Definition.definition()
	if err := m.validateForTier(def, tier); err != nil {
		return WorkflowDetailDTO{}, err
	}
	row, err := m.requireWorkflow(ctx, tenant, id)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	if int(row.DraftRevision) != body.ExpectedRevision {
		return WorkflowDetailDTO{}, errRevisionConflict
	}
	if body.Name != nil {
		row.Name = swiftstr.TrimWhitespacesAndNewlines(*body.Name)
	}
	if body.DescriptionText != nil {
		row.DescriptionText = body.DescriptionText
	}
	if body.Enabled != nil {
		row.Enabled = *body.Enabled
	}
	if row.DraftDefinition, err = json.Marshal(def); err != nil {
		return WorkflowDetailDTO{}, err
	}
	row.DraftRevision++
	row.UpdatedAt = m.now()
	if err := m.q.UpdateDraft(ctx, db.UpdateDraftParams{
		ID: id, Name: row.Name, DescriptionText: row.DescriptionText, Enabled: row.Enabled,
		DraftDefinition: row.DraftDefinition, DraftRevision: row.DraftRevision, UpdatedAt: row.UpdatedAt,
	}); err != nil {
		return WorkflowDetailDTO{}, err
	}
	return m.detailOf(ctx, row)
}

// publishWorkflow is WorkflowService.publish: no transaction, as in Swift.
func (m *Module) publishWorkflow(ctx context.Context, tenant, id uuid.UUID) (WorkflowDetailDTO, error) {
	tier, err := m.requireAuthorTier(ctx, tenant)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	row, err := m.requireWorkflow(ctx, tenant, id)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	def, err := decodeDefinition(row.DraftDefinition)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	if err := m.validateForTier(def, tier); err != nil {
		return WorkflowDetailDTO{}, err
	}
	latest, err := m.q.LatestVersionNumber(ctx, db.LatestVersionNumberParams{TenantID: tenant, WorkflowID: id})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WorkflowDetailDTO{}, err
	}
	now := m.now()
	versionID := uuid.New()
	if err := m.q.InsertVersion(ctx, db.InsertVersionParams{
		ID: versionID, TenantID: tenant, WorkflowID: id, Version: latest + 1, Definition: row.DraftDefinition, CreatedAt: now,
	}); err != nil {
		return WorkflowDetailDTO{}, err
	}
	if err := m.q.SetPublishedVersion(ctx, db.SetPublishedVersionParams{ID: id, VersionID: &versionID, UpdatedAt: now}); err != nil {
		return WorkflowDetailDTO{}, err
	}
	row.PublishedVersionID = &versionID
	row.UpdatedAt = now
	return m.detailOf(ctx, row)
}

func (m *Module) listVersions(ctx context.Context, tenant, id uuid.UUID) (WorkflowVersionsResponse, error) {
	out := WorkflowVersionsResponse{Versions: []WorkflowVersionDTO{}}
	if err := m.requireReadAccess(ctx, tenant); err != nil {
		return out, err
	}
	if _, err := m.requireWorkflow(ctx, tenant, id); err != nil {
		return out, err
	}
	rows, err := m.q.ListVersions(ctx, db.ListVersionsParams{TenantID: tenant, WorkflowID: id})
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		def, err := decodeDefinition(row.Definition)
		if err != nil {
			return out, err
		}
		out.Versions = append(out.Versions, WorkflowVersionDTO{
			Id: wire.UUID{UUID: row.ID}, WorkflowID: wire.UUID{UUID: row.WorkflowID}, Version: int(row.Version),
			Definition: def, CreatedAt: wire.Time{Time: row.CreatedAt},
		})
	}
	return out, nil
}

func (m *Module) restoreVersion(ctx context.Context, tenant, id uuid.UUID, version int) (WorkflowDetailDTO, error) {
	if _, err := m.requireAuthorTier(ctx, tenant); err != nil {
		return WorkflowDetailDTO{}, err
	}
	row, err := m.requireWorkflow(ctx, tenant, id)
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	snapshot, err := m.q.GetVersion(ctx, db.GetVersionParams{TenantID: tenant, WorkflowID: id, Version: int64(version)})
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowDetailDTO{}, errNotFound
	}
	if err != nil {
		return WorkflowDetailDTO{}, err
	}
	row.DraftDefinition = snapshot.Definition
	row.DraftRevision++
	row.UpdatedAt = m.now()
	if err := m.q.RestoreDraft(ctx, db.RestoreDraftParams{ID: id, DraftDefinition: row.DraftDefinition, DraftRevision: row.DraftRevision, UpdatedAt: row.UpdatedAt}); err != nil {
		return WorkflowDetailDTO{}, err
	}
	return m.detailOf(ctx, row)
}

// validateForTenant is WorkflowService.validateDefinition: it never fails, and even
// a tier without access is a 200 with an issue.
func (m *Module) validateForTenant(ctx context.Context, tenant uuid.UUID, def WorkflowDefinitionDTO) WorkflowValidationResponse {
	tier, err := m.requireAuthorTier(ctx, tenant)
	if err == nil {
		err = m.validateForTier(def, tier)
	}
	if err == nil {
		return WorkflowValidationResponse{Valid: true, Issues: []WorkflowValidationIssueDTO{}}
	}
	return WorkflowValidationResponse{Valid: false, Issues: []WorkflowValidationIssueDTO{{
		Id: wire.UUID{UUID: uuid.New()}, Code: validationCode(err), Message: validationMessage(err),
	}}}
}

func validationCode(err error) string {
	var invalid invalidError
	switch {
	case errors.Is(err, errForbidden):
		return "workflow_access_denied"
	case errors.As(err, &invalid):
		return invalid.reason
	case errors.Is(err, errNotFound), errors.Is(err, errRevisionConflict), errors.Is(err, errUnpublished), errors.Is(err, errActiveRunLimit):
		return "workflow_invalid"
	}
	return err.Error()
}

// validationMessage interpolates the error, so a reason appears as Swift prints
// the enum case that carries it: invalid("cycle").
func validationMessage(err error) string {
	if errors.Is(err, errForbidden) {
		return "A Pro or Ultimate plan is required to author workflows."
	}
	var invalid invalidError
	described := err.Error()
	if errors.As(err, &invalid) {
		described = `invalid("` + invalid.reason + `")`
	}
	return "The workflow cannot be published until this issue is fixed: " + described
}

// MARK: - Runs

// enqueue is WorkflowService.enqueue. Idempotency comes before admission: a retried
// delivery is not a new active run, and must still answer when the tenant is at its
// limit. Another delivery can win between that read and the insert, so the limit
// and the unique index both look again before they report anything.
func (m *Module) enqueue(ctx context.Context, tenant, workflowID uuid.UUID, trigger WorkflowTriggerKind, input StringMap, conversation *uuid.UUID, dedupeKey *string) (WorkflowRunDTO, error) {
	var out WorkflowRunDTO
	tier, err := m.requireAuthorTier(ctx, tenant)
	if err != nil {
		return out, err
	}
	workflow, err := m.requireWorkflow(ctx, tenant, workflowID)
	if err != nil {
		return out, err
	}
	if !workflow.Enabled || workflow.PublishedVersionID == nil {
		return out, errUnpublished
	}
	if existing, ok, err := m.existingRun(ctx, tenant, workflowID, dedupeKey); err != nil {
		return out, err
	} else if ok {
		return m.runDTO(ctx, existing, workflow)
	}
	policy, err := m.ensureCanEnqueue(ctx, tenant, tier)
	if errors.Is(err, errActiveRunLimit) {
		if existing, ok, err := m.existingRun(ctx, tenant, workflowID, dedupeKey); err != nil {
			return out, err
		} else if ok {
			return m.runDTO(ctx, existing, workflow)
		}
		return out, errActiveRunLimit
	}
	if err != nil {
		return out, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return out, err
	}
	now := m.now()
	run := db.WorkflowRun{
		ID: uuid.New(), TenantID: tenant, WorkflowID: workflowID, VersionID: *workflow.PublishedVersionID,
		Status: string(WorkflowRunStatusQueued), TriggerKind: string(trigger), Input: encoded, ConversationID: conversation,
		DedupeKey: dedupeKey, ManagedSpendLimitUsdMicros: policy.perRunUsdMicros, CreatedAt: now, UpdatedAt: now,
	}
	if err := m.q.InsertRun(ctx, db.InsertRunParams{
		ID: run.ID, TenantID: tenant, WorkflowID: workflowID, VersionID: run.VersionID, Status: run.Status,
		TriggerKind: run.TriggerKind, Input: encoded, ConversationID: conversation, DedupeKey: dedupeKey,
		ManagedSpendLimitUsdMicros: run.ManagedSpendLimitUsdMicros, Now: now,
	}); err != nil {
		if isConstraintFailure(err) {
			if existing, ok, lookup := m.existingRun(ctx, tenant, workflowID, dedupeKey); lookup != nil {
				return out, lookup
			} else if ok {
				return m.runDTO(ctx, existing, workflow)
			}
		}
		return out, err
	}
	m.appendEvent(ctx, tenant, run.ID, RunQueued, nil, nil)
	return m.runDTO(ctx, run, workflow)
}

func isConstraintFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23")
}

func (m *Module) existingRun(ctx context.Context, tenant, workflowID uuid.UUID, dedupeKey *string) (db.WorkflowRun, bool, error) {
	if dedupeKey == nil {
		return db.WorkflowRun{}, false, nil
	}
	row, err := m.q.RunByDedupeKey(ctx, db.RunByDedupeKeyParams{TenantID: tenant, WorkflowID: workflowID, DedupeKey: dedupeKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	return row, err == nil, err
}

func (m *Module) listRuns(ctx context.Context, tenant uuid.UUID, workflowID *uuid.UUID) (WorkflowRunsResponse, error) {
	out := WorkflowRunsResponse{Runs: []WorkflowRunDTO{}}
	if err := m.requireReadAccess(ctx, tenant); err != nil {
		return out, err
	}
	var (
		rows []db.WorkflowRun
		err  error
	)
	if workflowID != nil {
		rows, err = m.q.ListWorkflowRuns(ctx, db.ListWorkflowRunsParams{TenantID: tenant, WorkflowID: *workflowID})
	} else {
		rows, err = m.q.ListRuns(ctx, tenant)
	}
	if err != nil {
		return out, err
	}
	workflows := map[uuid.UUID]db.Workflow{}
	for _, row := range rows {
		workflow, ok := workflows[row.WorkflowID]
		if !ok {
			workflow, err = m.requireWorkflow(ctx, tenant, row.WorkflowID)
			if errors.Is(err, errNotFound) {
				continue
			}
			if err != nil {
				return out, err
			}
			workflows[row.WorkflowID] = workflow
		}
		dto, err := m.runDTO(ctx, row, workflow)
		if err != nil {
			return out, err
		}
		out.Runs = append(out.Runs, dto)
	}
	return out, nil
}

func (m *Module) runByID(ctx context.Context, tenant, id uuid.UUID) (WorkflowRunDTO, error) {
	var out WorkflowRunDTO
	if err := m.requireReadAccess(ctx, tenant); err != nil {
		return out, err
	}
	row, err := m.getRun(ctx, tenant, id)
	if err != nil {
		return out, err
	}
	workflow, err := m.requireWorkflow(ctx, tenant, row.WorkflowID)
	if err != nil {
		return out, err
	}
	return m.runDTO(ctx, row, workflow)
}

func (m *Module) getRun(ctx context.Context, tenant, id uuid.UUID) (db.WorkflowRun, error) {
	row, err := m.q.GetRun(ctx, db.GetRunParams{TenantID: tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errNotFound
	}
	return row, err
}

// cancelRun is WorkflowService.cancel: a terminal run is left as it is. paused is
// not terminal here, though isTerminal says it is for the event stream.
func (m *Module) cancelRun(ctx context.Context, tenant, id uuid.UUID) error {
	if _, err := m.requireAuthorTier(ctx, tenant); err != nil {
		return err
	}
	run, err := m.getRun(ctx, tenant, id)
	if err != nil {
		return err
	}
	switch WorkflowRunStatus(run.Status) {
	case WorkflowRunStatusSucceeded, WorkflowRunStatusFailed, WorkflowRunStatusCancelled, WorkflowRunStatusTimedOut:
		return nil
	}
	now := m.now()
	if err := m.q.CancelRun(ctx, db.CancelRunParams{ID: id, Now: &now}); err != nil {
		return err
	}
	m.appendEvent(ctx, tenant, id, RunCancelled, nil, nil)
	return m.q.CancelActiveNodeRuns(ctx, db.CancelActiveNodeRunsParams{RunID: id, Now: &now})
}

// retryRun is WorkflowService.retry: a new run on the latest published version. It
// checks the run before the tier, which enqueue does.
func (m *Module) retryRun(ctx context.Context, tenant, id uuid.UUID) (WorkflowRunDTO, error) {
	previous, err := m.getRun(ctx, tenant, id)
	if err != nil {
		return WorkflowRunDTO{}, err
	}
	switch WorkflowRunStatus(previous.Status) {
	case WorkflowRunStatusFailed, WorkflowRunStatusTimedOut, WorkflowRunStatusCancelled, WorkflowRunStatusPaused:
	default:
		return WorkflowRunDTO{}, errNotRetryable
	}
	input, err := decodeStrings(previous.Input)
	if err != nil {
		return WorkflowRunDTO{}, err
	}
	return m.enqueue(ctx, tenant, previous.WorkflowID, triggerOf(previous.TriggerKind), input, previous.ConversationID, nil)
}

func (m *Module) resumeRun(ctx context.Context, tenant, id uuid.UUID) (WorkflowRunDTO, error) {
	var out WorkflowRunDTO
	tier, err := m.requireAuthorTier(ctx, tenant)
	if err != nil {
		return out, err
	}
	row, err := m.getRun(ctx, tenant, id)
	if err != nil {
		return out, err
	}
	if WorkflowRunStatus(row.Status) != WorkflowRunStatusPaused {
		return out, errNotFound
	}
	workflow, err := m.requireWorkflow(ctx, tenant, row.WorkflowID)
	if err != nil {
		return out, err
	}
	if _, err := m.ensureCanEnqueue(ctx, tenant, tier); err != nil {
		return out, err
	}
	now := m.now()
	if err := m.q.ResumeRun(ctx, db.ResumeRunParams{ID: id, Now: now}); err != nil {
		return out, err
	}
	message := "Run resumed"
	m.appendEvent(ctx, tenant, id, RunQueued, nil, &message)
	row.Status = string(WorkflowRunStatusQueued)
	row.PauseReason, row.ErrorMessage, row.LeaseOwner, row.LeaseExpiresAt = nil, nil, nil, nil
	return m.runDTO(ctx, row, workflow)
}

// runDTO is WorkflowService.runDTO, scrubbed for the tenant.
func (m *Module) runDTO(ctx context.Context, row db.WorkflowRun, workflow db.Workflow) (WorkflowRunDTO, error) {
	var out WorkflowRunDTO
	version, err := m.q.VersionNumberByID(ctx, row.VersionID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	nodes, err := m.q.NodeRuns(ctx, row.ID)
	if err != nil {
		return out, err
	}
	hidden, err := m.modelHidden(ctx, workflow.TenantID)
	if err != nil {
		return out, err
	}
	status, ok := knownRunStatus(row.Status)
	if !ok {
		status = WorkflowRunStatusFailed
	}
	out = WorkflowRunDTO{
		Id: wire.UUID{UUID: row.ID}, WorkflowID: wire.UUID{UUID: row.WorkflowID}, WorkflowName: workflow.Name,
		Version: int(version), Status: status, Trigger: triggerOf(row.TriggerKind),
		StartedAt: wireTime(row.StartedAt), EndedAt: wireTime(row.EndedAt), CreatedAt: wire.Time{Time: row.CreatedAt},
		Error: row.ErrorMessage, PauseReason: pauseReasonOf(row.PauseReason),
		ManagedSpendUsdMicros: &row.ManagedSpendUsdMicros, ManagedSpendLimitUsdMicros: &row.ManagedSpendLimitUsdMicros,
		NodeRuns: make([]WorkflowNodeRunDTO, 0, len(nodes)),
	}
	for _, node := range nodes {
		dto, err := nodeRunDTO(node, hidden)
		if err != nil {
			return out, err
		}
		out.NodeRuns = append(out.NodeRuns, dto)
	}
	return out, nil
}

func nodeRunDTO(node db.WorkflowNodeRun, hidden bool) (WorkflowNodeRunDTO, error) {
	status := WorkflowNodeRunStatus(node.Status)
	switch status {
	case WorkflowNodeRunStatusPending, WorkflowNodeRunStatusRunning, WorkflowNodeRunStatusWaitingForApproval,
		WorkflowNodeRunStatusSucceeded, WorkflowNodeRunStatusFailed, WorkflowNodeRunStatusSkipped, WorkflowNodeRunStatusCancelled:
	default:
		status = WorkflowNodeRunStatusFailed
	}
	out := WorkflowNodeRunDTO{
		Id: wire.UUID{UUID: node.ID}, NodeID: wire.UUID{UUID: node.NodeID}, NodeName: node.NodeName, Status: status,
		Attempt: int(node.Attempt), StartedAt: wireTime(node.StartedAt), EndedAt: wireTime(node.EndedAt), Error: node.ErrorMessage,
		TokensIn: intOf(node.TokensIn), TokensOut: intOf(node.TokensOut), ManagedCostUsdMicros: node.ManagedCostUsdMicros,
	}
	if node.OutputSnapshot != nil {
		snapshot, err := decodeStrings(node.OutputSnapshot)
		if err != nil {
			return out, err
		}
		if text, ok := snapshot["text"]; ok {
			out.OutputPreview = &text
		}
	}
	// ModelDisclosurePolicy.scrub: managed tenants never learn the model.
	if !hidden {
		out.Provider, out.Model = providerOf(node.SelectedProvider), node.SelectedModel
	}
	return out, nil
}

// modelHidden is WorkflowService.disclosure: hidden unless the tenant chose BYOK.
func (m *Module) modelHidden(ctx context.Context, tenant uuid.UUID) (bool, error) {
	if m.opts.Preferences == nil {
		return true, nil
	}
	preference, err := m.opts.Preferences.Get(ctx, tenant)
	if err != nil {
		return true, err
	}
	return preference == nil || preference.Mode != llm.ModeBYOK, nil
}

func knownRunStatus(raw string) (WorkflowRunStatus, bool) {
	status := WorkflowRunStatus(raw)
	switch status {
	case WorkflowRunStatusQueued, WorkflowRunStatusRunning, WorkflowRunStatusWaitingForApproval, WorkflowRunStatusPaused,
		WorkflowRunStatusSucceeded, WorkflowRunStatusFailed, WorkflowRunStatusCancelled, WorkflowRunStatusTimedOut:
		return status, true
	}
	return "", false
}

// triggerOf is WorkflowTriggerKind(rawValue:) ?? .manual.
func triggerOf(raw string) WorkflowTriggerKind {
	switch kind := WorkflowTriggerKind(raw); kind {
	case Manual, Chat, Schedule, Webhook:
		return kind
	}
	return Manual
}

func pauseReasonOf(raw *string) *WorkflowPauseReason {
	if raw == nil {
		return nil
	}
	reason := WorkflowPauseReason(*raw)
	switch reason {
	case RunSpendLimit, DailySpendLimit, MonthlySpendLimit, GlobalSpendLimit, ProviderUnavailable:
		return &reason
	}
	return nil
}

// providerOf is ProviderID(rawValue:): a provider Swift doesn't know is left out.
func providerOf(raw *string) *string {
	if raw == nil {
		return nil
	}
	switch ProviderID(*raw) {
	case Xai, Anthropic, Openai, Ollama, OpenRouter, Nvidia, Gemini, Nous, Custom:
		return raw
	}
	return nil
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func intOf(v *int64) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func decodeStrings(raw []byte) (StringMap, error) {
	out := StringMap{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MARK: - Approvals

func (m *Module) pendingApprovals(ctx context.Context, tenant uuid.UUID) (WorkflowApprovalsResponse, error) {
	out := WorkflowApprovalsResponse{Approvals: []WorkflowApprovalDTO{}}
	if err := m.requireReadAccess(ctx, tenant); err != nil {
		return out, err
	}
	rows, err := m.q.PendingApprovals(ctx, tenant)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		workflow, err := m.requireWorkflow(ctx, tenant, row.WorkflowID)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		out.Approvals = append(out.Approvals, WorkflowApprovalDTO{
			Id: wire.UUID{UUID: row.ID}, RunID: wire.UUID{UUID: row.RunID}, WorkflowID: wire.UUID{UUID: row.WorkflowID},
			WorkflowName: workflow.Name, NodeID: wire.UUID{UUID: row.NodeID}, Title: row.Title, Message: row.Message,
			ExpiresAt: wire.Time{Time: row.ExpiresAt}, CreatedAt: wire.Time{Time: row.CreatedAt},
		})
	}
	return out, nil
}

// decideApproval is WorkflowService.decide, a step at a time with no transaction: a
// bad memory attachment answers 400 with the approval already decided, and the
// node run and the run untouched, as in Swift. An expired approval that's still
// pending can be decided.
func (m *Module) decideApproval(ctx context.Context, tenant, id uuid.UUID, body decisionRequest) error {
	if _, err := m.requireAuthorTier(ctx, tenant); err != nil {
		return err
	}
	approval, err := m.q.GetApproval(ctx, db.GetApprovalParams{TenantID: tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if approval.Status != statusPending {
		return nil
	}
	memoryIDs := make([]uuid.UUID, len(body.MemoryIDs))
	for i, memoryID := range body.MemoryIDs {
		memoryIDs[i] = memoryID.UUID
	}
	now := m.now()
	decided := "rejected"
	if body.Approved {
		decided = "approved"
	}
	if err := m.q.DecideApproval(ctx, db.DecideApprovalParams{ID: id, Status: decided, DecisionNote: body.Note, MemoryIds: memoryIDs, UpdatedAt: now}); err != nil {
		return err
	}
	if _, err := m.getRun(ctx, tenant, approval.RunID); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return err
	}
	if err := m.decideNodeRun(ctx, tenant, approval, body, memoryIDs, now); err != nil {
		return err
	}
	params := db.DecideRunParams{ID: approval.RunID, Status: string(WorkflowRunStatusQueued), UpdatedAt: now}
	kind, message := NodeCompleted, "Approval granted"
	if !body.Approved {
		rejected := "Approval rejected"
		params.Status, params.ErrorMessage, params.EndedAt = string(WorkflowRunStatusFailed), &rejected, &now
		kind, message = RunFailed, rejected
	}
	if err := m.q.DecideRun(ctx, params); err != nil {
		return err
	}
	m.appendEvent(ctx, tenant, approval.RunID, kind, &approval.NodeID, &message)
	return nil
}

func (m *Module) decideNodeRun(ctx context.Context, tenant uuid.UUID, approval db.WorkflowApproval, body decisionRequest, memoryIDs []uuid.UUID, now time.Time) error {
	nodeRunID, err := m.q.NodeRunForApproval(ctx, db.NodeRunForApprovalParams{RunID: approval.RunID, NodeID: approval.NodeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	output := StringMap{"approved": "false", "note": ""}
	status := WorkflowNodeRunStatusFailed
	if body.Approved {
		output["approved"] = "true"
		status = WorkflowNodeRunStatusSucceeded
	}
	if body.Note != nil {
		output["note"] = *body.Note
	}
	if err := m.attachMemories(ctx, tenant, memoryIDs, output); err != nil {
		return err
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return err
	}
	return m.q.DecideNodeRun(ctx, db.DecideNodeRunParams{ID: nodeRunID, Status: string(status), OutputSnapshot: encoded, EndedAt: &now})
}

// attachMemories is WorkflowService.attachedMemories: every id must be the tenant's.
func (m *Module) attachMemories(ctx context.Context, tenant uuid.UUID, memoryIDs []uuid.UUID, output StringMap) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	rows, err := m.q.AttachedMemories(ctx, db.AttachedMemoriesParams{TenantID: tenant, Ids: memoryIDs})
	if err != nil {
		return err
	}
	distinct := make(map[uuid.UUID]bool, len(memoryIDs))
	for _, id := range memoryIDs {
		distinct[id] = true
	}
	if len(rows) != len(distinct) {
		return errBadAttachment
	}
	attachedIDs := make([]string, len(rows))
	contents := make([]string, len(rows))
	for i, row := range rows {
		attachedIDs[i] = ids.Upper(row.ID)
		contents[i] = row.Content
	}
	if len(rows) > 0 {
		output["memoryIDs"] = strings.Join(attachedIDs, ",")
		output["attachedMemories"] = strings.Join(contents, "\n\n")
	}
	return nil
}

// MARK: - Templates

func (m *Module) instantiateTemplate(ctx context.Context, tenant uuid.UUID, templateID string, name *string) (WorkflowDetailDTO, error) {
	template, ok := templateByID(templateID)
	if !ok {
		return WorkflowDetailDTO{}, errNotFound
	}
	effective := template.Name
	if name != nil {
		if trimmed := swiftstr.TrimWhitespacesAndNewlines(*name); trimmed != "" {
			effective = trimmed
		}
	}
	description := template.DescriptionText
	return m.createWorkflow(ctx, tenant, effective, &description, template.Definition)
}

// runTemplate is WorkflowService.runTemplate. A refused enqueue leaves the workflow
// it just created and published, as in Swift.
func (m *Module) runTemplate(ctx context.Context, tenant uuid.UUID, templateID string, body runRequest) (WorkflowRunDTO, error) {
	template, ok := templateByID(templateID)
	if !ok {
		return WorkflowRunDTO{}, errNotFound
	}
	suffix := ids.Upper(uuid.New())[:8]
	name := template.Name + " — " + suffix
	created, err := m.instantiateTemplate(ctx, tenant, templateID, &name)
	if err != nil {
		return WorkflowRunDTO{}, err
	}
	if _, err := m.publishWorkflow(ctx, tenant, created.Workflow.Id.UUID); err != nil {
		return WorkflowRunDTO{}, err
	}
	return m.enqueue(ctx, tenant, created.Workflow.Id.UUID, Manual, body.Input, conversationOf(body.ConversationID), nil)
}

// MARK: - Events

// eventStreamOptions is WorkflowEventsSSEResponse: one poll a second, frames named
// by kind, ": keep-alive" when idle, no poll cap, and a stream that ends only when
// the run is terminal and nothing is left to send. It catches its own failures.
var eventStreamOptions = sse.Options{
	Interval:      time.Second,
	Heartbeat:     ": keep-alive\n\n",
	UntilDrained:  true,
	FinishOnError: true,
}

// errEventRows is what Swift's WorkflowEventStore.list throws on any row. It decodes
// rows into EventRow with SQLKit's default key strategy, so it asks for a column
// named "runID" where the table has run_id. An empty result never reaches the
// decoder. Every enqueued run has a runQueued event, so in production the event
// routes fail for every run: GET .../events is a 500, and .../events/stream is a 200
// that ends at once. Options.DecodeEventRows is the fix.
var errEventRows = errors.New("workflow_run_events: Swift's EventRow can't decode a row: no column named runID")

// appendEvent is WorkflowEventStore.append: best effort, a failure is only logged.
func (m *Module) appendEvent(ctx context.Context, tenant, run uuid.UUID, kind WorkflowRunEventKind, node *uuid.UUID, message *string) {
	if err := m.q.InsertEvent(ctx, db.InsertEventParams{
		TenantID: tenant, RunID: run, Kind: string(kind), NodeID: node, Message: message, Data: []byte("{}"),
	}); err != nil {
		slog.ErrorContext(ctx, "workflow event append failed", "run_id", ids.Upper(run), "kind", string(kind), "error", err)
	}
}

// listEvents is WorkflowEventStore.list. A kind Swift doesn't know is dropped.
func (m *Module) listEvents(ctx context.Context, tenant, run uuid.UUID, after int64, hidden bool) ([]WorkflowRunEventDTO, error) {
	rows, err := m.q.ListEvents(ctx, db.ListEventsParams{TenantID: tenant, RunID: run, Sequence: after})
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 && !m.opts.DecodeEventRows {
		return nil, errEventRows
	}
	out := make([]WorkflowRunEventDTO, 0, len(rows))
	for _, row := range rows {
		kind := WorkflowRunEventKind(row.Kind)
		switch kind {
		case RunQueued, RunStarted, NodeStarted, NodeOutput, NodeCompleted, ApprovalRequired, RunPaused, RunCompleted, RunFailed, RunCancelled:
		default:
			continue
		}
		data, err := decodeStrings(row.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, scrubEvent(WorkflowRunEventDTO{
			Id: row.Sequence, RunID: wire.UUID{UUID: row.RunID}, Kind: kind, NodeID: wireUUID(row.NodeID),
			Message: row.Message, Data: data, CreatedAt: wire.Time{Time: row.CreatedAt},
		}, hidden))
	}
	return out, nil
}

func wireUUID(id *uuid.UUID) *wire.UUID {
	if id == nil {
		return nil
	}
	return &wire.UUID{UUID: *id}
}

// scrubEvent is ModelDisclosurePolicy.scrub for an event. The message matches by
// prefix because the fallback message names a provider.
func scrubEvent(event WorkflowRunEventDTO, hidden bool) WorkflowRunEventDTO {
	if !hidden {
		return event
	}
	delete(event.Data, "provider")
	delete(event.Data, "model")
	delete(event.Data, "fallback")
	if event.Message != nil && strings.HasPrefix(*event.Message, managedUnavailablePrefix) {
		scrubbed := managedUnavailableScrub
		event.Message = &scrubbed
	}
	return event
}

func (m *Module) eventList(ctx context.Context, tenant, run uuid.UUID, after int64) (WorkflowRunEventsResponse, error) {
	out := WorkflowRunEventsResponse{Events: []WorkflowRunEventDTO{}}
	if _, err := m.runByID(ctx, tenant, run); err != nil {
		return out, err
	}
	hidden, err := m.modelHidden(ctx, tenant)
	if err != nil {
		return out, err
	}
	events, err := m.listEvents(ctx, tenant, run, after, hidden)
	if err != nil {
		return out, err
	}
	out.Events = events
	return out, nil
}

// eventSource is what WorkflowEventsSSEResponse polls.
type eventSource struct {
	m      *Module
	tenant uuid.UUID
	run    uuid.UUID
	hidden bool
}

func (s *eventSource) Events(ctx context.Context, after int64) ([]sse.Event, error) {
	events, err := s.m.listEvents(ctx, s.tenant, s.run, after, s.hidden)
	if err != nil {
		return nil, err
	}
	out := make([]sse.Event, len(events))
	for i, event := range events {
		out[i] = sse.Event{ID: event.Id, Name: string(event.Kind), Data: event}
	}
	return out, nil
}

// Terminal is WorkflowService.isTerminal: paused counts, and a failed or empty read
// ends the stream, as `try?` does.
func (s *eventSource) Terminal(ctx context.Context) (bool, error) {
	status, err := s.m.q.RunStatus(ctx, db.RunStatusParams{TenantID: s.tenant, ID: s.run})
	if err != nil {
		return true, nil //nolint:nilerr // Swift's guard let on try? treats a failed read as terminal
	}
	switch WorkflowRunStatus(status) {
	case WorkflowRunStatusSucceeded, WorkflowRunStatusFailed, WorkflowRunStatusCancelled, WorkflowRunStatusTimedOut, WorkflowRunStatusPaused:
		return true, nil
	}
	return false, nil
}
