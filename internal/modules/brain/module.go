// Package brain serves GET /v1/brain/graph: one read-only knowledge graph across
// every vault the caller can read. It is new in the Go server and has no Swift
// counterpart.
package brain

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/brain/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/brain/db"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// Module is the route group. Like /v1/memory/graph it has no entitlement gate.
type Module struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	access   *vaultaccess.Service
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	now      func() time.Time
}

// New builds the module. identify is auth.Identify and limits the process's
// shared rate-limit store.
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc, limits *ratelimit.Store) *Module {
	return &Module{pool: pool, q: db.New(pool), access: vaultaccess.New(pool), identify: identify, limits: limits, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	limit := ratelimit.TieredMiddleware(m.limits, ratelimit.BrainGraphByUser, ratelimit.UserOrIPKey, ratelimit.PGTier(m.pool))
	// identify never rejects, so Require turns a missing identity into the 401,
	// after the limiter as in the other Identify chains.
	r.GET("/v1/brain/graph", auth.Require(m.graph), m.identify, limit)
}

// params is the parsed query string.
type params struct {
	vaults []uuid.UUID
	links  map[string]bool
	limit  int32
}

const (
	defaultLimit = 400
	maxLimit     = 2000

	// similar edges: top 3 neighbours per memory at cosine similarity 0.78 or higher.
	minSimilarity  = 0.78
	similarPerNode = 3
)

var errInvalidVaults = wire.NewError(http.StatusBadRequest, "invalid vaults")

// parseParams reads the query string. X-Vault-ID is deliberately not read: this
// route spans vaults and takes its scope only from `vaults`.
func parseParams(c *echo.Context) (params, error) {
	p := params{links: parseLinks(c.QueryParam("links")), limit: parseLimit(c.QueryParam("limit"))}
	if raw := c.QueryParam("vaults"); raw != "" {
		seen := map[uuid.UUID]bool{}
		for _, part := range strings.Split(raw, ",") {
			id, err := ids.Parse(part)
			if err != nil {
				return params{}, errInvalidVaults
			}
			if !seen[id] {
				seen[id] = true
				p.vaults = append(p.vaults, id)
			}
		}
	}
	return p, nil
}

// parseLimit never fails: absent or unparsable is the default 400, and any number is clamped to 1..2000.
func parseLimit(raw string) int32 {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultLimit
	}
	return int32(min(max(n, 1), maxLimit)) //nolint:gosec // clamped to 1..2000
}

func (m *Module) graph(c *echo.Context) error {
	p, err := parseParams(c)
	if err != nil {
		return err
	}
	userID, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	scopes, err := m.scopes(c.Request().Context(), userID, p.vaults)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	vaultIDs := make([]uuid.UUID, 0, len(scopes))
	for _, s := range scopes {
		vaultIDs = append(vaultIDs, s.ID)
	}
	// One extra row tells us whether the limit cut something off.
	rows, err := m.q.Items(ctx, db.ItemsParams{VaultIds: vaultIDs, RowLimit: p.limit + 1})
	if err != nil {
		return err
	}
	truncated := len(rows) > int(p.limit)
	if truncated {
		rows = rows[:p.limit]
	}
	items := make([]item, 0, len(rows))
	var noteIDs, memoryIDs []uuid.UUID
	for _, r := range rows {
		items = append(items, itemFromRow(r))
		if r.Kind == "memory" {
			memoryIDs = append(memoryIDs, r.ID)
		} else {
			noteIDs = append(noteIDs, r.ID)
		}
	}
	var rels []relation
	if p.links["link"] && len(noteIDs) > 0 {
		links, err := m.q.NoteLinks(ctx, db.NoteLinksParams{VaultIds: vaultIDs, NoteIds: noteIDs})
		if err != nil {
			return err
		}
		for _, l := range links {
			if l.ToID != nil {
				rels = append(rels, relation{From: l.FromID.String(), To: l.ToID.String(), Kind: "link"})
			}
		}
	}
	if p.links["source"] && len(memoryIDs) > 0 {
		srcs, err := m.q.MemorySources(ctx, db.MemorySourcesParams{VaultIds: vaultIDs, MemoryIds: memoryIDs})
		if err != nil {
			return err
		}
		for _, s := range srcs {
			if s.NoteID != nil {
				rels = append(rels, relation{From: s.MemoryID.String(), To: s.NoteID.String(), Kind: "source"})
			}
		}
	}
	if p.links["similar"] && len(memoryIDs) > 0 {
		sims, err := m.q.SimilarMemories(ctx, db.SimilarMemoriesParams{
			VaultIds: vaultIDs, MemoryIds: memoryIDs, MinSimilarity: minSimilarity, PerNode: similarPerNode,
		})
		if err != nil {
			return err
		}
		for _, s := range sims {
			w := s.Similarity
			rels = append(rels, relation{From: s.FromID.String(), To: s.ToID.String(), Kind: "similar", Weight: &w})
		}
	}
	hubs, hubRels, err := m.hubs(ctx, p.links, vaultIDs, items, memoryIDs)
	if err != nil {
		return err
	}
	nodes, edges := assemble(items, hubs, append(rels, hubRels...))
	vaults := make([]BrainGraphVault, 0, len(scopes))
	for _, s := range scopes {
		vaults = append(vaults, BrainGraphVault{Id: wire.UUID{UUID: s.ID}, Name: s.Name, Personal: s.Personal})
	}
	return wire.JSON(c, http.StatusOK, BrainGraphResponse{
		GeneratedAt: wire.Time{Time: m.now()},
		Truncated:   truncated,
		Vaults:      vaults,
		Nodes:       nodes,
		Edges:       edges,
	})
}

// hubs runs the hub queries for the requested kinds. Each is bound to the admitted vaults and
// the selected items; assemble later drops hubs with fewer than two members.
func (m *Module) hubs(ctx context.Context, links map[string]bool, vaultIDs []uuid.UUID, items []item, memoryIDs []uuid.UUID) ([]hub, []relation, error) {
	var hubs []hub
	var rels []relation
	add := func(h []hub, r []relation) {
		hubs = append(hubs, h...)
		rels = append(rels, r...)
	}
	if links["tag"] {
		add(tagHubs(items))
	}
	if links["space"] {
		var spaceIDs []uuid.UUID
		for _, it := range items {
			if it.SpaceID != nil {
				spaceIDs = append(spaceIDs, *it.SpaceID)
			}
		}
		if len(spaceIDs) > 0 {
			rows, err := m.q.Spaces(ctx, db.SpacesParams{VaultIds: vaultIDs, SpaceIds: spaceIDs})
			if err != nil {
				return nil, nil, err
			}
			add(spaceHubs(items, rows))
		}
	}
	if links["author"] {
		var userIDs []uuid.UUID
		for _, it := range items {
			if it.CreatedBy != nil {
				userIDs = append(userIDs, *it.CreatedBy)
			}
		}
		if len(userIDs) > 0 {
			rows, err := m.q.Authors(ctx, userIDs)
			if err != nil {
				return nil, nil, err
			}
			add(userAuthorHubs(items, rows))
		}
		if len(memoryIDs) > 0 {
			rows, err := m.q.ModelAuthors(ctx, db.ModelAuthorsParams{VaultIds: vaultIDs, MemoryIds: memoryIDs})
			if err != nil {
				return nil, nil, err
			}
			add(modelAuthorHubs(rows))
		}
	}
	if links["entity"] && len(memoryIDs) > 0 {
		rows, err := m.q.Entities(ctx, db.EntitiesParams{VaultIds: vaultIDs, MemoryIds: memoryIDs})
		if err != nil {
			return nil, nil, err
		}
		add(entityHubs(rows))
	}
	return hubs, rels, nil
}
