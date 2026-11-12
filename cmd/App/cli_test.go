package main

import (
	"bytes"
	"context"
	"encoding/json"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// cliDB is a fresh database and the POSTGRES_* config pointing at it, as the
// Swift CLI suites build their reader from TestPostgres.
func cliDB(t *testing.T) (*pgxpool.Pool, config.Config) {
	t.Helper()
	pool, dsn := dbtest.PostgresDSN(t)
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	password, _ := u.User.Password()
	cfg, err := config.LoadFrom(map[string]string{})
	require.NoError(t, err)
	cfg.PostgresHost, cfg.PostgresPort = u.Hostname(), port
	cfg.PostgresUser, cfg.PostgresPassword = u.User.Username(), password
	cfg.PostgresDatabase = strings.TrimPrefix(u.Path, "/")
	return pool, cfg
}

// MigrateCommandTests: "migrate runs without throwing", "migrate is
// idempotent" and "is_admin column exists after migrate", against the
// `App migrate` subcommand.
func TestMigrateCommand(t *testing.T) {
	ctx := context.Background()
	pool, cfg := cliDB(t)
	cmd, err := parseArgs([]string{"migrate"})
	require.NoError(t, err)

	require.NoError(t, migrate(ctx, cfg, cmd), "migrate runs without error")
	require.NoError(t, migrate(ctx, cfg, cmd), "a second run is a no-op")

	var column string
	require.NoError(t, pool.QueryRow(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_name = 'users' AND column_name = 'is_admin'`).Scan(&column))
	require.Equal(t, "is_admin", column)
}

type adminRow struct {
	username, tier      string
	isAdmin, isVerified bool
}

func fetchAdmin(t *testing.T, pool *pgxpool.Pool, email string) *adminRow {
	t.Helper()
	var r adminRow
	err := pool.QueryRow(context.Background(), `SELECT username, tier, is_admin, is_verified FROM users WHERE email = $1`,
		strings.ToLower(email)).Scan(&r.username, &r.tier, &r.isAdmin, &r.isVerified)
	require.NoError(t, err, "admin user row should exist after bootstrap-admin")
	return &r
}

func bootstrapCfg(t *testing.T, cfg config.Config, email, username string) config.Config {
	t.Helper()
	cfg.BootstrapAdminEmail = email
	cfg.BootstrapAdminPassword = "bootstrap-pass-" + username
	cfg.BootstrapAdminUsername = username
	cfg.JWTHMACSecret = "bootstrap-admin-test-secret-32-chars"
	cfg.JWTKID = "test-kid"
	cfg.HermesGatewayKind = "logging"
	cfg.HermesDataRoot = t.TempDir()
	cfg.VaultRootPath = t.TempDir()
	return cfg
}

// BootstrapAdminCommandTests: the bootstrap-admin subcommand, on one migrated
// database with a random email per case as Swift's shared schema has.
func TestBootstrapAdminCommand(t *testing.T) {
	ctx := context.Background()
	pool, base := cliDB(t)
	require.NoError(t, migrate(ctx, base, Command{Name: "migrate", Flags: map[string]string{}}))
	slug := func() string { return strings.ToLower(uuid.NewString()[:8]) }

	// "bootstrap-admin creates a verified admin user"
	t.Run("creates a verified admin user", func(t *testing.T) {
		s := slug()
		email, username := "Admin-"+s+"@test.luminavault", "admin"+s
		var out bytes.Buffer
		require.NoError(t, bootstrapAdmin(ctx, bootstrapCfg(t, base, email, username), &out))

		row := fetchAdmin(t, pool, email)
		require.True(t, row.isAdmin)
		require.True(t, row.isVerified)
		require.Equal(t, username, row.username)
		require.Equal(t, "trial", row.tier, "trial defaults should still apply")

		var summary map[string]any
		require.NoError(t, json.Unmarshal(out.Bytes(), &summary))
		require.Equal(t, strings.ToLower(email), summary["email"])
		require.Equal(t, true, summary["created"])
		require.Equal(t, true, summary["isAdmin"])
		require.Equal(t, true, summary["isVerified"])
		require.Equal(t, username, summary["username"])
		require.Equal(t, strings.ToUpper(summary["userID"].(string)), summary["userID"], "Swift's uuidString is uppercase")
		require.Contains(t, out.String(), `"created" : true`, "Foundation's pretty-printed separator")
	})

	// "bootstrap-admin is idempotent"
	t.Run("is idempotent", func(t *testing.T) {
		s := slug()
		cfg := bootstrapCfg(t, base, "idem-"+s+"@test.luminavault", "idem"+s)
		require.NoError(t, bootstrapAdmin(ctx, cfg, &bytes.Buffer{}))
		var out bytes.Buffer
		require.NoError(t, bootstrapAdmin(ctx, cfg, &out))

		row := fetchAdmin(t, pool, cfg.BootstrapAdminEmail)
		require.True(t, row.isAdmin)
		require.True(t, row.isVerified)
		require.Contains(t, out.String(), `"created" : false`, "the second run promotes the existing row")
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = $1`, cfg.BootstrapAdminEmail).Scan(&n))
		require.Equal(t, 1, n)
	})

	// "bootstrap-admin throws when credentials are missing"
	t.Run("throws when credentials are missing", func(t *testing.T) {
		err := bootstrapAdmin(ctx, base, &bytes.Buffer{})
		require.ErrorIs(t, err, errBootstrapMissingCredentials)
		require.Contains(t, err.Error(), "BOOTSTRAP_ADMIN_EMAIL")
	})
}
