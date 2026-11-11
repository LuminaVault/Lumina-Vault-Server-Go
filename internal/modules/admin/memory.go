package admin

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/wire"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScoringConfig is MemoryScoringConfig.
type ScoringConfig struct {
	AccessWeight, QueryWeight, RecencyWeight, HalflifeDays float64
}

// DefaultScoringConfig is MemoryScoringConfig.default.
var DefaultScoringConfig = ScoringConfig{AccessWeight: 2.0, QueryWeight: 3.0, RecencyWeight: 1.0, HalflifeDays: 30}

// ComputeScore is MemoryScoring.compute: 2·ln(1+access) + 3·ln(1+hits) +
// exp(−ageDays/30). A nil createdAt is age 0, and a future one never goes negative.
func ComputeScore(accessCount, queryHitCount int64, createdAt *time.Time, now time.Time, cfg ScoringConfig) float64 {
	access := cfg.AccessWeight * math.Log1p(float64(accessCount))
	queries := cfg.QueryWeight * math.Log1p(float64(queryHitCount))
	ageDays := 0.0
	if createdAt != nil {
		ageDays = math.Max(0, now.Sub(*createdAt).Seconds()) / 86400
	}
	return access + queries + cfg.RecencyWeight*math.Exp(-ageDays/cfg.HalflifeDays)
}

// swiftDouble is String(Double): shortest round-trip digits, ".0" on an integral
// value, exponent form below 1e-4 or from 2^53.
func swiftDouble(f float64) string {
	if abs := math.Abs(f); abs != 0 && (abs < 1e-4 || abs >= 1<<53) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") && !math.IsInf(f, 0) && !math.IsNaN(f) {
		s += ".0"
	}
	return s
}

// Scoring is MemoryScoringService.
type Scoring struct {
	Pool   *pgxpool.Pool
	Config ScoringConfig
}

// RecomputeForTenant is recomputeForTenant: ComputeScore in SQL for every row of the
// tenant; Postgres has no log1p, so ln(1 + x). It returns the rows updated, 0 for
// a tenant that doesn't exist.
func (s Scoring) RecomputeForTenant(ctx context.Context, tenant uuid.UUID, now time.Time) (int, error) {
	// The weights are spelled into the SQL as Swift's String(Double) spells them.
	tag, err := s.Pool.Exec(ctx, fmt.Sprintf(`
		UPDATE memories SET score =
		    %s * ln(1 + access_count)
		  + %s * ln(1 + query_hit_count)
		  + %s * exp(
		        - GREATEST(0, EXTRACT(EPOCH FROM ($1::timestamptz - COALESCE(created_at, $1::timestamptz)))::float8)
		        / %s
		    )
		WHERE tenant_id = $2`,
		swiftDouble(s.Config.AccessWeight), swiftDouble(s.Config.QueryWeight),
		swiftDouble(s.Config.RecencyWeight), swiftDouble(s.Config.HalflifeDays*86400)), now, tenant)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// RecomputeAll is recomputeAll: every user, a failed tenant logged and skipped.
func (s Scoring) RecomputeAll(ctx context.Context, now time.Time) (int, error) {
	users, err := listUsers(ctx, s.Pool)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, u := range users {
		n, err := s.RecomputeForTenant(ctx, u.id, now)
		if err != nil {
			slog.WarnContext(ctx, "recompute failed", "tenant", u.id, "error", err)
			continue
		}
		total += n
	}
	slog.InfoContext(ctx, "memory scoring recompute", "tenants", len(users), "rowsUpdated", total)
	return total, nil
}

// PruningConfig is MemoryPruningConfig.
type PruningConfig struct {
	ScoreThreshold float64
	MinAgeMonths   int
}

// DefaultPruningConfig is MemoryPruningConfig.default.
var DefaultPruningConfig = PruningConfig{ScoreThreshold: 0.2, MinAgeMonths: 3}

// PruneResult is MemoryPruneResult.
type PruneResult struct {
	TenantID          wire.UUID `json:"tenantID"`
	CandidatesScanned int       `json:"candidatesScanned"`
	Archived          int       `json:"archived"`
}

// Pruning is MemoryPruningService.
type Pruning struct {
	Pool   *pgxpool.Pool
	Config PruningConfig
}

// PruneForTenant is pruneForTenant: rows scoring under the threshold and older than
// MinAgeMonths×30 days are copied to memories_archive, then deleted. The copy and
// the delete are separate statements, not one transaction, as in Swift.
func (p Pruning) PruneForTenant(ctx context.Context, tenant uuid.UUID, now time.Time) (PruneResult, error) {
	cutoff := now.Add(-time.Duration(p.Config.MinAgeMonths) * 30 * 24 * time.Hour)
	result := PruneResult{TenantID: wire.UUID{UUID: tenant}}
	const where = `tenant_id = $1 AND score < $2 AND COALESCE(created_at, NOW()) < $3`
	var n int
	if err := p.Pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM memories WHERE `+where,
		tenant, p.Config.ScoreThreshold, cutoff).Scan(&n); err != nil {
		return PruneResult{}, err
	}
	if n == 0 {
		return result, nil
	}
	if _, err := p.Pool.Exec(ctx, `
		INSERT INTO memories_archive (
		    id, tenant_id, content, tags, embedding,
		    score, access_count, query_hit_count, last_accessed_at,
		    created_at, archived_at
		)
		SELECT id, tenant_id, content, tags, embedding,
		       score, access_count, query_hit_count, last_accessed_at,
		       created_at, NOW()
		FROM memories
		WHERE `+where+`
		ON CONFLICT (id) DO NOTHING`, tenant, p.Config.ScoreThreshold, cutoff); err != nil {
		return PruneResult{}, err
	}
	tag, err := p.Pool.Exec(ctx, `DELETE FROM memories WHERE `+where, tenant, p.Config.ScoreThreshold, cutoff)
	if err != nil {
		return PruneResult{}, err
	}
	result.CandidatesScanned, result.Archived = n, int(tag.RowsAffected())
	return result, nil
}

// PruningSweep is MemoryPruningSweepSummary.
type PruningSweep struct {
	UsersScanned     int           `json:"usersScanned"`
	ScoreRowsUpdated int           `json:"scoreRowsUpdated"`
	MemoriesArchived int           `json:"memoriesArchived"`
	PerTenant        []PruneResult `json:"perTenant"`
	Failures         []string      `json:"failures"`
}

// RunPruningSweep is MemoryPruningJob.runForAllUsers: recompute then prune each
// user. A tenant whose prune fails keeps its recompute count, as in Swift.
func RunPruningSweep(ctx context.Context, pool *pgxpool.Pool, scoring Scoring, pruning Pruning, now time.Time) (PruningSweep, error) {
	users, err := listUsers(ctx, pool)
	if err != nil {
		return PruningSweep{}, err
	}
	s := PruningSweep{UsersScanned: len(users), PerTenant: []PruneResult{}, Failures: []string{}}
	for _, u := range users {
		rows, err := scoring.RecomputeForTenant(ctx, u.id, now)
		if err == nil {
			s.ScoreRowsUpdated += rows
			var r PruneResult
			r, err = pruning.PruneForTenant(ctx, u.id, now)
			if err == nil {
				s.MemoriesArchived += r.Archived
				if r.CandidatesScanned > 0 {
					s.PerTenant = append(s.PerTenant, r)
				}
				continue
			}
		}
		s.Failures = append(s.Failures, fmt.Sprintf("%s: %v", u.username, err))
		slog.WarnContext(ctx, "prune failed", "username", u.username, "error", err)
	}
	slog.InfoContext(ctx, "memory prune sweep", "tenants", len(users), "scored", s.ScoreRowsUpdated,
		"archived", s.MemoriesArchived, "failures", len(s.Failures))
	return s, nil
}

type user struct {
	id       uuid.UUID
	username string
}

// listUsers is User.query(on:).all(): no order.
func listUsers(ctx context.Context, pool *pgxpool.Pool) ([]user, error) {
	rows, err := pool.Query(ctx, `SELECT id, username FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []user
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.id, &u.username); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
