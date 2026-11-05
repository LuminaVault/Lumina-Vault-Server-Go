// Package vault serves /v1/vault: the Go port of LuminaVaultServer Sources/App/Vault
// (VaultController, VaultIndexController, VaultInitService, VaultExportService)
// and of the Indexing services behind the read-only routes. A vault file is a
// vault_files row plus a file under <vault>/tenants/<vault id>/raw on the shared PVC.
//
// The Skills EventBus event vault_file_created is dropped: its one subscriber
// only logs (docs/migration/coupling.md).
package vault

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/vault/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/achievementsink"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/idempotency"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/modules/spaces"
	"lumina/internal/modules/vault/db"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	filesPath   = "/v1/vault/files"
	filesPrefix = 3 // segments of filesPath before a catch-all path
)

// SpaceSeeder is spaces.Module.SeedDefaults.
type SpaceSeeder interface {
	SeedDefaults(ctx context.Context, vault uuid.UUID) ([]string, error)
}

// Options are the module's dependencies. Everything but Paths may be left out by
// a test: as in Swift, a missing Guard checks nothing, and Sink, Latches and
// Analytics do nothing when nil.
type Options struct {
	Paths          vaultpaths.Paths
	HermesDataRoot string
	Identify       echo.MiddlewareFunc
	Limits         *ratelimit.Store
	Spaces         SpaceSeeder
	Embeddings     embedding.Service
	Guard          *diskguard.Guard
	Sink           *achievementsink.Sink
	Latches        *onboardlatch.Latches
	// CaptureFiling suggests a Space for an unfiled note after upload. Nil, the default
	// and whenever capture.autoFile.enabled is off, means no suggestion.
	CaptureFiling func(tenant, file uuid.UUID)
	Analytics     *posthog.Client
	// MaxFileSize is VaultController.maxFileSize, 10 MiB unless a test lowers it.
	MaxFileSize int64
}

const defaultMaxFileSize = 10 * 1024 * 1024

type Module struct {
	pool       *pgxpool.Pool
	q          *db.Queries
	access     *vaultaccess.Service
	store      *vaultfiles.Store
	memories   *memorystore.Repository
	links      *chunkindex.LinkRepository
	paths      vaultpaths.Paths
	hermesRoot string
	identify   echo.MiddlewareFunc
	limits     *ratelimit.Store
	spaces     SpaceSeeder
	embeddings embedding.Service
	guard      *diskguard.Guard
	sink       *achievementsink.Sink
	latches    *onboardlatch.Latches
	analytics  *posthog.Client
	filing     func(tenant, file uuid.UUID)
	maxFile    int64
	now        func() time.Time
}

func New(pool *pgxpool.Pool, opts Options) *Module {
	maxFile := opts.MaxFileSize
	if maxFile <= 0 {
		maxFile = defaultMaxFileSize
	}
	return &Module{
		pool:       pool,
		q:          db.New(pool),
		access:     vaultaccess.New(pool),
		store:      vaultfiles.NewStore(pool, opts.Paths),
		memories:   memorystore.New(pool, nil),
		links:      chunkindex.NewLinkRepository(pool),
		paths:      opts.Paths,
		hermesRoot: opts.HermesDataRoot,
		identify:   opts.Identify,
		limits:     opts.Limits,
		spaces:     opts.Spaces,
		embeddings: opts.Embeddings,
		guard:      opts.Guard,
		sink:       opts.Sink,
		latches:    opts.Latches,
		analytics:  opts.Analytics,
		filing:     opts.CaptureFiling,
		maxFile:    maxFile,
		now:        time.Now,
	}
}

// Register mirrors App+build.swift's four /v1/vault groups, middleware in Swift's
// order. Every group sits behind auth.Identify: Swift's authenticator never
// rejects, so the 401 comes from the idempotency check or the handler.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	idem := idempotency.New(m.pool, idempotency.Options{})
	uploadLimit := ratelimit.Middleware(m.limits, ratelimit.VaultUploadByUser, ratelimit.UserOrIPKey)
	exportLimit := ratelimit.TieredMiddleware(m.limits, ratelimit.VaultExportByUser, ratelimit.UserOrIPKey, m.tier)

	files := []echo.MiddlewareFunc{m.identify, idem, uploadLimit}
	r.POST(filesPath, m.upload, files...)
	r.GET(filesPath, m.list, files...)
	r.POST(filesPath+"/move", m.move, files...)
	r.GET(filesPath+"/*", m.read, files...)
	r.DELETE(filesPath+"/*", m.delete, files...)
	// Hummingbird's trie picks the constant "move" over "**", and that node has
	// only POST, so these two are 404 and not a read of a file called "move".
	r.GET(filesPath+"/move", notFound)
	r.DELETE(filesPath+"/move", notFound)

	r.POST("/v1/vault/create", m.create, m.identify, idem)
	r.GET("/v1/vault/status", m.status, m.identify, idem)

	r.GET("/v1/vault/export", m.export, m.identify, exportLimit)

	r.GET("/v1/vault/index", m.indexStatus, m.identify)
	r.GET("/v1/vault/context/:vaultFileID", m.context, m.identify)
	r.GET("/v1/vault/links/:vaultFileID", m.documentLinks, m.identify)
	r.GET("/v1/vault/lint", m.lint, m.identify)
}

func notFound(*echo.Context) error { return echo.ErrNotFound }

// requireIdentity is ctx.requireIdentity() in a handler.
func requireIdentity(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, auth.ErrUnauthorized
	}
	return id, nil
}

// tier is the identified caller's users.tier, for the export limit.
func (m *Module) tier(c *echo.Context) (string, bool) {
	id, ok := auth.Identity(c)
	if !ok {
		return "", false
	}
	tier, err := m.q.UserTier(c.Request().Context(), id)
	if err != nil {
		return "", false
	}
	return tier, true
}

// parseISODate is VaultController.parseISODate: anything else is no filter at all.
func parseISODate(raw string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	return &t
}

// catchAll is `ctx.parameters.getCatchAll().joined(separator: "/")`: raw, with
// empty segments gone. Echo's "*" can't stand in for it, because Echo hands over a
// decoded value whenever the decoded path re-encodes to the original.
func catchAll(c *echo.Context) string {
	segments := wire.PathSegments(c.Request())
	if len(segments) <= filesPrefix {
		return ""
	}
	return strings.Join(segments[filesPrefix:], "/")
}

// parseVaultFileID reads the raw last segment, for the same reason as catchAll.
func parseVaultFileID(c *echo.Context) (uuid.UUID, error) {
	segments := wire.PathSegments(c.Request())
	if len(segments) > 0 {
		if id, err := ids.Parse(segments[len(segments)-1]); err == nil {
			return id, nil
		}
	}
	return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid vaultFileID")
}

// create is VaultInitService.create. Idempotent: an initialized vault only
// reports its status, and a failed attempt can be repeated.
func (m *Module) create(c *echo.Context) error {
	id, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	user, err := m.q.User(ctx, id)
	if err != nil {
		return userError(err)
	}
	if user.VaultInitialized {
		return wire.JSON(c, http.StatusOK, statusOf(user))
	}
	if err := m.paths.EnsureTenantDirectories(id); err != nil {
		return err
	}
	if err := m.initSoulIfMissing(id, user.Username, m.now()); err != nil {
		slog.ErrorContext(ctx, "vault.init soul-failed", "tenant", ids.Upper(id), "error", err)
		return wire.NewError(http.StatusServiceUnavailable, "could not initialize SOUL.md")
	}
	seeded, err := m.spaces.SeedDefaults(ctx, id)
	if err != nil {
		return err
	}
	updatedAt, err := m.q.MarkInitialized(ctx, db.MarkInitializedParams{ID: id, UpdatedAt: ptr(m.now())})
	if err != nil {
		return err
	}
	// Best effort: the global HNSW index still serves searches without this one.
	if err := m.ensureVectorIndex(ctx, id); err != nil {
		slog.ErrorContext(ctx, "vault.init.hnsw-index-failed", "tenant", ids.Upper(id), "error", err)
	}
	slog.InfoContext(ctx, "vault.created", "tenant", ids.Upper(id), "seeded", len(seeded))
	return wire.JSON(c, http.StatusOK, VaultStatusResponse{
		Initialized:       true,
		CreatedAt:         wireTime(updatedAt),
		DefaultSpaceSlugs: spaces.DefaultSlugs(),
	})
}

// status is VaultInitService.status.
func (m *Module) status(c *echo.Context) error {
	id, err := requireIdentity(c)
	if err != nil {
		return err
	}
	user, err := m.q.User(c.Request().Context(), id)
	if err != nil {
		return userError(err)
	}
	return wire.JSON(c, http.StatusOK, statusOf(user))
}

func statusOf(u db.UserRow) VaultStatusResponse {
	if !u.VaultInitialized {
		return VaultStatusResponse{DefaultSpaceSlugs: []string{}}
	}
	return VaultStatusResponse{
		Initialized:       true,
		CreatedAt:         wireTime(u.UpdatedAt),
		DefaultSpaceSlugs: spaces.DefaultSlugs(),
	}
}

// userError: auth saw this user a moment ago, so a missing row is a user deleted
// since, which Swift's authenticator would have refused.
func userError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.ErrUnauthorized
	}
	return err
}

// indexName is TenantVectorIndexService.indexName: 19 + 32 characters, always
// quote-safe, and uppercase, because a quoted identifier keeps its case and
// Swift's uuidString is uppercase.
func indexName(tenant uuid.UUID) string {
	return "idx_memories_emb_t_" + strings.ReplaceAll(ids.Upper(tenant), "-", "")
}

// ensureVectorIndex is TenantVectorIndexService.ensureIndex, the same DDL Swift
// runs. CONCURRENTLY can't run in a transaction. Both identifiers come from a
// UUID, so interpolating them is safe, and a DDL statement takes no parameters.
func (m *Module) ensureVectorIndex(ctx context.Context, tenant uuid.UUID) error {
	_, err := m.pool.Exec(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS "`+indexName(tenant)+`"
		ON memories
		USING hnsw (embedding vector_cosine_ops)
		WITH (m = 16, ef_construction = 64)
		WHERE tenant_id = '`+ids.Upper(tenant)+`'::uuid`)
	return err
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func wireUUID(id *uuid.UUID) *wire.UUID {
	if id == nil {
		return nil
	}
	return &wire.UUID{UUID: *id}
}

func ptr[T any](v T) *T { return &v }
