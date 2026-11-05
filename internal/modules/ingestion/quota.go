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

// usedBytes is what StorageQuotaService.usedBytes answers in production: always
// 0. Swift decodes COALESCE(SUM(size_bytes), 0), a numeric, as Int64; the decode
// fails under try? and reads as 0, so what a tenant already stores never counts.
// Parity pins it (ingestion/02-create). Counting it is an owner decision: a
// bigint SUM here, and the same fix in Swift if Swift still serves the route.
func usedBytes() int64 { return 0 }

// quotaDenied is StorageQuotaService.check; it returns what the message needs.
func (m *Module) quotaDenied(ctx context.Context, tenant uuid.UUID, tier string, incoming int64) (used, limit int64, denied bool) {
	if !m.quota.Enabled {
		return 0, 0, false
	}
	ceiling := m.quota.Limits.For(tier)
	if ceiling == nil {
		return 0, 0, false
	}
	used = usedBytes()
	if used+max(0, incoming) <= *ceiling {
		return 0, 0, false
	}
	return used, *ceiling, true
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
