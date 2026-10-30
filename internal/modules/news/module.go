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
	svc *Service
}

// New returns nil when cfg has no usable LV_SECRET_MASTER_KEY, matching
// Swift, which then never registers /v1/news.
func New(pool *pgxpool.Pool, cfg config.Config) *Module {
	box, err := secretbox.New(cfg.LVSecretMasterKey)
	if err != nil {
		return nil
	}
	return NewWithService(NewService(cfg.FeedsBaseURL, CuratedFeeds(cfg.FeedsDefaultFeeds), nil, PGInstalls(pool, box)))
}

// NewWithService wires a prepared Service (tests use a stub aggregator).
func NewWithService(svc *Service) *Module { return &Module{svc: svc} }

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/news/ticker", m.ticker, authMW)
}

func (m *Module) ticker(c *echo.Context) error {
	limit, err := strconv.Atoi(c.QueryParam("limit"))
	if err != nil {
		limit = defaultLimit // Swift: Int($0) ?? 30
	}
	resp, err := m.svc.Ticker(c.Request().Context(), auth.UserID(c), limit)
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
