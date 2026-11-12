package main

import (
	"lumina/internal/authservice"
	"lumina/internal/config"
	"lumina/internal/httpserver"
	"lumina/internal/modules/webauthnroutes"
	"lumina/internal/passkey"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// webAuthnModule is wave 8C: WebAuthnService as App+build.swift builds it, on the
// webauthn.* keys, sharing 8A's AuthService for issueTokens.
func webAuthnModule(pool *pgxpool.Pool, cfg config.Config, service *authservice.Service, limits *ratelimit.Store, identify echo.MiddlewareFunc) httpserver.Module {
	return webauthnroutes.New(pool, webauthnroutes.Options{
		Enabled: swiftstr.Lowercased(cfg.WebAuthnEnabled) == "true",
		Config: passkey.Config{
			RPID:    cfg.WebAuthnRelyingPartyID,
			RPName:  cfg.WebAuthnRelyingPartyName,
			Origins: passkey.ParseOrigins(cfg.WebAuthnRelyingPartyOrigin),
		},
		Service:  service,
		Limits:   limits,
		Identify: identify,
	})
}
