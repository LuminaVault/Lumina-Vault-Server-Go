// Command App is the LuminaVault API server, Go edition. It honours the Swift
// image contract so the infra chart runs it unchanged.
package main

import (
	"context"
	"fmt"
	"log/slog"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd.Name {
	case "serve":
		return serve(ctx, cmd)
	default:
		return fmt.Errorf("%s: not implemented", cmd.Name)
	}
}

func serve(ctx context.Context, cmd Command) error {
	e := echo.New()
	e.GET("/health", func(c *echo.Context) error { return c.String(200, "ok") })
	sc := echo.StartConfig{
		Address:         net.JoinHostPort(cmd.HTTPHost, strconv.Itoa(cmd.HTTPPort)),
		HideBanner:      true,
		HidePort:        true,
		GracefulTimeout: 10 * time.Second,
	}
	slog.Info("listening", "address", sc.Address)
	return sc.Start(ctx, e)
}
