package main

import (
	"lumina/internal/achievementsink"
	"lumina/internal/config"
	"lumina/internal/httpserver"
	soulmodule "lumina/internal/modules/soul"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/selfimprove"
	"lumina/internal/soul"
	"lumina/internal/vaultpaths"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// soulModule builds /v1/soul (8D). Swift's SoulController gets the capabilities
// service only when it built a resolver (LV_SECRET_MASTER_KEY set); without one,
// every tenant is managed.
func soulModule(pool *pgxpool.Pool, cfg config.Config, box *secretbox.Box, identify echo.MiddlewareFunc, limits *ratelimit.Store,
	paths vaultpaths.Paths, sink *achievementsink.Sink,
) httpserver.Module {
	opts := soulmodule.Options{
		Identify: identify, Limits: limits, Soul: soul.New(paths, cfg.HermesDataRoot), Achievements: sink,
	}
	if resolver := hermesResolver(pool, cfg, box); resolver != nil {
		opts.IsUserOverride = selfimprove.ResolverOverride(resolver)
	}
	return soulmodule.New(pool, opts)
}
