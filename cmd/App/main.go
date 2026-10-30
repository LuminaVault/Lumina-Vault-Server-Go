// Command App is the LuminaVault API server, Go edition. It honours the Swift
// image contract so the infra chart runs it unchanged.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/db"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/news"
	"lumina/internal/modules/kanban"
	"lumina/internal/modules/projects"
	"lumina/internal/ratelimit"
	"lumina/internal/modules/spaces"
	"lumina/internal/modules/profiles"
	"lumina/internal/modules/projects"
	"lumina/internal/modules/sessions"
	"lumina/internal/telemetry"
	"lumina/internal/vaultpaths"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("exit", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd, err := parseArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(flushCtx); err != nil {
			slog.Warn("telemetry shutdown", "error", err)
		}
	}()

	switch cmd.Name {
	case "serve":
		return serve(ctx, cfg, cmd)
	case "migrate":
		return migrate(ctx, cfg, cmd)
	default:
		return fmt.Errorf("%s: not implemented", cmd.Name)
	}
}

// migrate runs goose. Until Phase 12 the chart's migrate job still runs the
// Swift image, so on a Fluent database this refuses unless --adopt-fluent.
func migrate(ctx context.Context, cfg config.Config, cmd Command) error {
	pool, err := db.Open(ctx, db.DSN(cfg))
	if err != nil {
		return err
	}
	defer pool.Close()
	if cmd.Flags["adopt-fluent"] == "true" {
		manifest, err := migrations.FluentManifest()
		if err != nil {
			return err
		}
		return migrations.AdoptFluent(ctx, pool, migrations.SQL(), manifest)
	}
	return migrations.Up(ctx, pool, migrations.SQL())
}

func serve(ctx context.Context, cfg config.Config, cmd Command) error {
	deps := httpserver.Deps{}
	if cfg.FluentEnabled {
		pool, err := db.Open(ctx, db.DSN(cfg))
		if err != nil {
			return err
		}
		defer pool.Close()
		keys, err := auth.LoadKeys(cfg)
		if err != nil {
			return err
		}
		deps.Auth = auth.Middleware(keys, auth.PGUsers(pool))
		// One rate-limit store per process: Swift's buckets are shared by
		// every route group that uses the same policy name.
		limits := ratelimit.NewStore()
		// Migrated modules (modules.yaml status ≥ parity). Traffic only reaches
		// them through the api ingress.extraPaths that name them.
		deps.Modules = []httpserver.Module{
			projects.New(pool),
			achievements.New(pool),
		}
		if m := news.New(pool, cfg, auth.Identify(keys, auth.PGUsers(pool)), limits); m != nil { // nil without LV_SECRET_MASTER_KEY, as in Swift
			deps.Modules = append(deps.Modules, m)
			kanban.New(pool, auth.Identify(keys, auth.PGUsers(pool)), limits, vaultpaths.Paths{Root: cfg.VaultRootPath}),
			spaces.New(pool, vaultpaths.Paths{Root: cfg.VaultRootPath}, auth.Identify(keys, auth.PGUsers(pool))),
			profiles.New(pool),
			sessions.New(pool),
		}
	}
	e := httpserver.New(cfg, deps)
	sc := echo.StartConfig{
		Address:         net.JoinHostPort(cmd.HTTPHost, strconv.Itoa(cmd.HTTPPort)),
		HideBanner:      true,
		HidePort:        true,
		GracefulTimeout: 10 * time.Second,
	}
	slog.Info("listening", "address", sc.Address)
	if err := sc.Start(ctx, httpserver.Handler(cfg, e)); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
