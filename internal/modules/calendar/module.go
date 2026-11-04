// Package calendar serves /v1/calendar: the Go port of LuminaVaultServer
// Sources/App/Calendar (CalendarController, AppleCalendarController,
// MeetingBriefController, GoogleCalendarOAuthService, CalendarTokenStore,
// CalendarSyncService, GoogleCalendarClient and GoogleCalendarOAuthClient).
//
// Stays in Swift (jobs.md): CalendarSyncWorker and MeetingBriefScheduler. They
// share calendar_accounts and calendar_events with this package; tokens use the
// same SecretBox envelope and columns.
//
// Two things to know before flipping (coupling.md, PR body):
//   - OAuth sessions are in memory here as in Swift, so /v1/calendar/connect,
//     /v1/mail/gmail/connect and /v1/calendar/oauth/callback must reach one
//     process. calendar and mail flip together.
//   - The brief's live match needs the embedding chain. Go only has the
//     deterministic provider (apple.NewEmbedder); with a real provider a brief
//     without stored memories answers 500.
//
// Routes and middleware follow App+build.swift. status, connect, disconnect,
// events and the callback exist only with a SecretBox. sync and brief always
// exist. brief sits behind jwtAuthenticator + RateLimitMiddleware(.queryByUser).
package calendar

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/calendar/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/apple"
	"lumina/internal/modules/calendar/db"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	maxEvents      = 50
	maxSyncBatch   = 1000
	maxAttendees   = 100
	maxAttendeeLen = 256
	maxExternalID  = 512
	maxSyncTitle   = 1024
)

// Options are the Swift config keys the module reads.
type Options struct {
	// Google is nil without LV_SECRET_MASTER_KEY: those routes then do not exist.
	Google *Google
	// MaxDistance is meeting.brief.maxDistance.
	MaxDistance float32
	// Embeddings is the service behind the brief's live match.
	Embeddings apple.Embedder
}

type Module struct {
	q        *db.Queries
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	google   *Google
	brief    *briefService
	now      func() time.Time
}

// New builds the module. identify is auth.Identify and limits the process-wide
// rate-limit store.
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc, limits *ratelimit.Store, opts Options) *Module {
	q := db.New(pool)
	return &Module{
		q:        q,
		identify: identify,
		limits:   limits,
		google:   opts.Google,
		brief:    &briefService{q: q, embeddings: opts.Embeddings, maxDistance: opts.MaxDistance},
		now:      time.Now,
	}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	if m.google != nil {
		r.GET("/v1/calendar/status", m.status, authMW)
		r.POST("/v1/calendar/connect", m.connect, authMW)
		r.POST("/v1/calendar/disconnect", m.disconnect, authMW)
		r.GET("/v1/calendar/events", m.events, authMW)
		r.POST("/v1/calendar/events", m.createEvent, authMW)
		r.GET("/v1/calendar/oauth/callback", m.oauthCallback)
	}
	r.POST("/v1/calendar/sync", m.appleSync, authMW)
	r.GET("/v1/calendar/events/:id/brief", m.getBrief, m.identify, m.queryLimit, auth.Require)
}

func tenantOf(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	return id, nil
}

// MARK: Google

func (m *Module) status(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	s, err := m.google.Status(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	out := CalendarStatusResponse{Connected: s.Connected, NeedsReauth: s.NeedsReauth, AccountEmail: s.AccountEmail}
	if s.LastSyncedAt != nil {
		out.LastSyncedAt = &wire.Time{Time: *s.LastSyncedAt}
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) connect(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	url, err := m.google.Start(c.Request().Context(), tenant, ReturnToParam(c), PurposeCalendar)
	switch {
	case errors.Is(err, ErrNotConfigured):
		return wire.NewError(http.StatusServiceUnavailable, "Google Calendar is not configured on this server")
	case errors.Is(err, ErrInvalidReturn):
		return wire.NewError(http.StatusBadRequest, "invalid_return_to")
	case err != nil:
		return err
	}
	return wire.JSON(c, http.StatusOK, CalendarConnectStartResponse{AuthorizeURL: url})
}

// ReturnToParam is `?returnTo=`, nil when absent.
func ReturnToParam(c *echo.Context) *string {
	return queryParam(c, "returnTo")
}

func queryParam(c *echo.Context, name string) *string {
	q := c.QueryParams()
	if !q.Has(name) {
		return nil
	}
	v := q.Get(name)
	return &v
}

func (m *Module) disconnect(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	if err := m.google.Disconnect(c.Request().Context(), tenant); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) events(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	rows, err := m.q.UpcomingEvents(c.Request().Context(), db.UpcomingEventsParams{
		TenantID: tenant, Now: m.now(), Lim: parseLimit(c.QueryParam("limit")),
	})
	if err != nil {
		return err
	}
	out := CalendarEventsResponse{Events: make([]CalendarEventDTO, len(rows))}
	for i, r := range rows {
		out.Events[i] = CalendarEventDTO{
			Id: wire.UUID{UUID: r.ID}, Source: r.Source, ExternalID: r.ExternalID, Title: r.Title,
			Notes: r.Notes, Location: r.Location,
			StartsAt: wire.Time{Time: r.StartsAt}, EndsAt: wire.Time{Time: r.EndsAt},
			AllDay: r.AllDay, Status: r.Status, Organizer: r.Organizer, HtmlLink: r.HtmlLink,
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

// parseLimit is CalendarController.parseLimit: no value or a non-integer is the
// maximum, anything else is clamped to 1…50.
func parseLimit(raw string) int32 {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return maxEvents
	}
	return int32(max(1, min(n, maxEvents))) //nolint:gosec // clamped to 1…50
}

// createEventRequest is CalendarCreateEventRequest in Swift's field order, the
// order wire.Bind reports the first failing key in.
type createEventRequest struct {
	Title     string    `json:"title"`
	StartsAt  wire.Time `json:"startsAt"`
	EndsAt    wire.Time `json:"endsAt"`
	Location  *string   `json:"location,omitempty"`
	Notes     *string   `json:"notes,omitempty"`
	Attendees *[]string `json:"attendees,omitempty"`
}

func (m *Module) createEvent(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[createEventRequest](c)
	if err != nil {
		return err
	}
	title := swiftstr.TrimWhitespacesAndNewlines(body.Title)
	if title == "" {
		return wire.NewError(http.StatusBadRequest, "event title required")
	}
	if !body.EndsAt.After(body.StartsAt.Time) {
		return wire.NewError(http.StatusBadRequest, "event end must be after start")
	}
	var attendees []string
	if body.Attendees != nil {
		attendees = *body.Attendees
	}
	saved, err := m.google.sync.createEvent(c.Request().Context(), tenant, title, body.StartsAt.Time, body.EndsAt.Time, body.Location, body.Notes, attendees)
	if errors.Is(err, errNotConnected) || errors.Is(err, errNeedsReauth) {
		return wire.NewError(http.StatusConflict, "Google Calendar is not connected")
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, CalendarEventDTO{
		Id: wire.UUID{UUID: saved.ID}, Source: saved.Source, ExternalID: saved.ExternalID, Title: saved.Title,
		Notes: saved.Notes, Location: saved.Location,
		StartsAt: wire.Time{Time: saved.StartsAt}, EndsAt: wire.Time{Time: saved.EndsAt},
		AllDay: saved.AllDay, Status: saved.Status, Organizer: saved.Organizer, HtmlLink: saved.HtmlLink,
	})
}

// oauthCallback has no JWT: Google redirects the user's browser here, and the
// session behind `state` says who it is. Every outcome is a 303.
func (m *Module) oauthCallback(c *echo.Context) error {
	state := ""
	if s := queryParam(c, "state"); s != nil {
		state = *s
	}
	// The browser leaving must not abandon an exchange Google already consumed.
	ctx := context.WithoutCancel(c.Request().Context())
	return c.Redirect(http.StatusSeeOther, m.google.handleCallback(ctx, state, queryParam(c, "code"), queryParam(c, "error")))
}

// MARK: POST /v1/calendar/sync

type syncAttendee struct {
	Name  *string `json:"name,omitempty"`
	Email *string `json:"email,omitempty"`
}

// syncEventInput is AppleCalendarEventInput in Swift's field order.
type syncEventInput struct {
	ExternalID      string          `json:"externalID"`
	CalendarID      *string         `json:"calendarID,omitempty"`
	Title           string          `json:"title"`
	Notes           *string         `json:"notes,omitempty"`
	Location        *string         `json:"location,omitempty"`
	StartsAt        wire.Time       `json:"startsAt"`
	EndsAt          wire.Time       `json:"endsAt"`
	AllDay          bool            `json:"allDay"`
	Status          *string         `json:"status,omitempty"`
	Organizer       *string         `json:"organizer,omitempty"`
	RemoteUpdatedAt *wire.Time      `json:"remoteUpdatedAt,omitempty"`
	Attendees       *[]syncAttendee `json:"attendees,omitempty"`
}

type syncRequest struct {
	Events []syncEventInput `json:"events"`
}

func (m *Module) appleSync(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[syncRequest](c)
	if err != nil {
		return err
	}
	if len(body.Events) == 0 {
		return wire.NewError(http.StatusBadRequest, "events array required")
	}
	if len(body.Events) > maxSyncBatch {
		return wire.NewError(http.StatusRequestEntityTooLarge, "batch exceeds "+strconv.Itoa(maxSyncBatch)+" events")
	}
	ctx := c.Request().Context()
	// Consent gate, after the body. AppleConsentController.isAllowed fails closed.
	if allowed, err := m.q.CalendarConsent(ctx, tenant); err != nil || !allowed {
		return wire.NewError(http.StatusForbidden, "calendar access not allowed by the user")
	}
	var inserted, updated, skipped int
	for i := range body.Events {
		input := &body.Events[i]
		externalID := swiftstr.TrimWhitespacesAndNewlines(input.ExternalID)
		title := swiftstr.TrimWhitespacesAndNewlines(input.Title)
		// The external id is the upsert key. An empty title is fine (cancelled
		// and private events), but both have a length cap.
		if externalID == "" || swiftstr.Count(externalID) > maxExternalID || swiftstr.Count(title) > maxSyncTitle {
			skipped++
			continue
		}
		status := "confirmed"
		if input.Status != nil && strings.ToLower(trimWhitespaces(*input.Status)) == "cancelled" {
			status = "cancelled"
		}
		params := db.UpsertAppleEventParams{
			TenantID: tenant, ExternalID: externalID, CalendarID: input.CalendarID, Title: title,
			Notes: input.Notes, Location: input.Location, StartsAt: input.StartsAt.Time, EndsAt: input.EndsAt.Time,
			AllDay: input.AllDay, Status: status, Organizer: input.Organizer, Attendees: attendeesJSON(input.Attendees),
		}
		if input.RemoteUpdatedAt != nil {
			params.RemoteUpdatedAt = &input.RemoteUpdatedAt.Time
		}
		isNew, err := m.q.UpsertAppleEvent(ctx, params)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			skipped++ // the guard rejected a stale delta
		case err != nil:
			return err
		case isNew:
			inserted++
		default:
			updated++
		}
	}
	if err := m.q.StampCalendarConsentSync(ctx, tenant); err != nil {
		slog.WarnContext(ctx, "apple.calendar.sync stamp failed", "error", err) // try? in Swift
	}
	return wire.JSON(c, http.StatusOK, AppleSyncResponse{Inserted: inserted, Updated: updated, Skipped: skipped})
}

// trimWhitespaces is trimmingCharacters(in: .whitespaces): space separators and
// tab, but no newlines. That is narrower than swiftstr's trim.
func trimWhitespaces(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) })
}

// attendeesJSON is AppleCalendarController.attendeesJSON: nil leaves the column
// alone. Blank fields are dropped, an attendee with neither a name nor an email
// is skipped, and the list is capped. The shape is what the Google sync writes.
func attendeesJSON(attendees *[]syncAttendee) *string {
	if attendees == nil {
		return nil
	}
	clean := func(raw *string) *string {
		if raw == nil {
			return nil
		}
		v := swiftstr.TrimWhitespacesAndNewlines(*raw)
		if v == "" {
			return nil
		}
		v = swiftstr.Prefix(v, maxAttendeeLen)
		return &v
	}
	list := make([]googleAttendee, 0, min(len(*attendees), maxAttendees))
	for _, a := range *attendees {
		if len(list) == maxAttendees {
			break
		}
		name, email := clean(a.Name), clean(a.Email)
		if name == nil && email == nil {
			continue
		}
		list = append(list, googleAttendee{Email: email, DisplayName: name})
	}
	b, err := json.Marshal(storedAttendees{List: list})
	if err != nil {
		return nil
	}
	return ptr(string(b))
}

// MARK: GET /v1/calendar/events/{id}/brief

// queryLimit is RateLimitMiddleware(.queryByUser): 30 per 60 s for a trial or
// an unidentified caller, on Swift's standardTierScale. The bucket is shared
// with /v1/query once that moves.
func (m *Module) queryLimit(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		limit := 30
		if id, ok := auth.Identity(c); ok {
			if u, err := m.q.UserForBrief(c.Request().Context(), id); err == nil {
				limit = queryLimitFor(u.Tier)
			}
		}
		policy := ratelimit.Policy{Name: "queryByUser", Max: limit, Window: 60 * time.Second}
		if limited, retry := m.limits.Hit(policy, ratelimit.UserOrIPKey(c)); limited {
			c.Response().Header().Set("Retry-After", strconv.Itoa(retry))
			return ratelimit.ErrLimited
		}
		return next(c)
	}
}

// queryLimitFor is RateLimitPolicy.effectiveMax for queryByUser.
func queryLimitFor(tier string) int {
	switch tier {
	case "ultimate":
		return 120
	case "pro":
		return 60
	case "free", "lapsed":
		return 10 // (30 * 0.34).rounded()
	case "archived":
		return 3
	default:
		return 30
	}
}

func (m *Module) getBrief(c *echo.Context) error {
	tenant, err := tenantOf(c)
	if err != nil {
		return err
	}
	raw := c.Param("id")
	eventID, err := ids.Parse(raw)
	if err != nil || len(raw) != 36 { // uuid.Parse also takes braces, urn: and bare hex
		return wire.NewError(http.StatusBadRequest, "invalid event id")
	}
	resp, ok, err := m.brief.brief(c.Request().Context(), tenant, eventID)
	if err != nil {
		return err
	}
	if !ok {
		return wire.NewError(http.StatusNotFound, "event not found")
	}
	return wire.JSON(c, http.StatusOK, resp)
}
