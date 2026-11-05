// Package memoryagent holds the Hermes memory routes: POST /v1/memory/search,
// POST /v1/query and /v1/query/stream, GET and POST /v1/memos. The agent loops call
// the shared llm stack; workers stay in Swift. The telemetry inbox is a direct insert.
package memoryagent

import (
	"log/slog"
	"lumina/internal/achievementsink"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/embedding"
	"lumina/internal/hermesresolve"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/memoryagent/db"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// The spec's QueryRequest has no session_id and its MemorySearchResponse has `results`,
// and MemoRequest and MemoResponse differ too: LuminaVaultShared is what Swift serves,
// so these DTOs are written by hand from it.

type queryRequest struct {
	Query     string  `json:"query"`
	Limit     *int    `json:"limit,omitempty"`
	SessionID *string `json:"session_id,omitempty"`
}

// CitationDTO is MemoryCitationDTO.
type CitationDTO struct {
	ChunkID     string   `json:"chunkID"`
	DocumentID  string   `json:"documentID"`
	Path        *string  `json:"path,omitempty"`
	HeadingPath []string `json:"headingPath"`
	StartLine   int      `json:"startLine"`
	EndLine     int      `json:"endLine"`
}

// HitDTO is QueryHitDTO. Distance is a Swift Float on the wire.
type HitDTO struct {
	ID        wire.UUID    `json:"id"`
	Content   string       `json:"content"`
	Distance  wire.Float32 `json:"distance"`
	CreatedAt *wire.Time   `json:"createdAt,omitempty"`
	Snippet   *string      `json:"snippet,omitempty"`
	Citation  *CitationDTO `json:"citation,omitempty"`
}

type queryResponse struct {
	Summary   string   `json:"summary"`
	Hits      []HitDTO `json:"hits"`
	FollowUps []string `json:"followUps,omitempty"`
}

type searchHitDTO struct {
	ID        wire.UUID    `json:"id"`
	Content   string       `json:"content"`
	Distance  wire.Float32 `json:"distance"`
	CreatedAt *wire.Time   `json:"createdAt,omitempty"`
}

type searchResponse struct {
	Hits    []searchHitDTO `json:"hits"`
	Summary string         `json:"summary"`
}

type searchRequest struct {
	Query string `json:"query"`
	Limit *int   `json:"limit,omitempty"`
}

type memoRequest struct {
	Topic string  `json:"topic"`
	Hint  *string `json:"hint,omitempty"`
	Save  *bool   `json:"save,omitempty"`
}

type memoResponse struct {
	Memo            string      `json:"memo"`
	Path            *string     `json:"path,omitempty"`
	SourceMemoryIDs []wire.UUID `json:"sourceMemoryIds"`
	Summary         string      `json:"summary"`
}

// Options are the module's dependencies.
type Options struct {
	Identify echo.MiddlewareFunc
	// Hermes is the BYO Hermes middleware; nil without a secret box.
	Hermes       echo.MiddlewareFunc
	SearchGate   echo.MiddlewareFunc // entitlement memoryQuery, knowing about BYO Hermes
	QueryGate    echo.MiddlewareFunc // entitlement memoryQuery
	MemoGate     echo.MiddlewareFunc // entitlement memoGenerator
	Limits       *ratelimit.Store
	MaxStreams   int
	Service      *Service
	Memo         *MemoService
	FollowUps    *FollowUpGenerator
	Stream       llm.StreamService
	Memories     *memorystore.Repository
	Embeddings   embedding.Service
	Hybrid       *chunkindex.Hybrid
	Telemetry    Telemetry
	Preferences  llm.PreferenceSource
	Achievements *achievementsink.Sink
	DefaultModel string
}

// Module is the routes.
type Module struct {
	q      *db.Queries
	access *vaultaccess.Service
	opts   Options
}

// New builds the module.
func New(pool *pgxpool.Pool, opts Options) *Module {
	return &Module{q: db.New(pool), access: vaultaccess.New(pool), opts: opts}
}

// Register mounts Swift's groups. /v1/memory/search shares the capture stack with
// /v1/memory/upsert; the query group gates after its limit and counts in-flight
// requests; memos have no limit at all.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	stack := func(mws ...echo.MiddlewareFunc) []echo.MiddlewareFunc {
		out := []echo.MiddlewareFunc{m.opts.Identify}
		if m.opts.Hermes != nil {
			out = append(out, m.opts.Hermes)
		}
		for _, mw := range mws {
			if mw != nil {
				out = append(out, mw)
			}
		}
		return out
	}
	r.POST("/v1/memory/search", m.search, stack(m.opts.SearchGate,
		ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.CaptureByUser, ratelimit.UserOrIPKey, m.tier))...)

	query := stack(ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.QueryByUser, ratelimit.UserOrIPKey, m.tier),
		m.opts.QueryGate, ratelimit.InFlightMiddleware(ratelimit.NewInFlight(), m.opts.MaxStreams, ratelimit.UserOrIPKey))
	r.POST("/v1/query", m.query, query...)
	r.POST("/v1/query/stream", m.queryStream, query...)

	r.POST("/v1/memos", m.generateMemo, stack(m.opts.MemoGate)...)
	r.GET("/v1/memos", m.listMemos, stack(m.opts.MemoGate)...)
}

// tier is `context.identity?.tierEnum`.
func (m *Module) tier(c *echo.Context) (string, bool) {
	id, ok := auth.Identity(c)
	if !ok {
		return "", false
	}
	u, err := m.q.RoutingUser(c.Request().Context(), id)
	if err != nil {
		return "", false
	}
	return u.Tier, true
}

// routed is the request context with the BYO middleware's resolution bound, which is
// what makes the transport call the user's own Hermes.
func routed(c *echo.Context) *http.Request {
	req := c.Request()
	if res, ok := hermesresolve.FromContext(c); ok {
		return req.WithContext(llm.WithValues(req.Context(), func(v *llm.Values) { v.CurrentResolution = &res }))
	}
	return req
}

func (m *Module) search(c *echo.Context) error {
	body, err := wire.Bind[searchRequest](c)
	if err != nil {
		return err
	}
	if body.Query == "" {
		return wire.NewError(http.StatusBadRequest, "query required")
	}
	access, err := m.access.Resolve(c, vaultaccess.AI)
	if err != nil {
		return err
	}
	tenant := access.VaultID
	limit := 5
	if body.Limit != nil {
		limit = *body.Limit
	}
	ctx := llm.WithValues(routed(c).Context(), func(v *llm.Values) { v.BillingTenantID = &access.BillingSponsorUserID })
	answer, err := m.opts.Service.Search(ctx, tenant, ids.Upper(tenant), "", body.Query, limit)
	if err != nil {
		return err
	}
	hits := make([]searchHitDTO, len(answer.Hits))
	for i, h := range answer.Hits {
		hits[i] = searchHitDTO{ID: wire.UUID{UUID: h.ID}, Content: h.Content, Distance: wire.Float32(h.Distance)}
		if h.CreatedAt != nil {
			hits[i].CreatedAt = &wire.Time{Time: *h.CreatedAt}
		}
	}
	if len(hits) > 0 {
		actor, ok := auth.Identity(c)
		if !ok {
			return wire.ErrUnauthenticated
		}
		for range hits {
			if err := m.q.RecordMemoryRetrieved(c.Request().Context(), db.RecordMemoryRetrievedParams{VaultID: tenant, ActorUserID: actor}); err != nil {
				slog.WarnContext(c.Request().Context(), "memory_retrieved insert failed", "error", err)
			}
		}
	}
	return wire.JSON(c, http.StatusOK, searchResponse{Hits: hits, Summary: answer.Summary})
}

func (m *Module) query(c *echo.Context) error {
	actor, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[queryRequest](c)
	if err != nil {
		return err
	}
	if body.Query == "" {
		return wire.NewError(http.StatusBadRequest, "query required")
	}
	access, err := m.access.Resolve(c, vaultaccess.AI)
	if err != nil {
		return err
	}
	tenant := access.VaultID
	limit := 5
	if body.Limit != nil {
		limit = *body.Limit
	}
	base := routed(c).Context()
	ctx := llm.WithValues(base, func(v *llm.Values) {
		v.AnalyticsVaultID = &tenant
		v.BillingTenantID = &access.BillingSponsorUserID
	})
	sessionKey := ids.Upper(tenant)
	answer, err := m.opts.Service.Search(ctx, tenant, sessionKey, "", body.Query, limit)
	if err != nil {
		return err
	}
	if m.opts.Achievements != nil {
		m.opts.Achievements.Enqueue(actor, achievements.EventQueryRan)
	}
	hits := make([]HitDTO, len(answer.Hits))
	for i, h := range answer.Hits {
		hits[i] = hitDTO(h)
	}
	resp := queryResponse{Summary: answer.Summary, Hits: hits}
	if m.opts.FollowUps != nil {
		resp.FollowUps = m.opts.FollowUps.Generate(base, sessionKey, "", answer.Summary, hits)
	}
	return wire.JSON(c, http.StatusOK, resp)
}

func (m *Module) queryStream(c *echo.Context) error {
	actor, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[queryRequest](c)
	if err != nil {
		return err
	}
	if body.Query == "" {
		return wire.NewError(http.StatusBadRequest, "query required")
	}
	if m.opts.Stream == nil {
		return wire.NewError(http.StatusServiceUnavailable, "query streaming not configured on this server")
	}
	access, err := m.access.Resolve(c, vaultaccess.AI)
	if err != nil {
		return err
	}
	tenant := access.VaultID
	limit := 5
	if body.Limit != nil {
		limit = *body.Limit
	}
	limit = max(1, min(limit, 25))
	sessionID := ""
	if body.SessionID != nil {
		sessionID = *body.SessionID
	}
	base := routed(c).Context()

	// Retrieval failures surface as a normal HTTP error, before any frame. An embedding
	// outage is not one: the search runs lexically.
	vec, err := embedding.EmbedForQuery(base, m.opts.Embeddings, body.Query, tenant, "query")
	if err != nil {
		return err
	}
	var hits []memorystore.SearchResult
	if m.opts.Hybrid != nil {
		hits, err = m.opts.Hybrid.Search(base, tenant, body.Query, vec, limit, memorystore.SearchOptions{})
	} else {
		hits, err = m.opts.Memories.SearchDocuments(base, tenant, body.Query, vec, limit, memorystore.SearchOptions{})
	}
	if err != nil {
		return err
	}
	if m.opts.Telemetry != nil {
		m.opts.Telemetry.Record(base, newSample(tenant, "query", nil, hits, limit))
	}
	if m.opts.Achievements != nil {
		m.opts.Achievements.Enqueue(actor, achievements.EventQueryRan)
	}

	// Managed tenants get the model-identity guard; BYOK stays open.
	guarded := true
	if m.opts.Preferences != nil {
		if pref, err := m.opts.Preferences.Get(base, tenant); err == nil && pref != nil && pref.Mode == llm.ModeBYOK {
			guarded = false
		}
	}
	u, err := m.q.RoutingUser(base, actor)
	if err != nil {
		return err
	}
	user := &llm.User{ID: u.ID, Tier: u.Tier, TierOverride: u.TierOverride, PrivacyNoCNOrigin: u.PrivacyNoCnOrigin}
	temperature := 0.4
	req := llm.ChatRequest{Messages: BuildQueryPrompt(body.Query, hits, guarded), Temperature: &temperature}
	if m.opts.DefaultModel != "" {
		req.Model = m.opts.DefaultModel
	}
	dtos := make([]HitDTO, len(hits))
	for i, h := range hits {
		dtos[i] = hitDTO(h)
	}

	w := c.Response()
	rc := http.NewResponseController(w)
	streamHeaders(w)
	_ = rc.Flush()
	m.streamQuery(base, tenant, ids.Upper(tenant), sessionID, user, access.BillingSponsorUserID, req, dtos, func(e StreamEvent) bool {
		return frame(w, rc, e)
	})
	return nil
}

func (m *Module) generateMemo(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[memoRequest](c)
	if err != nil {
		return err
	}
	save := body.Save == nil || *body.Save
	result, err := m.opts.Memo.Generate(routed(c).Context(), tenant, ids.Upper(tenant), "", body.Topic, body.Hint, save)
	if err != nil {
		return err
	}
	sources := make([]wire.UUID, len(result.SourceIDs))
	for i, id := range result.SourceIDs {
		sources[i] = wire.UUID{UUID: id}
	}
	return wire.JSON(c, http.StatusOK, memoResponse{Memo: result.Memo, Path: result.Path, SourceMemoryIDs: sources, Summary: result.Summary})
}

func (m *Module) listMemos(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	rows, err := m.q.ListMemos(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	memos := make([]MemoSummaryDTO, 0, len(rows))
	for _, row := range rows {
		if row.CreatedAt == nil {
			continue
		}
		memos = append(memos, MemoSummaryDTO{
			Id: wire.UUID{UUID: row.ID}, Title: TitleFromPath(row.Path), Path: row.Path,
			CreatedAt: wire.Time{Time: *row.CreatedAt},
		})
	}
	return wire.JSON(c, http.StatusOK, MemoListResponse{Memos: memos})
}
