// Package agents serves /v1/agents: the Go port of LuminaVaultServer
// Sources/App/Agents (AgentsController, AgentsService, AgentGatewayClient,
// AgentRoomsController, AgentRoomOrchestrator). The Agents page lists where the
// user's agents run and their sessions; agent rooms are threads where the user
// and several of their agents talk, streamed as SSE.
package agents

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/agents/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/agents/db"
	"lumina/internal/ratelimit"
	"lumina/internal/sse"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	defaultLimit = 50
	maxLimit     = 200
	eventName    = "agent.room"
)

var (
	errUnknownInstance400 = wire.NewError(http.StatusBadRequest, "unknown_instance")
	errUnknownInstance404 = wire.NewError(http.StatusNotFound, "unknown_instance")
	errSessionNotFound    = wire.NewError(http.StatusNotFound, "session_not_found")
	errInvalidSessionID   = wire.NewError(http.StatusBadRequest, "invalid_session_id")
	errRoomNotFound       = wire.NewError(http.StatusNotFound, "room_not_found")
	errInvalidTitle       = wire.NewError(http.StatusBadRequest, "invalid_title")
	errInvalidMemberCount = wire.NewError(http.StatusBadRequest, "invalid_member_count")
	errInvalidBody        = wire.NewError(http.StatusBadRequest, "invalid_body")
	errRoomBusy           = wire.NewError(http.StatusConflict, "room_busy")
)

// Options are the module's dependencies.
type Options struct {
	// Identify is auth.Identify. Swift's group is jwtAuthenticator then
	// RateLimitMiddleware(.settingsByUser); each handler requires the identity.
	Identify echo.MiddlewareFunc
	Limits   *ratelimit.Store
	// Resolver is the BYO Hermes resolver, nil without a SecretBox or a gateway
	// URL that parses, as Swift's hermesEndpointResolver.
	Resolver resolver
	// Chat is the stack's RoutedService (llmService in App+build), the central
	// agent's voice in rooms.
	Chat chatter
	// HTTP reaches the user's own gateway; nil is llm.NoRedirectClient().
	HTTP *http.Client
	// KeepAlive is the rooms stream's keep-alive interval; 0 is sse.DefaultKeepAlive.
	KeepAlive time.Duration
}

// Module is the /v1/agents route group.
type Module struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	identify  echo.MiddlewareFunc
	limits    *ratelimit.Store
	svc       *service
	orch      *orchestrator
	keepAlive time.Duration
	now       func() time.Time
	log       *slog.Logger
}

// New builds the module.
func New(pool *pgxpool.Pool, opts Options) *Module {
	log := slog.Default().With("module", "agents")
	q := db.New(pool)
	client := opts.HTTP
	if client == nil {
		client = llm.NoRedirectClient()
	}
	svc := &service{pool: pool, q: q, resolver: opts.Resolver, http: client, log: log, now: time.Now}
	keepAlive := opts.KeepAlive
	if keepAlive == 0 {
		keepAlive = sse.DefaultKeepAlive
	}
	return &Module{
		pool: pool, q: q, identify: opts.Identify, limits: opts.Limits, svc: svc,
		orch: &orchestrator{
			q: q, speaker: liveSpeaker{q: q, llm: opts.Chat, agents: svc}, registry: newRunRegistry(),
			log: log.With("component", "rooms"), now: time.Now,
		},
		keepAlive: keepAlive, now: time.Now, log: log,
	}
}

// Register ignores the shared auth middleware: identify and the settingsByUser
// limit reproduce Swift's group, and each handler requires the identity, so an
// anonymous request still counts against the "ip:" bucket.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{m.identify, ratelimit.Middleware(m.limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey)}
	r.GET("/v1/agents/instances", m.instances, mw...)
	r.GET("/v1/agents/sessions", m.sessions, mw...)
	r.GET("/v1/agents/instances/:instance/sessions/:session/messages", m.messages, mw...)
	r.GET("/v1/agents/room-candidates", m.candidates, mw...)
	r.GET("/v1/agents/rooms", m.listRooms, mw...)
	r.POST("/v1/agents/rooms", m.createRoom, mw...)
	r.GET("/v1/agents/rooms/:room", m.roomDetail, mw...)
	r.DELETE("/v1/agents/rooms/:room", m.deleteRoom, mw...)
	r.POST("/v1/agents/rooms/:room/messages", m.postMessage, mw...)
	r.Streaming(http.MethodPost, "/v1/agents/rooms/:room/messages")
	r.POST("/v1/agents/rooms/:room/stop", m.stopRoom, mw...)
}

// requireUser is ctx.requireTenantID().
func requireUser(c *echo.Context) (uuid.UUID, error) {
	user, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return user, nil
}

// MARK: - AgentsController

func (m *Module) instances(c *echo.Context) error {
	user, err := requireUser(c)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, AgentInstancesResponse{Instances: m.svc.instances(c.Request().Context(), user)})
}

// nonEmpty is AgentsController.nonEmpty over a query parameter.
func nonEmpty(c *echo.Context, key string) *string {
	if v, ok := wire.QueryParam(c, key); ok && v != "" {
		return &v
	}
	return nil
}

func (m *Module) sessions(c *echo.Context) error {
	user, err := requireUser(c)
	if err != nil {
		return err
	}
	f := sessionsFilter{
		instanceID: nonEmpty(c, "instance"), profile: nonEmpty(c, "profile"), source: nonEmpty(c, "source"),
		limit: defaultLimit,
	}
	if v, ok := wire.QueryParam(c, "live"); ok {
		f.liveOnly = v == "true" || v == "1"
	}
	if n, ok := wire.QueryInt(c, "limit"); ok {
		f.limit = max(1, min(n, maxLimit))
	}
	if f.instanceID != nil && *f.instanceID != centralID && *f.instanceID != byoID {
		return errUnknownInstance400
	}
	return wire.JSON(c, http.StatusOK, m.svc.sessions(c.Request().Context(), user, f))
}

func (m *Module) messages(c *echo.Context) error {
	user, err := requireUser(c)
	if err != nil {
		return err
	}
	// Hummingbird's parameters are raw path segments: escapes stay as sent.
	segs := wire.PathSegments(c.Request())
	if len(segs) != 7 {
		return wire.NewError(http.StatusBadRequest, "invalid_path")
	}
	instance, session := segs[3], segs[5]
	res, err := m.svc.messages(c.Request().Context(), user, instance, nonEmpty(c, "profile"), session)
	switch {
	case err == nil:
		if res.Messages == nil {
			res.Messages = []AgentMessageDTO{}
		}
		return wire.JSON(c, http.StatusOK, res)
	case errors.Is(err, lookupUnknownInstance):
		return errUnknownInstance404
	case errors.Is(err, lookupSessionNotFound):
		return errSessionNotFound
	case isFailure(err, failInvalidID):
		return errInvalidSessionID
	default:
		var g *gatewayError
		if errors.As(err, &g) {
			return wire.NewError(http.StatusBadGateway, describe(err))
		}
		return err
	}
}

// MARK: - AgentRoomsController

func (m *Module) candidates(c *echo.Context) error {
	user, err := requireUser(c)
	if err != nil {
		return err
	}
	list, err := m.candidateList(c.Request().Context(), user)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, candidatesResponse{Candidates: list})
}

// candidateList is every agent the user can seat: each LuminaVault persona, and
// their own Hermes when it is linked and answering.
func (m *Module) candidateList(ctx context.Context, user uuid.UUID) ([]candidate, error) {
	personas, err := m.q.Personas(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make([]candidate, 0, len(personas)+1)
	for _, p := range personas {
		slug := p.Slug
		out = append(out, candidate{InstanceID: centralID, Profile: &slug, DisplayName: p.Label, SuggestedHandle: handleFrom(p.Slug)})
	}
	if client := m.svc.gatewayClient(ctx, user); client != nil {
		if info, err := client.instance(ctx); err == nil {
			profile := "hermes"
			if info.activeProfile != nil {
				profile = *info.activeProfile
			}
			name := "Hermes · " + profile
			if info.hostname != nil {
				name += " on " + *info.hostname
			}
			handleSource := profile
			if profile == "default" {
				handleSource = "hermes"
			}
			out = append(out, candidate{InstanceID: byoID, DisplayName: name, SuggestedHandle: handleFrom(handleSource)})
		}
	}
	return out, nil
}

func (m *Module) listRooms(c *echo.Context) error {
	user, err := requireUser(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	rooms, err := m.q.ListRooms(ctx, user)
	if err != nil {
		return err
	}
	out := make([]AgentRoomDTO, 0, len(rooms))
	for _, room := range rooms {
		dto, err := m.roomDTO(ctx, room)
		if err != nil {
			return err
		}
		out = append(out, dto)
	}
	return wire.JSON(c, http.StatusOK, AgentRoomsResponse{Rooms: out})
}

func (m *Module) createRoom(c *echo.Context) error {
	user, err := requireUser(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[createRequest](c)
	if err != nil {
		return err
	}
	title := swiftstr.TrimWhitespacesAndNewlines(body.Title)
	if title == "" || swiftstr.Count(title) > maxTitleLength {
		return errInvalidTitle
	}
	if n := len(body.Members); n < 1 || n > maxMembers {
		return errInvalidMemberCount
	}
	ctx := c.Request().Context()
	available, err := m.candidateList(ctx, user)
	if err != nil {
		return err
	}
	seats, err := resolveMembers(body.Members, available)
	if err != nil {
		return err
	}

	now := m.now()
	room := db.AgentRoom{
		ID: uuid.New(), TenantID: user, Title: title, TokenBudget: roomTokenBudget, SpentTokens: 0,
		CreatedAt: ptr(now), UpdatedAt: ptr(now),
	}
	err = pgx.BeginFunc(ctx, m.pool, func(tx pgx.Tx) error {
		q := m.q.WithTx(tx)
		if err := q.InsertRoom(ctx, db.InsertRoomParams{
			ID: room.ID, TenantID: room.TenantID, Title: room.Title, TokenBudget: room.TokenBudget,
			SpentTokens: room.SpentTokens, CreatedAt: room.CreatedAt, UpdatedAt: room.UpdatedAt,
		}); err != nil {
			return err
		}
		for _, s := range seats {
			if err := q.InsertMember(ctx, db.InsertMemberParams{
				ID: uuid.New(), RoomID: room.ID, InstanceID: s.instanceID, Profile: s.profile, Handle: s.handle,
				DisplayName: s.displayName, RespondMode: string(s.respondMode), CreatedAt: ptr(m.now()),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	dto, err := m.roomDTO(ctx, room)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, AgentRoomDetailResponse{Room: dto, Messages: []AgentRoomMessageDTO{}})
}

func (m *Module) roomDetail(c *echo.Context) error {
	room, err := m.ownedRoom(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	rows, err := m.q.LatestMessages(ctx, room.ID)
	if err != nil {
		return err
	}
	messages := make([]AgentRoomMessageDTO, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		messages = append(messages, messageDTO(rows[i]))
	}
	dto, err := m.roomDTO(ctx, room)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, AgentRoomDetailResponse{Room: dto, Messages: messages})
}

func (m *Module) deleteRoom(c *echo.Context) error {
	room, err := m.ownedRoom(c)
	if err != nil {
		return err
	}
	if err := m.q.DeleteRoom(c.Request().Context(), room.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) stopRoom(c *echo.Context) error {
	room, err := m.ownedRoom(c)
	if err != nil {
		return err
	}
	now := m.now()
	if err := m.q.SetStopRequested(c.Request().Context(), db.SetStopRequestedParams{ID: room.ID, StopRequestedAt: &now, UpdatedAt: &now}); err != nil {
		return err
	}
	return c.NoContent(http.StatusAccepted)
}

// postMessage posts as the user and streams the chain it starts: the user's
// message, thinking and message frames per agent turn, then done. Closing the
// stream cancels the chain before the next agent turn.
func (m *Module) postMessage(c *echo.Context) error {
	user, err := requireUser(c)
	if err != nil {
		return err
	}
	room, err := m.ownedRoom(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[postRequest](c)
	if err != nil {
		return err
	}
	text := swiftstr.TrimWhitespacesAndNewlines(body.Body)
	if text == "" || swiftstr.Count(text) > maxBodyLength {
		return errInvalidBody
	}
	// Refuse before the stream opens, so a second tab gets a clean 409.
	if m.orch.registry.isRunning(room.ID) {
		return errRoomBusy
	}
	stream := sse.Produce(c.Request().Context(), func(ctx context.Context, yield func(AgentRoomStreamEvent) bool) error {
		reason, err := m.orch.post(ctx, user, room, text, func(e AgentRoomStreamEvent) { yield(e) })
		if err != nil {
			m.log.WarnContext(ctx, "agent room chain failed", "error", err)
			return err
		}
		yield(AgentRoomStreamEvent{Kind: Done, Reason: &reason})
		return nil
	})
	return sse.ServeNamedStream(c, stream, eventName, m.keepAlive)
}

// ownedRoom is AgentRoomsController.ownedRoom: identity first, then the caller's
// room by its id, a malformed id being as unknown as another user's room.
func (m *Module) ownedRoom(c *echo.Context) (db.AgentRoom, error) {
	user, err := requireUser(c)
	if err != nil {
		return db.AgentRoom{}, err
	}
	segs := wire.PathSegments(c.Request())
	if len(segs) < 4 {
		return db.AgentRoom{}, errRoomNotFound
	}
	id, err := ids.Parse(segs[3])
	if err != nil {
		return db.AgentRoom{}, errRoomNotFound
	}
	room, err := m.q.OwnedRoom(c.Request().Context(), db.OwnedRoomParams{TenantID: user, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentRoom{}, errRoomNotFound
	}
	return room, err
}

func (m *Module) roomDTO(ctx context.Context, room db.AgentRoom) (AgentRoomDTO, error) {
	members, err := m.q.RoomMembers(ctx, room.ID)
	if err != nil {
		return AgentRoomDTO{}, err
	}
	out := AgentRoomDTO{
		Id: wire.UUID{UUID: room.ID}, Title: room.Title, TokenBudget: int(room.TokenBudget), SpentTokens: int(room.SpentTokens),
		Members: make([]AgentRoomMemberDTO, 0, len(members)), CreatedAt: wireTime(room.CreatedAt), UpdatedAt: wireTime(room.UpdatedAt),
	}
	for _, mem := range members {
		out.Members = append(out.Members, AgentRoomMemberDTO{
			Id: wire.UUID{UUID: mem.ID}, InstanceID: AgentRoomMemberDTOInstanceID(mem.InstanceID), Profile: mem.Profile,
			Handle: mem.Handle, DisplayName: mem.DisplayName, RespondMode: AgentRoomRespondMode(storedMode(mem.RespondMode)),
		})
	}
	return out, nil
}
