package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/jobs"
	"lumina/internal/modules/calendar/db"
	"lumina/internal/swiftstr"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MeetingBriefScheduler is MeetingBriefScheduler (Calendar/MeetingBriefScheduler.swift):
// once a minute, a quiet push about half an hour before an opted-in user's
// meeting saying how many of their notes bear on it. Each event is claimed in
// meeting_briefs before any work, so two processes send one push. It never
// touches the proactive push budget.
type MeetingBriefScheduler struct {
	Pool   *pgxpool.Pool
	Push   *apns.Service // nil sends nothing and still records "sent", as Swift's nil push
	brief  *briefService
	Logger *slog.Logger
}

const (
	briefWindowStart = 25 * time.Minute
	briefWindowEnd   = 35 * time.Minute
	briefBatchLimit  = 200
	briefThreadID    = "meeting"
	briefAction      = "meeting"
)

// NewMeetingBriefScheduler builds the scheduler on the same matching the brief
// route uses (embeddings and maxDistance from opts).
func NewMeetingBriefScheduler(pool *pgxpool.Pool, push *apns.Service, opts Options, logger *slog.Logger) *MeetingBriefScheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &MeetingBriefScheduler{
		Pool: pool, Push: push, Logger: logger,
		brief: &briefService{q: db.New(pool), embeddings: opts.Embeddings, maxDistance: opts.MaxDistance},
	}
}

// Job runs a tick every 60 s from boot, as Swift's run loop does.
func (s *MeetingBriefScheduler) Job() jobs.Job {
	return jobs.Job{Name: "MeetingBriefScheduler", Schedule: jobs.Every(time.Minute, 0), Run: func(ctx context.Context) error {
		sent, err := s.Tick(ctx, time.Now())
		if err != nil {
			s.Logger.WarnContext(ctx, "meeting.brief tick error "+err.Error())
			return nil
		}
		if sent > 0 {
			s.Logger.InfoContext(ctx, "meeting.brief tick sent="+strconv.Itoa(sent))
		}
		return nil
	}}
}

// BriefDeepLink is MeetingBriefScheduler.deepLink(eventID:).
func BriefDeepLink(eventID uuid.UUID) string {
	return "luminavault://meeting/" + strings.ToLower(eventID.String())
}

// BriefPushTitle is MeetingBriefService.pushTitle.
func BriefPushTitle(eventTitle string) string {
	title := flatten(eventTitle)
	if title == "" {
		return "Before your meeting"
	}
	if swiftstr.Count(title) > 80 {
		title = swiftstr.Prefix(title, 79) + "…"
	}
	return "Before " + title
}

// BriefPushBody is MeetingBriefService.pushBody: counts and first names only.
func BriefPushBody(count int, mentioned []string) string {
	notes := notesCount(count)
	switch len(mentioned) {
	case 0:
		return notes + " for this meeting"
	case 1:
		return notes + " on " + mentioned[0]
	default:
		return notes + " on " + mentioned[0] + " and " + mentioned[1]
	}
}

type briefCandidate struct {
	ev        briefEvent
	tenant    uuid.UUID
	selfEmail *string
}

// Tick is one pass at now. It returns how many briefs were pushed.
func (s *MeetingBriefScheduler) Tick(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT e.id, e.tenant_id, e.title, e.notes, e.location, e.starts_at, e.ends_at,
		       e.attendees::text AS attendees, u.email
		FROM apns_category_prefs p
		JOIN calendar_events e ON e.tenant_id = p.tenant_id
		JOIN users u ON u.id = p.tenant_id
		WHERE p.meeting_brief_enabled
		  AND e.starts_at >= $1
		  AND e.starts_at <= $2
		  AND NOT e.all_day
		  AND e.status <> 'cancelled'
		  AND NOT EXISTS (
		      SELECT 1 FROM meeting_briefs b
		      WHERE b.tenant_id = e.tenant_id AND b.event_id = e.id
		  )
		ORDER BY e.starts_at
		LIMIT $3`, now.Add(briefWindowStart), now.Add(briefWindowEnd), briefBatchLimit)
	if err != nil {
		return 0, err
	}
	candidates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (briefCandidate, error) {
		var c briefCandidate
		var attendees *string
		err := r.Scan(&c.ev.id, &c.tenant, &c.ev.title, &c.ev.notes, &c.ev.location, &c.ev.startsAt, &c.ev.endsAt, &attendees, &c.selfEmail)
		c.ev.attendees = []CalendarAttendeeDTO{}
		if attendees != nil {
			// attendees(fromJSON:): anything that doesn't decode is none.
			if list, err := attendeesFromJSON(*attendees); err == nil {
				c.ev.attendees = list
			}
		}
		return c, err
	})
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, c := range candidates {
		outcome, err := s.deliver(ctx, c, now)
		if err != nil {
			s.Logger.WarnContext(ctx, "meeting.brief failed tenant="+strings.ToUpper(c.tenant.String())+" event="+strings.ToUpper(c.ev.id.String())+": "+err.Error())
			continue
		}
		if outcome == "sent" {
			sent++
		}
	}
	return sent, nil
}

func (s *MeetingBriefScheduler) deliver(ctx context.Context, c briefCandidate, now time.Time) (string, error) {
	var won uuid.UUID
	err := s.Pool.QueryRow(ctx, `INSERT INTO meeting_briefs (tenant_id, event_id, claimed_at, outcome)
		VALUES ($1, $2, $3, 'claimed')
		ON CONFLICT (tenant_id, event_id) DO NOTHING
		RETURNING event_id`, c.tenant, c.ev.id, now).Scan(&won)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	if off, err := apns.IsCategorySuppressed(ctx, s.Pool, apns.CategoryMeeting, c.tenant); err != nil {
		return "", err
	} else if off {
		return "opted_out", s.record(ctx, c, "opted_out", nil, nil)
	}
	if has, err := apns.HasDeviceToken(ctx, s.Pool, c.tenant); err != nil {
		return "", err
	} else if !has {
		return "no_device", s.record(ctx, c, "no_device", nil, nil)
	}

	matches, err := s.brief.match(ctx, c.tenant, c.ev, c.selfEmail)
	if err != nil {
		// Nothing was sent: let a later tick in the window try again.
		_, _ = s.Pool.Exec(ctx, `DELETE FROM meeting_briefs
			WHERE tenant_id = $1 AND event_id = $2 AND outcome = 'claimed'`, c.tenant, c.ev.id)
		return "", err
	}
	if len(matches) == 0 {
		return "empty", s.record(ctx, c, "empty", nil, nil)
	}

	idList := make([]uuid.UUID, len(matches))
	for i, m := range matches {
		idList[i] = m.id
	}
	list, err := s.brief.q.BriefMemories(ctx, db.BriefMemoriesParams{TenantID: c.tenant, Ids: idList})
	if err != nil {
		return "", err
	}
	byID := map[uuid.UUID]string{}
	for _, r := range list {
		byID[r.ID] = r.Content
	}
	contents := []string{}
	for _, m := range matches {
		if content, ok := byID[m.id]; ok {
			contents = append(contents, content)
		}
	}
	mentioned := []string{}
	for _, m := range mentionedNames(c.ev.attendees, c.selfEmail, contents) {
		mentioned = append(mentioned, m.name)
	}
	if err := s.record(ctx, c, "sent", matches, &now); err != nil {
		return "", err
	}
	if s.Push == nil {
		return "sent", nil
	}
	err = s.Push.Notify(ctx, c.tenant, apns.Message{
		Title: BriefPushTitle(c.ev.title), Body: BriefPushBody(len(matches), mentioned),
		Category: apns.CategoryMeeting,
		Payload: map[string]string{
			"deepLink": BriefDeepLink(c.ev.id),
			"eventID":  strings.ToUpper(c.ev.id.String()),
		},
		InterruptionLevel: "passive", ThreadID: briefThreadID, ActionCategory: briefAction,
	})
	if err != nil {
		_ = s.record(ctx, c, "failed", matches, nil)
		return "", err
	}
	return "sent", nil
}

// encodeMatches is JSONEncoder().encode([Match]): uppercase ids.
func encodeMatches(matches []briefMatch) (*string, error) {
	type stored struct {
		ID         string  `json:"id"`
		Similarity float64 `json:"similarity"`
	}
	out := make([]stored, len(matches))
	for i, m := range matches {
		out[i] = stored{ID: strings.ToUpper(m.id.String()), Similarity: m.similarity}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

func (s *MeetingBriefScheduler) record(ctx context.Context, c briefCandidate, outcome string, matches []briefMatch, sentAt *time.Time) error {
	var raw *string
	if matches != nil {
		var err error
		if raw, err = encodeMatches(matches); err != nil {
			return err
		}
	}
	_, err := s.Pool.Exec(ctx, `UPDATE meeting_briefs
		SET outcome = $3,
		    memories = COALESCE($4::jsonb, memories),
		    sent_at = COALESCE($5, sent_at)
		WHERE tenant_id = $1 AND event_id = $2`, c.tenant, c.ev.id, outcome, raw, sentAt)
	return err
}
