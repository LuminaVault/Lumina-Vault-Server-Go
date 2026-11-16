package ingestion

import (
	"context"
	"fmt"
	"lumina/internal/config"

	"github.com/google/uuid"
)

// Limits is StorageQuotaService.Limits: what a tier may store, in bytes. nil is
// unlimited and 0 is "may not grow at all", two different values.
type Limits struct {
	Trial, Pro, Ultimate, Lapsed, Free *int64
}

func bytes(n int64) *int64 { return &n }

// DefaultLimits is Limits.default.
func DefaultLimits() Limits {
	const gib = int64(1024 * 1024 * 1024)
	return Limits{Trial: bytes(5 * gib), Pro: bytes(100 * gib), Ultimate: bytes(1024 * gib), Lapsed: bytes(0), Free: bytes(gib)}
}

// For is Limits.bytes(for:) over users.tier. Text that is no UserTier reads as
// lapsed (User.tierEnum).
func (l Limits) For(tier string) *int64 {
	switch tier {
	case "trial":
		return l.Trial
	case "pro":
		return l.Pro
	case "ultimate":
		return l.Ultimate
	case "free":
		return l.Free
	default:
		return l.Lapsed
	}
}

// LimitFrom is StorageQuotaService.limit: a negative configured value is
// unlimited, and everything else, 0 included, is taken literally.
func LimitFrom(configured int64) *int64 {
	if configured < 0 {
		return nil
	}
	return &configured
}

// Quota is StorageQuotaService, the per-tenant ceiling. Only this route has one:
// the other vault writers store text, so this is where a total-bytes limit binds.
type Quota struct {
	Limits  Limits
	Enabled bool
}

// QuotaFromConfig reads storage.quota.*. lapsed isn't configurable, as in Swift.
func QuotaFromConfig(cfg config.Config) Quota {
	return Quota{
		Enabled: cfg.StorageQuotaEnabled,
		Limits: Limits{
			Trial:    LimitFrom(cfg.StorageQuotaTrialBytes),
			Pro:      LimitFrom(cfg.StorageQuotaProBytes),
			Ultimate: LimitFrom(cfg.StorageQuotaUltimateBytes),
			Lapsed:   bytes(0),
			Free:     LimitFrom(cfg.StorageQuotaFreeBytes),
		},
	}
}

// usedBytes is what the tenant stores: its vault files plus its trash, which
// stays on disk until the 30-day purge. Swift's StorageQuotaService decoded
// COALESCE(SUM(size_bytes), 0), a numeric, as Int64 under try?, so it always read
// 0 and only the new batch met the ceiling. Fixed after the archive (backlog
// 1.3): the SUM is cast to bigint. Hand-written because vault_trash postdates the
// schema snapshot sqlc reads.
func (m *Module) usedBytes(ctx context.Context, tenant uuid.UUID) (int64, error) {
	var used int64
	err := m.pool.QueryRow(ctx, `SELECT
		COALESCE((SELECT SUM(size_bytes) FROM vault_files WHERE tenant_id = $1), 0)::bigint +
		COALESCE((SELECT SUM(size_bytes) FROM vault_trash WHERE tenant_id = $1), 0)::bigint`, tenant).Scan(&used)
	return used, err
}

// quotaDenied is StorageQuotaService.check; it returns what the message needs.
func (m *Module) quotaDenied(ctx context.Context, tenant uuid.UUID, tier string, incoming int64) (used, limit int64, denied bool, err error) {
	if !m.quota.Enabled {
		return 0, 0, false, nil
	}
	ceiling := m.quota.Limits.For(tier)
	if ceiling == nil {
		return 0, 0, false, nil
	}
	if used, err = m.usedBytes(ctx, tenant); err != nil {
		return 0, 0, false, err
	}
	if used+max(0, incoming) <= *ceiling {
		return 0, 0, false, nil
	}
	return used, *ceiling, true, nil
}

// quotaMessage is StorageQuotaService.message, the body of the 413.
func quotaMessage(used, limit int64) string {
	return "storage quota exceeded: " + formatBytes(used) + " of " + formatBytes(limit) + " used"
}

// formatBytes is StorageQuotaService.format.
func formatBytes(n int64) string {
	for _, u := range []struct {
		scale int64
		name  string
	}{
		{1024 * 1024 * 1024 * 1024, "TiB"},
		{1024 * 1024 * 1024, "GiB"},
		{1024 * 1024, "MiB"},
		{1024, "KiB"},
	} {
		if n >= u.scale {
			return fmt.Sprintf("%.1f %s", float64(n)/float64(u.scale), u.name)
		}
	}
	return fmt.Sprintf("%d B", n)
}
