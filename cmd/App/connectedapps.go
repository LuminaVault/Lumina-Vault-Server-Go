package main

import (
	"log/slog"
	"lumina/internal/config"
	"lumina/internal/connectedapps"
	"lumina/internal/httpserver"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	connectedappsmod "lumina/internal/modules/connectedapps"
)

// connectedAppsCallbackURL is where an app's authorization server sends the
// browser back: the public base URL /v1/mcp also uses. Empty when the server
// has none, and OAuth connects then answer 503.
func connectedAppsCallbackURL(cfg config.Config) string {
	base := cfg.MCPPublicBaseURL
	if base == "" {
		base = cfg.IngestionPublicBaseURL
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	return base + "/v1/connected-apps/oauth/callback"
}

// connectedAppsModule builds connected apps (Lumina as an MCP client of the
// user's other first-party apps): the settings API and the Service that chat
// and /v1/mcp will list and call tools through. Both are nil unless
// CONNECTED_APPS_ENABLED is "true", and nil without LV_SECRET_MASTER_KEY,
// because tokens are only ever stored sealed.
//
// Outbound requests go to registry URLs only, over HTTPS, checked at the URL
// (Validate) and again at dial time (the guarded client).
func connectedAppsModule(pool *pgxpool.Pool, cfg config.Config) (httpserver.Module, *connectedapps.Service) {
	if !strings.EqualFold(cfg.ConnectedAppsEnabled, "true") {
		return nil, nil
	}
	log := slog.Default().With("label", "lv.connected-apps")
	box, err := secretbox.New(cfg.LVSecretMasterKey)
	if err != nil {
		log.Warn("connected apps are enabled but LV_SECRET_MASTER_KEY is unusable; not mounting them")
		return nil, nil
	}
	guard := ssrfguard.Guard{RequireHTTPS: true}
	httpc := guard.Client(connectedapps.DefaultTimeout)
	registry := connectedapps.NewRegistry(cfg)
	store := connectedapps.NewStore(pool, box)
	callbackURL := connectedAppsCallbackURL(cfg)
	oauth := connectedapps.NewOAuth(pool, registry, store, httpc, guard, callbackURL)
	service := connectedapps.NewService(registry, store, oauth, &connectedapps.Client{HTTP: httpc, Guard: guard}, log)
	return connectedappsmod.New(connectedappsmod.Options{
		Registry: registry, Store: store, OAuth: oauth, Service: service,
		OAuthReady: callbackURL != "", WebReturnOrigins: cfg.CORSAllowedOrigins, Log: log,
	}), service
}
