// Package reminders serves /v1/reminders (user-scheduled timed messages): the
// Go port of LuminaVaultServer Sources/App/Reminders/RemindersController.swift.
// Firing stays in Swift's ReminderScheduler (single replica, jobs.md). It
// flips together with apple (decisions.md D-shared-prefixes).
package reminders

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/reminders/oapi-codegen.yaml api/openapi.yaml"

import (
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/reminders/db"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	defaultLimit = 100
	maxLimit     = 200
)

var (
	errNotFound   = wire.NewError(http.StatusNotFound, "reminder not found")
	errBadID      = wire.NewError(http.StatusBadRequest, "invalid reminder id")
	errTitleReq   = wire.NewError(http.StatusBadRequest, "reminder title required")
	errTitleEmpty = wire.NewError(http.StatusBadRequest, "reminder title cannot be empty")
	errCron       = wire.NewError(http.StatusBadRequest, "invalid recurrence cron expression")
)

type Module struct {
	q          *db.Queries
	classifier Classifier
	now        func() time.Time
}

// New wires the classifier as Swift does (never nil, so /detect always
// decodes its body). transport nil means UnavailableTransport.
func New(pool *pgxpool.Pool, transport ChatTransport, model string) *Module {
	if transport == nil {
		transport = UnavailableTransport{}
	}
	return &Module{q: db.New(pool), classifier: Classifier{Transport: transport, Model: model}, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/reminders", m.list, authMW)
	r.POST("/v1/reminders", m.create, authMW)
	r.POST("/v1/reminders/detect", m.detect, authMW)
	r.PATCH("/v1/reminders/:id", m.update, authMW)
	r.DELETE("/v1/reminders/:id", m.delete, authMW)
}

// Request bodies in Swift field order (LuminaVaultShared), so wire.Bind
// reports the same first failing key Foundation does.
type createRequest struct {
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	FireAt         wire.Time `json:"fireAt"`
	RecurrenceCron *string   `json:"recurrenceCron,omitempty"`
}

type patchRequest struct {
	Title          *string    `json:"title,omitempty"`
	Body           *string    `json:"body,omitempty"`
	FireAt         *wire.Time `json:"fireAt,omitempty"`
	RecurrenceCron *string    `json:"recurrenceCron,omitempty"`
}

type detectRequest struct {
	Text string `json:"text"`
}

func (m *Module) detect(c *echo.Context) error {
	tenant := auth.UserID(c)
	body, err := wire.Bind[detectRequest](c)
	if err != nil {
		return err
	}
	out := m.classifier.Classify(c.Request().Context(), body.Text, ids.Upper(tenant), m.now())
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) list(c *echo.Context) error {
	rows, err := m.q.ListReminders(c.Request().Context(), db.ListRemindersParams{
		TenantID: auth.UserID(c), Limit: parseLimit(c.QueryParam("limit")),
	})
	if err != nil {
		return err
	}
	out := ReminderListResponse{Reminders: make([]ReminderDTO, 0, len(rows))}
	for _, r := range rows {
		out.Reminders = append(out.Reminders, toDTO(r))
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) create(c *echo.Context) error {
	body, err := wire.Bind[createRequest](c)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		return errTitleReq
	}
	if err := validateCron(body.RecurrenceCron); err != nil {
		return err
	}
	// recurrenceCron is stored as sent: "" is valid and kept as "" on create
	// (only PATCH maps "" to nil), as in Swift.
	r, err := m.q.CreateReminder(c.Request().Context(), db.CreateReminderParams{
		ID:             uuid.New(),
		TenantID:       auth.UserID(c),
		Title:          title,
		Body:           body.Body,
		FireAt:         body.FireAt.Time,
		RecurrenceCron: body.RecurrenceCron,
		CreatedAt:      m.now(),
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(r))
}

func (m *Module) update(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	id, err := parseID(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[patchRequest](c)
	if err != nil {
		return err
	}
	r, err := m.q.GetReminder(ctx, db.GetReminderParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if body.Title != nil {
		trimmed := strings.TrimSpace(*body.Title)
		if trimmed == "" {
			return errTitleEmpty
		}
		r.Title = trimmed
	}
	if body.Body != nil {
		r.Body = *body.Body
	}
	if body.FireAt != nil {
		r.FireAt = body.FireAt.Time
		r.FiredAt = nil // editing the fire time re-arms a fired reminder
	}
	if body.RecurrenceCron != nil {
		if err := validateCron(body.RecurrenceCron); err != nil {
			return err
		}
		if *body.RecurrenceCron == "" {
			r.RecurrenceCron = nil
		} else {
			r.RecurrenceCron = body.RecurrenceCron
		}
	}
	r, err = m.q.UpdateReminder(ctx, db.UpdateReminderParams{
		ID: r.ID, TenantID: tenant, Title: r.Title, Body: r.Body, FireAt: r.FireAt,
		RecurrenceCron: r.RecurrenceCron, FiredAt: r.FiredAt,
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(r))
}

func (m *Module) delete(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	id, err := parseID(c)
	if err != nil {
		return err
	}
	if _, err := m.q.GetReminder(ctx, db.GetReminderParams{ID: id, TenantID: tenant}); errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	} else if err != nil {
		return err
	}
	if err := m.q.DeleteReminder(ctx, db.DeleteReminderParams{ID: id, TenantID: tenant}); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func validateCron(cron *string) error {
	if cron == nil || *cron == "" {
		return nil
	}
	if ParseCron(*cron) != nil {
		return errCron
	}
	return nil
}

func toDTO(r db.Reminder) ReminderDTO {
	dto := ReminderDTO{
		Id:             wire.UUID{UUID: r.ID},
		Title:          r.Title,
		Body:           r.Body,
		FireAt:         wire.Time{Time: r.FireAt},
		RecurrenceCron: r.RecurrenceCron,
		CreatedAt:      wire.Time{Time: r.CreatedAt},
	}
	if r.FiredAt != nil {
		dto.FiredAt = &wire.Time{Time: *r.FiredAt}
	}
	return dto
}

// parseID is Foundation's UUID(uuidString:): 36 characters, either case.
func parseID(c *echo.Context) (uuid.UUID, error) {
	raw := c.Param("id")
	id, err := ids.Parse(raw)
	if err != nil || len(raw) != 36 {
		return uuid.Nil, errBadID
	}
	return id, nil
}

// parseLimit mirrors Swift's parseLimit: missing or non-integer → 100,
// otherwise clamped to 1..200.
func parseLimit(raw string) int32 {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultLimit
	}
	return int32(max(1, min(n, maxLimit))) //nolint:gosec // clamped to 1..200
}
