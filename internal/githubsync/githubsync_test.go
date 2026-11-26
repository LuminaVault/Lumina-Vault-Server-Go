package githubsync_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io/fs"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/githubsync"
	"lumina/internal/migrations"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), p, migrations.SQL()))
	return p
}

func user(t *testing.T, p *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	_, err := p.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_verified, username, tier, created_at, updated_at)
		VALUES ($1, $2, 'x', true, $3, 'trial', now(), now())`, id, id.String()+"@test", "u"+strings.ReplaceAll(id.String(), "-", "")[:16])
	require.NoError(t, err)
	_, err = p.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(t, err)
	return id
}

func conn(vault uuid.UUID, repo int64, by *uuid.UUID) githubsync.Connection {
	return githubsync.Connection{
		VaultID: vault, InstallationID: 7, AccountLogin: "octocat", AccountType: "User", RepoID: repo,
		RepoFullName: "octocat/notes", RepoPrivate: true, Branch: "main", State: githubsync.StateActive,
		ConnectedByUserID: by, ConnectedGitHubLogin: "octocat",
	}
}

func TestStore(t *testing.T) {
	p := pool(t)
	s := githubsync.NewStore(p)
	ctx := context.Background()
	alice, bob := user(t, p), user(t, p)

	got, err := s.Get(ctx, alice)
	require.NoError(t, err)
	require.Nil(t, got)

	require.NoError(t, s.Insert(ctx, conn(alice, 456, &alice)))
	require.ErrorIs(t, s.Insert(ctx, conn(alice, 457, &alice)), githubsync.ErrAlreadyConnected)
	require.ErrorIs(t, s.Insert(ctx, conn(bob, 456, &bob)), githubsync.ErrRepoInUse, "one vault per repository")

	got, err = s.Get(ctx, alice)
	require.NoError(t, err)
	require.Equal(t, int64(456), got.RepoID)
	require.Equal(t, githubsync.StateActive, got.State)
	require.Nil(t, got.RemoteSHA)

	inUse, err := s.ReposInUse(ctx, []int64{456, 999}, bob)
	require.NoError(t, err)
	require.Equal(t, map[int64]bool{456: true}, inUse)
	inUse, err = s.ReposInUse(ctx, []int64{456}, alice)
	require.NoError(t, err)
	require.Empty(t, inUse, "a vault's own repository is not in use by another")

	owner, err := s.VaultOwner(ctx, alice)
	require.NoError(t, err)
	require.Equal(t, alice, owner)

	deleted, err := s.Delete(ctx, alice)
	require.NoError(t, err)
	require.Equal(t, int64(456), deleted.RepoID)
	deleted, err = s.Delete(ctx, alice)
	require.NoError(t, err)
	require.Nil(t, deleted)
}

// D13: account deletion needs no eraser change. The personal vault cascades
// from users and the connection from the vault; the connecting user of a team
// vault is only set to NULL.
func TestConnectionCascades(t *testing.T) {
	p := pool(t)
	s := githubsync.NewStore(p)
	ctx := context.Background()
	alice, bob := user(t, p), user(t, p)
	team := uuid.New()
	_, err := p.Exec(ctx, `INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at) VALUES ($1, 't', $2, $2, now(), now())`, team, alice)
	require.NoError(t, err)
	teamVault := uuid.New()
	_, err = p.Exec(ctx, `INSERT INTO vaults (id, team_id, name, created_at, updated_at) VALUES ($1, $2, 'team', now(), now())`, teamVault, team)
	require.NoError(t, err)

	require.NoError(t, s.Insert(ctx, conn(alice, 1, &alice)))
	require.NoError(t, s.Insert(ctx, conn(teamVault, 2, &bob)))
	owner, err := s.VaultOwner(ctx, teamVault)
	require.NoError(t, err)
	require.Equal(t, alice, owner, "a team vault's owner is the team's")

	_, err = p.Exec(ctx, `DELETE FROM users WHERE id = $1`, bob)
	require.NoError(t, err)
	c, err := s.Get(ctx, teamVault)
	require.NoError(t, err)
	require.NotNil(t, c, "the team vault's connection stays")
	require.Nil(t, c.ConnectedByUserID)

	_, err = p.Exec(ctx, `DELETE FROM vaults WHERE id = $1`, alice) // what the account eraser's cascade does
	require.NoError(t, err)
	c, err = s.Get(ctx, alice)
	require.NoError(t, err)
	require.Nil(t, c)
	_, err = p.Exec(ctx, `DELETE FROM vaults WHERE id = $1`, teamVault) // the team purge
	require.NoError(t, err)
	c, err = s.Get(ctx, teamVault)
	require.NoError(t, err)
	require.Nil(t, c)
}

// The tables are Go only. On a database without them (Swift-migrated parity)
// Get reads "not connected", and the migration goes down and up again.
func TestMigrationDownAndAbsentTable(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	// The migrations that add columns go down and up on their own first,
	// newest first: 00014 (attempts: confirm_count and reserved_at), then
	// 00013 (the merge consent, E4.8).
	for _, added := range []struct {
		file    string
		columns []string
	}{
		{"00014_github_sync_attempts.sql", []string{"confirm_count", "reserved_at"}},
		{"00013_github_sync_merge_existing.sql", []string{"merge_existing"}},
	} {
		sql, err := fs.ReadFile(migrations.SQL(), added.file)
		require.NoError(t, err)
		addUp, addDown, ok := strings.Cut(string(sql), "-- +goose Down")
		require.True(t, ok)
		has := func() int {
			var n int
			require.NoError(t, p.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
				WHERE table_name = 'github_sync_connections' AND column_name = ANY($1)`, added.columns).Scan(&n))
			return n
		}
		require.Equal(t, len(added.columns), has(), added.file)
		_, err = p.Exec(ctx, addDown)
		require.NoError(t, err)
		require.Zero(t, has(), added.file)
		_, err = p.Exec(ctx, strings.SplitN(addUp, "-- +goose Up", 2)[1])
		require.NoError(t, err)
		require.Equal(t, len(added.columns), has(), added.file)
	}

	raw, err := fs.ReadFile(migrations.SQL(), "00012_github_sync.sql")
	require.NoError(t, err)
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	require.True(t, ok)
	_, err = p.Exec(ctx, down)
	require.NoError(t, err)

	// The worker's reads and writes take the absent table the same way.
	store := githubsync.NewStore(p)
	due, err := store.Due(ctx, time.Now())
	require.NoError(t, err)
	require.Empty(t, due)
	key := githubsync.Key{Vault: uuid.New(), RepoID: 1, Branch: "main"}
	for name, write := range map[string]func() (bool, error){
		"MarkPushed": func() (bool, error) { return store.MarkPushed(ctx, key, "abc", time.Now(), true) },
		"MarkRetry": func() (bool, error) {
			return store.MarkRetry(ctx, key, "sync_failed", time.Now(), time.Now(), false, false)
		},
		"Reserve":            func() (bool, error) { return store.Reserve(ctx, key, time.Now(), time.Now(), true) },
		"Release":            func() (bool, error) { return store.Release(ctx, key, time.Now()) },
		"MarkNeedsAttention": func() (bool, error) { return store.MarkNeedsAttention(ctx, key, "push_rejected", time.Now()) },
		"SetRepoFullName":    func() (bool, error) { return store.SetRepoFullName(ctx, key, "a/b", time.Now()) },
		"Requeue":            func() (bool, error) { return store.Requeue(ctx, key.Vault, []string{"push_rejected"}, "x", time.Now()) },
	} {
		changed, err := write()
		require.NoError(t, err, name)
		require.False(t, changed, name)
	}

	c, err := githubsync.NewStore(p).Get(ctx, uuid.New())
	require.NoError(t, err)
	require.Nil(t, c)

	// Only the absent table reads as "not connected". A table that lost a
	// column (42703) is a broken schema, and says so.
	_, err = p.Exec(ctx, `CREATE TABLE github_sync_connections (vault_id uuid PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = githubsync.NewStore(p).Get(ctx, uuid.New())
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "42703", pgErr.Code)
	_, err = p.Exec(ctx, `DROP TABLE github_sync_connections`)
	require.NoError(t, err)

	_, err = p.Exec(ctx, strings.SplitN(up, "-- +goose Up", 2)[1])
	require.NoError(t, err)
	var n int
	require.NoError(t, p.QueryRow(ctx, `SELECT count(*) FROM github_sync_connections`).Scan(&n))
	require.NoError(t, p.QueryRow(ctx, `SELECT count(*) FROM github_webhook_deliveries`).Scan(&n))
}

func TestClassify(t *testing.T) {
	const limit = 5 << 20
	for _, c := range []struct {
		path, mode string
		size       int64
		want       githubsync.HeldReason
	}{
		{"inbox/plan.md", "100644", 10, githubsync.Materialized},
		{"tools/run.txt", "100755", 10, githubsync.Materialized},
		{"papers/a.pdf", "100644", 6 << 20, githubsync.HeldTooLarge},
		{".obsidian/app.json", "100644", 10, githubsync.HeldHidden},
		{".gitignore", "100644", 10, githubsync.IgnoreFile},
		{".gitignore", "100644", 6 << 20, githubsync.HeldTooLarge},
		{".gitignore", "120000", 10, githubsync.HeldNotAFile},
		{"notes/.gitignore", "100644", 10, githubsync.HeldHidden},
		{".gitattributes", "100644", 10, githubsync.HeldHidden},
		{"notes/.draft.md", "100644", 10, githubsync.HeldHidden},
		{"a/.git/config.md", "100644", 10, githubsync.HeldHidden},
		{"README", "100644", 10, githubsync.HeldUnsupported},
		{"diagram.canvas", "100644", 10, githubsync.HeldUnsupported},
		{"My Note.md", "100644", 10, githubsync.HeldBadName},
		{"link.md", "120000", 10, githubsync.HeldNotAFile},
		{"vendor/lib", "160000", 0, githubsync.HeldNotAFile},
	} {
		require.Equal(t, c.want, githubsync.Classify(c.path, c.mode, c.size, limit), c.path)
	}
}

func TestSummarize(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "inbox"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "inbox", "same.md"), []byte("same"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "inbox", "differs.md"), []byte("local"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "inbox", "samesize.md"), []byte("LOCAL!"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("mine\n"), 0o600))
	// Another size cannot be the same blob: counted without being read.
	require.NoError(t, os.WriteFile(filepath.Join(root, "inbox", "unreadable.md"), []byte("longer than remote"), 0o000))
	entries := []githubsync.RemoteEntry{
		{Path: ".gitignore", Mode: "100644", Type: "blob", SHA: githubsync.BlobID([]byte("theirs\n")), Size: 7},
		{Path: "inbox/samesize.md", Mode: "100644", Type: "blob", SHA: githubsync.BlobID([]byte("remote")), Size: 6},
		{Path: "inbox/unreadable.md", Mode: "100644", Type: "blob", SHA: githubsync.BlobID([]byte("r")), Size: 1},
		{Path: "inbox", Mode: "040000", Type: "tree"},
		{Path: "inbox/same.md", Mode: "100644", Type: "blob", SHA: githubsync.BlobID([]byte("same")), Size: 4},
		{Path: "inbox/differs.md", Mode: "100644", Type: "blob", SHA: githubsync.BlobID([]byte("remote")), Size: 6},
		{Path: "inbox/new.md", Mode: "100644", Type: "blob", SHA: githubsync.BlobID([]byte("n")), Size: 1},
		{Path: "img/a.png", Mode: "100644", Type: "blob", SHA: "x", Size: 100},
		{Path: ".obsidian/app.json", Mode: "100644", Type: "blob", SHA: "y", Size: 20},
		{Path: "My Note.md", Mode: "100644", Type: "blob", SHA: "z", Size: 3},
	}
	s, err := githubsync.Summarize(context.Background(), entries, root, 5<<20)
	require.NoError(t, err)
	// The root .gitignore is bytes only: not a file, not held, not a conflict.
	require.Equal(t, githubsync.Summary{Files: 6, Notes: 5, Conflicts: 3, Held: 2, Bytes: 148}, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = githubsync.Summarize(ctx, entries, root, 5<<20)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391", githubsync.BlobID(nil), "git's empty blob")
}

func TestSettingsFrom(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	good := config.Config{
		LVEnvironment: "production", GitHubSyncEnabled: "true", VaultGitEnabled: "true",
		GitHubAppID: "123", GitHubAppClientID: "Iv1.x", GitHubAppClientSecret: "s", GitHubAppPrivateKey: pemKey,
		GitHubAppWebhookSecret: "w", GitHubAppSlug: "luminavault",
		GitHubSyncCallbackURL: "https://api.luminavault.fyi/v1/github-sync/callback", GitHubSyncPollInterval: "15m",
		GitHubSyncMaxRepoMB: 200, GitHubSyncMaxFiles: 5000,
		GitHubAPIBaseURL: "https://api.github.com", GitHubGitBaseURL: "https://github.com",
	}
	require.True(t, githubsync.Enabled(good))
	s, err := githubsync.SettingsFrom(good)
	require.NoError(t, err)
	require.Equal(t, int64(123), s.AppID)
	require.Equal(t, int64(200<<20), s.MaxRepoBytes)
	require.Zero(t, s.PushTimeout, "unset: the syncer's default")
	slow := good
	slow.GitHubSyncPushTimeout = "5m"
	s, err = githubsync.SettingsFrom(slow)
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, s.PushTimeout)

	yes := good
	yes.VaultGitEnabled = "1"
	_, err = githubsync.SettingsFrom(yes)
	require.NoError(t, err, "VAULT_GIT_ENABLED is read as cmd/App reads it")

	staging := good
	staging.LVEnvironment = "staging"
	staging.GitHubSyncCallbackURL = "http://localhost:8080/v1/github-sync/callback"
	_, err = githubsync.SettingsFrom(staging)
	require.NoError(t, err, "staging's port-forward callback")

	for name, change := range map[string]func(*config.Config){
		"no vault git":     func(c *config.Config) { c.VaultGitEnabled = "false" },
		"no app id":        func(c *config.Config) { c.GitHubAppID = "" },
		"no secret":        func(c *config.Config) { c.GitHubAppClientSecret = "" },
		"no webhook":       func(c *config.Config) { c.GitHubAppWebhookSecret = "" },
		"bad key":          func(c *config.Config) { c.GitHubAppPrivateKey = "nope" },
		"http api":         func(c *config.Config) { c.GitHubAPIBaseURL = "http://api.github.com" },
		"short poll":       func(c *config.Config) { c.GitHubSyncPollInterval = "10s" },
		"bad push timeout": func(c *config.Config) { c.GitHubSyncPushTimeout = "soon" },
		"no push timeout":  func(c *config.Config) { c.GitHubSyncPushTimeout = "0s" },
		"no callback":      func(c *config.Config) { c.GitHubSyncCallbackURL = "" },
		"zero max files":   func(c *config.Config) { c.GitHubSyncMaxFiles = 0 },
		"http callback":    func(c *config.Config) { c.GitHubSyncCallbackURL = "http://api.luminavault.fyi/cb" },
		"relative git url": func(c *config.Config) { c.GitHubGitBaseURL = "github.com" },
	} {
		cfg := good
		change(&cfg)
		_, err := githubsync.SettingsFrom(cfg)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), "BEGIN", name)
	}
	require.False(t, githubsync.Enabled(config.Config{}))
}

// One rule decides "development" for the settings check and for the HTTP
// client's HTTPS requirement.
func TestIsDev(t *testing.T) {
	for env, want := range map[string]bool{
		"": true, "dev": true, "development": true, "test": true,
		"staging": false, "production": false, "prod": false,
	} {
		require.Equal(t, want, githubsync.IsDev(config.Config{LVEnvironment: env}), env)
	}
}
