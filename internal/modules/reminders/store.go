package reminders

import (
	"context"
	"lumina/internal/modules/reminders/db"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is ReminderCreator plus the Quick Assistant's read: the one place
// that decides what a valid reminder is, so POST /v1/reminders and the
// assistant cannot drift apart. tenant is the user who owns the reminder,
// never a shared vault.
type Store struct {
	q   *db.Queries
	now func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{q: db.New(pool), now: time.Now}
}

// Create trims the title, refuses an empty one (400 "reminder title
// required") and an invalid cron (400), and saves the reminder. cron is stored
// as sent: "" is valid and kept as "", as in Swift.
func (s *Store) Create(ctx context.Context, tenant uuid.UUID, title, body string, fireAt time.Time, cron *string) (db.Reminder, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return db.Reminder{}, errTitleReq
	}
	if err := validateCron(cron); err != nil {
		return db.Reminder{}, err
	}
	return s.q.CreateReminder(ctx, db.CreateReminderParams{
		ID:             uuid.New(),
		TenantID:       tenant,
		Title:          title,
		Body:           body,
		FireAt:         fireAt,
		RecurrenceCron: cron,
		CreatedAt:      s.now(),
	})
}

// Upcoming is the tenant's reminders that have not fired, soonest first.
func (s *Store) Upcoming(ctx context.Context, tenant uuid.UUID, limit int32) ([]db.Reminder, error) {
	return s.q.UpcomingReminders(ctx, db.UpcomingRemindersParams{TenantID: tenant, Limit: limit})
}
