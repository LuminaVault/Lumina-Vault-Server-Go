// Package health serves /v1/health and /v1/health/daily: the Go port of
// LuminaVaultServer Sources/App/Health/HealthIngestController.swift (HealthKit
// ingest and reads). Not the liveness probe (/health).
//
// Swift side effects not ported: PostHogAnalytics.capture("health_events_synced")
// (product analytics, no Go client yet) and the EventBus `health_event_synced`
// publish, whose only subscriber (SkillRunner) just logs it (coupling.md).
package health

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/health/oapi-codegen.yaml api/openapi.yaml"

import (
	"encoding/json"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/health/db"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	defaultLimit    = 100
	maxLimit        = 200
	defaultWindow   = 7 * 24 * time.Hour
	dailyDefaultDay = 7
	dailyMaxDays    = 90
	maxBatchSize    = 1000
	maxTypeLength   = 64
	day             = 24 * time.Hour
)

// sumAggregationTypes are summed per day; every other type is averaged.
var sumAggregationTypes = map[string]bool{
	"steps":           true,
	"active_energy":   true,
	"mindful_minutes": true,
	"sleep_session":   true,
}

var (
	errEventsRequired = wire.NewError(http.StatusBadRequest, "events array required")
	errBatchTooLarge  = wire.NewError(http.StatusRequestEntityTooLarge, "batch exceeds 1000 events")
	errTypeRequired   = wire.NewError(http.StatusBadRequest, "type query parameter required")
	errDaysRange      = wire.NewError(http.StatusBadRequest, "days must be between 1 and 90")
)

type Module struct {
	q    *db.Queries
	gate echo.MiddlewareFunc
	now  func() time.Time
}

// New wires the module. gate is the ingest-only EntitlementMiddleware
// (.healthIngest); reads are never gated (HER-202).
func New(pool *pgxpool.Pool, gate echo.MiddlewareFunc) *Module {
	return &Module{q: db.New(pool), gate: gate, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	ingest := []echo.MiddlewareFunc{authMW}
	if m.gate != nil {
		ingest = append(ingest, m.gate)
	}
	r.POST("/v1/health", m.ingest, ingest...)
	r.GET("/v1/health", m.list, authMW)
	r.GET("/v1/health/daily", m.daily, authMW)
}

// eventInput is Swift's HealthEventInput, in declaration order.
type eventInput struct {
	Type         string             `json:"type"`
	RecordedAt   wire.Time          `json:"recordedAt"`
	ValueNumeric *float64           `json:"valueNumeric,omitempty"`
	ValueText    *string            `json:"valueText,omitempty"`
	Unit         *string            `json:"unit,omitempty"`
	Source       *string            `json:"source,omitempty"`
	Metadata     *map[string]string `json:"metadata,omitempty"`
}

type ingestRequest struct {
	Events []eventInput `json:"events"`
}

// The wire types below are LuminaVaultShared's; the spec's HealthEventDTO
// (eventType, metadata) and HealthIngestResponse ({ingested}) are wrong.
type ingestedRef struct {
	ID         wire.UUID `json:"id"`
	Type       string    `json:"type"`
	RecordedAt wire.Time `json:"recordedAt"`
}

type ingestResponse struct {
	Inserted int           `json:"inserted"`
	Skipped  int           `json:"skipped"`
	Events   []ingestedRef `json:"events"`
}

type eventDTO struct {
	ID           wire.UUID `json:"id"`
	Type         string    `json:"type"`
	RecordedAt   wire.Time `json:"recordedAt"`
	ValueNumeric *float64  `json:"valueNumeric,omitempty"`
	ValueText    *string   `json:"valueText,omitempty"`
	Unit         *string   `json:"unit,omitempty"`
	Source       *string   `json:"source,omitempty"`
}

type listResponse struct {
	Events []eventDTO `json:"events"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}

func (m *Module) ingest(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	body, err := wire.Bind[ingestRequest](c)
	if err != nil {
		return err
	}
	if len(body.Events) == 0 {
		return errEventsRequired
	}
	if len(body.Events) > maxBatchSize {
		return errBatchTooLarge
	}
	refs := []ingestedRef{}
	skipped := 0
	for _, in := range body.Events {
		trimmed := swiftstr.TrimWhitespacesAndNewlines(in.Type)
		if trimmed == "" || swiftstr.Count(trimmed) > maxTypeLength || (in.ValueNumeric == nil && in.ValueText == nil) {
			skipped++
			continue
		}
		var metadata []byte
		if in.Metadata != nil {
			if metadata, err = json.Marshal(*in.Metadata); err != nil {
				return err
			}
		}
		id, eventType, created := uuid.New(), strings.ToLower(trimmed), m.now()
		// One INSERT per row, outside a transaction, as Swift's row.save does.
		if err := m.q.InsertHealthEvent(ctx, db.InsertHealthEventParams{
			ID: id, TenantID: tenant, EventType: eventType,
			ValueNumeric: in.ValueNumeric, ValueText: in.ValueText, Unit: in.Unit,
			RecordedAt: in.RecordedAt.Time, Source: in.Source, Metadata: metadata, CreatedAt: &created,
		}); err != nil {
			return err
		}
		refs = append(refs, ingestedRef{ID: wire.UUID{UUID: id}, Type: eventType, RecordedAt: in.RecordedAt})
	}
	slog.InfoContext(ctx, "health ingest", "tenant", ids.Upper(tenant), "inserted", len(refs), "skipped", skipped)
	return wire.JSON(c, http.StatusOK, ingestResponse{Inserted: len(refs), Skipped: skipped, Events: refs})
}

func (m *Module) list(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	raw := c.Request().URL.RawQuery

	limit := defaultLimit
	if v, ok := swiftInt(raw, "limit"); ok {
		limit = v
	}
	limit = max(1, min(maxLimit, limit))
	offset := 0
	if v, ok := swiftInt(raw, "offset"); ok {
		offset = v
	}
	offset = max(0, offset)
	var eventType *string
	if v, ok := queryParam(raw, "type"); ok {
		if t := strings.ToLower(trimWhitespaces(v)); t != "" {
			eventType = &t
		}
	}
	now := m.now()
	from, to := now.Add(-defaultWindow), now
	if v, ok := queryParam(raw, "from"); ok {
		if t, ok := parseISO(v); ok {
			from = t
		}
	}
	if v, ok := queryParam(raw, "to"); ok {
		if t, ok := parseISO(v); ok {
			to = t
		}
	}
	rows, err := m.q.ListHealthEvents(ctx, db.ListHealthEventsParams{
		TenantID: tenant, FromAt: from, ToAt: to, EventType: eventType,
		RowLimit: int64(limit), RowOffset: int64(offset),
	})
	if err != nil {
		return err
	}
	out := listResponse{Events: make([]eventDTO, 0, len(rows)), Limit: limit, Offset: offset}
	for _, r := range rows {
		out.Events = append(out.Events, eventDTO{
			ID: wire.UUID{UUID: r.ID}, Type: r.EventType, RecordedAt: wire.Time{Time: r.RecordedAt},
			ValueNumeric: r.ValueNumeric, ValueText: r.ValueText, Unit: r.Unit, Source: r.Source,
		})
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) daily(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	raw := c.Request().URL.RawQuery

	v, ok := queryParam(raw, "type")
	eventType := strings.ToLower(trimWhitespaces(v))
	if !ok || eventType == "" {
		return errTypeRequired
	}
	days := dailyDefaultDay
	if n, ok := swiftInt(raw, "days"); ok {
		days = n
	}
	if days < 1 || days > dailyMaxDays {
		return errDaysRange
	}

	now := m.now().UTC()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(day)
	start := endOfDay.Add(-time.Duration(days) * day)

	type bucket struct {
		value float64
		count int
	}
	indexed := map[int64]bucket{}
	params := db.DailySumParams{TenantID: tenant, EventType: eventType, FromAt: start, ToAt: endOfDay}
	if sumAggregationTypes[eventType] {
		rows, err := m.q.DailySum(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rows {
			indexed[r.Day.Time.Unix()] = bucket{r.Value, int(r.SampleCount)}
		}
	} else {
		rows, err := m.q.DailyAvg(ctx, db.DailyAvgParams(params))
		if err != nil {
			return err
		}
		for _, r := range rows {
			indexed[r.Day.Time.Unix()] = bucket{r.Value, int(r.SampleCount)}
		}
	}

	out := HealthDailyResponse{Type: eventType, Days: make([]HealthDayAggregateDTO, 0, days)}
	for i := range days {
		d := start.Add(time.Duration(i) * day)
		b := indexed[d.Unix()] // a gap day is value 0, sampleCount 0
		out.Days = append(out.Days, HealthDayAggregateDTO{Date: wire.Time{Time: d}, Type: eventType, Value: b.value, SampleCount: b.count})
	}
	return wire.JSON(c, http.StatusOK, out)
}

// swiftInt is `req.uri.queryParameters[key].flatMap { Int($0) }`: absent or
// not a base-10 Int (an optional sign, ASCII digits, in range) is "no value".
func swiftInt(rawQuery, key string) (int, bool) {
	v, ok := queryParam(rawQuery, key)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// parseISO is the controller's ISO8601DateFormatter pair: internet date-time,
// with or without fractional seconds.
func parseISO(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}
