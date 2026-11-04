// Package diskguard refuses user-file writes that would leave the data volume
// nearly full: the Go port of LuminaVaultServer
// Sources/App/Vault/DiskSpaceGuard.swift.
//
// The vault's PVC is a directory on the node's root disk with no size of its
// own, and a full root disk takes the whole node down. A write is refused when,
// after it, free space would drop below MinFreeBytes or below MinFreeRatio of
// the volume. A floor of zero is off. When the volume can't be measured the
// guard fails open and logs, so an unreadable statvfs doesn't block uploads.
//
// DiskSpaceMonitorService, the once-a-minute log, stays in Swift for wave 4
// (wave-4-plan.md "Jobs"). Sample is here for when it moves.
package diskguard

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

// Swift's defaults (DiskSpaceGuard.swift). Config carries the two floors.
const (
	DefaultMinFreeBytes int64 = 3 * 1024 * 1024 * 1024
	DefaultMinFreeRatio       = 0.10
	DefaultCacheTTL           = 5 * time.Second
	DefaultWarnInterval       = 60 * time.Second
)

// Space is the free space on the volume that holds the vault.
type Space struct {
	FreeBytes  int64
	TotalBytes int64
}

// Provider measures the volume holding path.
type Provider func(path string) (Space, error)

// Options are the guard's injectable parts. Zero values get the defaults.
type Options struct {
	CacheTTL     time.Duration
	WarnInterval time.Duration
	Provider     Provider
	Now          func() time.Time
	Logger       *slog.Logger
}

// Guard caches its measurement for CacheTTL, so a check costs one syscall every
// few seconds at most. One process should share one Guard, as Swift does.
type Guard struct {
	path         string
	minFreeBytes int64
	minFreeRatio float64
	cacheTTL     time.Duration
	warnInterval time.Duration
	provider     Provider
	now          func() time.Time
	logger       *slog.Logger

	mu                  sync.Mutex
	cached              *Space // nil inside the TTL means the last measurement failed
	cachedAt            time.Time
	lastWarnedAt        time.Time
	lastFailureLoggedAt time.Time
}

// New clamps the floors as Swift does: bytes to at least 0, ratio to 0...1.
func New(path string, minFreeBytes int64, minFreeRatio float64, opts Options) *Guard {
	g := &Guard{
		path:         path,
		minFreeBytes: max(0, minFreeBytes),
		minFreeRatio: min(max(0, minFreeRatio), 1),
		cacheTTL:     opts.CacheTTL,
		warnInterval: opts.WarnInterval,
		provider:     opts.Provider,
		now:          opts.Now,
		logger:       opts.Logger,
	}
	if g.cacheTTL == 0 {
		g.cacheTTL = DefaultCacheTTL
	}
	if g.warnInterval == 0 {
		g.warnInterval = DefaultWarnInterval
	}
	if g.provider == nil {
		g.provider = StatvfsProvider
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}
	return g
}

// Check returns *InsufficientStorageError when writing incomingBytes would
// leave the volume below either floor. Return it from a handler as it is:
// wire.HTTPErrorHandler renders it.
func (g *Guard) Check(incomingBytes int64) error {
	space, ok := g.measure()
	if !ok {
		return nil
	}
	if IsBelowFloor(space, 0, g.minFreeBytes, g.minFreeRatio) {
		g.warnIfDue(space)
	}
	if IsBelowFloor(space, max(0, incomingBytes), g.minFreeBytes, g.minFreeRatio) {
		return &InsufficientStorageError{}
	}
	return nil
}

// Sample re-measures, respecting the cache, and logs if the volume is already
// below a floor.
func (g *Guard) Sample() {
	if space, ok := g.measure(); ok && IsBelowFloor(space, 0, g.minFreeBytes, g.minFreeRatio) {
		g.warnIfDue(space)
	}
}

// IsBelowFloor reports whether space, less incoming, is below either floor.
func IsBelowFloor(space Space, incoming, minFreeBytes int64, minFreeRatio float64) bool {
	after := space.FreeBytes - incoming
	if after < minFreeBytes {
		return true
	}
	if space.TotalBytes <= 0 || minFreeRatio <= 0 {
		return false
	}
	return float64(after)/float64(space.TotalBytes) < minFreeRatio
}

func (g *Guard) measure() (Space, bool) {
	current := g.now()
	g.mu.Lock()
	if !g.cachedAt.IsZero() && current.Sub(g.cachedAt) < g.cacheTTL {
		defer g.mu.Unlock()
		if g.cached == nil {
			return Space{}, false
		}
		return *g.cached, true
	}
	g.mu.Unlock()

	space, err := g.provider(g.path)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.cachedAt = current
	if err == nil {
		g.cached = &space
		return space, true
	}
	// The failure is cached too, so a broken mount isn't one failing syscall
	// per upload.
	g.cached = nil
	if g.lastFailureLoggedAt.IsZero() || current.Sub(g.lastFailureLoggedAt) >= g.warnInterval {
		g.lastFailureLoggedAt = current
		g.logger.Warn("storage.disk_check_failed", "path", g.path, "error", err.Error())
	}
	return Space{}, false
}

func (g *Guard) warnIfDue(space Space) {
	current := g.now()
	g.mu.Lock()
	due := g.lastWarnedAt.IsZero() || current.Sub(g.lastWarnedAt) >= g.warnInterval
	if due {
		g.lastWarnedAt = current
	}
	g.mu.Unlock()
	if !due {
		return
	}
	// Error, not Warn: the platform's Loki alert counts only error and critical,
	// and a node about to run out of disk is what that alert is for.
	g.logger.Error("storage.disk_low",
		"path", g.path,
		"free_bytes", space.FreeBytes,
		"total_bytes", space.TotalBytes,
		"min_free_bytes", g.minFreeBytes,
		"min_free_ratio", g.minFreeRatio,
	)
}

// StatvfsProvider measures the nearest existing ancestor of path, which on first
// boot, before the vault root exists, is the volume it will be created on. It
// reads blocks available to an unprivileged process, not free blocks: the server
// doesn't run as root, so the reserved blocks aren't space it can use.
func StatvfsProvider(path string) (Space, error) {
	probe, err := filepath.Abs(path)
	if err != nil {
		return Space{}, err
	}
	for probe != "/" {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		probe = filepath.Dir(probe)
	}
	fragment, available, blocks, err := statvfs(probe)
	if err != nil {
		return Space{}, fmt.Errorf("statvfs(%s) failed: %w", probe, err)
	}
	return Space{FreeBytes: clampInt64(available * fragment), TotalBytes: clampInt64(blocks * fragment)}, nil
}

// clampInt64 is Swift's Int64(clamping:).
func clampInt64(v uint64) int64 {
	const maxInt64 = 1<<63 - 1
	if v > maxInt64 {
		return maxInt64
	}
	return int64(v)
}

// insufficientStorageBody is Swift's bodyData: the coded error envelope, with
// keys sorted. It has no cta, because nothing the user can buy fixes a full
// server disk, and no Retry-After, because there is no honest time to promise.
const insufficientStorageBody = `{"error":{"code":"insufficient_storage","message":"LuminaVault is short on storage space right now, so this wasn't saved. Please try again later."}}`

// InsufficientStorageError is the HTTP 507 for a refused write. Its
// Content-Type is exactly application/json, without the charset of the
// wire.Error envelope.
type InsufficientStorageError struct{}

func (*InsufficientStorageError) Error() string { return "insufficient_storage" }

// StatusCode is 507.
func (*InsufficientStorageError) StatusCode() int { return http.StatusInsufficientStorage }

// WriteResponse implements wire.ResponseError.
func (*InsufficientStorageError) WriteResponse(c *echo.Context) error {
	return c.Blob(http.StatusInsufficientStorage, "application/json", []byte(insufficientStorageBody))
}
