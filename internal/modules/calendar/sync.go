package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/modules/calendar/db"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	windowBack    = 7 * 24 * time.Hour
	windowForward = 30 * 24 * time.Hour
)

// syncService is CalendarSyncService, for what the routes need: the initial
// sync after the OAuth callback and POST /v1/calendar/events.
// CalendarSyncWorker keeps running this in Swift.
type syncService struct {
	q      *db.Queries
	tokens *tokenStore
	client calendarClient
	now    func() time.Time
}

// sync pulls one tenant's Google Calendar into calendar_events and returns how
// many events it upserted. It is a no-op without a connected Calendar grant.
//
// Swift quirk, kept: on 410 Gone the loop means to restart as a full window
// sync, but `continue` in a repeat-while jumps to the condition, which is false
// because pageToken was just cleared. So the call ends with nothing fetched and
// the expired token stored again. The callback path never reaches it, since a
// re-link clears sync_token first. Swift's CalendarSyncWorker does.
func (s *syncService) sync(ctx context.Context, tenant uuid.UUID) (int, error) {
	a, err := s.q.GoogleAccount(ctx, tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if a.Status != "connected" || !grants(&a.Scope, scopeCalendarEvents) {
		return 0, nil
	}
	access, err := s.tokens.validAccessToken(ctx, tenant)
	if errors.Is(err, errNeedsReauth) {
		slog.InfoContext(ctx, "calendar.sync skipped: needs reauth", "tenant", tenant)
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	windowStart := s.now().Add(-windowBack)
	windowEnd := s.now().Add(windowForward)

	upserted := 0
	syncToken := a.SyncToken
	var pageToken, newSyncToken *string
	for {
		var timeMin, timeMax *time.Time
		if syncToken == nil {
			timeMin, timeMax = &windowStart, &windowEnd
		}
		result, err := s.client.listEvents(ctx, access, syncToken, timeMin, timeMax, pageToken)
		if errors.Is(err, errSyncTokenExpired) {
			break // the repeat-while's condition, see above
		}
		if err != nil {
			return upserted, err
		}
		for _, ev := range result.events {
			if _, err := s.upsert(ctx, tenant, ev); err != nil {
				return upserted, err
			}
			upserted++
		}
		pageToken = result.nextPageToken
		if result.nextSyncToken != nil { // only on the final page
			newSyncToken = result.nextSyncToken
		}
		if pageToken == nil {
			break
		}
	}
	stored := a.SyncToken
	if newSyncToken != nil {
		stored = newSyncToken
	}
	err = s.q.StoreSyncResult(ctx, db.StoreSyncResultParams{
		TenantID: tenant, SyncToken: stored, Now: s.now(), WindowStart: windowStart, WindowEnd: windowEnd,
	})
	return upserted, err
}

// createEvent creates an event on Google live, caches it, and returns the row.
func (s *syncService) createEvent(ctx context.Context, tenant uuid.UUID, title string, startsAt, endsAt time.Time, location, notes *string, attendees []string) (db.UpsertGoogleEventRow, error) {
	access, err := s.tokens.validAccessToken(ctx, tenant)
	if err != nil {
		return db.UpsertGoogleEventRow{}, err
	}
	remote, err := s.client.insertEvent(ctx, access, title, startsAt, endsAt, location, notes, attendees)
	if err != nil {
		return db.UpsertGoogleEventRow{}, err
	}
	return s.upsert(ctx, tenant, remote)
}

func (s *syncService) upsert(ctx context.Context, tenant uuid.UUID, ev remoteEvent) (db.UpsertGoogleEventRow, error) {
	status := "confirmed"
	if ev.cancelled {
		status = "cancelled"
	}
	var attendees, recurrence *string
	if len(ev.attendees) > 0 {
		b, err := json.Marshal(storedAttendees{List: ev.attendees})
		if err != nil {
			return db.UpsertGoogleEventRow{}, err
		}
		attendees = ptr(string(b))
	}
	if len(ev.recurrence) > 0 {
		b, err := json.Marshal(struct {
			Rules []string `json:"rules"`
		}{ev.recurrence})
		if err != nil {
			return db.UpsertGoogleEventRow{}, err
		}
		recurrence = ptr(string(b))
	}
	return s.q.UpsertGoogleEvent(ctx, db.UpsertGoogleEventParams{
		ID:              uuid.New(),
		TenantID:        tenant,
		ExternalID:      ev.externalID,
		Title:           ev.title,
		Notes:           ev.notes,
		Location:        ev.location,
		StartsAt:        ev.startsAt,
		EndsAt:          ev.endsAt,
		AllDay:          ev.allDay,
		Status:          status,
		Organizer:       ev.organizer,
		Attendees:       attendees,
		Recurrence:      recurrence,
		HtmlLink:        ev.htmlLink,
		Etag:            ev.etag,
		RemoteUpdatedAt: ev.updatedAt,
		Now:             s.now(),
	})
}
