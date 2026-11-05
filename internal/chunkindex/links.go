package chunkindex

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OutgoingLink is OutgoingLinkRow. Swift's synthesized Codable gives these keys and
// omits nil fields.
type OutgoingLink struct {
	SourceLine      int     `json:"sourceLine"`
	RawTarget       string  `json:"rawTarget"`
	TargetSlug      string  `json:"targetSlug"`
	TargetHeading   *string `json:"targetHeading,omitempty"`
	Label           *string `json:"label,omitempty"`
	ResolutionState string  `json:"resolutionState"`
	// TargetPath is nil unless resolved.
	TargetPath *string `json:"targetPath,omitempty"`
}

// IncomingLink is IncomingLinkRow, a backlink.
type IncomingLink struct {
	SourcePath      *string `json:"sourcePath,omitempty"`
	SourceLine      int     `json:"sourceLine"`
	RawTarget       string  `json:"rawTarget"`
	TargetSlug      string  `json:"targetSlug"`
	ResolutionState string  `json:"resolutionState"`
}

// LinkCounts is VaultLinkCounts.
type LinkCounts struct {
	Resolved   int `json:"resolved"`
	Unresolved int `json:"unresolved"`
	Ambiguous  int `json:"ambiguous"`
}

// LinkRepository is VaultLinkRepository, the [[wikilink]] graph. Every statement binds
// tenant_id explicitly.
type LinkRepository struct {
	pool *pgxpool.Pool
}

// NewLinkRepository builds a repository.
func NewLinkRepository(pool *pgxpool.Pool) *LinkRepository {
	return &LinkRepository{pool: pool}
}

// ReplaceLinks is replaceLinks: every link of one source document, delete then insert
// in a transaction. Links resolve against the vault as it is now, read before the
// transaction. One to a note that does not exist yet is stored unresolved and upgraded
// later by Reresolve.
func (r *LinkRepository) ReplaceLinks(ctx context.Context, tenant, sourceVaultFile uuid.UUID, sourcePath *string, links []ParsedWikilink) error {
	candidates, err := r.targets(ctx, tenant)
	if err != nil {
		return err
	}
	resolved := ResolveWikilinks(links, candidates)
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM vault_links WHERE tenant_id = $1 AND source_vault_file_id = $2`, tenant, sourceVaultFile); err != nil {
			return err
		}
		for _, item := range resolved {
			if _, err := tx.Exec(ctx, `
				INSERT INTO vault_links (
					tenant_id, source_vault_file_id, source_path, source_line,
					raw_target, target_slug, target_heading, label,
					target_vault_file_id, resolved, resolution_state, created_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())`,
				tenant, sourceVaultFile, sourcePath, item.Link.Line, item.Link.RawTarget, item.Link.TargetSlug,
				item.Link.TargetHeading, item.Link.Label, item.TargetVaultFileID, item.State == Resolved, string(item.State)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("chunkindex: replace links: %w", err)
	}
	return nil
}

// Reresolve is reresolve: every link in the tenant against the current documents.
// It returns how many rows changed. Each row updates on its own, as in Swift.
func (r *LinkRepository) Reresolve(ctx context.Context, tenant uuid.UUID) (int, error) {
	candidates, err := r.targets(ctx, tenant)
	if err != nil {
		return 0, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT link_id, source_line, raw_target, target_slug, target_heading, label, target_vault_file_id, resolution_state
		FROM vault_links
		WHERE tenant_id = $1`, tenant)
	if err != nil {
		return 0, fmt.Errorf("chunkindex: reresolve: %w", err)
	}
	type stored struct {
		id     int64
		target *uuid.UUID
		state  string
	}
	var (
		kept   []stored
		parsed []ParsedWikilink
	)
	defer rows.Close()
	for rows.Next() {
		var (
			s stored
			l ParsedWikilink
		)
		if err := rows.Scan(&s.id, &l.Line, &l.RawTarget, &l.TargetSlug, &l.TargetHeading, &l.Label, &s.target, &s.state); err != nil {
			return 0, fmt.Errorf("chunkindex: reresolve: %w", err)
		}
		kept = append(kept, s)
		parsed = append(parsed, l)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("chunkindex: reresolve: %w", err)
	}
	rows.Close()

	changed := 0
	for i, item := range ResolveWikilinks(parsed, candidates) {
		s := kept[i]
		if s.state == string(item.State) && sameTarget(s.target, item.TargetVaultFileID) {
			continue
		}
		if _, err := r.pool.Exec(ctx, `
			UPDATE vault_links
			SET target_vault_file_id = $1, resolved = $2, resolution_state = $3
			WHERE tenant_id = $4 AND link_id = $5`,
			item.TargetVaultFileID, item.State == Resolved, string(item.State), tenant, s.id); err != nil {
			return changed, fmt.Errorf("chunkindex: reresolve: %w", err)
		}
		changed++
	}
	return changed, nil
}

func sameTarget(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Outgoing is outgoing: one document's links, in source order.
func (r *LinkRepository) Outgoing(ctx context.Context, tenant, vaultFile uuid.UUID) ([]OutgoingLink, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT l.source_line, l.raw_target, l.target_slug, l.target_heading, l.label,
		       l.resolution_state, t.path
		FROM vault_links l
		LEFT JOIN vault_files t
			ON t.id = l.target_vault_file_id AND t.tenant_id = l.tenant_id
		WHERE l.tenant_id = $1 AND l.source_vault_file_id = $2
		ORDER BY l.source_line, l.link_id`, tenant, vaultFile)
	if err != nil {
		return nil, fmt.Errorf("chunkindex: outgoing links: %w", err)
	}
	defer rows.Close()
	out := []OutgoingLink{}
	for rows.Next() {
		var l OutgoingLink
		if err := rows.Scan(&l.SourceLine, &l.RawTarget, &l.TargetSlug, &l.TargetHeading, &l.Label, &l.ResolutionState, &l.TargetPath); err != nil {
			return nil, fmt.Errorf("chunkindex: outgoing links: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Incoming is incoming: every link that resolves to this document.
func (r *LinkRepository) Incoming(ctx context.Context, tenant, vaultFile uuid.UUID) ([]IncomingLink, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT l.source_path, l.source_line, l.raw_target, l.target_slug, l.resolution_state
		FROM vault_links l
		WHERE l.tenant_id = $1 AND l.target_vault_file_id = $2
		ORDER BY l.source_path NULLS LAST, l.source_line, l.link_id`, tenant, vaultFile)
	if err != nil {
		return nil, fmt.Errorf("chunkindex: incoming links: %w", err)
	}
	defer rows.Close()
	out := []IncomingLink{}
	for rows.Next() {
		var l IncomingLink
		if err := rows.Scan(&l.SourcePath, &l.SourceLine, &l.RawTarget, &l.TargetSlug, &l.ResolutionState); err != nil {
			return nil, fmt.Errorf("chunkindex: incoming links: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Counts is counts, by resolution state, for vault status and lint.
func (r *LinkRepository) Counts(ctx context.Context, tenant uuid.UUID) (LinkCounts, error) {
	var c LinkCounts
	rows, err := r.pool.Query(ctx, `
		SELECT resolution_state, COUNT(*) FROM vault_links WHERE tenant_id = $1 GROUP BY resolution_state`, tenant)
	if err != nil {
		return c, fmt.Errorf("chunkindex: link counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			state string
			total int
		)
		if err := rows.Scan(&state, &total); err != nil {
			return c, fmt.Errorf("chunkindex: link counts: %w", err)
		}
		switch ResolutionState(state) {
		case Resolved:
			c.Resolved = total
		case Unresolved:
			c.Unresolved = total
		case Ambiguous:
			c.Ambiguous = total
		}
	}
	return c, rows.Err()
}

// DefaultOrphanLimit is orphanPaths' default limit.
const DefaultOrphanLimit = 200

// OrphanPaths is orphanPaths: documents nothing links to, for lint.
func (r *LinkRepository) OrphanPaths(ctx context.Context, tenant uuid.UUID, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT v.path
		FROM vault_files v
		WHERE v.tenant_id = $1
		  AND NOT EXISTS (
		      SELECT 1 FROM vault_links l
		      WHERE l.tenant_id = v.tenant_id AND l.target_vault_file_id = v.id
		  )
		ORDER BY v.path
		LIMIT $2`, tenant, max(1, limit))
	if err != nil {
		return nil, fmt.Errorf("chunkindex: orphan paths: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("chunkindex: orphan paths: %w", err)
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

// targets are the candidate link targets: every file row in the tenant, including
// any under _deleted_ paths, as Swift's raw SELECT has it.
func (r *LinkRepository) targets(ctx context.Context, tenant uuid.UUID) ([]WikilinkTarget, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, path FROM vault_files WHERE tenant_id = $1`, tenant)
	if err != nil {
		return nil, fmt.Errorf("chunkindex: link targets: %w", err)
	}
	defer rows.Close()
	var out []WikilinkTarget
	for rows.Next() {
		var t WikilinkTarget
		if err := rows.Scan(&t.VaultFileID, &t.Path); err != nil {
			return nil, fmt.Errorf("chunkindex: link targets: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
