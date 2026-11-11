package devicerpc_test

import (
	"context"
	"errors"
	"lumina/internal/apns"
	"lumina/internal/dbtest"
	"lumina/internal/devicerpc"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type fakePush struct {
	tenants []uuid.UUID
	msgs    []apns.Message
	err     error
}

func (f *fakePush) Notify(_ context.Context, tenant uuid.UUID, m apns.Message) error {
	f.tenants = append(f.tenants, tenant)
	f.msgs = append(f.msgs, m)
	return f.err
}

type row struct {
	tenant           uuid.UUID
	kind, payload    string
	domain           *string
	created, expires time.Time
	delivered        *time.Time
}

func readRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) row {
	t.Helper()
	var r row
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT tenant_id, kind, domain, payload, created_at, expires_at, delivered_at FROM device_command_queue WHERE id = $1`, id).
		Scan(&r.tenant, &r.kind, &r.domain, &r.payload, &r.created, &r.expires, &r.delivered))
	return r
}

func TestEnqueueStoresTheWriteAndAnnouncesIt(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	// From DeviceCommandQueueTests: a queued write is pending for its owner only.
	push := &fakePush{}
	q := devicerpc.NewQueue(pool, push)
	alice := uuid.New()
	command := devicerpc.NewCommand(devicerpc.KindReminderCreate, "reminders", map[string]string{"title": "Call mom", "due": ""})
	before := time.Now()
	require.NoError(t, q.Enqueue(ctx, alice, command))

	r := readRow(t, pool, command.ID)
	require.Equal(t, alice, r.tenant)
	require.Equal(t, "reminder_create", r.kind)
	require.Equal(t, "reminders", *r.domain)
	require.JSONEq(t, `{"title":"Call mom","due":""}`, r.payload)
	require.Nil(t, r.delivered)
	require.WithinDuration(t, before, r.created, 5*time.Second)
	require.WithinDuration(t, r.created.Add(24*time.Hour), r.expires, time.Second)

	require.Equal(t, []uuid.UUID{alice}, push.tenants)
	require.Equal(t, apns.Message{
		Title:    "Lumina has something for your iPhone",
		Body:     "Open LuminaVault to add the reminder “Call mom”.",
		Category: apns.CategoryDeviceCommand,
		Payload:  map[string]string{"deviceCommandID": ids.Upper(command.ID)},
	}, push.msgs[0])

	// Bob has nothing queued.
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM device_command_queue WHERE tenant_id = $1`, uuid.New()).Scan(&n))
	require.Zero(t, n)

	// The push is best effort: a failed push or none at all still queues.
	failing := devicerpc.NewQueue(pool, &fakePush{err: errors.New("apns down")})
	calendar := devicerpc.NewCommand(devicerpc.KindCalendarCreate, "calendar", map[string]string{"title": "Dentist"})
	require.NoError(t, failing.Enqueue(ctx, alice, calendar))
	readRow(t, pool, calendar.ID)

	silent := devicerpc.NewQueue(pool, nil)
	noDomain := devicerpc.NewCommand(devicerpc.KindCalendarCreate, "", nil)
	require.NoError(t, silent.Enqueue(ctx, alice, noDomain))
	r = readRow(t, pool, noDomain.ID)
	require.Nil(t, r.domain)
	require.Equal(t, "{}", r.payload)

	// The same id twice is an error, not a second row.
	require.Error(t, silent.Enqueue(ctx, alice, noDomain))
}

// From DeviceCommandQueueTests: only writes queue, and the push says what is waiting.
func TestOnlyWritesQueueAndThePushSaysWhatIsWaiting(t *testing.T) {
	require.Equal(t, []devicerpc.Kind{devicerpc.KindReminderCreate, devicerpc.KindCalendarCreate}, devicerpc.Queueable)
	require.True(t, devicerpc.IsQueueable(devicerpc.KindCalendarCreate))
	require.False(t, devicerpc.IsQueueable(devicerpc.KindDeviceFetch))
	require.False(t, devicerpc.IsQueueable(devicerpc.KindPing))
	require.Equal(t, "Open LuminaVault to add the reminder “Call mom”.",
		devicerpc.Announcement(devicerpc.KindReminderCreate, map[string]string{"title": "Call mom"}))
	require.Equal(t, "Open LuminaVault to add “an item” to your calendar.",
		devicerpc.Announcement(devicerpc.KindCalendarCreate, map[string]string{}))
	require.Equal(t, "Open LuminaVault to finish a request from Lumina.", devicerpc.Announcement(devicerpc.KindDeviceFetch, nil))
}
