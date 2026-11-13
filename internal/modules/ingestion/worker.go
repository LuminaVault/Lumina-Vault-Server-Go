package ingestion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/chunkindex"
	"lumina/internal/embedding"
	"lumina/internal/hermescaps"
	"lumina/internal/ids"
	"lumina/internal/jobs"
	"lumina/internal/memorystore"
	"lumina/internal/modules/ingestion/db"
	"lumina/internal/swiftstr"
	"lumina/internal/telemetry"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// WorkerIdleInterval is MultimodalIngestionWorker's idleInterval.
	WorkerIdleInterval = 2 * time.Second

	processingLeaseSeconds   = 60 * 60
	maximumAutomaticAttempts = 3
	abandonedUploadHours     = 24

	stateAnalyzing = "analyzing"
	stateSaving    = "saving"

	eventTerminal = "terminal"
)

// LinkCapturer is LinkCaptureService.captureLink, reduced to the file id the
// worker keeps.
type LinkCapturer func(ctx context.Context, tenant uuid.UUID, url string, spaceID *uuid.UUID) (uuid.UUID, error)

// Notifier is APNSNotificationService.notifyIngestion.
type Notifier interface {
	NotifyIngestion(ctx context.Context, tenant, batchID, itemID uuid.UUID, completed bool, fileName *string) error
}

// CapabilitySource is ingestionCapabilities: the tenant's Hermes capabilities.
type CapabilitySource func(ctx context.Context, tenant uuid.UUID) hermescaps.Capabilities

// WorkerDeps are the worker's half of MultimodalIngestionService's dependencies.
type WorkerDeps struct {
	LinkCapture  LinkCapturer
	Processor    Processor
	Memories     *memorystore.Repository
	Embeddings   embedding.Service
	Push         Notifier // optional
	Capabilities CapabilitySource
	// PublicBaseURL is INGESTION_PUBLIC_BASE_URL; nil when unset.
	PublicBaseURL *url.URL
	Indexer       *chunkindex.Indexer // optional
	Log           *slog.Logger
}

// Worker is MultimodalIngestionWorker over processNext.
type Worker struct {
	m *Module
	d WorkerDeps
}

// NewWorker builds the worker over the route module's tables and volume.
func (m *Module) NewWorker(d WorkerDeps) *Worker {
	if d.Log == nil {
		d.Log = slog.Default().With("label", "lv.ingestion.worker")
	}
	if d.Capabilities == nil {
		d.Capabilities = func(context.Context, uuid.UUID) hermescaps.Capabilities { return hermescaps.ManagedDefault() }
	}
	return &Worker{m: m, d: d}
}

// Job polls every two seconds and, like the Swift loop, keeps claiming while
// there is work. A failed tick is logged and retried on the next one.
func (w *Worker) Job() jobs.Job {
	return jobs.Job{
		Name:     "MultimodalIngestionWorker",
		Schedule: jobs.Every(WorkerIdleInterval, 0),
		Run: func(ctx context.Context) error {
			w.Drain(ctx)
			return nil
		},
	}
}

// Drain runs processNext until it finds nothing, fails, or ctx ends.
func (w *Worker) Drain(ctx context.Context) {
	for ctx.Err() == nil {
		processed, err := w.ProcessNext(ctx)
		if err != nil {
			if ctx.Err() == nil {
				w.d.Log.WarnContext(ctx, fmt.Sprintf("multimodal ingestion tick failed: %v", err))
			}
			return
		}
		if !processed {
			return
		}
	}
}

// ProcessNext is processNext: recover expired leases, expire abandoned uploads,
// then claim and process the oldest due queued item. It reports whether it
// claimed one.
func (w *Worker) ProcessNext(ctx context.Context) (bool, error) {
	if err := w.RecoverExpiredWork(ctx); err != nil {
		return false, err
	}
	if err := w.expireAbandonedUploads(ctx); err != nil {
		return false, err
	}
	var id uuid.UUID
	err := w.m.pool.QueryRow(ctx, `
WITH candidate AS (
    SELECT id FROM ingestion_items
    WHERE state = 'queued' AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
    ORDER BY created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE ingestion_items item
SET state = 'extracting', attempts = attempts + 1,
    lease_expires_at = NOW() + ($1 * INTERVAL '1 second'),
    updated_at = NOW()
FROM candidate
WHERE item.id = candidate.id
RETURNING item.id`, processingLeaseSeconds).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	item, err := w.loadItem(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	telemetry.Global.Ingestion.Claimed.Add(ctx, 1)
	created := item.CreatedAt
	telemetry.Global.Ingestion.RecordQueueLatency(ctx, &created)
	batch, err := w.m.requireBatch(ctx, item.TenantID, item.BatchID)
	if err != nil {
		return false, err
	}
	var space *uuid.UUID
	if batch.SpaceID.Valid {
		s := uuid.UUID(batch.SpaceID.Bytes)
		space = &s
	}
	return true, w.process(ctx, &item, space)
}

func (w *Worker) loadItem(ctx context.Context, id uuid.UUID) (db.IngestionItem, error) {
	rows, err := w.m.pool.Query(ctx, `SELECT `+itemColumns+` FROM ingestion_items WHERE id = $1 LIMIT 1`, id)
	if err != nil {
		return db.IngestionItem{}, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[db.IngestionItem])
}

const itemColumns = `id, tenant_id, batch_id, kind, state, file_name, content_type, size_bytes, uploaded_bytes,
expected_sha256, url, vault_file_id, memory_id, summary, error_message, credibility, attempts, created_at,
updated_at, next_attempt_at, lease_expires_at, source_token_hash, source_token_expires_at, content_sha256,
pipeline_version, reused_from_item_id, graph_ready_at, terminal_notified_at`

// column names the fields process assigns. save writes exactly those, as a Fluent
// save writes the fields assigned since the last one.
type column string

const (
	colState            column = "state"
	colLease            column = "lease_expires_at"
	colError            column = "error_message"
	colVaultFile        column = "vault_file_id"
	colContentSHA       column = "content_sha256"
	colTokenHash        column = "source_token_hash"
	colTokenExpires     column = "source_token_expires_at" //nolint:gosec // a column name, not a credential
	colNextAttempt      column = "next_attempt_at"
	colMemory           column = "memory_id"
	colSummary          column = "summary"
	colCredibility      column = "credibility"
	colReusedFromItemID column = "reused_from_item_id"
)

func (c column) value(item *db.IngestionItem) any {
	switch c {
	case colState:
		return item.State
	case colLease:
		return item.LeaseExpiresAt
	case colError:
		return item.ErrorMessage
	case colVaultFile:
		return item.VaultFileID
	case colContentSHA:
		return item.ContentSha256
	case colTokenHash:
		return item.SourceTokenHash
	case colTokenExpires:
		return item.SourceTokenExpiresAt
	case colNextAttempt:
		return item.NextAttemptAt
	case colMemory:
		return item.MemoryID
	case colSummary:
		return item.Summary
	case colCredibility:
		return item.Credibility
	case colReusedFromItemID:
		return item.ReusedFromItemID
	}
	panic("ingestion: unknown column " + string(c))
}

// save is item.save(on:): the named columns plus updated_at, keyed by id alone.
func (w *Worker) save(ctx context.Context, item *db.IngestionItem, cols ...column) error {
	now := w.m.now()
	sets := make([]string, 0, len(cols)+1)
	args := []any{item.ID}
	for _, c := range cols {
		args = append(args, c.value(item))
		sets = append(sets, string(c)+" = $"+strconv.Itoa(len(args)))
	}
	args = append(args, now)
	sets = append(sets, "updated_at = $"+strconv.Itoa(len(args)))
	if _, err := w.m.pool.Exec(ctx, `UPDATE ingestion_items SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...); err != nil {
		return err
	}
	item.UpdatedAt = now
	return nil
}

func strPtr(s string) *string { return &s }

func (w *Worker) process(ctx context.Context, item *db.IngestionItem, space *uuid.UUID) error {
	tenant := item.TenantID
	if item.Kind == "url" && item.Url != nil {
		if err := w.captureURL(ctx, item, space); err != nil {
			item.State = stateFailed
			item.LeaseExpiresAt = nil
			item.ErrorMessage = strPtr("Could not capture URL source: " + swiftstr.Prefix(describe(err), 400))
			if err := w.save(ctx, item, colState, colLease, colError, colVaultFile); err != nil {
				return err
			}
			return w.finishTerminalFailure(ctx, item)
		}
	}
	reused, err := w.reuseCompletedAnalysis(ctx, item, space)
	if err != nil || reused {
		return err
	}
	caps := w.d.Capabilities(ctx, tenant)
	if blocked, message := capabilityBlock(caps, item); blocked {
		item.State = stateBlockedCapability
		item.LeaseExpiresAt = nil
		item.ErrorMessage = &message
		if err := w.save(ctx, item, colState, colLease, colError); err != nil {
			return err
		}
		return w.finishTerminalFailure(ctx, item)
	}
	source, err := w.prepareSource(ctx, item, caps)
	if err != nil {
		item.State = stateFailed
		item.LeaseExpiresAt = nil
		item.SourceTokenHash = nil
		item.SourceTokenExpiresAt = nil
		item.ErrorMessage = strPtr("Could not prepare source: " + swiftstr.Prefix(describe(err), 400))
		if err := w.save(ctx, item, colState, colLease, colTokenHash, colTokenExpires, colError); err != nil {
			return err
		}
		return w.finishTerminalFailure(ctx, item)
	}

	contentType := "application/octet-stream"
	if item.ContentType != nil {
		contentType = *item.ContentType
	} else if item.Kind == "url" {
		contentType = "text/html"
	}
	result, err := w.d.Processor.Process(ctx, tenant, source, contentType)
	if err != nil {
		return w.deferOrBlock(ctx, item, err)
	}

	var current string
	err = w.m.pool.QueryRow(ctx, `SELECT state FROM ingestion_items WHERE tenant_id = $1 AND id = $2`, tenant, item.ID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && current == stateCancelled {
		return nil
	}
	if err := w.saveResult(ctx, item, space, result); err != nil {
		item.State = stateFailed
		item.LeaseExpiresAt = nil
		item.SourceTokenHash = nil
		item.SourceTokenExpiresAt = nil
		item.ErrorMessage = strPtr("Could not save derived memory: " + swiftstr.Prefix(describe(err), 400))
		if err := w.save(ctx, item, colState, colLease, colTokenHash, colTokenExpires, colError, colMemory); err != nil {
			return err
		}
		w.d.Log.WarnContext(ctx, fmt.Sprintf("ingestion save failed tenant=%s item=%s: %v", ids.Upper(tenant), ids.Upper(item.ID), err))
		return w.finishTerminalFailure(ctx, item)
	}
	return w.m.refreshBatch(ctx, tenant, item.BatchID)
}

// captureURL files a URL item's page through link capture when it has no vault
// file yet and copies that file's sha256 onto the item.
func (w *Worker) captureURL(ctx context.Context, item *db.IngestionItem, space *uuid.UUID) error {
	if !item.VaultFileID.Valid {
		fileID, err := w.d.LinkCapture(ctx, item.TenantID, *item.Url, space)
		if err != nil {
			return err
		}
		item.VaultFileID = pgtype.UUID{Bytes: fileID, Valid: true}
	}
	var sha string
	err := w.m.pool.QueryRow(ctx, `SELECT sha256 FROM vault_files WHERE tenant_id = $1 AND id = $2`,
		item.TenantID, uuid.UUID(item.VaultFileID.Bytes)).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	item.ContentSha256 = &sha
	return w.save(ctx, item, colVaultFile, colContentSHA)
}

// capabilityBlock is the three capability gates, in Swift's order.
func capabilityBlock(caps hermescaps.Capabilities, item *db.IngestionItem) (bool, string) {
	if caps.MultimodalIngestion != nil && *caps.MultimodalIngestion == hermescaps.Unsupported {
		if caps.IngestionRemoteSourceURL != nil && *caps.IngestionRemoteSourceURL {
			return true, "Connected Hermes advertises remote sources but not multimodal ingestion. Upgrade or enable its ingestion API."
		}
		return true, "Connected Hermes does not advertise multimodal ingestion with remote source URLs. Upgrade Hermes or use managed processing."
	}
	if caps.IngestionMaxSourceBytes != nil && item.SizeBytes != nil && *item.SizeBytes > *caps.IngestionMaxSourceBytes {
		return true, fmt.Sprintf("Connected Hermes accepts sources up to %d bytes.", *caps.IngestionMaxSourceBytes)
	}
	if caps.IngestionSupportedMimeTypes != nil && item.ContentType != nil &&
		!SupportsContentType(*item.ContentType, *caps.IngestionSupportedMimeTypes) {
		return true, "Connected Hermes does not support " + *item.ContentType + "."
	}
	return false, ""
}

// prepareSource is the source string: the URL itself, a one-hour public token URL
// for a BYO Hermes that fetches remote sources, or the file's path on the volume.
func (w *Worker) prepareSource(ctx context.Context, item *db.IngestionItem, caps hermescaps.Capabilities) (string, error) {
	if item.Kind == "url" && item.Url != nil {
		return *item.Url, nil
	}
	if !item.VaultFileID.Valid {
		return "", wire.NewError(http.StatusConflict, "item has no source")
	}
	var path string
	err := w.m.pool.QueryRow(ctx, `SELECT path FROM vault_files WHERE tenant_id = $1 AND id = $2`,
		item.TenantID, uuid.UUID(item.VaultFileID.Bytes)).Scan(&path)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", wire.NewError(http.StatusConflict, "item has no source")
	}
	if err != nil {
		return "", err
	}
	if caps.IsUserOverride && caps.IngestionRemoteSourceURL != nil && *caps.IngestionRemoteSourceURL {
		if w.d.PublicBaseURL == nil {
			return "", wire.NewError(http.StatusServiceUnavailable, "INGESTION_PUBLIC_BASE_URL is required for BYO Hermes file ingestion")
		}
		token, err := randomSourceToken()
		if err != nil {
			return "", err
		}
		expires := w.m.now().Add(processingLeaseSeconds * time.Second)
		item.SourceTokenHash = strPtr(sourceTokenHash(token))
		item.SourceTokenExpiresAt = &expires
		if err := w.save(ctx, item, colTokenHash, colTokenExpires); err != nil {
			return "", err
		}
		return w.d.PublicBaseURL.JoinPath("v1/ingestion-sources", token).String(), nil
	}
	// appendingPathComponent(row.path).path: joined, not cleaned.
	return w.m.paths.Raw(item.TenantID) + "/" + path, nil
}

// deferOrBlock handles a processor failure: requeue with backoff while attempts
// remain, otherwise block the item for good.
func (w *Worker) deferOrBlock(ctx context.Context, item *db.IngestionItem, cause error) error {
	retry := item.Attempts < maximumAutomaticAttempts
	reason := swiftstr.Prefix(describe(cause), 400)
	if retry {
		telemetry.Global.Ingestion.Retried.Add(ctx, 1)
		next := w.m.now().Add(retryDelay(int(item.Attempts)))
		item.State = stateQueued
		item.NextAttemptAt = &next
		item.ErrorMessage = strPtr("Hermes processing deferred: " + reason)
	} else {
		telemetry.Global.Ingestion.Failed.Add(ctx, 1)
		item.State = stateBlockedCapability
		item.NextAttemptAt = nil
		item.ErrorMessage = strPtr(fmt.Sprintf("Hermes multimodal ingestion unavailable after %d attempts: %s", item.Attempts, reason))
	}
	item.LeaseExpiresAt = nil
	item.SourceTokenHash = nil
	item.SourceTokenExpiresAt = nil
	if err := w.save(ctx, item, colState, colNextAttempt, colLease, colTokenHash, colTokenExpires, colError); err != nil {
		return err
	}
	w.d.Log.WarnContext(ctx, fmt.Sprintf("ingestion blocked tenant=%s item=%s: %v", ids.Upper(item.TenantID), ids.Upper(item.ID), cause))
	if retry {
		return w.m.refreshBatch(ctx, item.TenantID, item.BatchID)
	}
	return w.finishTerminalFailure(ctx, item)
}

// retryDelay is min(300, 2^min(attempt, 8)) seconds.
func retryDelay(attempt int) time.Duration {
	return time.Duration(min(300, 1<<min(max(0, attempt), 8))) * time.Second
}

// saveResult writes the derived memory, indexes it and moves the item to
// analyzing; knowledge extraction takes it from there to completed.
func (w *Worker) saveResult(ctx context.Context, item *db.IngestionItem, space *uuid.UUID, result Result) error {
	item.State = stateSaving
	if err := w.save(ctx, item, colState); err != nil {
		return err
	}
	content := "# " + result.Title + "\n\n" + result.Markdown + "\n\n## Source assessment\n\n" + result.Credibility.Rationale
	// The analysis already cost a Hermes run; an embedding outage saves the
	// memory without a vector rather than fail the item.
	vec, err := embedding.EmbedForWrite(ctx, w.d.Embeddings, content, item.TenantID, "ingestion.save")
	if err != nil {
		return err
	}
	sourceFile := optionalUUID(item.VaultFileID)
	memory, err := w.d.Memories.Create(ctx, memorystore.CreateParams{
		Tenant: item.TenantID, Content: content, Embedding: vec, Tags: result.Tags,
		SourceVaultFileID: sourceFile, SpaceID: space, ReviewState: "auto",
	})
	if err != nil {
		return err
	}
	item.MemoryID = pgtype.UUID{Bytes: memory.ID, Valid: true}
	w.index(ctx, item, memory.ID, space, content)
	credibility, err := json.Marshal(result.Credibility)
	if err != nil {
		return err
	}
	item.Summary = &result.Summary
	item.Credibility = credibility
	item.State = stateAnalyzing
	item.ErrorMessage = nil
	item.NextAttemptAt = nil
	item.LeaseExpiresAt = nil
	item.SourceTokenHash = nil
	item.SourceTokenExpiresAt = nil
	if err := w.save(ctx, item, colMemory, colSummary, colCredibility, colState, colError, colNextAttempt,
		colLease, colTokenHash, colTokenExpires); err != nil {
		return err
	}
	return w.m.recordEvent(ctx, *item, eventStateChanged, &item.State, item.UploadedBytes)
}

// reuseCompletedAnalysis clones the newest completed analysis of the same bytes
// under the same pipeline into a new memory for this item.
func (w *Worker) reuseCompletedAnalysis(ctx context.Context, item *db.IngestionItem, space *uuid.UUID) (bool, error) {
	if item.ContentSha256 == nil {
		return false, nil
	}
	var (
		sourceID     uuid.UUID
		sourceMemory pgtype.UUID
		summary      *string
		credibility  []byte
	)
	err := w.m.pool.QueryRow(ctx, `
SELECT id, memory_id, summary, credibility FROM ingestion_items
WHERE tenant_id = $1 AND content_sha256 = $2 AND pipeline_version = $3 AND state = 'completed' AND id <> $4
ORDER BY updated_at DESC LIMIT 1`,
		item.TenantID, *item.ContentSha256, item.PipelineVersion, item.ID).Scan(&sourceID, &sourceMemory, &summary, &credibility)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !sourceMemory.Valid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	original, err := w.d.Memories.Find(ctx, item.TenantID, uuid.UUID(sourceMemory.Bytes))
	if err != nil || original == nil {
		return false, err
	}
	vec, err := embedding.EmbedForWrite(ctx, w.d.Embeddings, original.Content, item.TenantID, "ingestion.reuse")
	if err != nil {
		return false, err
	}
	memory, err := w.d.Memories.Create(ctx, memorystore.CreateParams{
		Tenant: item.TenantID, Content: original.Content, Embedding: vec, Tags: original.Tags,
		SourceVaultFileID: optionalUUID(item.VaultFileID), SpaceID: space, ReviewState: "auto",
	})
	if err != nil {
		return false, err
	}
	item.MemoryID = pgtype.UUID{Bytes: memory.ID, Valid: true}
	// Chunks are keyed by memory, and this is a new memory.
	w.index(ctx, item, memory.ID, space, original.Content)
	item.Summary = summary
	item.Credibility = credibility
	item.ReusedFromItemID = pgtype.UUID{Bytes: sourceID, Valid: true}
	item.State = stateAnalyzing
	item.LeaseExpiresAt = nil
	item.SourceTokenHash = nil
	item.SourceTokenExpiresAt = nil
	item.ErrorMessage = nil
	if err := w.save(ctx, item, colMemory, colSummary, colCredibility, colReusedFromItemID, colState, colLease,
		colTokenHash, colTokenExpires, colError); err != nil {
		return false, err
	}
	if err := w.m.recordEvent(ctx, *item, eventStateChanged, &item.State, item.UploadedBytes); err != nil {
		return false, err
	}
	telemetry.Global.Ingestion.Deduplicated.Add(ctx, 1)
	return true, w.m.refreshBatch(ctx, item.TenantID, item.BatchID)
}

func (w *Worker) index(ctx context.Context, item *db.IngestionItem, memoryID uuid.UUID, space *uuid.UUID, content string) {
	if w.d.Indexer == nil {
		return
	}
	file := optionalUUID(item.VaultFileID)
	w.d.Indexer.IndexBestEffort(ctx, chunkindex.IndexParams{
		Tenant: item.TenantID, MemoryID: memoryID, VaultFileID: file, SpaceID: space,
		SourcePath: w.sourcePath(ctx, item.TenantID, file), Content: content,
	})
}

// sourcePath is the vault-relative citation path; a failed lookup degrades the
// citation rather than the ingest.
func (w *Worker) sourcePath(ctx context.Context, tenant uuid.UUID, file *uuid.UUID) *string {
	if file == nil {
		return nil
	}
	var path string
	if err := w.m.pool.QueryRow(ctx, `SELECT path FROM vault_files WHERE tenant_id = $1 AND id = $2`, tenant, *file).Scan(&path); err != nil {
		return nil
	}
	return &path
}

func optionalUUID(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	u := uuid.UUID(id.Bytes)
	return &u
}

// RecoverExpiredWork is recoverExpiredWork: it requeues items whose worker lease
// ran out mid-flight.
func (w *Worker) RecoverExpiredWork(ctx context.Context) error {
	_, err := w.m.pool.Exec(ctx, `
UPDATE ingestion_items
SET state = 'queued', next_attempt_at = NOW(), lease_expires_at = NULL,
    error_message = 'Recovered after an interrupted worker lease', updated_at = NOW()
WHERE state IN ('extracting', 'analyzing', 'saving')
  AND lease_expires_at IS NOT NULL AND lease_expires_at <= NOW()`)
	return err
}

type staleUpload struct {
	id, tenant, batch uuid.UUID
	fileName          *string
}

// expireAbandonedUploads fails uploads idle for a day, drops their chunks, and
// tells the user once.
func (w *Worker) expireAbandonedUploads(ctx context.Context) error {
	rows, err := w.m.pool.Query(ctx, `
UPDATE ingestion_items
SET state = 'failed', error_message = 'Upload expired after 24 hours', updated_at = NOW()
WHERE state = 'awaiting_upload'
  AND updated_at < NOW() - ($1 * INTERVAL '1 hour')
RETURNING id, tenant_id, batch_id, file_name`, abandonedUploadHours)
	if err != nil {
		return err
	}
	stale, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (staleUpload, error) {
		var s staleUpload
		err := r.Scan(&s.id, &s.tenant, &s.batch, &s.fileName)
		return s, err
	})
	if err != nil {
		return err
	}
	for _, s := range stale {
		_ = os.RemoveAll(w.m.chunkDir(s.tenant, s.id))
		if _, err := w.m.pool.Exec(ctx, `
INSERT INTO ingestion_events (tenant_id, batch_id, item_id, type, state)
VALUES ($1, $2, $3, 'terminal', 'failed')`, s.tenant, s.batch, s.id); err != nil {
			w.d.Log.WarnContext(ctx, fmt.Sprintf("ingestion terminal event failed item=%s: %v", ids.Upper(s.id), err))
		}
		w.notifyTerminalFailure(ctx, s.tenant, s.batch, s.id, s.fileName)
	}
	seen := map[uuid.UUID]bool{}
	for _, s := range stale {
		if seen[s.batch] {
			continue
		}
		seen[s.batch] = true
		if err := w.m.refreshBatch(ctx, s.tenant, s.batch); err != nil {
			return err
		}
	}
	return nil
}

// finishTerminalFailure records the terminal event, pushes once, and refreshes
// the batch.
func (w *Worker) finishTerminalFailure(ctx context.Context, item *db.IngestionItem) error {
	if err := w.m.recordEvent(ctx, *item, eventTerminal, &item.State, item.UploadedBytes); err != nil {
		w.d.Log.WarnContext(ctx, fmt.Sprintf("ingestion terminal event failed item=%s: %v", ids.Upper(item.ID), err))
	}
	if item.TerminalNotifiedAt == nil {
		w.notifyTerminalFailure(ctx, item.TenantID, item.BatchID, item.ID, item.FileName)
	}
	return w.m.refreshBatch(ctx, item.TenantID, item.BatchID)
}

func (w *Worker) notifyTerminalFailure(ctx context.Context, tenant, batchID, itemID uuid.UUID, fileName *string) {
	if w.d.Push == nil {
		return
	}
	err := w.d.Push.NotifyIngestion(ctx, tenant, batchID, itemID, false, fileName)
	if err == nil {
		_, err = w.m.pool.Exec(ctx, `
UPDATE ingestion_items SET terminal_notified_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND terminal_notified_at IS NULL`, itemID, tenant)
	}
	if err != nil {
		telemetry.Global.Ingestion.APNSFailures.Add(ctx, 1)
		w.d.Log.WarnContext(ctx, fmt.Sprintf("ingestion terminal push failed item=%s: %v", ids.Upper(itemID), err))
	}
}

func randomSourceToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// describe is String(describing: error) for the errors Swift's worker stores: a
// Hummingbird HTTPError reads "HTTPError: <reason phrase>, <message>".
func describe(err error) string {
	var we *wire.Error
	if errors.As(err, &we) {
		reason := http.StatusText(we.Status)
		if we.Status == http.StatusUnprocessableEntity {
			reason = "Unprocessable Content"
		}
		return "HTTPError: " + reason + ", " + we.Message
	}
	return err.Error()
}
