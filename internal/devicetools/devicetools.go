// Package devicetools is Swift's PersonalDataTools (Sources/App/Apple/
// PersonalDataTools.swift): the Health, Calendar and Reminders tools an agent
// calls on the user's own data. Reads come from the synced tables, falling back
// to a live device round-trip through the devicerpc.Broker; writes always go to
// the phone, and are queued when the app isn't connected.
//
// Every method returns the JSON text Swift's tools return, built with
// JSONSerialization from string-valued dictionaries. MCP (5F) and skills (5E)
// parse it back.
package devicetools

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/devicerpc"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Broker is the device round-trip, *devicerpc.Broker in production.
type Broker interface {
	Request(ctx context.Context, tenant uuid.UUID, cmd devicerpc.Command) (devicerpc.Result, error)
}

// Queue stores a write the app wasn't connected for, *devicerpc.Queue in
// production.
type Queue interface {
	Enqueue(ctx context.Context, tenant uuid.UUID, cmd devicerpc.Command) error
}

// Tools is PersonalDataTools.
type Tools struct {
	pool   *pgxpool.Pool
	broker Broker
	// queue nil is Swift's `deviceQueue == nil`: writes the app misses fail.
	queue Queue
}

// New returns the tools. queue may be nil.
func New(pool *pgxpool.Pool, broker Broker, queue Queue) *Tools {
	return &Tools{pool: pool, broker: broker, queue: queue}
}

// Apple data domains (AppleDataDomain raw values).
const (
	DomainHealth    = "health"
	DomainCalendar  = "calendar"
	DomainReminders = "reminders"
)

// isAllowed is AppleConsentController.isAllowed: any read failure is "not allowed".
func (t *Tools) isAllowed(ctx context.Context, tenant uuid.UUID, domain string) (allowed, writes bool) {
	_ = t.pool.QueryRow(ctx, `SELECT allowed, allow_writes FROM apple_consent WHERE tenant_id = $1 AND domain = $2`,
		tenant, domain).Scan(&allowed, &writes)
	return allowed, writes
}

// HealthQuery is healthQuery: daily totals and averages per metric.
func (t *Tools) HealthQuery(ctx context.Context, tenant uuid.UUID, metric *string, days *int) string {
	if allowed, _ := t.isAllowed(ctx, tenant, DomainHealth); !allowed {
		return errorJSON("health access not allowed by the user")
	}
	n := clamp(days, 30, 365)
	const head = `SELECT event_type, unit, date_trunc('day', recorded_at) AS day,
		       SUM(value_numeric) AS total, AVG(value_numeric) AS avg
		FROM health_events
		WHERE tenant_id = $1 AND recorded_at >= NOW() - ($2 * INTERVAL '1 day')`
	const tail = ` GROUP BY event_type, unit, day ORDER BY day`
	query, args := head+tail, []any{tenant, n}
	if metric != nil && *metric != "" {
		query, args = head+` AND event_type = $3`+tail, append(args, *metric)
	}
	rows, err := t.pool.Query(ctx, query, args...)
	if err != nil {
		return errorJSON("health_query failed: " + err.Error())
	}
	defer rows.Close()
	points := []map[string]string{}
	for rows.Next() {
		var (
			eventType  string
			unit       *string
			day        time.Time
			total, avg *float64
		)
		if err := rows.Scan(&eventType, &unit, &day, &total, &avg); err != nil {
			return errorJSON("health_query failed: " + err.Error())
		}
		points = append(points, map[string]string{
			"metric": eventType,
			"unit":   deref(unit),
			"day":    day.UTC().Format("2006-01-02"),
			"total":  fmt.Sprintf("%.2f", derefFloat(total)),
			"avg":    fmt.Sprintf("%.2f", derefFloat(avg)),
		})
	}
	if err := rows.Err(); err != nil {
		return errorJSON("health_query failed: " + err.Error())
	}
	return encodeJSON(map[string]any{"status": "ok", "days": fmt.Sprint(n), "points": points})
}

// CalendarQuery is calendarQuery: the synced events, or a live fetch when the
// cache has none (or can't be read).
func (t *Tools) CalendarQuery(ctx context.Context, tenant uuid.UUID, days *int) string {
	n := clamp(days, 7, 90)
	if allowed, _ := t.isAllowed(ctx, tenant, DomainCalendar); !allowed {
		return errorJSON("calendar access not allowed by the user")
	}
	fallback := func() string {
		return t.DeviceRead(ctx, tenant, DomainCalendar, map[string]string{"days": fmt.Sprint(n)})
	}
	rows, err := t.pool.Query(ctx, `SELECT title, starts_at, ends_at, location
		FROM calendar_events
		WHERE tenant_id = $1
		  AND status <> 'cancelled'
		  AND starts_at >= NOW()
		  AND starts_at < NOW() + ($2 * INTERVAL '1 day')
		ORDER BY starts_at ASC`, tenant, n)
	if err != nil {
		return fallback()
	}
	defer rows.Close()
	events := []map[string]string{}
	for rows.Next() {
		var (
			title      string
			start, end time.Time
			location   *string
		)
		if err := rows.Scan(&title, &start, &end, &location); err != nil {
			return fallback()
		}
		events = append(events, map[string]string{
			"title": title, "start": iso(start), "end": iso(end), "location": deref(location),
		})
	}
	if rows.Err() != nil || len(events) == 0 {
		return fallback()
	}
	return encodeJSON(map[string]any{"status": "ok", "items": events})
}

// RemindersList is remindersList: open reminders, soonest due first, at most
// 100. The items are a JSON string inside the JSON, as Swift encodes them.
func (t *Tools) RemindersList(ctx context.Context, tenant uuid.UUID) string {
	if allowed, _ := t.isAllowed(ctx, tenant, DomainReminders); !allowed {
		return errorJSON("reminders access not allowed by the user")
	}
	rows, err := t.pool.Query(ctx, `SELECT title, due_at, notes
		FROM apple_reminders
		WHERE tenant_id = $1 AND completed = false
		ORDER BY due_at ASC NULLS LAST
		LIMIT 100`, tenant)
	if err != nil {
		return errorJSON("reminders_list failed: " + err.Error())
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var (
			title string
			due   *time.Time
			notes *string
		)
		if err := rows.Scan(&title, &due, &notes); err != nil {
			return errorJSON("reminders_list failed: " + err.Error())
		}
		item := map[string]string{"title": title}
		if due != nil {
			item["due"] = iso(*due)
		}
		if notes != nil && *notes != "" {
			item["notes"] = *notes
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return errorJSON("reminders_list failed: " + err.Error())
	}
	if len(items) == 0 {
		return t.DeviceRead(ctx, tenant, DomainReminders, map[string]string{})
	}
	return encodeJSON(map[string]any{"status": "ok", "items": encodeJSON(items)})
}

// ReminderCreate is reminderCreate.
func (t *Tools) ReminderCreate(ctx context.Context, tenant uuid.UUID, title string, notes, due *string) string {
	return t.DeviceWrite(ctx, tenant, DomainReminders, devicerpc.KindReminderCreate, map[string]string{
		"title": title, "notes": deref(notes), "due": deref(due),
	})
}

// CalendarCreate is calendarCreate.
func (t *Tools) CalendarCreate(ctx context.Context, tenant uuid.UUID, title, start string, end, location *string) string {
	return t.DeviceWrite(ctx, tenant, DomainCalendar, devicerpc.KindCalendarCreate, map[string]string{
		"title": title, "start": start, "end": deref(end), "location": deref(location),
	})
}

// DeviceWrite is deviceWrite: a change on the phone, behind the domain's write
// consent. A phone that doesn't answer gets the command queued when it can be.
func (t *Tools) DeviceWrite(ctx context.Context, tenant uuid.UUID, domain string, kind devicerpc.Kind, payload map[string]string) string {
	allowed, writes := t.isAllowed(ctx, tenant, domain)
	if !allowed {
		return errorJSON(domain + " access not allowed by the user")
	}
	if !writes {
		return errorJSON(domain + " changes not allowed by the user")
	}
	cmd := devicerpc.NewCommand(kind, domain, payload)
	result, err := t.broker.Request(ctx, tenant, cmd)
	if err != nil {
		if t.queue != nil && devicerpc.IsQueueable(kind) {
			if err := t.queue.Enqueue(ctx, tenant, cmd); err != nil {
				return errorJSON("device did not respond, and the write could not be queued")
			}
			return encodeJSON(map[string]any{
				"status":  "queued",
				"message": "The iPhone app is not open. LuminaVault sent a notification; this happens when the app opens (within 24 hours).",
			})
		}
		return errorJSON("device did not respond (offline or timed out)")
	}
	if !result.OK {
		return errorJSON(resultError(result))
	}
	out := map[string]any{"status": "ok"}
	for k, v := range result.Payload {
		out[k] = v
	}
	return encodeJSON(out)
}

// DeviceRead is deviceRead: a live fetch from the phone.
func (t *Tools) DeviceRead(ctx context.Context, tenant uuid.UUID, domain string, payload map[string]string) string {
	if allowed, _ := t.isAllowed(ctx, tenant, domain); !allowed {
		return errorJSON(domain + " access not allowed by the user")
	}
	result, err := t.broker.Request(ctx, tenant, devicerpc.NewCommand(devicerpc.KindDeviceFetch, domain, payload))
	if err != nil {
		return errorJSON("device did not respond (offline or timed out)")
	}
	if !result.OK {
		return errorJSON(resultError(result))
	}
	items, ok := result.Payload["items"]
	if !ok {
		items = "[]"
	}
	return encodeJSON(map[string]any{"status": "ok", "items": items})
}

func resultError(r devicerpc.Result) string {
	if r.Error != nil {
		return *r.Error
	}
	return "device reported failure"
}

// encodeJSON is PersonalDataTools.encodeJSON. Key order isn't Swift's (a
// dictionary's is unspecified), which is fine: every caller parses it again.
func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"status":"error","reason":"could not encode tool result"}`
	}
	return string(b)
}

func errorJSON(reason string) string {
	return encodeJSON(map[string]any{"status": "error", "reason": reason})
}

// iso is ISO8601DateFormatter with [.withInternetDateTime].
func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func clamp(v *int, def, upper int) int {
	n := def
	if v != nil {
		n = *v
	}
	return max(1, min(n, upper))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefFloat(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
