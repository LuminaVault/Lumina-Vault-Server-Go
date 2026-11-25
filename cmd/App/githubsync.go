package main

import (
	"log/slog"
	"lumina/internal/config"
	"lumina/internal/githubapp"
	"lumina/internal/githubsync"
	"lumina/internal/httpserver"
	"lumina/internal/ratelimit"
	"lumina/internal/ssrfguard"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultpaths"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	githubsyncmod "lumina/internal/modules/githubsync"
)

// githubSyncModule builds GitHub sync's routes (spec 2026-11-19-e4-github-sync,
// D5). Nothing is built or mounted unless GITHUB_SYNC_ENABLED is true; with
// the switch on but VAULT_GIT_ENABLED off or a GitHub key missing or bad, one
// warning is logged and the routes stay unmounted (404), so a half-configured
// server never offers a flow that cannot finish.
//
// Requests to GitHub go through the SSRF-guarded client, HTTPS only outside
// development, and never follow a redirect.
func githubSyncModule(pool *pgxpool.Pool, cfg config.Config, paths vaultpaths.Paths, limits *ratelimit.Store) httpserver.Module {
	if !githubsync.Enabled(cfg) {
		return nil
	}
	log := slog.Default().With("label", "lv.github-sync")
	settings, err := githubsync.SettingsFrom(cfg)
	if err != nil {
		log.Warn("GitHub sync is enabled but not configured; not mounting it", "reason", err.Error())
		return nil
	}
	guard := ssrfguard.Guard{RequireHTTPS: !githubsync.IsDev(cfg)}
	client, err := githubapp.New(githubapp.Config{
		AppID: settings.AppID, ClientID: settings.ClientID, ClientSecret: settings.ClientSecret, PrivateKey: settings.PrivateKey,
		APIBaseURL: settings.APIBaseURL, WebBaseURL: settings.GitBaseURL, HTTP: guard.Client(30 * time.Second),
	})
	if err != nil {
		log.Warn("GitHub sync is enabled but its client cannot be built; not mounting it", "reason", err.Error())
		return nil
	}
	return githubsyncmod.New(githubsyncmod.Options{
		Client: client, Store: githubsync.NewStore(pool), Access: vaultaccess.New(pool), Paths: paths,
		CallbackURL: settings.CallbackURL, Slug: settings.Slug,
		MaxRepoBytes: settings.MaxRepoBytes, MaxFiles: settings.MaxFiles,
		WebReturnOrigins: cfg.CORSAllowedOrigins, Limits: limits, Log: log,
	})
}
