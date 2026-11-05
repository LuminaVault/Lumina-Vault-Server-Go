package me

import (
	"lumina/internal/modules/me/db"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
)

const (
	learnedDefaultWindow = 7 * 24 * time.Hour
	learnedMaxWindow     = 90 * 24 * time.Hour
)

func (m *Module) getLearned(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	now := m.now()
	since, err := parseSince(wire.Query(c, "since"), now)
	if err != nil {
		return err
	}
	soul, err := m.q.LearnedSoulChanges(ctx(c), db.LearnedSoulChangesParams{TenantID: u.id, Since: since, Now: now})
	if err != nil {
		return err
	}
	mems, err := m.q.LearnedMemories(ctx(c), db.LearnedMemoriesParams{TenantID: u.id, Since: since, Now: now})
	if err != nil {
		return err
	}
	out := LearnedDigestResponse{
		Since: wt(since), Until: wt(now),
		SoulChanges: make([]LearnedSoulChangeDTO, 0, len(soul)),
		Memories:    make([]LearnedMemoryDTO, 0, len(mems)),
	}
	for _, s := range soul {
		out.SoulChanges = append(out.SoulChanges, LearnedSoulChangeDTO{Id: wire.UUID{UUID: s.ID}, Title: s.Title, Summary: s.Summary, CreatedAt: wt(s.CreatedAt)})
	}
	for _, r := range mems {
		kind := ""
		if r.Kind != nil {
			kind = *r.Kind
		}
		out.Memories = append(out.Memories, LearnedMemoryDTO{
			Id: wire.UUID{UUID: r.ID}, Snippet: relatedSnippet(r.Content), Kind: kind,
			ReviewState: r.ReviewState, CreatedAt: wt(orZero(r.CreatedAt)),
		})
	}
	return wire.JSON(c, http.StatusOK, out)
}

// parseSince is MeLearnedController.parseSince.
func parseSince(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		return now.Add(-learnedDefaultWindow), nil
	}
	since, ok := parseISO8601(raw)
	if !ok {
		return time.Time{}, badRequest("since must be an ISO 8601 date-time")
	}
	if since.After(now) {
		return time.Time{}, badRequest("since must not be in the future")
	}
	if since.Before(now.Add(-learnedMaxWindow)) {
		return time.Time{}, badRequest("since must be within the last 90 days")
	}
	return since, nil
}

// parseISO8601 is ISO8601DateFormatter with and without fractional seconds.
// An unencoded "+01:00" offset arrives form-decoded as a space.
func parseISO8601(input string) (time.Time, bool) {
	raw := strings.ReplaceAll(input, " ", "+")
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	// ISO8601DateFormatter keeps milliseconds.
	return t.Truncate(time.Millisecond), true
}
