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
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

// Policy is Swift's RateLimitPolicy without tierScale (no ported policy uses one yet).
type Policy struct {
	Name   string
	Max    int
	Window time.Duration
}

// SettingsByUser is RateLimitPolicy.settingsByUser: 30 per 60 s, keyed by user or IP.
var SettingsByUser = Policy{Name: "settingsByUser", Max: 30, Window: 60 * time.Second}

// AchievementsByUser is RateLimitPolicy.achievementsByUser: 60 per 60 s, keyed by user or IP.
var AchievementsByUser = Policy{Name: "achievementsByUser", Max: 60, Window: 60 * time.Second}

// ErrLimited is the Swift 429 body.
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
