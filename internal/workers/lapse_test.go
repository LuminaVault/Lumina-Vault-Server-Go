package workers

import (
	"context"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Ports Tests/AppTests/Billing/LapseArchiverTests.swift (7), on temp dirs and a
// test database, plus the dry-run mode the plan adds before the job moves.

type lapseHarness struct {
	t     *testing.T
	pool  *pgxpool.Pool
	job   *LapseArchiver
	vault string
	cold  string
}

// One container for the file: each subtest truncates users first.
func TestLapseArchiver(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	harness := func(t *testing.T, dryRun bool) lapseHarness {
		t.Helper()
		_, err := pool.Exec(ctx, `DELETE FROM users`)
		require.NoError(t, err)
		base := t.TempDir()
		h := lapseHarness{t: t, pool: pool, vault: filepath.Join(base, "vault"), cold: filepath.Join(base, "cold")}
		h.job = &LapseArchiver{Pool: pool, Paths: vaultpaths.Paths{Root: h.vault}, ColdStorageRoot: h.cold, DryRun: dryRun}
		return h
	}

	t.Run("expired trial becomes free", func(t *testing.T) {
		h := harness(t, false)
		now := time.Now()
		id := h.user("trial", "none", now.Add(-time.Minute))
		s := h.run(now)
		require.Equal(t, 1, s.MovedToFree)
		require.Equal(t, 0, s.Lapsed)
		require.Equal(t, "free", h.tier(id))
	})

	t.Run("expired paid subscription becomes lapsed", func(t *testing.T) {
		for _, tier := range []string{"pro", "ultimate"} {
			h := harness(t, false)
			now := time.Now()
			id := h.user(tier, "none", now.Add(-time.Minute))
			s := h.run(now)
			require.Equal(t, 1, s.Lapsed)
			require.Equal(t, 0, s.MovedToFree)
			require.Equal(t, "lapsed", h.tier(id))
		}
	})

	t.Run("a free account is never archived or deleted", func(t *testing.T) {
		h := harness(t, false)
		now := time.Now()
		id := h.user("free", "none", now.Add(-400*24*time.Hour))
		s := h.run(now)
		require.Equal(t, 0, s.Archived)
		require.Equal(t, 0, s.HardDeleted)
		require.Equal(t, "free", h.tier(id))
	})

	t.Run("lapsed ninety days archives and moves vault", func(t *testing.T) {
		h := harness(t, false)
		now := time.Now()
		id := h.user("lapsed", "none", now.Add(-91*24*time.Hour))
		h.vaultFile(id)
		s := h.run(now)
		require.Equal(t, 1, s.Archived)
		require.Equal(t, "archived", h.tier(id))
		require.NoDirExists(t, filepath.Join(h.vault, "tenants", ids.Upper(id)))
		require.DirExists(t, filepath.Join(h.cold, ids.Upper(id)))
	})

	t.Run("archived after full retention hard deletes row and cold vault", func(t *testing.T) {
		h := harness(t, false)
		now := time.Now()
		id := h.user("archived", "none", now.Add(-366*24*time.Hour))
		cold := filepath.Join(h.cold, ids.Upper(id))
		require.NoError(t, os.MkdirAll(cold, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(cold, "note.md"), []byte("cold"), 0o600))
		s := h.run(now)
		require.Equal(t, 1, s.HardDeleted)
		require.False(t, h.exists(id))
		require.NoDirExists(t, cold)
	})

	t.Run("archive storage error does not advance tier", func(t *testing.T) {
		h := harness(t, false)
		now := time.Now()
		id := h.user("lapsed", "none", now.Add(-91*24*time.Hour))
		h.vaultFile(id)
		require.NoError(t, os.MkdirAll(filepath.Join(h.cold, ids.Upper(id)), 0o755))
		s := h.run(now)
		require.Equal(t, 0, s.Archived)
		require.Len(t, s.Failures, 1)
		require.Equal(t, "archive", s.Failures[0].Phase)
		require.Equal(t, "lapsed", h.tier(id))
	})

	t.Run("override users are skipped", func(t *testing.T) {
		h := harness(t, false)
		now := time.Now()
		id := h.user("lapsed", "ultimate", now.Add(-91*24*time.Hour))
		s := h.run(now)
		require.Equal(t, 0, s.Archived)
		require.Equal(t, "lapsed", h.tier(id))
	})

	// One run carries a long-expired subscriber through every phase, as Swift's
	// in-memory user list does; a dry run reports the same and changes nothing.
	t.Run("dry run reports the cascade and changes nothing", func(t *testing.T) {
		for _, dry := range []bool{true, false} {
			h := harness(t, dry)
			now := time.Now()
			id := h.user("pro", "none", now.Add(-400*24*time.Hour))
			h.vaultFile(id)
			s := h.run(now)
			require.Equal(t, LapseSummary{Lapsed: 1, Archived: 1, HardDeleted: 1}, s)
			if dry {
				require.Equal(t, "pro", h.tier(id))
				require.DirExists(t, filepath.Join(h.vault, "tenants", ids.Upper(id)))
				require.NoDirExists(t, h.cold)
			} else {
				require.False(t, h.exists(id))
				require.NoDirExists(t, filepath.Join(h.cold, ids.Upper(id)))
			}
		}
	})
}

func (h lapseHarness) user(tier, override string, expires time.Time) uuid.UUID {
	h.t.Helper()
	id := uuid.New()
	_, err := h.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, tier_expires_at, created_at, updated_at)
		VALUES ($1, $2, 'x', true, $3, $4, $5, $6, now(), now())`, id, id.String()+"@t.test", "u"+id.String()[:8], tier, override, expires)
	require.NoError(h.t, err)
	return id
}

func (h lapseHarness) vaultFile(id uuid.UUID) {
	h.t.Helper()
	dir := filepath.Join(h.vault, "tenants", ids.Upper(id), "raw")
	require.NoError(h.t, os.MkdirAll(dir, 0o755))
	require.NoError(h.t, os.WriteFile(filepath.Join(dir, "note.md"), []byte("hello"), 0o600))
}

func (h lapseHarness) run(now time.Time) LapseSummary {
	h.t.Helper()
	s, err := h.job.Run(context.Background(), now)
	require.NoError(h.t, err)
	return s
}

func (h lapseHarness) tier(id uuid.UUID) string {
	h.t.Helper()
	var tier string
	require.NoError(h.t, h.pool.QueryRow(context.Background(), `SELECT tier FROM users WHERE id = $1`, id).Scan(&tier))
	return tier
}

func (h lapseHarness) exists(id uuid.UUID) bool {
	h.t.Helper()
	var one int
	err := h.pool.QueryRow(context.Background(), `SELECT 1 FROM users WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	require.NoError(h.t, err)
	return true
}
