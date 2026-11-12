// Package chat serves /v1/conversations, /v1/chat/inbox, /v1/me/chat-preferences and
// /v1/me/preferences/hybrid-execution: the Go port of LuminaVaultServer
// Sources/App/Chat (ConversationController, ChatExperienceController, ChatGrounding,
// ChatAttachmentPrompt, HermesEscalationPolicy). ProactiveChatDelivery is a skills
// worker's, and stays in Swift.
package chat

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/achievementsink"
	"lumina/internal/auth"
	"lumina/internal/capture"
	"lumina/internal/chunkindex"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/modules/quickassistant"
	"lumina/internal/onboardlatch"
	"lumina/internal/posthog"
	"lumina/internal/ratelimit"
	"lumina/internal/selfimprove"
	"lumina/internal/soul"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

var (
	errInvalidID         = wire.NewError(http.StatusBadRequest, "invalid conversation id")
	errNotFound          = wire.NewError(http.StatusNotFound, "conversation not found")
	errPinnedForeign     = wire.NewError(http.StatusBadRequest, "pinned memory does not belong to the caller")
	errModelUnavailable  = wire.NewError(http.StatusUnprocessableEntity, "selected model is unavailable")
	errContentRequired   = wire.NewError(http.StatusBadRequest, "content required")
	errParallelForbidden = wire.NewError(http.StatusForbidden, "router_parallel_requires_ultimate")
	errExecNotFound      = wire.NewError(http.StatusNotFound, "local execution not found")
	errExecCommitted     = wire.NewError(http.StatusConflict, "local execution already committed")
	errExecExpired       = wire.NewError(http.StatusGone, "local execution expired")
	errToolQuery         = wire.NewError(http.StatusBadRequest, "tool query required")
	errPrivateFallback   = wire.NewError(http.StatusBadRequest, "private_profile_cannot_fallback_to_cloud")
	errSQLUnavailable    = wire.NewError(http.StatusInternalServerError, "sql_unavailable")
)

// Options are the module's dependencies. Nil optional ones behave as Swift's nil.
type Options struct {
	Identify echo.MiddlewareFunc
	// Hermes is the BYO Hermes middleware; nil without a SecretBox.
	Hermes echo.MiddlewareFunc
	// Profile is hermesprofile's middleware (HermesProfileMiddleware).
	Profile echo.MiddlewareFunc
	// Gate is Entitlement .memoryQuery with the BYO credential exemption.
	Gate   echo.MiddlewareFunc
	Limits *ratelimit.Store

	Stream       llm.StreamService
	FollowUps    *memoryagent.FollowUpGenerator
	Memories     *memorystore.Repository
	Embeddings   embedding.Service
	Hybrid       *chunkindex.Hybrid
	Telemetry    memoryagent.Telemetry
	Preferences  llm.PreferenceSource
	Traces       llm.TraceRecorder
	Links        *capture.Service // nil when autoSaveLinks.enabled is off
	Latches      *onboardlatch.Latches
	Analytics    *posthog.Client
	Activity     *selfimprove.Activity
	Soul         *soul.Service
	Runs         HermesRuns // nil: never escalate
	Achievements *achievementsink.Sink

	DefaultModel                    string
	ParallelEnabled                 bool
	HybridExecutionEnabled          bool
	LocalExecutionToolBrokerEnabled bool
	MaxGroundingDistance            float32
	// FixLocalPrepare stores the prepared execution. Off, prepare is Swift's bare 500
	// (see errPrepareSave), and so tools, commit and cancel never find one.
	FixLocalPrepare bool

	// QuickAssistant is QuickAssistantRuntime: nil unless QUICK_ASSISTANT_ENABLED
	// is on and its table exists. Nil, a stream body's quickAssistant is never
	// read and chat is exactly what it was before Quick Assistant.
	QuickAssistant *QuickAssistant
}

// QuickAssistant is what a Quick Assistant turn runs on: the tool loop and the
// services its tools use. A turn's BYOK pin reads Options.Preferences.
type QuickAssistant struct {
	Service *quickassistant.Service
	Deps    quickassistant.Deps
}

// Module is the chat routes.
type Module struct {
	pool        *pgxpool.Pool
	access      *vaultaccess.Service
	opts        Options
	maxDistance float32
	tier        func(*echo.Context) (string, bool)
	now         func() time.Time
	log         *slog.Logger
	// afterTurn runs when a stream's turn has finished, persistence included.
	// Tests use it to wait for a turn whose client went away.
	afterTurn func()
}

// New builds the module.
func New(pool *pgxpool.Pool, opts Options) *Module {
	d := opts.MaxGroundingDistance
	if d == 0 {
		d = DefaultMaxDistance
	}
	return &Module{
		pool: pool, access: vaultaccess.New(pool), opts: opts, maxDistance: d,
		tier: ratelimit.PGTier(pool), now: time.Now, log: slog.Default().With("module", "chat"),
	}
}

// Register mounts Swift's groups. /v1/conversations: jwt, BYO Hermes, the profile
// middleware, conversationByUser, then Entitlement .memoryQuery; the stream route
// registers on the same group (ConversationController gets no streamRouter), so it
// has no in-flight limit. /v1/chat: jwt and conversationByUser. The two preference
// groups: jwt and settingsByUser.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	conv := m.chain(m.opts.Identify, m.opts.Hermes, m.opts.Profile,
		ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.ConversationByUser, ratelimit.UserOrIPKey, m.tier), m.opts.Gate)
	r.POST("/v1/conversations", m.create, conv...)
	r.GET("/v1/conversations", m.list, conv...)
	r.GET("/v1/conversations/:id", m.getOne, conv...)
	r.GET("/v1/conversations/:id/traces", m.traces, conv...)
	r.DELETE("/v1/conversations/:id", m.delete, conv...)
	r.POST("/v1/conversations/:id/messages/stream", m.streamReply, conv...)
	r.Streaming(http.MethodPost, "/v1/conversations/:id/messages/stream")
	if m.opts.HybridExecutionEnabled {
		r.POST("/v1/conversations/:id/messages/prepare", m.prepareLocalReply, conv...)
		r.POST("/v1/conversations/:id/messages/commit", m.commitLocalReply, conv...)
		r.DELETE("/v1/conversations/:id/local-executions/:executionID", m.cancelLocalReply, conv...)
		if m.opts.LocalExecutionToolBrokerEnabled {
			r.POST("/v1/conversations/:id/local-executions/:executionID/tools", m.invokeLocalTool, conv...)
		}
	}

	inbox := m.chain(m.opts.Identify, ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.ConversationByUser, ratelimit.UserOrIPKey, m.tier))
	r.GET("/v1/chat/inbox", m.inbox, inbox...)
	settings := m.chain(m.opts.Identify, ratelimit.Middleware(m.opts.Limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey))
	r.GET("/v1/me/chat-preferences", m.getPreferences, settings...)
	r.PUT("/v1/me/chat-preferences", m.putPreferences, settings...)
	if m.opts.HybridExecutionEnabled {
		r.GET("/v1/me/preferences/hybrid-execution", m.getHybridPreferences, settings...)
		r.PUT("/v1/me/preferences/hybrid-execution", m.putHybridPreferences, settings...)
	}
}

func (m *Module) chain(mws ...echo.MiddlewareFunc) []echo.MiddlewareFunc {
	out := make([]echo.MiddlewareFunc, 0, len(mws))
	for _, mw := range mws {
		if mw != nil {
			out = append(out, mw)
		}
	}
	return out
}

func requireUser(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

func parseID(c *echo.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil || len(c.Param(name)) != 36 {
		return uuid.Nil, false
	}
	return id, true
}

func conversationID(c *echo.Context) (uuid.UUID, error) {
	id, ok := parseID(c, "id")
	if !ok {
		return uuid.Nil, errInvalidID
	}
	return id, nil
}

// MARK: - rows

type conversationRow struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Title         string
	SpaceID       *uuid.UUID
	Pinned        []uuid.UUID
	RouteProvider *string
	RouteModel    *string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const conversationColumns = `id, tenant_id, title, space_id, pinned_memory_ids, route_provider, route_model, created_at, updated_at`

func scanConversation(row pgx.Row) (*conversationRow, error) {
	var c conversationRow
	if err := row.Scan(&c.ID, &c.TenantID, &c.Title, &c.SpaceID, &c.Pinned, &c.RouteProvider, &c.RouteModel, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// routeOverride is Conversation.routeOverride: only a known provider and a non-empty model.
func (c *conversationRow) routeOverride() *llm.WireRoute {
	if c.RouteProvider == nil || c.RouteModel == nil || *c.RouteModel == "" || !knownProvider(*c.RouteProvider) {
		return nil
	}
	return &llm.WireRoute{Provider: llm.ProviderID(*c.RouteProvider), Model: *c.RouteModel}
}

func knownProvider(p string) bool {
	switch p {
	case "xai", "anthropic", "openai", "ollama", "openRouter", "nvidia", "gemini", "nous", "custom":
		return true
	}
	return false
}

func (c *conversationRow) dto() ConversationDTO {
	pinned := make([]wire.UUID, len(c.Pinned))
	for i, id := range c.Pinned {
		pinned[i] = wire.UUID{UUID: id}
	}
	out := ConversationDTO{
		ID: wire.UUID{UUID: c.ID}, Title: c.Title, CreatedAt: wire.Time{Time: c.CreatedAt},
		UpdatedAt: wire.Time{Time: c.UpdatedAt}, PinnedMemoryIDs: pinned,
	}
	if c.SpaceID != nil {
		out.SpaceID = &wire.UUID{UUID: *c.SpaceID}
	}
	if r := c.routeOverride(); r != nil {
		out.RouteOverride = &RouteDTO{Provider: ProviderID(r.Provider), Model: r.Model}
	}
	return out
}

// fetch is ConversationController.fetch: a foreign row is a 404.
func (m *Module) fetch(ctx context.Context, tenant, id uuid.UUID) (*conversationRow, error) {
	c, err := scanConversation(m.pool.QueryRow(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE tenant_id = $1 AND id = $2 LIMIT 1`, tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return c, err
}

type messageRow struct {
	ID                  uuid.UUID
	ConversationID      uuid.UUID
	Role                string
	Content             string
	SourceMemoryIDs     []uuid.UUID
	ParallelExecutionID *uuid.UUID
	ToolCallCount       int
	Origin              *string
	SourceLabel         *string
	CreatedAt           time.Time
}

const messageColumns = `id, conversation_id, role, content, source_memory_ids, parallel_execution_id, tool_call_count, origin, source_label, created_at`

func scanMessage(row pgx.Row) (*messageRow, error) {
	var r messageRow
	err := row.Scan(&r.ID, &r.ConversationID, &r.Role, &r.Content, &r.SourceMemoryIDs, &r.ParallelExecutionID, &r.ToolCallCount, &r.Origin, &r.SourceLabel, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (m *Module) messages(ctx context.Context, conversation uuid.UUID) ([]messageRow, error) {
	rows, err := m.pool.Query(ctx, `SELECT `+messageColumns+` FROM conversation_messages WHERE conversation_id = $1 ORDER BY created_at ASC`, conversation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []messageRow
	for rows.Next() {
		r, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

type traceRow struct {
	ID                     uuid.UUID
	MessageID              *uuid.UUID
	Provider, Model        string
	ToolCalls              *llm.AgentToolCalls
	FailoverCount          int
	TokensIn, TokensOut    int64
	EstimatedCostUsdMicros int64
	LatencyMs              int64
	CredentialMode         *string
	OccurredAt             time.Time
}

func (m *Module) traceRows(ctx context.Context, tenant uuid.UUID, messageIDs []uuid.UUID, ordered bool) ([]traceRow, error) {
	q := `SELECT id, conversation_message_id, provider, model, tool_calls, failover_count, tokens_in, tokens_out,
		estimated_cost_usd_micros, latency_ms, credential_mode, occurred_at
		FROM agent_turn_traces WHERE tenant_id = $1 AND conversation_message_id = ANY($2::uuid[])`
	if ordered {
		q += ` ORDER BY occurred_at ASC`
	}
	rows, err := m.pool.Query(ctx, q, tenant, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []traceRow
	for rows.Next() {
		var t traceRow
		var raw []byte
		if err := rows.Scan(&t.ID, &t.MessageID, &t.Provider, &t.Model, &raw, &t.FailoverCount, &t.TokensIn, &t.TokensOut,
			&t.EstimatedCostUsdMicros, &t.LatencyMs, &t.CredentialMode, &t.OccurredAt); err != nil {
			return nil, err
		}
		if raw != nil {
			var calls llm.AgentToolCalls
			if err := calls.UnmarshalJSON(raw); err != nil {
				return nil, err
			}
			t.ToolCalls = &calls
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// dto is AgentTurnTrace.toDTO; nil for a provider the wire doesn't know.
func (t traceRow) dto() *TraceDTO {
	if !knownProvider(t.Provider) {
		return nil
	}
	out := &TraceDTO{
		ID: wire.UUID{UUID: t.ID}, Provider: t.Provider, Model: t.Model, ToolNames: []string{},
		FailoverCount: t.FailoverCount, TokensIn: t.TokensIn, TokensOut: t.TokensOut,
		EstimatedCostUsdMicros: t.EstimatedCostUsdMicros, LatencyMs: t.LatencyMs, OccurredAt: wire.Time{Time: t.OccurredAt},
	}
	if t.MessageID != nil {
		out.ConversationMessageID = &wire.UUID{UUID: *t.MessageID}
	}
	if t.ToolCalls != nil {
		if t.ToolCalls.Names != nil {
			out.ToolNames = t.ToolCalls.Names
		}
		out.ToolCallCount = t.ToolCalls.Count
	}
	if t.CredentialMode != nil && (*t.CredentialMode == "managed" || *t.CredentialMode == "byok") {
		out.CredentialMode = t.CredentialMode
	}
	return out
}

// messageDTO is ConversationMessage.toDTO(trace:).
func messageDTO(r messageRow, trace *traceRow) MessageDTO {
	sources := make([]wire.UUID, len(r.SourceMemoryIDs))
	for i, id := range r.SourceMemoryIDs {
		sources[i] = wire.UUID{UUID: id}
	}
	role := r.Role
	if role != "user" && role != "assistant" && role != "system" {
		role = "user"
	}
	origin := "reply"
	if r.Origin != nil && *r.Origin == "proactive" {
		origin = "proactive"
	}
	count := r.ToolCallCount
	out := MessageDTO{
		ID: wire.UUID{UUID: r.ID}, ConversationID: wire.UUID{UUID: r.ConversationID}, Role: role, Content: r.Content,
		SourceMemoryIDs: sources, ToolCallCount: &count, Origin: origin, SourceLabel: r.SourceLabel,
		CreatedAt: wire.Time{Time: r.CreatedAt},
	}
	if r.ParallelExecutionID != nil {
		out.ParallelExecutionID = &wire.UUID{UUID: *r.ParallelExecutionID}
	}
	if trace != nil {
		if knownProvider(trace.Provider) {
			p := trace.Provider
			out.Provider = &p
		}
		model := trace.Model
		out.Model = &model
	}
	return out
}

// MARK: - CRUD

func (m *Module) create(c *echo.Context) error {
	actor, err := requireUser(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[createRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	access, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	var pinned []uuid.UUID
	if body.PinnedMemoryIDs != nil {
		for i, id := range *body.PinnedMemoryIDs {
			if i == 5 {
				break
			}
			pinned = append(pinned, id.UUID)
		}
	}
	for _, id := range pinned {
		found, err := m.opts.Memories.Find(ctx, access.VaultID, id)
		if err != nil {
			return err
		}
		if found == nil {
			return errPinnedForeign
		}
	}
	var routeProvider, routeModel *string
	if r := body.RouteOverride; r != nil {
		if _, ok := llm.FromShared(llm.ProviderID(r.Provider)); !ok {
			return errModelUnavailable
		}
		if _, ok := llm.CatalogEntry(llm.ProviderID(r.Provider), r.Model); !ok {
			return errModelUnavailable
		}
		p, model := string(r.Provider), r.Model
		routeProvider, routeModel = &p, &model
	}
	title := "New conversation"
	if body.Title != nil {
		if t := swiftstr.TrimWhitespacesAndNewlines(*body.Title); t != "" {
			title = t
		}
	}
	var spaceID *uuid.UUID
	if body.SpaceID != nil {
		spaceID = &body.SpaceID.UUID
	}
	if pinned == nil {
		pinned = []uuid.UUID{}
	}
	now := m.now()
	row, err := scanConversation(m.pool.QueryRow(ctx, `INSERT INTO conversations
		(id, tenant_id, title, space_id, pinned_memory_ids, route_provider, route_model, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING `+conversationColumns,
		uuid.New(), actor, title, spaceID, pinned, routeProvider, routeModel, now))
	if err != nil {
		return err
	}
	if !access.IsPersonal {
		if _, err := m.pool.Exec(ctx, `INSERT INTO analytics_events
			(vault_id, actor_user_id, event_name, source, dimensions, idempotency_key)
			VALUES ($1, $2, 'conversation_created', 'server', '{}'::jsonb, $3)
			ON CONFLICT (actor_user_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
			access.VaultID, actor, "conversation-created:"+ids.Upper(row.ID)); err != nil {
			m.log.WarnContext(ctx, "conversation_created analytics insert failed", "error", err)
		}
	}
	return wire.JSON(c, http.StatusOK, row.dto())
}

func (m *Module) list(c *echo.Context) error {
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	rows, err := m.pool.Query(c.Request().Context(), `SELECT `+conversationColumns+` FROM conversations
		WHERE tenant_id = $1 ORDER BY updated_at DESC LIMIT 100`, tenant)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := listResponse{Conversations: []ConversationDTO{}}
	for rows.Next() {
		r, err := scanConversation(rows)
		if err != nil {
			return err
		}
		out.Conversations = append(out.Conversations, r.dto())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) getOne(c *echo.Context) error {
	id, err := conversationID(c)
	if err != nil {
		return err
	}
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	conv, err := m.fetch(ctx, tenant, id)
	if err != nil {
		return err
	}
	msgs, err := m.messages(ctx, id)
	if err != nil {
		return err
	}
	messageIDs := make([]uuid.UUID, len(msgs))
	for i, msg := range msgs {
		messageIDs[i] = msg.ID
	}
	traces, err := m.traceRows(ctx, tenant, messageIDs, false)
	if err != nil {
		return err
	}
	byMessage := map[uuid.UUID]*traceRow{}
	for i := range traces {
		if t := &traces[i]; t.MessageID != nil {
			byMessage[*t.MessageID] = t
		}
	}
	out := detailResponse{Conversation: conv.dto(), Messages: make([]MessageDTO, len(msgs))}
	for i, msg := range msgs {
		out.Messages[i] = messageDTO(msg, byMessage[msg.ID])
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) traces(c *echo.Context) error {
	id, err := conversationID(c)
	if err != nil {
		return err
	}
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if _, err := m.fetch(ctx, tenant, id); err != nil {
		return err
	}
	msgs, err := m.messages(ctx, id)
	if err != nil {
		return err
	}
	out := tracesResponse{Traces: []TraceDTO{}}
	if len(msgs) == 0 {
		return wire.JSON(c, http.StatusOK, out)
	}
	messageIDs := make([]uuid.UUID, len(msgs))
	for i, msg := range msgs {
		messageIDs[i] = msg.ID
	}
	rows, err := m.traceRows(ctx, tenant, messageIDs, true)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if dto := r.dto(); dto != nil {
			out.Traces = append(out.Traces, *dto)
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) delete(c *echo.Context) error {
	id, err := conversationID(c)
	if err != nil {
		return err
	}
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if _, err := m.fetch(ctx, tenant, id); err != nil {
		return err
	}
	if _, err := m.pool.Exec(ctx, `DELETE FROM conversations WHERE id = $1`, id); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
