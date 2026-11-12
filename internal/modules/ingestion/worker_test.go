package ingestion_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/hermescaps"
	"lumina/internal/memorystore"
	"lumina/internal/modules/ingestion"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type stubProcessor struct {
	mu      sync.Mutex
	result  ingestion.Result
	err     error
	sources []string
	types   []string
}

func (p *stubProcessor) Process(_ context.Context, _ uuid.UUID, source, contentType string) (ingestion.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources = append(p.sources, source)
	p.types = append(p.types, contentType)
	return p.result, p.err
}

type stubEmbeddings struct{}

func (stubEmbeddings) Embed(context.Context, string, uuid.UUID) ([]float32, error) {
	return make([]float32, 1536), nil
}

type pushCall struct {
	tenant, batch, item uuid.UUID
	completed           bool
	fileName            *string
}

type stubPush struct {
	mu    sync.Mutex
	calls []pushCall
	err   error
}

func (p *stubPush) NotifyIngestion(_ context.Context, tenant, batchID, itemID uuid.UUID, completed bool, fileName *string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, pushCall{tenant, batchID, itemID, completed, fileName})
	return p.err
}

func score(n int) *int { return &n }

func sampleResult() ingestion.Result {
	return ingestion.Result{
		Title: "Quarterly report", Markdown: "Revenue grew.", Summary: "A short summary.", Tags: []string{"finance"},
		Credibility: ingestion.Credibility{Score: score(80), Confidence: 0.9, Signals: []string{"primary"}, Rationale: "Signed by the CFO.", Version: "v1"},
	}
}

type workerEnv struct {
	env
	w       *ingestion.Worker
	proc    *stubProcessor
	push    *stubPush
	caps    hermescaps.Capabilities
	links   []string
	linkErr error
}

func setupWorker(t *testing.T) *workerEnv {
	t.Helper()
	v := setup(t, plentyOfDisk)
	// Park parity's seeded items so the worker only sees what a test queues.
	_, err := v.pool.Exec(context.Background(), `UPDATE ingestion_items SET state = 'cancelled'`)
	require.NoError(t, err)
	we := &workerEnv{env: v, proc: &stubProcessor{result: sampleResult()}, push: &stubPush{}, caps: hermescaps.ManagedDefault()}
	// The fixtures upload text/plain, which managedDefault doesn't list.
	*we.caps.IngestionSupportedMimeTypes = append(*we.caps.IngestionSupportedMimeTypes, "text/plain")
	we.w = v.m.NewWorker(ingestion.WorkerDeps{
		LinkCapture: func(ctx context.Context, tenant uuid.UUID, rawURL string, _ *uuid.UUID) (uuid.UUID, error) {
			we.links = append(we.links, rawURL)
			if we.linkErr != nil {
				return uuid.Nil, we.linkErr
			}
			id := uuid.New()
			_, err := v.pool.Exec(ctx, `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
				VALUES ($1, $2, 'links/page.md', 'text/markdown', 4, 'urlsha', now(), now())`, id, tenant)
			return id, err
		},
		Processor:    we.proc,
		Memories:     memorystore.New(v.pool, nil),
		Embeddings:   stubEmbeddings{},
		Push:         we.push,
		Capabilities: func(context.Context, uuid.UUID) hermescaps.Capabilities { return we.caps },
	})
	return we
}

// queuedFile uploads and completes one text file, leaving it queued with its vault file.
func (we *workerEnv) queuedFile(user uuid.UUID, content string) batch {
	we.t.Helper()
	b := we.createFileBatch(user, len(content))
	r := we.do(user, http.MethodPut, b.itemPath()+"/chunks/0", content, "Content-Type", "application/octet-stream")
	require.Equal(we.t, http.StatusNoContent, r.code, string(r.body))
	completed := decodeBatch(we.t, we.do(user, http.MethodPost, b.itemPath()+"/complete", ""))
	require.Equal(we.t, "queued", completed.Items[0].State)
	completed.ID = b.ID
	return completed
}

type itemRow struct {
	state       string
	errMsg      *string
	attempts    int
	memory      *uuid.UUID
	summary     *string
	credibility []byte
	next, lease *time.Time
	tokenHash   *string
	reusedFrom  *uuid.UUID
	notified    *time.Time
	vaultFile   *uuid.UUID
	sha         *string
}

func (we *workerEnv) item(id string) itemRow {
	we.t.Helper()
	var r itemRow
	require.NoError(we.t, we.pool.QueryRow(context.Background(), `SELECT state, error_message, attempts, memory_id, summary,
		credibility, next_attempt_at, lease_expires_at, source_token_hash, reused_from_item_id, terminal_notified_at,
		vault_file_id, content_sha256 FROM ingestion_items WHERE id = $1`, id).Scan(&r.state, &r.errMsg, &r.attempts,
		&r.memory, &r.summary, &r.credibility, &r.next, &r.lease, &r.tokenHash, &r.reusedFrom, &r.notified, &r.vaultFile, &r.sha))
	return r
}

func (we *workerEnv) events(id string) []string {
	we.t.Helper()
	rows, err := we.pool.Query(context.Background(), `SELECT type || ':' || coalesce(state, '') FROM ingestion_events WHERE item_id = $1 AND type <> 'progress' ORDER BY id`, id)
	require.NoError(we.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(we.t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func (we *workerEnv) batchState(id string) string {
	we.t.Helper()
	var s string
	require.NoError(we.t, we.pool.QueryRow(context.Background(), `SELECT state FROM ingestion_batches WHERE id = $1`, id).Scan(&s))
	return s
}

func (we *workerEnv) processNext() bool {
	we.t.Helper()
	ok, err := we.w.ProcessNext(context.Background())
	require.NoError(we.t, err)
	return ok
}

// Swift "expired worker lease is recovered after a simulated crash".
func TestExpiredWorkerLeaseIsRecoveredAfterASimulatedCrash(t *testing.T) {
	we := setupWorker(t)
	b := we.createFileBatch(alice, 5)
	_, err := we.pool.Exec(context.Background(), `UPDATE ingestion_items SET state = 'extracting', lease_expires_at = now() - interval '1 second' WHERE id = $1`, b.Items[0].ID)
	require.NoError(t, err)

	require.NoError(t, we.w.RecoverExpiredWork(context.Background()))

	r := we.item(b.Items[0].ID)
	require.Equal(t, "queued", r.state)
	require.Nil(t, r.lease)
	require.Equal(t, "Recovered after an interrupted worker lease", *r.errMsg)
}

// A file item is analyzed from its path on the volume into an auto-reviewed memory
// tagged with the result, and moves to analyzing for knowledge extraction.
func TestWorkerSavesTheDerivedMemory(t *testing.T) {
	we := setupWorker(t)
	b := we.queuedFile(alice, "durable ingestion upload")
	id := b.Items[0].ID

	require.True(t, we.processNext())
	require.False(t, we.processNext())

	r := we.item(id)
	require.Equal(t, "analyzing", r.state)
	require.Equal(t, 1, r.attempts)
	require.Nil(t, r.errMsg)
	require.Nil(t, r.lease)
	require.Nil(t, r.next)
	require.Equal(t, "A short summary.", *r.summary)
	var cred map[string]any
	require.NoError(t, json.Unmarshal(r.credibility, &cred))
	require.Equal(t, map[string]any{"score": 80.0, "confidence": 0.9, "signals": []any{"primary"}, "rationale": "Signed by the CFO.", "version": "v1"}, cred)
	require.NotNil(t, r.memory)

	var path string
	require.NoError(t, we.pool.QueryRow(context.Background(), `SELECT path FROM vault_files WHERE id = $1`, *r.vaultFile).Scan(&path))
	require.Equal(t, []string{we.paths.Raw(alice) + "/" + path}, we.proc.sources)
	require.Equal(t, []string{"text/plain"}, we.proc.types)

	var content, review string
	var tags []string
	var source *uuid.UUID
	require.NoError(t, we.pool.QueryRow(context.Background(), `SELECT content, review_state, tags, source_vault_file_id FROM memories WHERE id = $1`,
		*r.memory).Scan(&content, &review, &tags, &source))
	require.Equal(t, "# Quarterly report\n\nRevenue grew.\n\n## Source assessment\n\nSigned by the CFO.", content)
	require.Equal(t, "auto", review)
	require.Equal(t, []string{"finance"}, tags)
	require.Equal(t, *r.vaultFile, *source)

	require.Contains(t, we.events(id), "state_changed:analyzing")
	require.Equal(t, "active", we.batchState(b.ID))
	require.Empty(t, we.push.calls)
}

// A processor failure requeues with backoff until the third attempt, which blocks
// the item, records the terminal event and pushes once.
func TestWorkerDefersThenBlocksAfterThreeAttempts(t *testing.T) {
	we := setupWorker(t)
	we.proc.err = wire.NewError(http.StatusBadGateway, "Hermes ingestion endpoint failed")
	b := we.queuedFile(alice, "flaky")
	id := b.Items[0].ID

	require.True(t, we.processNext())
	r := we.item(id)
	require.Equal(t, "queued", r.state)
	require.Equal(t, "Hermes processing deferred: HTTPError: Bad Gateway, Hermes ingestion endpoint failed", *r.errMsg)
	require.NotNil(t, r.next)
	require.WithinDuration(t, time.Now().Add(2*time.Second), *r.next, 2*time.Second)
	require.Nil(t, r.lease)
	require.False(t, we.processNext(), "not due yet")

	for range 2 {
		_, err := we.pool.Exec(context.Background(), `UPDATE ingestion_items SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, id)
		require.NoError(t, err)
		require.True(t, we.processNext())
	}
	r = we.item(id)
	require.Equal(t, "blocked_capability", r.state)
	require.Equal(t, 3, r.attempts)
	require.Equal(t, "Hermes multimodal ingestion unavailable after 3 attempts: HTTPError: Bad Gateway, Hermes ingestion endpoint failed", *r.errMsg)
	require.Nil(t, r.next)
	require.NotNil(t, r.notified)
	require.Equal(t, []string{"state_changed:queued", "terminal:blocked_capability"}, we.events(id), "complete's event, then the worker's")
	require.Len(t, we.push.calls, 1)
	call := we.push.calls[0]
	require.Equal(t, alice, call.tenant)
	require.Equal(t, uuid.MustParse(id), call.item)
	require.False(t, call.completed)
	require.Equal(t, "integration.txt", *call.fileName)
	require.Equal(t, "attention", we.batchState(b.ID))
}

// The three capability gates block before the processor runs.
func TestWorkerCapabilityGates(t *testing.T) {
	unsupported, yes := hermescaps.Unsupported, true
	maxBytes := int64(3)
	for name, tc := range map[string]struct {
		caps func(*hermescaps.Capabilities)
		want string
	}{
		"unsupported": {
			func(c *hermescaps.Capabilities) { c.MultimodalIngestion = &unsupported },
			"Connected Hermes does not advertise multimodal ingestion with remote source URLs. Upgrade Hermes or use managed processing.",
		},
		"unsupported remote": {
			func(c *hermescaps.Capabilities) {
				c.MultimodalIngestion = &unsupported
				c.IngestionRemoteSourceURL = &yes
			},
			"Connected Hermes advertises remote sources but not multimodal ingestion. Upgrade or enable its ingestion API.",
		},
		"too big": {
			func(c *hermescaps.Capabilities) { c.IngestionMaxSourceBytes = &maxBytes },
			"Connected Hermes accepts sources up to 3 bytes.",
		},
		"mime": {
			func(c *hermescaps.Capabilities) { c.IngestionSupportedMimeTypes = &[]string{"image/*"} },
			"Connected Hermes does not support text/plain.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			we := setupWorker(t)
			tc.caps(&we.caps)
			b := we.queuedFile(alice, "gated")
			require.True(t, we.processNext())
			r := we.item(b.Items[0].ID)
			require.Equal(t, "blocked_capability", r.state)
			require.Equal(t, tc.want, *r.errMsg)
			require.Nil(t, r.lease)
			require.Empty(t, we.proc.sources)
			require.Len(t, we.push.calls, 1)
		})
	}
}

// A BYO Hermes that fetches remote sources gets a one-hour token URL on the
// public base; without INGESTION_PUBLIC_BASE_URL the item fails to prepare.
func TestWorkerBYOSourceURL(t *testing.T) {
	yes := true
	we := setupWorker(t)
	we.caps.IsUserOverride, we.caps.IngestionRemoteSourceURL = true, &yes
	b := we.queuedFile(alice, "remote")
	require.True(t, we.processNext())
	r := we.item(b.Items[0].ID)
	require.Equal(t, "failed", r.state)
	require.Equal(t, "Could not prepare source: HTTPError: Service Unavailable, INGESTION_PUBLIC_BASE_URL is required for BYO Hermes file ingestion", *r.errMsg)
	require.Nil(t, r.tokenHash)

	we = setupWorker(t)
	we.caps.IsUserOverride, we.caps.IngestionRemoteSourceURL = true, &yes
	base, err := url.Parse("https://api.example.test")
	require.NoError(t, err)
	we.w = we.m.NewWorker(ingestion.WorkerDeps{
		Processor: we.proc, Memories: memorystore.New(we.pool, nil), Embeddings: stubEmbeddings{}, Push: we.push,
		Capabilities:  func(context.Context, uuid.UUID) hermescaps.Capabilities { return we.caps },
		PublicBaseURL: base,
	})
	var captured string
	we.proc.err = errors.New("stop after source")
	b = we.queuedFile(alice, "remote")
	require.True(t, we.processNext())
	captured = we.proc.sources[0]
	token, ok := strings.CutPrefix(captured, "https://api.example.test/v1/ingestion-sources/")
	require.True(t, ok, captured)
	require.Len(t, token, 64)
	// The failure path clears the token, as in Swift.
	r = we.item(b.Items[0].ID)
	require.Equal(t, "queued", r.state)
	require.Nil(t, r.tokenHash)
}

// A completed item with the same bytes and pipeline is cloned instead of analyzed.
func TestWorkerReusesACompletedAnalysis(t *testing.T) {
	we := setupWorker(t)
	first := we.queuedFile(alice, "same bytes")
	require.True(t, we.processNext())
	_, err := we.pool.Exec(context.Background(), `UPDATE ingestion_items SET state = 'completed' WHERE id = $1`, first.Items[0].ID)
	require.NoError(t, err)
	source := we.item(first.Items[0].ID)

	second := we.queuedFile(alice, "same bytes")
	require.True(t, we.processNext())
	r := we.item(second.Items[0].ID)
	require.Equal(t, "analyzing", r.state)
	require.Equal(t, uuid.MustParse(first.Items[0].ID), *r.reusedFrom)
	require.Equal(t, *source.summary, *r.summary)
	require.JSONEq(t, string(source.credibility), string(r.credibility))
	require.NotEqual(t, *source.memory, *r.memory)
	require.Len(t, we.proc.sources, 1, "the processor ran only for the first item")
	var content string
	require.NoError(t, we.pool.QueryRow(context.Background(), `SELECT content FROM memories WHERE id = $1`, *r.memory).Scan(&content))
	require.True(t, strings.HasPrefix(content, "# Quarterly report"))
	require.Contains(t, we.events(second.Items[0].ID), "state_changed:analyzing")
}

// A URL item is captured into the vault first; a capture failure fails the item.
func TestWorkerCapturesURLItems(t *testing.T) {
	we := setupWorker(t)
	b := decodeBatch(t, we.do(alice, http.MethodPost, "/v1/ingestions", `{"items":[{"kind":"url","url":"https://example.com/a"}]}`))
	_, err := we.pool.Exec(context.Background(), `UPDATE ingestion_items SET state = 'queued' WHERE id = $1`, b.Items[0].ID)
	require.NoError(t, err)
	require.True(t, we.processNext())
	r := we.item(b.Items[0].ID)
	require.Equal(t, "analyzing", r.state)
	require.Equal(t, []string{"https://example.com/a"}, we.links)
	require.NotNil(t, r.vaultFile)
	require.Equal(t, "urlsha", *r.sha)
	require.Equal(t, []string{"https://example.com/a"}, we.proc.sources)
	require.Equal(t, []string{"text/html"}, we.proc.types)

	we = setupWorker(t)
	we.linkErr = wire.NewError(http.StatusBadRequest, "invalid url")
	b = decodeBatch(t, we.do(alice, http.MethodPost, "/v1/ingestions", `{"items":[{"kind":"url","url":"https://example.com/b"}]}`))
	_, err = we.pool.Exec(context.Background(), `UPDATE ingestion_items SET state = 'queued' WHERE id = $1`, b.Items[0].ID)
	require.NoError(t, err)
	require.True(t, we.processNext())
	r = we.item(b.Items[0].ID)
	require.Equal(t, "failed", r.state)
	require.Equal(t, "Could not capture URL source: HTTPError: Bad Request, invalid url", *r.errMsg)
	require.Equal(t, []string{"terminal:failed"}, we.events(b.Items[0].ID))
}

// An item cancelled while Hermes ran keeps its cancelled state.
func TestWorkerLeavesACancelledItemAlone(t *testing.T) {
	we := setupWorker(t)
	b := we.queuedFile(alice, "cancel me")
	id := b.Items[0].ID
	we.w = we.m.NewWorker(ingestion.WorkerDeps{
		Processor: processorFunc(func() (ingestion.Result, error) {
			_, err := we.pool.Exec(context.Background(), `UPDATE ingestion_items SET state = 'cancelled' WHERE id = $1`, id)
			return sampleResult(), err
		}),
		Memories: memorystore.New(we.pool, nil), Embeddings: stubEmbeddings{}, Push: we.push,
		Capabilities: func(context.Context, uuid.UUID) hermescaps.Capabilities { return we.caps },
	})
	require.True(t, we.processNext())
	r := we.item(id)
	require.Equal(t, "cancelled", r.state)
	require.Nil(t, r.memory)
}

type processorFunc func() (ingestion.Result, error)

func (f processorFunc) Process(context.Context, uuid.UUID, string, string) (ingestion.Result, error) {
	return f()
}

// Uploads idle for a day fail, lose their chunks and push once.
func TestWorkerExpiresAbandonedUploads(t *testing.T) {
	we := setupWorker(t)
	b := we.createFileBatch(alice, 10)
	id := b.Items[0].ID
	r := we.do(alice, http.MethodPut, b.itemPath()+"/chunks/0", "12345", "Content-Type", "application/octet-stream")
	require.Equal(t, http.StatusNoContent, r.code, string(r.body))
	chunks := filepath.Join(we.paths.TenantRoot(alice), "tmp", "ingestion", strings.ToUpper(id))
	_, err := os.Stat(chunks)
	require.NoError(t, err)
	_, err = we.pool.Exec(context.Background(), `UPDATE ingestion_items SET updated_at = now() - interval '25 hours' WHERE id = $1`, id)
	require.NoError(t, err)

	require.False(t, we.processNext())
	row := we.item(id)
	require.Equal(t, "failed", row.state)
	require.Equal(t, "Upload expired after 24 hours", *row.errMsg)
	require.NotNil(t, row.notified)
	_, err = os.Stat(chunks)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, []string{"terminal:failed"}, we.events(id))
	require.Len(t, we.push.calls, 1)
	require.Equal(t, "attention", we.batchState(b.ID))
}
