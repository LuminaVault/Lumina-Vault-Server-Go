// Package ratelimit is the Go port of LuminaVaultServer
// Sources/App/Middleware/RateLimitMiddleware.swift with the in-memory
// PersistDriver (`rateLimit.storageKind=memory`, the default).
//
// Swift keeps a fixed window per key: the first request opens it, every
// request (allowed or not) increments it, and a request more than Window after
// the window opened starts a new one. Over the limit is 429
// `rate limit exceeded` with Retry-After. Buckets are namespaced by policy
// name, so every module whose routes use the same policy must share one
// Store: in Swift, kanban and spaces both count against "settingsByUser".
package ratelimit

import (
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

// Policy is Swift's RateLimitPolicy. TierScale nil means every tier gets Max.
type Policy struct {
	Name      string
	Max       int
	Window    time.Duration
	TierScale func(tier string) float64
}

// EffectiveMax is RateLimitPolicy.effectiveMax for a caller with a user row:
// max(1, (Max * scale).rounded()). rounded() is away from zero, as math.Round.
func (p Policy) EffectiveMax(tier string) int {
	if p.TierScale == nil {
		return p.Max
	}
	return max(1, int(math.Round(float64(p.Max)*p.TierScale(tier))))
}

// ForTier is p with Max set to its EffectiveMax. The name is unchanged, so every
// tier counts against one bucket per caller, as in Swift.
func (p Policy) ForTier(tier string) Policy {
	p.Max = p.EffectiveMax(tier)
	return p
}

// StandardTierScale is RateLimitPolicy.standardTierScale over users.tier. A
// value that is no UserTier reads as lapsed (User.tierEnum, EntitlementChecker.swift).
func StandardTierScale(tier string) float64 {
	switch tier {
	case "ultimate":
		return 4
	case "pro":
		return 2
	case "trial":
		return 1
	case "archived":
		return 0.1
	default: // free, lapsed, unknown
		return 0.34
	}
}

// SettingsByUser is RateLimitPolicy.settingsByUser: 30 per 60 s, keyed by user or IP.
var SettingsByUser = Policy{Name: "settingsByUser", Max: 30, Window: 60 * time.Second}

// AchievementsByUser is RateLimitPolicy.achievementsByUser: 60 per 60 s, keyed by user or IP.
var AchievementsByUser = Policy{Name: "achievementsByUser", Max: 60, Window: 60 * time.Second}

// CaptureByUser is RateLimitPolicy.captureByUser: 60 per 60 s on the standard
// tier scale. Capture, memory upsert, memory search and knowledge share it.
var CaptureByUser = Policy{Name: "captureByUser", Max: 60, Window: 60 * time.Second, TierScale: StandardTierScale}

// MeTodayByUser is RateLimitPolicy.meTodayByUser: 60 per 60 s, keyed by user or
// IP. `me` and /v1/me/suggestions share it.
var MeTodayByUser = Policy{Name: "meTodayByUser", Max: 60, Window: 60 * time.Second}

// VaultUploadByUser is RateLimitPolicy.vaultUploadByUser: 30 per 60 s, keyed by
// user or IP, on every route of the /v1/vault/files group.
var VaultUploadByUser = Policy{Name: "vaultUploadByUser", Max: 30, Window: 60 * time.Second}

// VaultExportByUser is RateLimitPolicy.vaultExportByUser: 3 per 300 s on the
// standard tier scale, keyed by user or IP.
var VaultExportByUser = Policy{Name: "vaultExportByUser", Max: 3, Window: 300 * time.Second, TierScale: StandardTierScale}

// QueryByUser is RateLimitPolicy.queryByUser: 30 per 60 s on the standard tier
// scale. The calendar brief and /v1/query share it.
var QueryByUser = Policy{Name: "queryByUser", Max: 30, Window: 60 * time.Second, TierScale: StandardTierScale}

// MemoryCompileByUser is RateLimitPolicy.memoryCompileByUser: 5 per 60 s on the
// standard tier ladder.
var MemoryCompileByUser = Policy{Name: "memoryCompileByUser", Max: 5, Window: 60 * time.Second, TierScale: StandardTierScale}

// ErrLimited is the Swift 429 body.
// VisionEmbedByUserPerMinute and VisionEmbedByUserDaily are
// RateLimitPolicy.visionEmbedByUser*: stacked on /v1/vision, keyed by user or IP.
var (
	VisionEmbedByUserPerMinute = Policy{Name: "visionEmbedByUserPerMinute", Max: 10, Window: 60 * time.Second}
	VisionEmbedByUserDaily     = Policy{Name: "visionEmbedByUserDaily", Max: 200, Window: 86400 * time.Second}
)

var ErrLimited = wire.NewError(http.StatusTooManyRequests, "rate limit exceeded")

type bucket struct {
	start time.Time
	count int
	// expires mirrors the PersistDriver TTL (window + 5 s, refreshed on every hit).
	expires time.Time
}

// Store holds every bucket of the process. Create one in main and pass it to
// every module.
type Store struct {
	mu        sync.Mutex
	buckets   map[string]bucket
	now       func() time.Time
	lastSweep time.Time
}

func NewStore() *Store { return &Store{buckets: map[string]bucket{}, now: time.Now} }

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Hit counts one request for key under p. It reports whether the request is
// over the limit and, if so, the Retry-After seconds Swift would send.
func (s *Store) Hit(p Policy, key string) (limited bool, retryAfter int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweep(now)
	k := "rl:" + p.Name + ":" + key
	b, ok := s.buckets[k]
	if !ok || !now.Before(b.expires) {
		b = bucket{start: now}
	}
	elapsed := now.Sub(b.start)
	if elapsed > p.Window {
		b = bucket{start: now}
		elapsed = 0
	}
	b.count++
	b.expires = now.Add(p.Window + 5*time.Second)
	s.buckets[k] = b
	if b.count <= p.Max {
		return false, 0
	}
	// Swift: max(1, Int(window - (now - start))), Int() truncating.
	return true, max(1, int((p.Window - elapsed).Seconds()))
}

// sweep drops expired buckets at most once a minute so the map stays bounded.
func (s *Store) sweep(now time.Time) {
	if now.Sub(s.lastSweep) < time.Minute {
		return
	}
	s.lastSweep = now
	for k, b := range s.buckets {
		if !now.Before(b.expires) {
			delete(s.buckets, k)
		}
	}
}

// Middleware limits requests under p. Put it after auth.Identify: the key
// uses the identity when there is one, as Swift's userOrIPKey does.
func Middleware(s *Store, p Policy, key func(*echo.Context) string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if limited, retry := s.Hit(p, key(c)); limited {
				c.Response().Header().Set("Retry-After", strconv.Itoa(retry))
				return ErrLimited
			}
			return next(c)
		}
	}
}

// TieredMiddleware limits requests under p scaled to the caller's tier. tier
// reports false when there is no user row to read a tier from: an unidentified
// caller, as in Swift's `context.identity?.tierEnum`, gets the baseline Max.
func TieredMiddleware(s *Store, p Policy, key func(*echo.Context) string, tier func(*echo.Context) (string, bool)) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			policy := p
			if t, ok := tier(c); ok {
				policy = p.ForTier(t)
			}
			if limited, retry := s.Hit(policy, key(c)); limited {
				c.Response().Header().Set("Retry-After", strconv.Itoa(retry))
				return ErrLimited
			}
			return next(c)
		}
	}
}

// UserOrIPKey is RateLimitPolicy.userOrIPKey: "u:<UPPERCASE UUID>" for an
// identified caller, otherwise "ip:" + IPKey.
func UserOrIPKey(c *echo.Context) string {
	if id, ok := auth.Identity(c); ok {
		return "u:" + ids.Upper(id)
	}
	return "ip:" + IPKey(c.Request())
}

// IPKey is RateLimitPolicy.ipKey: the first X-Forwarded-For entry, else
// X-Real-IP, else "anon". The socket address is never used.
func IPKey(r *http.Request) string {
	if xff, ok := r.Header["X-Forwarded-For"]; ok && len(xff) > 0 {
		// Swift's split(separator:) drops empty pieces before taking the first.
		for part := range strings.SplitSeq(xff[0], ",") {
			if part != "" {
				return strings.Trim(part, " \t")
			}
		}
		return "anon"
	}
	if real, ok := r.Header["X-Real-Ip"]; ok && len(real) > 0 {
		return real[0]
	}
	return "anon"
}

// InFlight is InFlightLimitStore: concurrent requests per key. Swift gives each
// middleware its own store, so build one per route group.
type InFlight struct {
	mu     sync.Mutex
	counts map[string]int
}

// NewInFlight builds an empty store.
func NewInFlight() *InFlight { return &InFlight{counts: map[string]int{}} }

func (s *InFlight) acquire(key string, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts[key] >= limit {
		return false
	}
	s.counts[key]++
	return true
}

func (s *InFlight) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts[key] <= 1 {
		delete(s.counts, key)
		return
	}
	s.counts[key]--
}

// errTooManyStreams is InFlightLimitMiddleware's 429.
var errTooManyStreams = wire.NewError(http.StatusTooManyRequests, "too many concurrent streams")

// InFlightMiddleware is InFlightLimitMiddleware: at most limit (floored at 1) requests
// per key at once, held until the response has been written. Over it, a 429 with
// Retry-After: 1.
func InFlightMiddleware(s *InFlight, limit int, key func(*echo.Context) string) echo.MiddlewareFunc {
	limit = max(1, limit)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			k := "if:" + key(c)
			if !s.acquire(k, limit) {
				c.Response().Header().Set("Retry-After", "1")
				return errTooManyStreams
			}
			defer s.release(k)
			return next(c)
		}
	}
}
