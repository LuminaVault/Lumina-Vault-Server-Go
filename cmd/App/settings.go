package main

import (
	"lumina/internal/config"
	"lumina/internal/hermescaps"
	"lumina/internal/modules/settings"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// settingsModule is /v1/settings/hermes and /v1/me/hermes/capabilities (6A).
// Swift mounts both only when LV_SECRET_MASTER_KEY yields a secret box.
func settingsModule(pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box, identify echo.MiddlewareFunc, limits *ratelimit.Store) *settings.Module {
	if box == nil {
		return nil
	}
	guard := ssrfguard.Guard{
		AllowPrivateRanges: strings.EqualFold(cfg.BYOHermesAllowPrivate, "true"),
		RequireHTTPS:       cfg.BYOHermesRequireHTTPS(),
		AllowTailnetHTTP:   strings.EqualFold(cfg.BYOHermesAllowTailnet, "true"),
	}
	return settings.New(pool, settings.Options{
		Identify: identify, Limits: limits, Box: box, Guard: guard,
		Capabilities: hermescaps.New(pool, hermesResolver(pool, cfg, box), box, guard),
	})
}
