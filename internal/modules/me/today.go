package me

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/modules/me/db"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// todayTTL is MeTodayCache(ttl: 300) and MeTodayController.clientMaxAgeSeconds.
const todayTTL = 300 * time.Second

// todayResponse and its parts are the server-local DTOs of
// Sources/App/Me/MeTodayDTOs.swift (snake_case CodingKeys; nil omitted). The
// spec's MeTodayResponse describes something else entirely.
type todayResponse struct {
	TodayNudge                *string             `json:"today_nudge,omitempty"`
	LastMemory                *todayLastMemory    `json:"last_memory,omitempty"`
	OpenSpacesCount           int                 `json:"open_spaces_count"`
	UnlockedAchievementsToday []todayUnlocked     `json:"unlocked_achievements_today"`
	HealthSummary             *todayHealthSummary `json:"health_summary,omitempty"`
	Tier                      string              `json:"tier"`
	TrialDaysRemaining        *int                `json:"trial_days_remaining,omitempty"`
	GeneratedAt               wire.Time           `json:"generated_at"`
}

type todayLastMemory struct {
	ID      wire.UUID `json:"id"`
	Title   string    `json:"title"`
	SavedAt wire.Time `json:"saved_at"`
}

type todayUnlocked struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	UnlockedAt wire.Time `json:"unlocked_at"`
}

type todayHealthSummary struct {
	StepsToday     *int    `json:"steps_today,omitempty"`
	SleepLastNight *string `json:"sleep_last_night,omitempty"`
}

// todayEntry is MeTodayCache.Entry: the encoded body, its ETag and the
// generatedAt the TTL is measured from.
type todayEntry struct {
	body        []byte
	etag        string
	generatedAt time.Time
}

// todayCache is MeTodayCache without the EventBus listener (see package doc).
type todayCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[uuid.UUID]todayEntry
}

func newTodayCache(ttl time.Duration) *todayCache {
	return &todayCache{ttl: ttl, entries: map[uuid.UUID]todayEntry{}}
}

func (tc *todayCache) get(tenant uuid.UUID, now time.Time) (todayEntry, bool) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	e, ok := tc.entries[tenant]
	if !ok {
		return todayEntry{}, false
	}
	if now.Sub(e.generatedAt) >= tc.ttl {
		delete(tc.entries, tenant)
		return todayEntry{}, false
	}
	return e, true
}

func (tc *todayCache) put(tenant uuid.UUID, e todayEntry) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.entries[tenant] = e
}

// Invalidate drops a tenant's entry (MeTodayCache.invalidate). Nothing in Go
// publishes memory_upserted or achievement_unlocked yet.
func (m *Module) InvalidateToday(tenant uuid.UUID) {
	m.today.mu.Lock()
	defer m.today.mu.Unlock()
	delete(m.today.entries, tenant)
}

func (m *Module) getToday(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	ifNoneMatch := c.Request().Header.Get("If-None-Match")
	now := m.now()
	if e, ok := m.today.get(u.id, now); ok {
		return respondToday(c, e, ifNoneMatch)
	}
	payload, err := m.buildToday(c, u, now)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	e := todayEntry{body: body, etag: todayETag(body), generatedAt: now}
	m.today.put(u.id, e)
	return respondToday(c, e, ifNoneMatch)
}

// respondToday is MeTodayController.respond: Cache-Control and a quoted ETag
// on both the 200 and the 304.
func respondToday(c *echo.Context, e todayEntry, ifNoneMatch string) error {
	h := c.Response().Header()
	h.Set("Content-Type", wire.ContentTypeJSON)
	h.Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(todayTTL.Seconds())))
	h.Set("ETag", `"`+e.etag+`"`)
	if ifNoneMatch != "" && strings.Trim(ifNoneMatch, `"`) == e.etag {
		c.Response().WriteHeader(http.StatusNotModified)
		return nil
	}
	c.Response().WriteHeader(http.StatusOK)
	_, err := c.Response().Write(e.body)
	return err
}

// todayETag is the first 16 hex characters of the body's SHA-256.
func todayETag(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:16]
}

func (m *Module) buildToday(c *echo.Context, u user, now time.Time) (todayResponse, error) {
	out := todayResponse{Tier: u.tier, GeneratedAt: wt(now), UnlockedAchievementsToday: []todayUnlocked{}}

	last, err := m.q.TodayLastMemory(ctx(c), u.id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return out, err
	case last.CreatedAt != nil:
		out.LastMemory = &todayLastMemory{ID: wire.UUID{UUID: last.ID}, Title: titlePreview(last.Content), SavedAt: wt(*last.CreatedAt)}
	}

	spaces, err := m.q.TodaySpaceCount(ctx(c), u.id)
	if err != nil {
		return out, err
	}
	out.OpenSpacesCount = int(spaces)

	// Calendar.current: the process timezone.
	dayStart := startOfDay(now, time.Local)
	unlocks, err := m.q.TodayRecentUnlocks(ctx(c), u.id)
	if err != nil {
		return out, err
	}
	for _, r := range unlocks {
		if r.UnlockedAt == nil || r.UnlockedAt.Before(dayStart) {
			continue
		}
		title := r.AchievementKey
		if s, ok := m.subs[r.AchievementKey]; ok {
			title = s.Label
		}
		out.UnlockedAchievementsToday = append(out.UnlockedAchievementsToday, todayUnlocked{ID: r.AchievementKey, Title: title, UnlockedAt: wt(*r.UnlockedAt)})
	}

	steps, err := m.q.TodayStepsTotal(ctx(c), db.TodayStepsTotalParams{TenantID: u.id, DayStart: dayStart})
	if err != nil {
		return out, err
	}
	sleep, err := m.q.TodaySleepTotal(ctx(c), db.TodaySleepTotalParams{TenantID: u.id, PrevDay: dayStart.Add(-24 * time.Hour), DayStart: dayStart})
	if err != nil {
		return out, err
	}
	// The steps statement always returns a row, so the summary is never nil.
	stepsToday := int(steps)
	summary := &todayHealthSummary{StepsToday: &stepsToday}
	if sleep > 0 {
		d := iso8601Duration(int(sleep))
		summary.SleepLastNight = &d
	}
	out.HealthSummary = summary
	return out, nil
}

// iso8601Duration is MeTodayService.iso8601Duration (minutes → PT#H#M).
func iso8601Duration(minutes int) string {
	hours, mins := minutes/60, minutes%60
	switch {
	case hours > 0 && mins > 0:
		return fmt.Sprintf("PT%dH%dM", hours, mins)
	case hours > 0:
		return fmt.Sprintf("PT%dH", hours)
	default:
		return fmt.Sprintf("PT%dM", mins)
	}
}
