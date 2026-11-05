package ingestion_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/diskguard"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/ingestion"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	ann   = uuid.MustParse("1A6E0000-0000-4000-8000-0000000000A1")
	src   = uuid.MustParse("1A6E0000-0000-4000-8000-0000000000A2")
	oli   = uuid.MustParse("1A6E0000-0000-4000-8000-0000000000A8")
	gra   = uuid.MustParse("1A6E0000-0000-4000-8000-0000000000AA")

	teamVault = "1A6E2100-0000-4000-8000-000000000001"

	gib = int64(1024 * 1024 * 1024)
)

type env struct {
	t     *testing.T
	e     *echo.Echo
	pool  *pgxpool.Pool
	paths vaultpaths.Paths
}

// setup seeds parity's rows. free is what the injected disk reports.
func setup(t *testing.T, free int64) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "ingestion.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	paths := vaultpaths.Paths{Root: t.TempDir()}
	guard := diskguard.New(paths.Root, 3*gib, 0.10, diskguard.Options{
		Provider: func(string) (diskguard.Space, error) {
			return diskguard.Space{FreeBytes: free, TotalBytes: 100 * gib}, nil
		},
	})
	m := ingestion.New(pool, auth.Identify(keys, auth.PGUsers(pool)), ratelimit.NewStore(), paths, guard,
		ingestion.Quota{Limits: ingestion.DefaultLimits(), Enabled: true})
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	return env{t: t, e: e, pool: pool, paths: paths}
}

const plentyOfDisk = 50 * 1024 * 1024 * 1024

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (v env) do(user uuid.UUID, method, path, body string, hdr ...string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

type item struct {
	ID            string  `json:"id"`
	State         string  `json:"state"`
	UploadedBytes int64   `json:"uploadedBytes"`
	VaultFileID   *string `json:"vaultFileID"`
	ContentSHA256 *string `json:"contentSHA256"`
	Error         *string `json:"error"`
}

type batch struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Items []item `json:"items"`
}

func decodeBatch(t *testing.T, r resp) batch {
	t.Helper()
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var b batch
	require.NoError(t, json.Unmarshal(r.body, &b))
	return b
}

// createFileBatch is the Swift tests' createFileBatch: integration.txt, text/plain.
func (v env) createFileBatch(user uuid.UUID, size int, hdr ...string) batch {
	v.t.Helper()
	body := `{"items":[{"kind":"file","fileName":"integration.txt","contentType":"text/plain","sizeBytes":` + itoa(size) + `}]}`
	return decodeBatch(v.t, v.do(user, http.MethodPost, "/v1/ingestions", body, hdr...))
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (b batch) itemPath() string {
	return "/v1/ingestions/" + b.ID + "/items/" + b.Items[0].ID
}

// Swift "upload completes into the vault and cancellation is idempotent".
func TestUploadCompletesIntoTheVaultAndCancellationIsIdempotent(t *testing.T) {
	v := setup(t, plentyOfDisk)
	content := "durable ingestion upload"
	b := v.createFileBatch(alice, len(content))
	require.Len(t, b.Items, 1)

	r := v.do(alice, http.MethodPut, b.itemPath()+"/chunks/0", content, "Content-Type", "application/octet-stream")
	require.Equal(t, http.StatusNoContent, r.code, string(r.body))
	require.Empty(t, r.body)
	chunk := filepath.Join(v.paths.TenantRoot(alice), "tmp", "ingestion", strings.ToUpper(b.Items[0].ID), "0")
	raw, err := os.ReadFile(chunk)
	require.NoError(t, err)
	require.Equal(t, content, string(raw))

	completed := decodeBatch(t, v.do(alice, http.MethodPost, b.itemPath()+"/complete", ""))
	stored := completed.Items[0]
	require.Equal(t, "queued", stored.State)
	require.EqualValues(t, len(content), stored.UploadedBytes)
	require.NotNil(t, stored.VaultFileID)
	require.NotNil(t, stored.ContentSHA256)

	// The file is where the worker looks, its row points at it, the chunks are gone.
	var path string
	var size int64
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT path, size_bytes FROM vault_files WHERE id = $1`, *stored.VaultFileID).Scan(&path, &size))
	require.Equal(t, "inbox/"+strings.ToLower(stored.ID)+"-integration.txt", path)
	require.EqualValues(t, len(content), size)
	raw, err = os.ReadFile(filepath.Join(v.paths.Raw(alice), path))
	require.NoError(t, err)
	require.Equal(t, content, string(raw))
	_, err = os.Stat(filepath.Dir(chunk))
	require.ErrorIs(t, err, os.ErrNotExist)

	cancelled := decodeBatch(t, v.do(alice, http.MethodDelete, b.itemPath(), ""))
	require.Equal(t, "cancelled", cancelled.Items[0].State)
	again := decodeBatch(t, v.do(alice, http.MethodDelete, b.itemPath(), ""))
	require.Equal(t, "cancelled", again.Items[0].State)
}

// Swift "ingestion batches and mutations are tenant isolated".
func TestBatchesAndMutationsAreTenantIsolated(t *testing.T) {
	v := setup(t, plentyOfDisk)
	b := v.createFileBatch(alice, 7)

	r := v.do(bob, http.MethodGet, "/v1/ingestions/"+b.ID, "")
	require.Equal(t, http.StatusNotFound, r.code)
	require.JSONEq(t, `{"error":{"message":"ingestion batch not found"}}`, string(r.body))
	r = v.do(bob, http.MethodDelete, b.itemPath(), "")
	require.Equal(t, http.StatusNotFound, r.code)
	require.JSONEq(t, `{"error":{"message":"ingestion item not found"}}`, string(r.body))

	owner := decodeBatch(t, v.do(alice, http.MethodGet, "/v1/ingestions/"+b.ID, ""))
	require.Equal(t, "awaiting_upload", owner.Items[0].State)
}

// Swift "shared vault ingestion uses requested vault partition".
func TestSharedVaultUsesTheRequestedPartition(t *testing.T) {
	v := setup(t, plentyOfDisk)
	content := "team vault upload"
	vault := []string{"X-Vault-ID", teamVault}
	b := v.createFileBatch(oli, len(content), vault...)

	r := v.do(oli, http.MethodPut, b.itemPath()+"/chunks/0", content, vault...)
	require.Equal(t, http.StatusNoContent, r.code, string(r.body))
	completed := decodeBatch(t, v.do(oli, http.MethodPost, b.itemPath()+"/complete", "", vault...))
	require.NotNil(t, completed.Items[0].VaultFileID)

	ctx := context.Background()
	var tenant uuid.UUID
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT tenant_id FROM ingestion_batches WHERE id = $1`, b.ID).Scan(&tenant))
	require.Equal(t, uuid.MustParse(teamVault), tenant)
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT tenant_id FROM vault_files WHERE id = $1`, *completed.Items[0].VaultFileID).Scan(&tenant))
	require.Equal(t, uuid.MustParse(teamVault), tenant)

	// The batch isn't in the caller's personal vault.
	require.Equal(t, http.StatusNotFound, v.do(oli, http.MethodGet, "/v1/ingestions/"+b.ID, "").code)
}

// Swift "failed item can retry and clears transient worker state".
func TestFailedItemCanRetryAndClearsTransientWorkerState(t *testing.T) {
	v := setup(t, plentyOfDisk)
	ctx := context.Background()
	b := v.createFileBatch(alice, 5)
	future := time.Now().Add(5 * time.Minute)
	_, err := v.pool.Exec(ctx, `UPDATE ingestion_items SET state = 'failed', error_message = 'upstream unavailable',
		next_attempt_at = $2, lease_expires_at = $2, source_token_hash = $3, source_token_expires_at = $2, terminal_notified_at = now()
		WHERE id = $1`, b.Items[0].ID, future, strings.Repeat("f", 64))
	require.NoError(t, err)

	retried := decodeBatch(t, v.do(alice, http.MethodPost, b.itemPath()+"/retry", ""))
	require.Equal(t, "queued", retried.Items[0].State)
	require.Nil(t, retried.Items[0].Error)

	var nextAttempt, lease, tokenExpiry, notified *time.Time
	var hash *string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT next_attempt_at, lease_expires_at, source_token_hash, source_token_expires_at, terminal_notified_at
		FROM ingestion_items WHERE id = $1`, b.Items[0].ID).Scan(&nextAttempt, &lease, &hash, &tokenExpiry, &notified))
	require.Nil(t, nextAttempt)
	require.Nil(t, lease)
	require.Nil(t, hash)
	require.Nil(t, tokenExpiry)
	require.Nil(t, notified)
}

// Swift "expired public source token returns not found".
func TestExpiredPublicSourceTokenReturnsNotFound(t *testing.T) {
	v := setup(t, plentyOfDisk)
	token := strings.Repeat("9", 64)
	b := v.createFileBatch(alice, 5)
	_, err := v.pool.Exec(context.Background(), `UPDATE ingestion_items SET source_token_hash = encode(sha256(convert_to($2, 'UTF8')), 'hex'), source_token_expires_at = $3 WHERE id = $1`,
		b.Items[0].ID, token, time.Now().Add(-time.Second))
	require.NoError(t, err)
	r := v.do(uuid.Nil, http.MethodGet, "/v1/ingestion-sources/"+token, "")
	require.Equal(t, http.StatusNotFound, r.code)
	require.JSONEq(t, `{"error":{"message":"ingestion source not found or expired"}}`, string(r.body))
}

// Swift DiskSpaceGuardHTTPTests "ingestion batch is refused before any row is created".
func TestBatchIsRefusedBeforeAnyRowIsCreated(t *testing.T) {
	v := setup(t, gib)
	r := v.do(alice, http.MethodPost, "/v1/ingestions",
		`{"items":[{"kind":"file","fileName":"big.pdf","contentType":"application/pdf","sizeBytes":1048576}]}`)
	require.Equal(t, http.StatusInsufficientStorage, r.code, string(r.body))
	require.Equal(t, "application/json", r.header.Get("Content-Type"))
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.body, &body))
	require.Equal(t, "insufficient_storage", body.Error.Code)
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM ingestion_batches WHERE tenant_id = $1`, alice).Scan(&n))
	require.Zero(t, n)
}

// The guard's other two call sites, which parity's disk can't reach.
func TestChunkAndCompleteAreRefusedOnAFullDisk(t *testing.T) {
	const batchPath = "/v1/ingestions/1A6E3000-0000-4000-8000-000000000001/items/1A6E4000-0000-4000-8000-00000000000A"
	full := setup(t, gib)
	r := full.do(ann, http.MethodPut, batchPath+"/chunks/0", "x")
	require.Equal(t, http.StatusInsufficientStorage, r.code, string(r.body))
	_, err := os.Stat(full.paths.TenantRoot(ann))
	require.ErrorIs(t, err, os.ErrNotExist, "refused before the directory is made")

	// A fully uploaded item whose assembly would need a second copy.
	_, err = full.pool.Exec(context.Background(), `UPDATE ingestion_items SET uploaded_bytes = size_bytes WHERE id = '1a6e4000-0000-4000-8000-00000000000a'`)
	require.NoError(t, err)
	r = full.do(ann, http.MethodPost, batchPath+"/complete", "")
	require.Equal(t, http.StatusInsufficientStorage, r.code, string(r.body))
}

func TestDisabledQuotaAllowsAnySize(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "ingestion.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	paths := vaultpaths.Paths{Root: t.TempDir()}
	guard := diskguard.New(paths.Root, 0, 0, diskguard.Options{
		Provider: func(string) (diskguard.Space, error) {
			return diskguard.Space{FreeBytes: 100 * gib, TotalBytes: 100 * gib}, nil
		},
	})
	m := ingestion.New(pool, auth.Identify(keys, auth.PGUsers(pool)), ratelimit.NewStore(), paths, guard, ingestion.Quota{Limits: ingestion.DefaultLimits()})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{m}})
	v := env{t: t, e: e, pool: pool, paths: paths}
	lapsed := uuid.MustParse("1A6E0000-0000-4000-8000-0000000000A3")
	v.createFileBatch(lapsed, 5)
}

// The source route's bytes and headers, which the worker's tokens depend on. A
// source with no file name reads as "source", which no route can produce.
func TestSourceServesTheFile(t *testing.T) {
	v := setup(t, plentyOfDisk)
	ctx := context.Background()
	const batchPath = "/v1/ingestions/1A6E3000-0000-4000-8000-0000000000B1/items/1A6E4000-0000-4000-8000-0000000000B5"
	require.Equal(t, http.StatusNoContent, v.do(src, http.MethodPut, batchPath+"/chunks/0", "small").code)
	completed := decodeBatch(t, v.do(src, http.MethodPost, batchPath+"/complete", ""))
	var sha string
	for _, it := range completed.Items {
		if it.ContentSHA256 != nil {
			sha = *it.ContentSHA256
		}
	}
	require.NotEmpty(t, sha)
	token := strings.Repeat("e", 64)

	r := v.do(uuid.Nil, http.MethodGet, "/v1/ingestion-sources/"+token, "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Equal(t, "small", string(r.body))
	require.Equal(t, "text/plain", r.header.Get("Content-Type"))
	require.Equal(t, "private, no-store", r.header.Get("Cache-Control"))
	require.Equal(t, "bytes", r.header.Get("Accept-Ranges"))
	require.Equal(t, `"`+sha+`"`, r.header.Get("ETag"))
	require.Equal(t, `attachment; filename="small.txt"`, r.header.Get("Content-Disposition"))
	require.Equal(t, "5", r.header.Get("Content-Length"))

	r = v.do(uuid.Nil, http.MethodHead, "/v1/ingestion-sources/"+token, "")
	require.Equal(t, http.StatusOK, r.code)
	require.Empty(t, r.body)
	require.Equal(t, "5", r.header.Get("Content-Length"))

	r = v.do(uuid.Nil, http.MethodGet, "/v1/ingestion-sources/"+token, "", "Range", "bytes=1-3")
	require.Equal(t, http.StatusPartialContent, r.code)
	require.Equal(t, "mal", string(r.body))
	require.Equal(t, "3", r.header.Get("Content-Length"))
	require.Equal(t, "bytes 1-3/5", r.header.Get("Content-Range"))

	r = v.do(uuid.Nil, http.MethodGet, "/v1/ingestion-sources/"+token, "", "Range", "bytes=9-")
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, r.code)
	require.Empty(t, r.body)
	require.Empty(t, r.header.Get("ETag"))

	_, err := v.pool.Exec(ctx, `UPDATE ingestion_items SET file_name = NULL WHERE id = '1a6e4000-0000-4000-8000-0000000000b5'`)
	require.NoError(t, err)
	r = v.do(uuid.Nil, http.MethodGet, "/v1/ingestion-sources/"+token, "")
	require.Equal(t, `attachment; filename="source"`, r.header.Get("Content-Disposition"))

	// The worker's retry clears the token, and with it the file's URL.
	_, err = v.pool.Exec(ctx, `UPDATE ingestion_items SET source_token_hash = NULL WHERE id = '1a6e4000-0000-4000-8000-0000000000b5'`)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, v.do(uuid.Nil, http.MethodGet, "/v1/ingestion-sources/"+token, "").code)
}

// Frames of a terminal batch: exact bytes, since the iOS and web readers are hand-written.
func TestEventsStreamFrames(t *testing.T) {
	v := setup(t, plentyOfDisk)
	r := v.do(ann, http.MethodGet, "/v1/ingestions/1A6E3000-0000-4000-8000-000000000005/events?after=3", "")
	require.Equal(t, http.StatusOK, r.code)
	require.Equal(t, "text/event-stream", r.header.Get("Content-Type"))
	require.Equal(t, "no-cache", r.header.Get("Cache-Control"))
	require.Equal(t, "no", r.header.Get("X-Accel-Buffering"))
	const batchID = "1A6E3000-0000-4000-8000-000000000005"
	require.Equal(t,
		`id: 4`+"\nevent: ingestion\n"+`data: {"id":4,"batchID":"`+batchID+`","type":"terminal","createdAt":"2026-02-06T00:00:04Z"}`+"\n\n"+
			`id: 5`+"\nevent: ingestion\n"+`data: {"id":5,"batchID":"`+batchID+`","type":"snapshot","uploadedBytes":0,"createdAt":"2026-02-06T00:00:05Z"}`+"\n\n"+
			`id: 6`+"\nevent: ingestion\n"+`data: {"id":6,"batchID":"`+batchID+`","type":"heartbeat","state":"completed","createdAt":"2026-02-06T00:00:06Z"}`+"\n\n",
		string(r.body))
}

func TestDetailProjectsTheGraph(t *testing.T) {
	v := setup(t, plentyOfDisk)
	r := v.do(gra, http.MethodGet, "/v1/ingestions/1A6E3000-0000-4000-8000-0000000000D1", "")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var got struct {
		State     string `json:"state"`
		Total     int    `json:"total"`
		Completed int    `json:"completed"`
		Failed    int    `json:"failed"`
		Items     []struct {
			State    string `json:"state"`
			Entities []struct {
				Name string `json:"name"`
			} `json:"entities"`
			Relationships []struct {
				Predicate string `json:"predicate"`
				Evidence  string `json:"evidence"`
			} `json:"relationships"`
			Credibility *struct {
				Score      *int    `json:"score"`
				Confidence float64 `json:"confidence"`
			} `json:"credibility"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(r.body, &got))
	require.Equal(t, 3, got.Total)
	require.Equal(t, 1, got.Completed)
	require.Equal(t, 1, got.Failed, "the state the enum doesn't know reads as failed")
	first := got.Items[0]
	require.Len(t, first.Entities, 2)
	require.Equal(t, "Alpha", first.Entities[0].Name)
	require.Len(t, first.Relationships, 1, "an edge to a claim is dropped")
	require.Equal(t, "the first quote", first.Relationships[0].Evidence)
	require.Equal(t, 72, *first.Credibility.Score)
	require.Equal(t, "failed", got.Items[1].State)
	require.Nil(t, got.Items[2].Credibility.Score)
}

func TestInvalidItemLeavesTheRowsBeforeIt(t *testing.T) {
	v := setup(t, plentyOfDisk)
	r := v.do(alice, http.MethodPost, "/v1/ingestions",
		`{"items":[{"kind":"url","url":"https://kept.test"},{"kind":"url","url":"ftp://nope"},{"kind":"url","url":"https://never.test"}]}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.JSONEq(t, `{"error":{"message":"invalid URL ingestion item"}}`, string(r.body))
	var batches, items int
	ctx := context.Background()
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM ingestion_batches WHERE tenant_id = $1`, alice).Scan(&batches))
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM ingestion_items WHERE tenant_id = $1`, alice).Scan(&items))
	require.Equal(t, 1, batches)
	require.Equal(t, 1, items)
}

func TestChunkOverTheDeclaredSizeStaysOnDiskUncounted(t *testing.T) {
	v := setup(t, plentyOfDisk)
	const itemPath = "/v1/ingestions/1A6E3000-0000-4000-8000-000000000001/items/1A6E4000-0000-4000-8000-000000000004"
	r := v.do(ann, http.MethodPut, itemPath+"/chunks/0", "abcde")
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code)
	require.JSONEq(t, `{"error":{"message":"uploaded bytes exceed declared size"}}`, string(r.body))
	_, err := os.Stat(filepath.Join(v.paths.TenantRoot(ann), "tmp", "ingestion", "1A6E4000-0000-4000-8000-000000000004", "0"))
	require.NoError(t, err)
	var uploaded int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT uploaded_bytes FROM ingestion_items WHERE id = '1a6e4000-0000-4000-8000-000000000004'`).Scan(&uploaded))
	require.Zero(t, uploaded)
	require.Equal(t, http.StatusNoContent, v.do(ann, http.MethodPut, itemPath+"/chunks/0", "abcde").code)
}

func TestChunkDirectoryHoldsOnlyChunks(t *testing.T) {
	v := setup(t, plentyOfDisk)
	const itemPath = "/v1/ingestions/1A6E3000-0000-4000-8000-000000000001/items/1A6E4000-0000-4000-8000-000000000001"
	for index, body := range map[string]string{"10": "cccc", "0": "aaaa", "2": "bbbb"} {
		require.Equal(t, http.StatusNoContent, v.do(ann, http.MethodPut, itemPath+"/chunks/"+index, body).code)
	}
	root := filepath.Join(v.paths.TenantRoot(ann), "tmp", "ingestion")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file is left beside the item's directory")
	info, err := os.Stat(filepath.Join(root, entries[0].Name(), "0"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	completed := decodeBatch(t, v.do(ann, http.MethodPost, itemPath+"/complete", ""))
	var path string
	for _, it := range completed.Items {
		if it.ID == "1A6E4000-0000-4000-8000-000000000001" {
			require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT path FROM vault_files WHERE id = $1`, *it.VaultFileID).Scan(&path))
		}
	}
	raw, err := os.ReadFile(filepath.Join(v.paths.Raw(ann), path))
	require.NoError(t, err)
	require.Equal(t, "aaaabbbbcccc", string(raw), "numeric order, not text order")
}

// Every success body must validate against api/openapi.yaml (decisions.md D2).
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, plentyOfDisk)
	const itemPath = "/v1/ingestions/1A6E3000-0000-4000-8000-000000000002/items/1A6E4000-0000-4000-8000-000000000010"
	require.Equal(t, http.StatusNoContent, v.do(ann, http.MethodPut, itemPath+"/chunks/0", "abc").code)
	for _, c := range []struct {
		user         uuid.UUID
		method, path string
		body         string
	}{
		{ann, http.MethodPost, "/v1/ingestions", `{"items":[{"kind":"url","url":"https://example.test"}]}`},
		{ann, http.MethodGet, "/v1/ingestions", ""},
		{gra, http.MethodGet, "/v1/ingestions", ""},
		{gra, http.MethodGet, "/v1/ingestions/1A6E3000-0000-4000-8000-0000000000D1", ""},
		{ann, http.MethodPost, itemPath + "/complete", ""},
		{ann, http.MethodPost, "/v1/ingestions/1A6E3000-0000-4000-8000-000000000003/items/1A6E4000-0000-4000-8000-000000000020/retry", ""},
		{ann, http.MethodDelete, itemPath, ""},
	} {
		r := v.do(c.user, c.method, c.path, c.body)
		require.Equal(t, http.StatusOK, r.code, "%s %s: %s", c.method, c.path, r.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.header.Get("Content-Type")}},
		}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}

// Swift's usedBytes reads as 0 (see usedBytes), so only the new bytes meet the
// ceiling. ing_fay stores 1073741000 B of her 1 GiB, ing_pam 99 GiB of his 100.
func TestWhatATenantStoresDoesNotCountAgainstTheCeiling(t *testing.T) {
	v := setup(t, plentyOfDisk)
	fay := uuid.MustParse("1A6E0000-0000-4000-8000-0000000000A4")
	pam := uuid.MustParse("1A6E0000-0000-4000-8000-0000000000A5")
	create := func(user uuid.UUID, size string) resp {
		return v.do(user, http.MethodPost, "/v1/ingestions",
			`{"items":[{"kind":"file","fileName":"a.txt","contentType":"text/plain","sizeBytes":`+size+`}]}`)
	}
	require.Equal(t, http.StatusOK, create(fay, "825").code)
	require.Equal(t, http.StatusOK, create(pam, "2147483648").code)

	r := create(fay, "1073741825")
	require.Equal(t, http.StatusRequestEntityTooLarge, r.code)
	require.JSONEq(t, `{"error":{"message":"storage quota exceeded: 0 B of 1.0 GiB used"}}`, string(r.body))
}

// Over a real connection: the recorder never sets net/http's Content-Length.
func TestSourceLengthUnderGzip(t *testing.T) {
	v := setup(t, plentyOfDisk)
	const itemPath = "/v1/ingestions/1A6E3000-0000-4000-8000-0000000000B1/items/1A6E4000-0000-4000-8000-0000000000B1"
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.Equal(t, http.StatusNoContent, v.do(src, http.MethodPut, itemPath+"/chunks/0", strings.Repeat("x", 1024)).code)
	require.Equal(t, http.StatusOK, v.do(src, http.MethodPost, itemPath+"/complete", "").code)
	srv := httptest.NewServer(v.e)
	defer srv.Close()
	get := func(method string, hdr ...string) (int, http.Header) {
		req, err := http.NewRequest(method, srv.URL+"/v1/ingestion-sources/"+token, http.NoBody)
		require.NoError(t, err)
		req.Header.Set("Accept-Encoding", "gzip")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode, res.Header
	}
	_, whole := get(http.MethodGet)
	require.Equal(t, "gzip", whole.Get("Content-Encoding"))
	require.Empty(t, whole.Get("Content-Length"))
	status, ranged := get(http.MethodGet, "Range", "bytes=0-599")
	require.Equal(t, http.StatusPartialContent, status)
	require.Equal(t, "gzip", ranged.Get("Content-Encoding"))
	require.Empty(t, ranged.Get("Content-Length"))
	_, short := get(http.MethodGet, "Range", "bytes=0-9")
	require.Empty(t, short.Get("Content-Encoding"))
	require.Equal(t, "10", short.Get("Content-Length"))
	_, head := get(http.MethodHead)
	require.Empty(t, head.Get("Content-Encoding"))
	require.Equal(t, "1024", head.Get("Content-Length"))
}
