package devicerpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/ids"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QueueLifetime is DeviceCommandQueue.lifetime: a write older than this is
// dropped rather than run a day late.
const QueueLifetime = 24 * time.Hour

// Queueable is DeviceCommandQueue.queueable: only writes queue; a read the
// phone missed is simply unavailable.
var Queueable = []Kind{KindReminderCreate, KindCalendarCreate}

// IsQueueable reports whether kind is in Queueable.
func IsQueueable(kind Kind) bool {
	for _, k := range Queueable {
		if k == kind {
			return true
		}
	}
	return false
}

// Announcement is DeviceCommandQueue.announcement(for:), the push body.
func Announcement(kind Kind, payload map[string]string) string {
	title := payload["title"]
	if title == "" {
		title = "an item"
	}
	switch kind {
	case KindReminderCreate:
		return "Open LuminaVault to add the reminder “" + title + "”."
	case KindCalendarCreate:
		return "Open LuminaVault to add “" + title + "” to your calendar."
	default:
		return "Open LuminaVault to finish a request from Lumina."
	}
}

// Notifier sends one alert to every device of a tenant. *apns.Service is the
// real one.
type Notifier interface {
	Notify(ctx context.Context, tenant uuid.UUID, m apns.Message) error
}

// Queue is the enqueue half of DeviceCommandQueue, over device_command_queue.
// The read and result halves are the apple module's routes.
type Queue struct {
	pool *pgxpool.Pool
	push Notifier
	now  func() time.Time
}

// NewQueue returns a Queue. push may be nil: the push is best effort.
func NewQueue(pool *pgxpool.Pool, push Notifier) *Queue {
	return &Queue{pool: pool, push: push, now: time.Now}
}

// Enqueue stores a write the app wasn't connected for, then announces it with
// a visible push (iOS throttles silent pushes and never delivers them to a
// force-quit app). It returns an error only for the insert; a failed push is
// logged and the command stays queued. The caller checks IsQueueable first, as
// PersonalDataTools.deviceWrite does.
func (q *Queue) Enqueue(ctx context.Context, tenant uuid.UUID, cmd Command) error {
	payload := cmd.Payload
	if payload == nil {
		payload = map[string]string{}
	}
	args, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var domain *string
	if cmd.Domain != "" {
		domain = &cmd.Domain
	}
	now := q.now()
	if _, err := q.pool.Exec(ctx, `INSERT INTO device_command_queue (id, tenant_id, kind, domain, payload, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		cmd.ID, tenant, string(cmd.Kind), domain, string(args), now, now.Add(QueueLifetime)); err != nil {
		return err
	}
	slog.InfoContext(ctx, fmt.Sprintf("device.command queued tenant=%s id=%s kind=%s", ids.Upper(tenant), ids.Upper(cmd.ID), cmd.Kind))

	if q.push == nil {
		return nil
	}
	if err := q.push.Notify(ctx, tenant, apns.Message{
		Title:    "Lumina has something for your iPhone",
		Body:     Announcement(cmd.Kind, cmd.Payload),
		Category: apns.CategoryDeviceCommand,
		Payload:  map[string]string{"deviceCommandID": ids.Upper(cmd.ID)},
	}); err != nil {
		slog.WarnContext(ctx, fmt.Sprintf("device.command push failed id=%s: %v", ids.Upper(cmd.ID), err))
	}
	return nil
}
