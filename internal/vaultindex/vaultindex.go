// Package vaultindex is the Go port of LuminaVaultServer's VaultIndexStatusService
// and VaultNavigationService (Sources/App/Indexing): read-only views over the chunk
// and link indexes, for /v1/vault and for MCP's status and context tools. Neither
// touches the disk, so neither takes a vaultfiles path check, and neither writes.
//
// VaultLinkRepository's Go form is chunkindex.LinkRepository.
package vaultindex

import (
	"context"
	"fmt"
	"lumina/internal/chunkindex"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultindex/db"
	"lumina/internal/wire"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// SamplePathLimit is VaultIndexStatusService.samplePathLimit.
	SamplePathLimit = 20
	// DefaultSiblingLimit is VaultNavigationService.defaultSiblingLimit.
	DefaultSiblingLimit = 25
	// MaxSiblingLimit is VaultNavigationService.maxSiblingLimit.
	MaxSiblingLimit = 100
)

// ErrNoFileWithID is the 404 from Context, and from VaultIndexController.documentLinks.
var ErrNoFileWithID = wire.NewError(http.StatusNotFound, "no vault file with that id")

// Status is VaultIndexStatus, with the wire's sorted field order. These are the same
// shapes as the generated models in modules/vault, which can't be imported from here.
type Status struct {
	AmbiguousLinkCount     int        `json:"ambiguousLinkCount"`
	ChunkCount             int        `json:"chunkCount"`
	DocumentCount          int        `json:"documentCount"`
	LastIndexedAt          *wire.Time `json:"lastIndexedAt,omitempty"`
	MemoryCount            int        `json:"memoryCount"`
	ResolvedLinkCount      int        `json:"resolvedLinkCount"`
	Stale                  bool       `json:"stale"`
	StaleDocumentCount     int        `json:"staleDocumentCount"`
	StaleReasons           []string   `json:"staleReasons"`
	StaleSample            []string   `json:"staleSample"`
	UnindexedDocumentCount int        `json:"unindexedDocumentCount"`
	UnindexedSample        []string   `json:"unindexedSample"`
	UnresolvedLinkCount    int        `json:"unresolvedLinkCount"`
}

// ContextHeading is VaultContextPacket.Heading.
type ContextHeading struct {
	Path      []string `json:"path"`
	StartLine int      `json:"startLine"`
}

// ContextNeighbour is VaultContextPacket.Neighbour.
type ContextNeighbour struct {
	Path        string    `json:"path"`
	VaultFileID wire.UUID `json:"vaultFileID"`
}

// ContextPacket is VaultContextPacket.
type ContextPacket struct {
	ChunkCount int `json:"chunkCount"`
	// DanglingLinks are links that resolve to nothing or to more than one document, as written.
	DanglingLinks []string         `json:"danglingLinks"`
	Headings      []ContextHeading `json:"headings"`
	// LinkedFrom is the backlinks.
	LinkedFrom []ContextNeighbour `json:"linkedFrom"`
	LinksTo    []ContextNeighbour `json:"linksTo"`
	Path       string             `json:"path"`
	// Siblings are other documents in the same Space. Empty for an unfiled document.
	Siblings    []ContextNeighbour `json:"siblings"`
	SpaceID     *wire.UUID         `json:"spaceID,omitempty"`
	VaultFileID wire.UUID          `json:"vaultFileID"`
}

// Service is VaultIndexStatusService and VaultNavigationService.
type Service struct {
	q     *db.Queries
	store *vaultfiles.Store
	links *chunkindex.LinkRepository
}

func New(pool *pgxpool.Pool, store *vaultfiles.Store, links *chunkindex.LinkRepository) *Service {
	return &Service{q: db.New(pool), store: store, links: links}
}

// Status is VaultIndexStatusService.status.
func (s *Service) Status(ctx context.Context, tenant uuid.UUID) (Status, error) {
	var out Status
	counts, err := s.q.IndexCounts(ctx, tenant)
	if err != nil {
		return out, err
	}
	lastIndexedAt, err := s.q.LastIndexedAt(ctx, tenant)
	if err != nil {
		return out, err
	}
	unindexed, err := s.q.UnindexedPaths(ctx, tenant)
	if err != nil {
		return out, err
	}
	stale, err := s.q.StalePaths(ctx, tenant)
	if err != nil {
		return out, err
	}
	links, err := s.links.Counts(ctx, tenant)
	if err != nil {
		return out, err
	}
	var lastIndexed *time.Time
	if lastIndexedAt.Valid {
		lastIndexed = &lastIndexedAt.Time
	}
	reasons := []string{}
	if len(unindexed) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d document(s) have no chunks — run the chunk backfill", len(unindexed)))
	}
	if len(stale) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d document(s) changed since they were indexed", len(stale)))
	}
	if links.Ambiguous > 0 {
		reasons = append(reasons, fmt.Sprintf("%d wiki link(s) match more than one document", links.Ambiguous))
	}
	return Status{
		DocumentCount:          int(counts.Documents),
		MemoryCount:            int(counts.Memories),
		ChunkCount:             int(counts.Chunks),
		ResolvedLinkCount:      links.Resolved,
		UnresolvedLinkCount:    links.Unresolved,
		AmbiguousLinkCount:     links.Ambiguous,
		UnindexedDocumentCount: len(unindexed),
		StaleDocumentCount:     len(stale),
		UnindexedSample:        firstN(unindexed, SamplePathLimit),
		StaleSample:            firstN(stale, SamplePathLimit),
		LastIndexedAt:          wireTime(lastIndexed),
		Stale:                  len(reasons) > 0,
		StaleReasons:           reasons,
	}, nil
}

// Context is VaultNavigationService.context. siblingLimit is clamped to
// 1...MaxSiblingLimit. A file the tenant doesn't have is ErrNoFileWithID.
func (s *Service) Context(ctx context.Context, tenant, id uuid.UUID, siblingLimit int) (ContextPacket, error) {
	var out ContextPacket
	siblingLimit = max(1, min(siblingLimit, MaxSiblingLimit))
	file, err := s.store.FindByID(ctx, tenant, id)
	if err != nil {
		return out, err
	}
	if file == nil {
		return out, ErrNoFileWithID
	}
	headingRows, err := s.q.HeadingRows(ctx, db.HeadingRowsParams{TenantID: tenant, VaultFileID: &id})
	if err != nil {
		return out, err
	}
	headings := []ContextHeading{}
	for _, row := range headingRows {
		path := chunkindex.DecodeHeadingPath(&row.HeadingPathJson)
		if len(path) > 0 {
			headings = append(headings, ContextHeading{Path: path, StartLine: int(row.StartLine)})
		}
	}
	chunkCount, err := s.q.ChunkCount(ctx, db.ChunkCountParams{TenantID: tenant, VaultFileID: &id})
	if err != nil {
		return out, err
	}
	// An unfiled document has no Space and so no siblings, not "every unfiled note".
	siblings := []ContextNeighbour{}
	if file.SpaceID != nil {
		rows, err := s.q.Siblings(ctx, db.SiblingsParams{TenantID: tenant, SpaceID: file.SpaceID, ID: id, Limit: int32(siblingLimit)}) //nolint:gosec // clamped to 1...100
		if err != nil {
			return out, err
		}
		for _, row := range rows {
			siblings = append(siblings, ContextNeighbour{VaultFileID: wire.UUID{UUID: row.ID}, Path: row.Path})
		}
	}
	outgoing, err := s.links.Outgoing(ctx, tenant, id)
	if err != nil {
		return out, err
	}
	incoming, err := s.links.Incoming(ctx, tenant, id)
	if err != nil {
		return out, err
	}
	var targets, sources []string
	dangling := []string{}
	for _, l := range outgoing {
		if l.TargetPath != nil {
			targets = append(targets, *l.TargetPath)
		}
		if l.ResolutionState != string(chunkindex.Resolved) {
			dangling = append(dangling, l.RawTarget)
		}
	}
	for _, l := range incoming {
		if l.SourcePath != nil {
			sources = append(sources, *l.SourcePath)
		}
	}
	linksTo, err := s.neighbours(ctx, tenant, targets)
	if err != nil {
		return out, err
	}
	linkedFrom, err := s.neighbours(ctx, tenant, sources)
	if err != nil {
		return out, err
	}
	slices.Sort(dangling)
	return ContextPacket{
		VaultFileID:   wire.UUID{UUID: id},
		Path:          file.Path,
		SpaceID:       wireUUID(file.SpaceID),
		ChunkCount:    int(chunkCount),
		Headings:      headings,
		Siblings:      siblings,
		LinksTo:       linksTo,
		LinkedFrom:    linkedFrom,
		DanglingLinks: slices.Compact(dangling),
	}, nil
}

// neighbours is resolveNeighbours: deduplicated, and paths that no longer exist
// are dropped.
func (s *Service) neighbours(ctx context.Context, tenant uuid.UUID, paths []string) ([]ContextNeighbour, error) {
	out := []ContextNeighbour{}
	if len(paths) == 0 {
		return out, nil
	}
	rows, err := s.q.NeighboursByPath(ctx, db.NeighboursByPathParams{TenantID: tenant, Column2: paths})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out = append(out, ContextNeighbour{VaultFileID: wire.UUID{UUID: row.ID}, Path: row.Path})
	}
	return out, nil
}

// firstN is `Array(s.prefix(n))`, never nil, because these lists are required.
func firstN[T any](s []T, n int) []T {
	if s == nil {
		return []T{}
	}
	return s[:min(n, len(s))]
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func wireUUID(id *uuid.UUID) *wire.UUID {
	if id == nil {
		return nil
	}
	return &wire.UUID{UUID: *id}
}
