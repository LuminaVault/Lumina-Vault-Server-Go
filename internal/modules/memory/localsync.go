package memory

import (
	"encoding/base64"
	"encoding/json"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/modules/memory/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v5"
)

const (
	cursorPrefix = "v1."
	kindMemory   = 0
	kindDeletion = 1
	// psqlEpochFromReference is PostgresNIO's date start (2000-01-01) as seconds since
	// Foundation's reference date (2001-01-01).
	psqlEpochFromReference = -31622400.0
	psqlEpochUnixMicros    = int64(946684800) * 1_000_000
	syncDefaultLimit       = 200
	syncMaxLimit           = 500
)

// internetDateTime is ISO8601DateFormatter's default format: no fractional seconds,
// which time.RFC3339 would let through on parsing.
var internetDateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2})$`)

// parseInternetDate is ISO8601DateFormatter().date(from:).
func parseInternetDate(s string) (time.Time, bool) {
	if !internetDateTime.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// syncCursor is LocalMemorySyncCursor. On the wire timestamp is Foundation's default
// Date encoding, a Double of seconds since 2001-01-01. A cursor Swift issued and a
// cursor Go issued decode the same on either server, so clients survive a flip.
type syncCursor struct {
	timestamp float64
	id        uuid.UUID
	kind      int
}

type cursorJSON struct {
	Timestamp wire.Double `json:"timestamp"`
	ID        wire.UUID   `json:"id"`
	Kind      int         `json:"kind"`
}

// referenceSeconds is Swift's Date for a row's timestamp: PostgresNIO decodes the
// microseconds to a Double, then offsets it from its date start.
func referenceSeconds(t time.Time) float64 {
	return float64(t.UnixMicro()-psqlEpochUnixMicros)/1e6 + psqlEpochFromReference
}

// bindTime is that Date bound back as a query parameter: PostgresNIO truncates to
// whole microseconds, so a cursor can land one microsecond early and the row at its
// own position comes back. That is Swift's behaviour and it is kept.
func bindTime(seconds float64) time.Time {
	micros := int64((seconds - psqlEpochFromReference) * 1e6)
	return time.UnixMicro(micros + psqlEpochUnixMicros).UTC()
}

func (c syncCursor) encode() (string, error) {
	b, err := json.Marshal(cursorJSON{Timestamp: wire.Double(c.timestamp), ID: wire.UUID{UUID: c.id}, Kind: c.kind})
	if err != nil {
		return "", err
	}
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeCursor is LocalMemorySyncCursor.decode. Nil for an empty value. The old
// timestamp-only form stays accepted for clients that persisted one.
func decodeCursor(raw string) (*syncCursor, error) {
	if raw == "" {
		return nil, nil
	}
	rest, opaque := strings.CutPrefix(raw, cursorPrefix)
	if !opaque {
		t, ok := parseInternetDate(raw)
		if !ok {
			return nil, errBadCursor
		}
		// A whole-second Date has no Postgres round trip to reproduce.
		return &syncCursor{timestamp: float64(t.Unix()-978307200) + float64(t.Nanosecond())/1e9, kind: kindMemory}, nil
	}
	encoded := strings.NewReplacer("-", "+", "_", "/").Replace(rest)
	encoded += strings.Repeat("=", (4-len(encoded)%4)%4)
	// Data(base64Encoded:) refuses line breaks; Go's decoder skips them.
	if strings.ContainsAny(encoded, "\r\n") {
		return nil, errBadCursor
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errBadCursor
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, errBadCursor
	}
	var (
		out syncCursor
		id  string
	)
	if json.Unmarshal(fields["timestamp"], &out.timestamp) != nil || fields["timestamp"] == nil ||
		json.Unmarshal(fields["id"], &id) != nil || json.Unmarshal(fields["kind"], &out.kind) != nil || fields["kind"] == nil {
		return nil, errBadCursor
	}
	if out.id, err = ids.Parse(id); err != nil {
		return nil, errBadCursor
	}
	if out.kind != kindMemory && out.kind != kindDeletion {
		return nil, errBadCursor
	}
	return &out, nil
}

type syncEvent struct {
	at        time.Time
	id        uuid.UUID
	kind      int
	memory    *db.LocalSyncMemoriesRow
	tombstone *db.LocalSyncTombstonesRow
}

// orderedEvents is LocalMemorySyncEvent.ordered.
func orderedEvents(a, b *syncEvent) bool {
	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	if c := memorystore.CompareIDs(a.id, b.id); c != 0 {
		return c < 0
	}
	return a.kind < b.kind
}

// localSync serves GET /v1/memory/local-sync, mounted only with
// hybridExecution.enabled. With it off the path falls to /:id and answers 400.
func (m *Module) localSync(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	requested, ok := wire.QueryInt(c, "limit")
	if !ok {
		requested = syncDefaultLimit
	}
	limit := max(1, min(requested, syncMaxLimit))
	cursor, err := decodeCursor(wire.Query(c, "cursor"))
	if err != nil {
		return err
	}
	var (
		after     pgtype.Timestamptz
		afterID   uuid.UUID
		inclusive bool
	)
	if cursor != nil {
		after = pgtype.Timestamptz{Time: bindTime(cursor.timestamp), Valid: true}
		afterID = cursor.id
		inclusive = cursor.kind == kindMemory
	}
	ctx := c.Request().Context()
	rows, err := m.q.LocalSyncMemories(ctx, db.LocalSyncMemoriesParams{TenantID: tenant, After: after, AfterID: afterID, RowLimit: int32(limit)}) //nolint:gosec // limit is at most 500
	if err != nil {
		return err
	}
	tombstones, err := m.q.LocalSyncTombstones(ctx, db.LocalSyncTombstonesParams{TenantID: tenant, After: after, AfterID: afterID, Inclusive: inclusive, RowLimit: int32(limit)}) //nolint:gosec // limit is at most 500
	if err != nil {
		return err
	}
	events := make([]*syncEvent, 0, len(rows)+len(tombstones))
	for i := range rows {
		if r := &rows[i]; r.UpdatedAt != nil {
			events = append(events, &syncEvent{at: *r.UpdatedAt, id: r.ID, kind: kindMemory, memory: r})
		}
	}
	for i := range tombstones {
		if t := &tombstones[i]; t.DeletedAt != nil {
			events = append(events, &syncEvent{at: *t.DeletedAt, id: t.ID, kind: kindDeletion, tombstone: t})
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return orderedEvents(events[i], events[j]) })
	events = events[:min(limit, len(events))]

	out := LocalMemorySyncResponse{Memories: []LocalMemorySyncItemDTO{}, DeletedIDs: []wire.UUID{}}
	for _, e := range events {
		if e.memory == nil {
			out.DeletedIDs = append(out.DeletedIDs, wire.UUID{UUID: e.tombstone.MemoryID})
			continue
		}
		created := time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
		if e.memory.CreatedAt != nil {
			created = *e.memory.CreatedAt
		}
		out.Memories = append(out.Memories, LocalMemorySyncItemDTO{
			Id:        wire.UUID{UUID: e.memory.ID},
			Content:   e.memory.Content,
			Source:    MemorySourceKindDTO(memorystore.ParseSourceKind(e.memory.OriginKind)),
			CreatedAt: wire.Time{Time: created},
			UpdatedAt: wire.Time{Time: e.at},
		})
	}
	if len(events) > 0 {
		last := events[len(events)-1]
		next, err := syncCursor{timestamp: referenceSeconds(last.at), id: last.id, kind: last.kind}.encode()
		if err != nil {
			return err
		}
		out.NextCursor = &next
	}
	return wire.JSON(c, http.StatusOK, out)
}
