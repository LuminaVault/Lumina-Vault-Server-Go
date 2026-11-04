package diskguard_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/diskguard"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

const gib int64 = 1024 * 1024 * 1024

// probe counts provider calls and lets a test move the clock and the reading:
// DiskSpaceGuardTests' Probe.
type probe struct {
	mu    sync.Mutex
	calls int
	space *diskguard.Space
	clock time.Time
	logs  bytes.Buffer
}

func newProbe(free, total int64) *probe {
	return &probe{space: &diskguard.Space{FreeBytes: free, TotalBytes: total}, clock: time.Unix(1_800_000_000, 0)}
}

func (p *probe) guardrail(minFreeBytes int64, minFreeRatio float64) *diskguard.Guard {
	return diskguard.New("/irrelevant", minFreeBytes, minFreeRatio, diskguard.Options{
		Provider: func(string) (diskguard.Space, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.calls++
			if p.space == nil {
				return diskguard.Space{}, errors.New("statvfs(/irrelevant) failed: errno 5")
			}
			return *p.space, nil
		},
		Now:    func() time.Time { p.mu.Lock(); defer p.mu.Unlock(); return p.clock },
		Logger: slog.New(slog.NewJSONHandler(&p.logs, nil)),
	})
}

func (p *probe) defaults() *diskguard.Guard { return p.guardrail(3*gib, 0.10) }

func (p *probe) advance(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clock = p.clock.Add(d)
}

func requireInsufficient(t *testing.T, err error) {
	t.Helper()
	var insufficient *diskguard.InsufficientStorageError
	require.ErrorAs(t, err, &insufficient)
}

// Swift: "allows a write that leaves both floors intact".
func TestAllowsWriteThatLeavesBothFloorsIntact(t *testing.T) {
	require.NoError(t, newProbe(50*gib, 100*gib).defaults().Check(10*gib))
}

// Swift: "rejects when the write would cross the byte floor". 4 GiB free of 10:
// the ratio floor (1 GiB) is fine, but a 2 GiB write leaves 2 GiB < 3 GiB.
func TestRejectsWhenWriteCrossesByteFloor(t *testing.T) {
	requireInsufficient(t, newProbe(4*gib, 10*gib).defaults().Check(2*gib))
}

// Swift: "rejects when the write would cross the ratio floor". 1 TiB volume:
// 120 GiB free clears 3 GiB easily, but a 30 GiB upload leaves 90 GiB, under 10%.
func TestRejectsWhenWriteCrossesRatioFloor(t *testing.T) {
	requireInsufficient(t, newProbe(120*gib, 1024*gib).defaults().Check(30*gib))
}

// Swift: "rejects even tiny writes once the volume is already low".
func TestRejectsTinyWritesOnceLow(t *testing.T) {
	requireInsufficient(t, newProbe(1*gib, 100*gib).defaults().Check(1))
}

// Swift: "zero floors only refuse what does not fit".
func TestZeroFloorsOnlyRefuseWhatDoesNotFit(t *testing.T) {
	g := newProbe(1000, 100*gib).guardrail(0, 0)
	require.NoError(t, g.Check(1000))
	requireInsufficient(t, g.Check(1001))
}

// Swift: "caches the measurement for a few seconds".
func TestCachesMeasurement(t *testing.T) {
	p := newProbe(50*gib, 100*gib)
	g := p.defaults()
	for range 20 {
		require.NoError(t, g.Check(1))
	}
	require.Equal(t, 1, p.calls)

	// The disk fills; a cached reading may lag by at most the TTL.
	p.space = &diskguard.Space{FreeBytes: gib, TotalBytes: 100 * gib}
	require.NoError(t, g.Check(1))
	p.advance(6 * time.Second)
	requireInsufficient(t, g.Check(1))
	require.Equal(t, 2, p.calls)
}

// Swift: "fails open when the volume cannot be measured".
func TestFailsOpenWhenUnmeasurable(t *testing.T) {
	p := newProbe(0, 0)
	p.space = nil
	require.NoError(t, p.defaults().Check(10*gib))
}

// Swift: "statvfs reads a real volume".
func TestStatvfsReadsRealVolume(t *testing.T) {
	dir := t.TempDir()
	space, err := diskguard.StatvfsProvider(dir)
	require.NoError(t, err)
	require.Positive(t, space.TotalBytes)
	require.GreaterOrEqual(t, space.FreeBytes, int64(0))
	require.LessOrEqual(t, space.FreeBytes, space.TotalBytes)
	// A path that doesn't exist yet measures its nearest existing ancestor.
	missing, err := diskguard.StatvfsProvider(filepath.Join(dir, "not-yet", "deeper"))
	require.NoError(t, err)
	require.Equal(t, space.TotalBytes, missing.TotalBytes)
}

// Swift: "507 body uses the coded error envelope", with the exact bytes.
func Test507BodyUsesCodedEnvelope(t *testing.T) {
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	g := newProbe(0, 100*gib).defaults()
	e.POST("/write", func(*echo.Context) error { return g.Check(1) })

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/write", nil))

	require.Equal(t, 507, rec.Code)
	require.Equal(t, []string{"application/json"}, rec.Header().Values("Content-Type"), "no charset, unlike wire.Error")
	require.Equal(t,
		`{"error":{"code":"insufficient_storage","message":"LuminaVault is short on storage space right now, so this wasn't saved. Please try again later."}}`,
		rec.Body.String())
	require.Empty(t, rec.Header().Get("Retry-After"))

	var body struct {
		Error map[string]string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotContains(t, body.Error, "cta")
}

func TestCheckIsNilOnOKThroughHandler(t *testing.T) {
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	g := newProbe(50*gib, 100*gib).defaults()
	e.POST("/write", func(c *echo.Context) error {
		if err := g.Check(1); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/write", nil))
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestIsBelowFloor(t *testing.T) {
	space := diskguard.Space{FreeBytes: 100, TotalBytes: 1000}
	require.False(t, diskguard.IsBelowFloor(space, 0, 100, 0.10), "exactly on both floors")
	require.True(t, diskguard.IsBelowFloor(space, 1, 100, 0), "byte floor")
	require.True(t, diskguard.IsBelowFloor(space, 1, 0, 0.10), "ratio floor")
	require.False(t, diskguard.IsBelowFloor(diskguard.Space{FreeBytes: 5}, 0, 0, 0.10), "unknown total skips the ratio")
	require.True(t, diskguard.IsBelowFloor(diskguard.Space{}, 1, 0, 0), "a write that doesn't fit")
}

func TestFloorsAreClamped(t *testing.T) {
	// Negative floors are off; a ratio above 1 is 1.
	require.NoError(t, newProbe(10, 100).guardrail(-5, -1).Check(10))
	requireInsufficient(t, newProbe(100, 100).guardrail(0, 9).Check(1))
}

func TestNegativeIncomingCountsAsZero(t *testing.T) {
	require.NoError(t, newProbe(50*gib, 100*gib).defaults().Check(-10*gib))
	requireInsufficient(t, newProbe(4*gib, 10*gib).defaults().Check(2*gib))
}

func TestLowDiskLogRepeatsOnlyEachInterval(t *testing.T) {
	p := newProbe(gib, 100*gib)
	g := p.defaults()
	for range 3 {
		requireInsufficient(t, g.Check(1))
	}
	require.Equal(t, 1, strings.Count(p.logs.String(), "storage.disk_low"))
	require.Contains(t, p.logs.String(), `"level":"ERROR"`, "the Loki alert counts error and critical")
	for _, field := range []string{`"path":"/irrelevant"`, `"free_bytes":1073741824`, `"total_bytes":107374182400`, `"min_free_bytes":3221225472`, `"min_free_ratio":0.1`} {
		require.Contains(t, p.logs.String(), field)
	}
	p.advance(61 * time.Second)
	requireInsufficient(t, g.Check(1))
	require.Equal(t, 2, strings.Count(p.logs.String(), "storage.disk_low"))
}

// The floor is judged twice: a write that would cross it while the volume is
// still above it is refused without the low-disk alert.
func TestRefusingABigWriteDoesNotLogLowDisk(t *testing.T) {
	p := newProbe(50*gib, 100*gib)
	requireInsufficient(t, p.defaults().Check(49*gib))
	require.Empty(t, p.logs.String())
}

func TestSampleLogsOnlyWhenLow(t *testing.T) {
	p := newProbe(50*gib, 100*gib)
	g := p.defaults()
	g.Sample()
	require.Empty(t, p.logs.String())
	p.space = &diskguard.Space{FreeBytes: gib, TotalBytes: 100 * gib}
	p.advance(6 * time.Second)
	g.Sample()
	require.Contains(t, p.logs.String(), "storage.disk_low")
}

// A failure is cached for the TTL too, and logged at most once per interval.
func TestFailureIsCachedAndLoggedOnce(t *testing.T) {
	p := newProbe(0, 0)
	p.space = nil
	g := p.defaults()
	for range 5 {
		require.NoError(t, g.Check(1))
	}
	require.Equal(t, 1, p.calls)
	require.Equal(t, 1, strings.Count(p.logs.String(), "storage.disk_check_failed"))
	require.Contains(t, p.logs.String(), `"level":"WARN"`)

	p.advance(6 * time.Second)
	require.NoError(t, g.Check(1))
	require.Equal(t, 2, p.calls)
	require.Equal(t, 1, strings.Count(p.logs.String(), "storage.disk_check_failed"))

	p.advance(60 * time.Second)
	require.NoError(t, g.Check(1))
	require.Equal(t, 2, strings.Count(p.logs.String(), "storage.disk_check_failed"))
}

func TestRecoversAfterFailure(t *testing.T) {
	p := newProbe(0, 0)
	p.space = nil
	g := p.defaults()
	require.NoError(t, g.Check(1))
	p.space = &diskguard.Space{FreeBytes: gib, TotalBytes: 100 * gib}
	p.advance(6 * time.Second)
	requireInsufficient(t, g.Check(1))
}

func TestDefaultsMatchSwift(t *testing.T) {
	require.Equal(t, int64(3221225472), diskguard.DefaultMinFreeBytes)
	require.InEpsilon(t, 0.10, diskguard.DefaultMinFreeRatio, 1e-12)
}

func TestNewUsesRealProviderByDefault(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, diskguard.New(dir, 0, 0, diskguard.Options{}).Check(0))
	requireInsufficient(t, diskguard.New(dir, 1<<62, 0, diskguard.Options{}).Check(1))
}
