package workers

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/cronexpr"
	"lumina/internal/jobs"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReminderScheduler is ReminderScheduler (Sources/App/Reminders/ReminderScheduler.swift):
// once a minute, push every reminder whose fire_at has arrived and fired_at is null,
// at most 4 at a time, then stamp one-shots and move recurring ones to their next
// cron minute. It claims nothing, so it must never run in both servers (jobs.md).
//
// Swift sleeps to the next minute boundary before its first tick; the runner starts
// it at boot instead, so a reminder already due fires up to a minute earlier.
type ReminderScheduler struct {
	Pool *pgxpool.Pool
	// Push may be nil; a nil or disabled service sends nothing, as Swift's optional.
	Push          *apns.Service
	Logger        *slog.Logger
	MaxConcurrent int
}

// Job ticks at boot and then every minute.
func (s *ReminderScheduler) Job() jobs.Job {
	return jobs.Job{
		Name:     "ReminderScheduler",
		Schedule: jobs.Every(time.Minute, 0),
		Run: func(ctx context.Context) error {
			fired, err := s.Tick(ctx, time.Now())
			if err == nil && fired > 0 {
				s.logger().Info(fmt.Sprintf("reminders.tick fired=%d", fired))
			}
			return err
		},
	}
}

func (s *ReminderScheduler) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

type dueReminder struct {
	id, tenant            uuid.UUID
	title, body           string
	recurrenceCron        *string
}

// Tick is ReminderScheduler.tick(at:): fires every due reminder and returns how
// many there were.
func (s *ReminderScheduler) Tick(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, tenant_id, title, body, recurrence_cron FROM reminders
		WHERE fired_at IS NULL AND fire_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	var due []dueReminder
	for rows.Next() {
		var r dueReminder
		if err := rows.Scan(&r.id, &r.tenant, &r.title, &r.body, &r.recurrenceCron); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}
	limit := s.MaxConcurrent
	if limit <= 0 {
		limit = 4
	}
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, r := range due {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			s.fire(ctx, r, now)
		})
	}
	wg.Wait()
	return len(due), nil
}

// fire pushes one reminder, then advances or stamps it. A failed push never blocks
// the update, or a dead token would re-fire the same reminder forever.
func (s *ReminderScheduler) fire(ctx context.Context, r dueReminder, now time.Time) {
	tenant := strings.ToUpper(r.tenant.String())
	if s.Push != nil {
		if err := s.Push.Notify(ctx, r.tenant, apns.Message{Title: r.title, Body: r.body, Category: apns.CategoryReminder}); err != nil {
			s.logger().Warn(fmt.Sprintf("reminders.push failed tenant=%s: %v", tenant, err))
		}
	}
	var err error
	next, recurring := s.next(r, now)
	if recurring {
		_, err = s.Pool.Exec(ctx, `UPDATE reminders SET fire_at = $1, fired_at = NULL WHERE id = $2`, next, r.id)
	} else {
		_, err = s.Pool.Exec(ctx, `UPDATE reminders SET fired_at = $1 WHERE id = $2`, now, r.id)
	}
	if err != nil {
		s.logger().Warn(fmt.Sprintf("reminders.persist failed tenant=%s: %v", tenant, err))
	}
}

func (s *ReminderScheduler) next(r dueReminder, now time.Time) (time.Time, bool) {
	if r.recurrenceCron == nil {
		return time.Time{}, false
	}
	expression, ok := cronexpr.Parse(*r.recurrenceCron)
	if !ok {
		return time.Time{}, false
	}
	return NextOccurrence(expression, now)
}

// NextOccurrence is ReminderScheduler.nextOccurrence: the next minute after `after`
// that matches in GMT, searching at most 366 days. false means none, and the
// reminder then fires as a one-shot. The candidate keeps `after`'s sub-second part.
func NextOccurrence(expression cronexpr.Expression, after time.Time) (time.Time, bool) {
	candidate := after.Add(time.Duration(60-after.Second()) * time.Second)
	limit := after.Add(366 * 24 * time.Hour)
	for !candidate.After(limit) {
		if expression.Matches(candidate, time.UTC) {
			return candidate, true
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, false
}
