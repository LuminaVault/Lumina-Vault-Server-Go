// Package memory serves /v1/memory and /v1/me/suggestions: the Go port of
// LuminaVaultServer Sources/App/Memory (MemoryController, MemoryGraphService,
// LocalMemorySyncCursor, SuggestionsController). POST /v1/memory/search runs the agent
// loop and stays on Swift until wave-4-agent (modules.yaml heldPaths).
package memory

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/memory/oapi-codegen.yaml api/openapi.yaml"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/memory/db"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	defaultLimit = 20
	maxLimit     = 100
	// reviewBodyLimit is reviewAction's collect(upTo: 4096).
	reviewBodyLimit = 4096
	// reviewSoon and reviewLater are how far off "show me this again" is.
	reviewSoon  = 3 * 24 * time.Hour
	reviewLater = 30 * 24 * time.Hour
	// DefaultRelatedMaxDistance is memory.related.maxDistance's default.
	DefaultRelatedMaxDistance float32 = 0.45
)

var (
	errNotFound       = wire.NewError(http.StatusNotFound, "memory not found")
	errBadID          = wire.NewError(http.StatusBadRequest, "invalid memory id")
	errContent        = wire.NewError(http.StatusBadRequest, "content required")
	errContentEmpty   = wire.NewError(http.StatusBadRequest, "content cannot be empty")
	errSpaceIDInvalid = wire.NewError(http.StatusBadRequest, "`space_id` is not a valid UUID")
	errSpaceIDForeign = wire.NewError(http.StatusBadRequest, "`space_id` does not belong to the caller")
	errSpaceForeign   = wire.NewError(http.StatusBadRequest, "`spaceId` does not belong to the caller")
	errPatchEmpty     = wire.NewError(http.StatusBadRequest, "patch body must include content, tags, reviewState, kind, or spaceId")
	errReviewBody     = wire.NewError(http.StatusBadRequest, `body must be {"action": "kept|soon|later|never"}`)
	errBadCursor      = wire.NewError(http.StatusBadRequest, "invalid local memory sync cursor")
	errHealthFilter   = wire.NewError(http.StatusBadRequest, "healthFilter must be one of review-overdue, pending, unorganized, unused")
	errReviewState    = wire.NewError(http.StatusBadRequest, `reviewState must be a comma-separated subset of ["auto", "pending", "approved", "rejected"]`)
	errKind           = wire.NewError(http.StatusBadRequest, "kind must be one of preference, decision, fact, note")
)

// suggestions is SuggestionsController.defaults, a fixed list until HER-37a.
var suggestions = []string{
	"What patterns do I have in my Stocks space lately?",
	"Summarize everything I learned about sleep this month",
	"Connect my recent travel notes with my health data",
	"What ideas have I had about AI agents in the last 3 months?",
	"Where did I leave off on my Hermes project?",
}

// AchievementSink is the achievements worker's inbox (achievementsink.Sink).
type AchievementSink interface {
	Enqueue(tenant uuid.UUID, event achievements.Event)
}

// Preferences is UserLLMPreferenceRepository, read for model disclosure.
type Preferences interface {
	Get(ctx context.Context, tenant uuid.UUID) (*llm.Preference, error)
}

// Options are the module's dependencies beyond the pool.
type Options struct {
	Identify echo.MiddlewareFunc
	Limits   *ratelimit.Store
	Paths    vaultpaths.Paths
	// Embeddings embeds on upsert and on a content patch.
	Embeddings embedding.Service
	// Indexer rebuilds a corrected memory's chunks. Nil skips that, as a Swift test
	// wiring does.
	Indexer *chunkindex.Indexer
	// Preferences nil means every tenant is managed: models stay hidden.
	Preferences Preferences
	// Hermes is HermesResolutionMiddleware on the capture group. Nil without
	// LV_SECRET_MASTER_KEY.
	Hermes echo.MiddlewareFunc
	// CaptureGate is EntitlementMiddleware(requires: .capture).
	CaptureGate echo.MiddlewareFunc
	// Achievements nil drops the event.
	Achievements AchievementSink
	// OnUpserted is EventBus memory_upserted. Its one real subscriber is MeTodayCache.
	OnUpserted func(tenant uuid.UUID)
	// HybridExecutionEnabled is hybridExecution.enabled: it mounts /local-sync.
	HybridExecutionEnabled bool
	// RelatedMaxDistance is memory.related.maxDistance.
	RelatedMaxDistance float32
}

// Module is the memory route group.
type Module struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	repo   *memorystore.Repository
	files  *vaultfiles.Store
	access *vaultaccess.Service
	opts   Options
	now    func() time.Time
}

// New builds the module.
func New(pool *pgxpool.Pool, repo *memorystore.Repository, opts Options) *Module {
	return &Module{
		pool:   pool,
		q:      db.New(pool),
		repo:   repo,
		files:  vaultfiles.NewStore(pool, opts.Paths),
		access: vaultaccess.New(pool),
		opts:   opts,
		now:    time.Now,
	}
}

// SetClock replaces the clock (tests).
func (m *Module) SetClock(now func() time.Time) { m.now = now }

// Register mounts Swift's three groups. Capture: JWT, BYO Hermes, entitlement, then
// the capture limit. Suggestions: JWT and the limit it shares with the rest of
// /v1/me. Read: JWT only. Each handler requires identity itself, in Swift's order.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	capture := []echo.MiddlewareFunc{m.opts.Identify}
	if m.opts.Hermes != nil {
		capture = append(capture, m.opts.Hermes)
	}
	if m.opts.CaptureGate != nil {
		capture = append(capture, m.opts.CaptureGate)
	}
	capture = append(capture, ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.CaptureByUser, ratelimit.UserOrIPKey, m.tier))
	r.POST("/v1/memory/upsert", m.upsert, capture...)

	// The handler never asks for identity, so a request with no token gets the list.
	r.GET("/v1/me/suggestions", m.suggestions, m.opts.Identify,
		ratelimit.Middleware(m.opts.Limits, ratelimit.MeTodayByUser, ratelimit.UserOrIPKey))

	r.GET("/v1/memory", m.list, m.opts.Identify)
	r.GET("/v1/memory/graph", m.graph, m.opts.Identify)
	r.GET("/v1/memory/facets", m.facets, m.opts.Identify)
	if m.opts.HybridExecutionEnabled {
		r.GET("/v1/memory/local-sync", m.localSync, m.opts.Identify)
	}
	r.GET("/v1/memory/:id", m.get, m.opts.Identify)
	r.GET("/v1/memory/:id/lineage", m.lineage, m.opts.Identify)
	r.GET("/v1/memory/:id/related", m.related, m.opts.Identify)
	r.GET("/v1/memory/:id/provenance", m.provenance, m.opts.Identify)
	r.POST("/v1/memory/:id/review", m.review, m.opts.Identify)
	r.DELETE("/v1/memory/:id", m.delete, m.opts.Identify)
	r.PATCH("/v1/memory/:id", m.patch, m.opts.Identify)
}

// tier is the identified caller's users.tier for the tiered limit.
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

func parseID(c *echo.Context) (uuid.UUID, error) {
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, errBadID
	}
	return id, nil
}

// visible is MemoryController.disclosure: only a BYOK tenant sees model identity. It
// reads the caller's preference, not the vault owner's.
func (m *Module) visible(c *echo.Context) (bool, error) {
	if m.opts.Preferences == nil {
		return false, nil
	}
	user, ok := auth.Identity(c)
	if !ok {
		return false, wire.ErrUnauthenticated
	}
	pref, err := m.opts.Preferences.Get(c.Request().Context(), user)
	if err != nil {
		return false, err
	}
	return pref != nil && pref.Mode == llm.ModeBYOK, nil
}

func (m *Module) suggestions(c *echo.Context) error {
	return wire.JSON(c, http.StatusOK, SuggestionsResponse{Suggestions: suggestions})
}

// MARK: POST /v1/memory/upsert

// upsert is a deterministic save: embed, then persist. An embedding failure saves the
// note without a vector for the heal pass, which stays in Swift.
func (m *Module) upsert(c *echo.Context) error {
	actor, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[upsertRequest](c)
	if err != nil {
		return err
	}
	content := swiftstr.TrimWhitespacesAndNewlines(body.Content)
	if content == "" {
		return errContent
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	ctx := c.Request().Context()
	space, err := m.resolveOptionalSpace(c, tenant)
	if err != nil {
		return err
	}
	vector, err := embedding.EmbedForWrite(ctx, m.opts.Embeddings, content, tenant, "memory.upsert")
	if err != nil {
		return err
	}
	contribution := memorystore.UserContribution(memorystore.OperationCreate, memorystore.SourceManual, nil)
	saved, err := m.repo.Create(ctx, memorystore.CreateParams{
		Tenant:       tenant,
		Content:      content,
		Embedding:    vector,
		SpaceID:      space,
		Contribution: &contribution,
		Kind:         (*string)(body.Kind),
	})
	if err != nil {
		return err
	}
	if err := m.repo.StampCreator(ctx, saved, actor); err != nil {
		return err
	}
	if body.Lat != nil || body.Lng != nil || body.AccuracyM != nil || body.PlaceName != nil {
		geo := memorystore.Geo{Lat: body.Lat, Lng: body.Lng, AccuracyM: body.AccuracyM, PlaceName: body.PlaceName}
		if err := m.repo.SetGeo(ctx, saved, geo); err != nil {
			return err
		}
	}
	key := "memory-created:" + ids.Upper(saved.ID)
	if err := m.q.InsertMemoryCreatedEvent(ctx, db.InsertMemoryCreatedEventParams{VaultID: tenant, ActorUserID: actor, IdempotencyKey: &key}); err != nil {
		slog.DebugContext(ctx, "memory_created analytics insert failed", "error", err)
	}
	if m.opts.OnUpserted != nil {
		m.opts.OnUpserted(tenant)
	}
	if m.opts.Achievements != nil {
		m.opts.Achievements.Enqueue(tenant, achievements.EventMemoryUpserted)
	}
	return wire.JSON(c, http.StatusOK, upsertResponse{
		MemoryID: wire.UUID{UUID: saved.ID},
		Content:  saved.Content,
		Summary:  "Saved to your vault.",
	})
}

// resolveOptionalSpace is resolveOptionalSpaceID on ?space_id=. Absent or empty is
// unfiled; malformed or another tenant's is a 400.
func (m *Module) resolveOptionalSpace(c *echo.Context, tenant uuid.UUID) (*uuid.UUID, error) {
	raw := wire.Query(c, "space_id")
	if raw == "" {
		return nil, nil
	}
	id, err := ids.Parse(raw)
	if err != nil {
		return nil, errSpaceIDInvalid
	}
	if _, err := m.q.SpaceByID(c.Request().Context(), db.SpaceByIDParams{TenantID: tenant, ID: id}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errSpaceIDForeign
		}
		return nil, err
	}
	return &id, nil
}

// MARK: GET /v1/memory

func (m *Module) list(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	ctx := c.Request().Context()
	params := memorystore.ListParams{Tenant: tenant}

	// ?space=<slug> lists one Space; ?space=inbox lists unfiled memories.
	if raw := wire.Query(c, "space"); raw != "" {
		if raw == "inbox" {
			params.Space = &memorystore.SpaceFilter{Unfiled: true}
		} else {
			id, err := m.q.SpaceIDBySlug(ctx, db.SpaceIDBySlugParams{TenantID: tenant, Slug: raw})
			if errors.Is(err, pgx.ErrNoRows) {
				return wire.NewError(http.StatusNotFound, "unknown space `"+raw+"`")
			}
			if err != nil {
				return err
			}
			params.Space = &memorystore.SpaceFilter{ID: id}
		}
	}

	limit, ok := wire.QueryInt(c, "limit")
	if !ok {
		limit = defaultLimit
	}
	params.Limit = min(max(limit, 1), maxLimit)
	offset, _ := wire.QueryInt(c, "offset")
	params.Offset = max(0, offset)
	params.Tag = wire.Query(c, "tag")

	if raw := wire.Query(c, "healthFilter"); raw != "" {
		switch h := memorystore.HealthFilter(raw); h {
		case memorystore.HealthReviewOverdue, memorystore.HealthPending, memorystore.HealthUnorganized, memorystore.HealthUnused:
			params.Health = h
		default:
			return errHealthFilter
		}
	}
	if raw := wire.Query(c, "reviewState"); raw != "" {
		for part := range strings.SplitSeq(raw, ",") {
			switch state := swiftstr.TrimWhitespaces(part); state {
			case memorystore.ReviewAuto, memorystore.ReviewPending, memorystore.ReviewApproved, memorystore.ReviewRejected:
				params.ReviewStates = append(params.ReviewStates, state)
			}
		}
		if len(params.ReviewStates) == 0 {
			return errReviewState
		}
	}
	if raw := wire.Query(c, "kind"); raw != "" {
		if !validKind(raw) {
			return errKind
		}
		params.Kind = &raw
	}

	rows, err := m.repo.ListPaginated(ctx, params)
	if err != nil {
		return err
	}
	memoryIDs := make([]uuid.UUID, len(rows))
	for i := range rows {
		memoryIDs[i] = rows[i].ID
	}
	summaries, err := m.repo.Summaries(ctx, tenant, memoryIDs)
	if err != nil {
		return err
	}
	visible, err := m.visible(c)
	if err != nil {
		return err
	}
	out := listResponse{Memories: make([]memoryDTO, 0, len(rows)), Limit: params.Limit, Offset: params.Offset}
	for i := range rows {
		out.Memories = append(out.Memories, m.dto(&rows[i], summaries, visible))
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) dto(row *memorystore.Memory, summaries map[uuid.UUID]memorystore.ProvenanceSummary, visible bool) memoryDTO {
	var summary *memorystore.ProvenanceSummary
	if s, ok := summaries[row.ID]; ok {
		summary = &s
	}
	out := toDTO(row, summary)
	if !visible {
		scrubSummary(out.Provenance)
	}
	return out
}

// respond is the tail shared by get and patch: the summary, the DTO, the scrub.
func (m *Module) respond(c *echo.Context, tenant uuid.UUID, row *memorystore.Memory) error {
	summaries, err := m.repo.Summaries(c.Request().Context(), tenant, []uuid.UUID{row.ID})
	if err != nil {
		return err
	}
	visible, err := m.visible(c)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, m.dto(row, summaries, visible))
}

// MARK: GET /v1/memory/:id

func (m *Module) get(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	row, err := m.repo.Find(c.Request().Context(), a.VaultID, id)
	if err != nil {
		return err
	}
	if row == nil {
		return errNotFound
	}
	return m.respond(c, a.VaultID, row)
}

// MARK: DELETE /v1/memory/:id

func (m *Module) delete(c *echo.Context) error {
	id, err := parseID(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	deleted, err := m.repo.Forget(c.Request().Context(), a.VaultID, id)
	if err != nil {
		return err
	}
	if !deleted {
		return errNotFound
	}
	return c.NoContent(http.StatusNoContent)
}

// MARK: POST /v1/memory/:id/review

func (m *Module) review(c *echo.Context) error {
	id, err := parseID(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	action, err := reviewAction(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	row, err := m.repo.Find(ctx, a.VaultID, id)
	if err != nil {
		return err
	}
	if row == nil {
		return errNotFound
	}
	actor, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	now := m.now()
	row.LastReviewedAt = &now
	row.ReviewCount++
	applyReviewAction(action, row, now)
	row.UpdatedByUserID = &actor
	if err := m.repo.SaveReview(ctx, row); err != nil {
		return err
	}
	if err := m.q.InsertMemoryReviewedEvent(ctx, db.InsertMemoryReviewedEventParams{VaultID: a.VaultID, ActorUserID: actor}); err != nil {
		slog.DebugContext(ctx, "memory_reviewed analytics insert failed", "error", err)
	}
	muted := row.ResurfaceMuted
	return wire.JSON(c, http.StatusOK, MemoryReviewResponse{
		MemoryId:       wire.UUID{UUID: id},
		ReviewedAt:     wire.Time{Time: now},
		ReviewCount:    int(row.ReviewCount),
		NextReviewAt:   wireTimePtr(row.NextReviewAt),
		ResurfaceMuted: &muted,
	})
}

// applyReviewAction is applyReviewAction. Asking to see it again is an explicit yes,
// so it also lifts an earlier never.
func applyReviewAction(action MemoryReviewAction, row *memorystore.Memory, now time.Time) {
	switch action {
	case Soon, Later:
		next := now.Add(reviewSoon)
		if action == Later {
			next = now.Add(reviewLater)
		}
		row.NextReviewAt, row.ResurfaceMuted = &next, false
	case Never:
		row.NextReviewAt, row.ResurfaceMuted = nil, true
	default:
		row.NextReviewAt = nil
	}
}

// reviewAction is reviewAction: an absent or blank body is kept, anything else must
// decode as MemoryReviewRequest. Blank is judged byte by byte as a Latin-1 scalar, so
// 0x85 and 0xA0 count too.
func reviewAction(c *echo.Context) (MemoryReviewAction, error) {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, reviewBodyLimit+1))
	if err != nil {
		return "", err
	}
	if len(raw) > reviewBodyLimit {
		return "", wire.ErrBodyTooLarge
	}
	if isAllBlankBytes(raw) {
		return Kept, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", errReviewBody
	}
	var action string
	if err := json.Unmarshal(obj["action"], &action); err != nil {
		return "", errReviewBody
	}
	switch a := MemoryReviewAction(action); a {
	case Kept, Soon, Later, Never:
		return a, nil
	}
	return "", errReviewBody
}

func isAllBlankBytes(raw []byte) bool {
	for _, b := range raw {
		switch b {
		case '\t', '\n', 0x0B, 0x0C, '\r', ' ', 0x85, 0xA0:
		default:
			return false
		}
	}
	return true
}

// MARK: PATCH /v1/memory/:id

func (m *Module) patch(c *echo.Context) error {
	id, err := parseID(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	raw, err := wire.ReadBody(c)
	if err != nil {
		return err
	}
	c.Request().Body = io.NopCloser(bytes.NewReader(raw))
	body, err := wire.Bind[patchRequest](c)
	if err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	_, moves := keys["spaceId"]

	if body.Content == nil && body.Tags == nil && body.ReviewState == nil && body.Kind == nil && !moves {
		return errPatchEmpty
	}
	ctx := c.Request().Context()
	userUpdate := memorystore.UserContribution(memorystore.OperationUpdate, memorystore.SourceManual, nil)
	if moves {
		var target *uuid.UUID
		if body.SpaceID != nil {
			target = &body.SpaceID.UUID
		}
		if err := m.moveToSpace(ctx, target, id, tenant, userUpdate); err != nil {
			return err
		}
	}

	if body.Content != nil {
		if *body.Content == "" {
			return errContentEmpty
		}
		vector, err := embedding.EmbedForWrite(ctx, m.opts.Embeddings, *body.Content, tenant, "memory.patch")
		if err != nil {
			return err
		}
		updated, err := m.repo.UpdateContent(ctx, tenant, id, *body.Content, vector, &userUpdate)
		if err != nil {
			return err
		}
		if !updated {
			return errNotFound
		}
	}

	if body.Tags != nil {
		var contribution *memorystore.Contribution
		if body.Content == nil {
			contribution = &userUpdate
		}
		updated, err := m.repo.UpdateTags(ctx, tenant, id, *body.Tags, contribution)
		if err != nil {
			return err
		}
		if !updated {
			return errNotFound
		}
	}

	// Only pending to approved or rejected is legal; anything else is 422, so a client
	// can't un-reject a memory or stamp auto rows pending out of band.
	if body.ReviewState != nil {
		current, err := m.repo.Find(ctx, tenant, id)
		if err != nil {
			return err
		}
		if current == nil {
			return errNotFound
		}
		target := *body.ReviewState
		if current.ReviewState != memorystore.ReviewPending || (target != memorystore.ReviewApproved && target != memorystore.ReviewRejected) {
			return wire.NewError(http.StatusUnprocessableEntity, "reviewState transition "+current.ReviewState+" → "+target+" not allowed")
		}
		if err := m.repo.UpdateReviewState(ctx, tenant, id, target); err != nil {
			return err
		}
		if body.Content == nil && body.Tags == nil {
			if err := m.repo.RecordContribution(ctx, tenant, id, userUpdate); err != nil {
				return err
			}
		}
		if target == memorystore.ReviewRejected {
			if err := m.repo.RecordRejected(ctx, tenant, memorystore.ContentHash(current.Content), current.SourceVaultFileID); err != nil {
				return err
			}
		}
	}

	row, err := m.repo.Find(ctx, tenant, id)
	if err != nil {
		return err
	}
	if row == nil {
		return errNotFound
	}
	actor, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	if body.Kind != nil && body.Content == nil && body.Tags == nil && body.ReviewState == nil && !moves {
		// A kind-only change is still the user's edit; the other fields record their
		// own contribution above.
		if err := m.repo.RecordContribution(ctx, tenant, id, userUpdate); err != nil {
			return err
		}
	}
	if err := m.repo.SaveEdit(ctx, row, actor, (*string)(body.Kind)); err != nil {
		return err
	}
	if body.Content != nil && m.opts.Indexer != nil {
		// Chunked from the corrected text with no file path: the source file still
		// holds the old wording, so its line numbers no longer describe this memory.
		m.opts.Indexer.IndexBestEffort(ctx, chunkindex.IndexParams{
			Tenant:   tenant,
			MemoryID: id,
			SpaceID:  row.SpaceID,
			Content:  *body.Content,
		})
	}
	return m.respond(c, tenant, row)
}

// moveToSpace files a memory into a Space, or out of every Space when target is nil. A
// memory compiled from a note is filed wherever its note is, so moving it moves the
// note, which re-files the memory and its chunks. Moving to where it already is does
// nothing and records nothing.
func (m *Module) moveToSpace(ctx context.Context, target *uuid.UUID, id, tenant uuid.UUID, contribution memorystore.Contribution) error {
	memory, err := m.repo.Find(ctx, tenant, id)
	if err != nil {
		return err
	}
	if memory == nil {
		return errNotFound
	}
	var slug *string
	if target != nil {
		space, err := m.q.SpaceByID(ctx, db.SpaceByIDParams{TenantID: tenant, ID: *target})
		if errors.Is(err, pgx.ErrNoRows) {
			return errSpaceForeign
		}
		if err != nil {
			return err
		}
		slug = &space.Slug
	}
	if sameSpace(memory.SpaceID, target) {
		return nil
	}
	var file *vaultfiles.VaultFile
	if memory.SourceVaultFileID != nil {
		if file, err = m.files.FindByID(ctx, tenant, *memory.SourceVaultFileID); err != nil {
			return err
		}
	}
	if file != nil {
		destination, err := vaultfiles.MoveDestination(file.Path, slug)
		if err != nil {
			return err
		}
		if err := m.files.MoveFile(ctx, file, destination, tenant); err != nil {
			return err
		}
	} else if _, err := m.repo.SetSpace(ctx, tenant, id, target); err != nil {
		return err
	}
	return m.repo.RecordContribution(ctx, tenant, id, contribution)
}

func sameSpace(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// MARK: GET /v1/memory/:id/provenance, /facets

func (m *Module) provenance(c *echo.Context) error {
	id, err := parseID(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	rows, exists, err := m.repo.Timeline(c.Request().Context(), a.VaultID, id)
	if err != nil {
		return err
	}
	if !exists {
		return errNotFound
	}
	visible, err := m.visible(c)
	if err != nil {
		return err
	}
	out := MemoryProvenanceResponse{MemoryID: wire.UUID{UUID: id}, Contributions: make([]MemoryContributionDTO, 0, len(rows))}
	for i := range rows {
		dto := contributionDTO(&rows[i])
		if !visible {
			scrubContribution(dto)
		}
		out.Contributions = append(out.Contributions, *dto)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) facets(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	f, err := m.repo.Facets(c.Request().Context(), a.VaultID)
	if err != nil {
		return err
	}
	visible, err := m.visible(c)
	if err != nil {
		return err
	}
	out := MemoryFacetsResponse{
		Providers: []MemoryFacetDTO{},
		Models:    []MemoryFacetDTO{},
		Sources:   facetDTOs(f.Sources),
		OldestAt:  wireTimePtr(f.OldestAt),
		NewestAt:  wireTimePtr(f.NewestAt),
	}
	if visible {
		out.Providers, out.Models = facetDTOs(f.Providers), facetDTOs(f.Models)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func facetDTOs(in []memorystore.Facet) []MemoryFacetDTO {
	out := make([]MemoryFacetDTO, 0, len(in))
	for _, f := range in {
		out = append(out, MemoryFacetDTO{Value: f.Value, Count: f.Count})
	}
	return out
}

// MARK: GET /v1/memory/:id/related

// related is the tenant's memories closest in meaning to this one, from the embedding
// already stored on it: no embedding call. Reading this is not recall, so it leaves
// query_hit_count alone.
func (m *Module) related(c *echo.Context) error {
	id, err := parseID(c)
	if err != nil {
		return err
	}
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	limit, ok := wire.QueryInt(c, "limit")
	if !ok {
		limit = 5
	}
	limit = min(max(limit, 1), 10)
	ctx := c.Request().Context()
	stored, exists, err := m.repo.StoredEmbedding(ctx, tenant, id)
	if err != nil {
		return err
	}
	if !exists {
		return errNotFound
	}
	out := relatedResponse{MemoryID: wire.UUID{UUID: id}, Items: []relatedItem{}}
	if len(stored) == 0 {
		return wire.JSON(c, http.StatusOK, out)
	}
	// One extra so dropping the memory itself still leaves limit.
	results, err := m.repo.SemanticSearch(ctx, tenant, stored, limit+1, memorystore.SearchOptions{SkipHitCount: true})
	if err != nil {
		return err
	}
	hits := make([]memorystore.SearchResult, 0, len(results))
	for _, hit := range results {
		if hit.ID != id && hit.Distance <= m.opts.RelatedMaxDistance {
			hits = append(hits, hit)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Distance < hits[j].Distance })
	hits = hits[:min(limit, len(hits))]
	if len(hits) == 0 {
		return wire.JSON(c, http.StatusOK, out)
	}
	hitIDs := make([]uuid.UUID, len(hits))
	for i := range hits {
		hitIDs[i] = hits[i].ID
	}
	rows, err := m.repo.FindMany(ctx, tenant, hitIDs)
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID]*memorystore.Memory, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	now := m.now()
	for i := range hits {
		hit := &hits[i]
		item := relatedItem{
			ID:         wire.UUID{UUID: hit.ID},
			Snippet:    relatedSnippet(hit.Content),
			Similarity: wire.Double(max(0, min(1, 1-float64(hit.Distance)))),
		}
		createdAt := hit.CreatedAt
		if row := byID[hit.ID]; row != nil {
			item.Kind, item.SpaceID = knownKind(row.Kind), wireUUIDPtr(row.SpaceID)
			if createdAt == nil {
				createdAt = row.CreatedAt
			}
		}
		if createdAt == nil {
			createdAt = &now
		}
		item.CreatedAt = wire.Time{Time: *createdAt}
		out.Items = append(out.Items, item)
	}
	return wire.JSON(c, http.StatusOK, out)
}

// relatedSnippet is relatedSnippet: the first ~160 characters on one line. It splits
// on Characters whose first scalar is White_Space, as Character.isWhitespace does.
func relatedSnippet(content string) string {
	var flat strings.Builder
	pending := false
	count := 0
	var characters []string
	for _, ch := range swiftstr.Characters(content) {
		first := []rune(ch)[0]
		if unicode.IsSpace(first) {
			pending = count > 0
			continue
		}
		if pending {
			characters = append(characters, " ")
			count++
			pending = false
		}
		characters = append(characters, ch)
		count++
	}
	limit := len(characters)
	over := swiftstr.CharacterCount(strings.Join(characters, "")) > 160
	if over {
		limit = 159
	}
	for _, ch := range characters[:limit] {
		flat.WriteString(ch)
	}
	if over {
		flat.WriteString("…")
	}
	return flat.String()
}

// MARK: GET /v1/memory/:id/lineage

func (m *Module) lineage(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	row, err := m.repo.FindLineage(c.Request().Context(), a.VaultID, id)
	if err != nil {
		return err
	}
	if row == nil {
		return errNotFound
	}
	out := lineageResponse{MemoryID: wire.UUID{UUID: row.MemoryID}}
	if row.SourceVaultFileID != nil && row.SourcePath != nil {
		out.Source = &lineageSource{
			VaultFileID: wire.UUID{UUID: *row.SourceVaultFileID},
			Path:        *row.SourcePath,
			CreatedAt:   wireTimePtr(row.SourceCreatedAt),
		}
		out.Trace = "Hermes learned this from your " + traceDate(row.SourceCreatedAt) + " note at " + *row.SourcePath + "."
	} else {
		out.Trace = "Hermes learned this directly — no source file recorded."
	}
	return wire.JSON(c, http.StatusOK, out)
}

// traceDate is formatTraceDate: YYYY-MM-DD in UTC, so the trace reads the same across
// client locales.
func traceDate(t *time.Time) string {
	if t == nil {
		return "earlier"
	}
	return t.UTC().Format(time.DateOnly)
}
