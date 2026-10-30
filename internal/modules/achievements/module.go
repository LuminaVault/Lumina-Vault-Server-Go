// Package achievements serves /v1/achievements: the Go port of
// LuminaVaultServer Sources/App/Achievements/AchievementsController.swift and
// the read half of AchievementsService.swift (wave 2). Progress rows are
// written by Swift's AchievementsWorker, which stays in Swift (jobs.md).
//
// No oapi-codegen models: the spec's AchievementsListResponse and
// AchievementsRecentResponse ({achievements:[AchievementDTO]}, {key,title,…})
// do not describe what Swift sends, so the DTOs below are hand-written from
// LuminaVaultShared 5.32.0 (the version Swift's Package.resolved pins).
package achievements

import (
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/modules/achievements/db"
	"lumina/internal/wire"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	recentDefaultLimit = 10
	recentMaxLimit     = 50
	serviceMaxLimit    = 100 // AchievementsService.recentUnlocks clamps again
)

// ListResponse is LuminaVaultShared AchievementsListResponse.
type ListResponse struct {
	CatalogVersion int            `json:"catalogVersion"`
	Archetypes     []ArchetypeDTO `json:"archetypes"`
}

type ArchetypeDTO struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Sub   []SubDTO `json:"sub"`
}

type SubDTO struct {
	Key        string     `json:"key"`
	Label      string     `json:"label"`
	Target     int64      `json:"target"`
	Progress   int64      `json:"progress"`
	UnlockedAt *wire.Time `json:"unlockedAt,omitempty"`
}

// RecentResponse is LuminaVaultShared AchievementsRecentResponse.
type RecentResponse struct {
	Unlocks []UnlockDTO `json:"unlocks"`
}

type UnlockDTO struct {
	Key        string    `json:"key"`
	Label      string    `json:"label"`
	UnlockedAt wire.Time `json:"unlockedAt"`
}

type Module struct {
	q       *db.Queries
	catalog Catalog
}

func New(pool *pgxpool.Pool) *Module {
	return &Module{q: db.New(pool), catalog: Current}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/achievements", m.list, authMW)
	r.GET("/v1/achievements/recent", m.recent, authMW)
}

func (m *Module) list(c *echo.Context) error {
	rows, err := m.q.TenantProgress(c.Request().Context(), auth.UserID(c))
	if err != nil {
		return err
	}
	progress := make(map[string]db.TenantProgressRow, len(rows))
	for _, r := range rows {
		progress[r.AchievementKey] = r
	}
	out := ListResponse{CatalogVersion: m.catalog.CatalogVersion, Archetypes: make([]ArchetypeDTO, 0, len(m.catalog.Archetypes))}
	for _, a := range m.catalog.Archetypes {
		dto := ArchetypeDTO{Key: a.Key, Label: a.Label, Sub: make([]SubDTO, 0, len(a.Subs))}
		for _, s := range a.Subs {
			sub := SubDTO{Key: s.Key, Label: s.Label, Target: s.Target}
			if row, ok := progress[s.Key]; ok {
				sub.Progress = row.ProgressCount
				if row.UnlockedAt != nil {
					sub.UnlockedAt = &wire.Time{Time: *row.UnlockedAt}
				}
			}
			dto.Sub = append(dto.Sub, sub)
		}
		out.Archetypes = append(out.Archetypes, dto)
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) recent(c *echo.Context) error {
	limit := min(parseLimit(c.QueryParam("limit")), serviceMaxLimit)
	rows, err := m.q.RecentUnlocks(c.Request().Context(), db.RecentUnlocksParams{
		TenantID: auth.UserID(c),
		Limit:    int32(limit), //nolint:gosec // clamped to 1..50
	})
	if err != nil {
		return err
	}
	byKey := m.catalog.SubsByKey()
	out := RecentResponse{Unlocks: make([]UnlockDTO, 0, len(rows))}
	for _, r := range rows {
		if r.UnlockedAt == nil {
			continue
		}
		label := r.AchievementKey // Swift: unknown (retired) keys fall back to the key
		if s, ok := byKey[r.AchievementKey]; ok {
			label = s.Label
		}
		out.Unlocks = append(out.Unlocks, UnlockDTO{Key: r.AchievementKey, Label: label, UnlockedAt: wire.Time{Time: *r.UnlockedAt}})
	}
	return wire.JSON(c, http.StatusOK, out)
}

// parseLimit mirrors AchievementsController.parseLimit: missing or not an
// Int → 10, otherwise clamped to 1..50.
func parseLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return recentDefaultLimit
	}
	return max(1, min(n, recentMaxLimit))
}
