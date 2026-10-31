// Package news serves /v1/news: the Go port of LuminaVaultServer
// Sources/App/News (NewsTickerController.swift, NewsTickerService.swift),
// the Home strip of the first-party `news-ticker` plugin (wave 2).
//
// Swift mounts /v1/news only inside the plugins block, which exists only
// when LV_SECRET_MASTER_KEY opens a SecretBox; without it the route is an
// unknown 404. New returns nil in that case and main registers nothing.
package news

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/news/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/httpserver"
	"lumina/internal/modules/news/db"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/wire"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const defaultLimit = 30

type Module struct {
	svc      *Service
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
}

// New returns nil when cfg has no usable LV_SECRET_MASTER_KEY, matching
// Swift, which then never registers /v1/news. identify is auth.Identify and
// limits the process-wide store: the group's settingsByUser bucket is shared
// with /v1/plugins (and kanban, spaces) in Swift.
func New(pool *pgxpool.Pool, cfg config.Config, identify echo.MiddlewareFunc, limits *ratelimit.Store) *Module {
	box, err := secretbox.New(cfg.LVSecretMasterKey)
	if err != nil {
		return nil
	}
	svc := NewService(cfg.FeedsBaseURL, CuratedFeeds(cfg.FeedsDefaultFeeds), nil, PGInstalls(pool, box))
	return &Module{svc: svc, identify: identify, limits: limits}
}

// Register mounts the route behind Swift's chain for the group:
// jwtAuthenticator (identify, never rejects), then
// RateLimitMiddleware(.settingsByUser), then requireTenantID() in the handler.
// So an unauthenticated request still counts against the "ip:" bucket and
// can get a 429 before its 401.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.GET("/v1/news/ticker", m.ticker, m.identify, ratelimit.Middleware(m.limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey))
}

func (m *Module) ticker(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	limit, err := strconv.Atoi(c.QueryParam("limit"))
	if err != nil {
		limit = defaultLimit // Swift: Int($0) ?? 30
	}
	resp, err := m.svc.Ticker(c.Request().Context(), tenant, limit)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, resp)
}

type pgInstalls struct {
	q   *db.Queries
	box *secretbox.Box
}

// PGInstalls reads plugin_installs as PluginService.installState does.
func PGInstalls(pool *pgxpool.Pool, box *secretbox.Box) Installs {
	return pgInstalls{q: db.New(pool), box: box}
}

func (p pgInstalls) NewsTickerInstall(ctx context.Context, tenantID uuid.UUID) (*InstallState, error) {
	row, err := p.q.NewsTickerInstall(ctx, tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // nil state = not installed, as in Swift
	}
	if err != nil {
		return nil, err
	}
	return &InstallState{Enabled: row.Status == "enabled", Config: p.openConfig(row, tenantID)}, nil
}

// openConfig: a value that does not open or is not a JSON object of strings
// reads as an empty config, never an error (Swift's `try?`).
func (p pgInstalls) openConfig(row db.NewsTickerInstallRow, tenantID uuid.UUID) map[string]string {
	plain, err := p.box.Open(secretbox.Sealed{Ciphertext: row.ConfigCiphertext, Nonce: row.ConfigNonce}, tenantID)
	if err != nil {
		return map[string]string{}
	}
	var cfg map[string]string
	if json.Unmarshal([]byte(plain), &cfg) != nil || cfg == nil {
		return map[string]string{}
	}
	return cfg
}
