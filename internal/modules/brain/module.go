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
	"unicode/utf8"

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
	// focus is a node id; empty means no focus. depth is 1..3 and only used with focus.
	focus string
	depth int
}

const (
	defaultLimit = 400
	maxLimit     = 2000
	// focusLimit is the item window of a focused view when `limit` is absent: the local graph
	// of a note must not depend on whether the note is among the newest 400.
	focusLimit = maxLimit
	maxFocus   = 300
	maxDepth   = 3

	// similar edges: top 3 neighbours per memory at cosine similarity 0.78 or higher.
	minSimilarity  = 0.78
	similarPerNode = 3
)

var (
	errInvalidVaults = wire.NewError(http.StatusBadRequest, "invalid vaults")
	errInvalidFocus  = wire.NewError(http.StatusBadRequest, "invalid focus")
	errNodeNotFound  = wire.NewError(http.StatusNotFound, "node not found")
)

// parseParams reads the query string. X-Vault-ID is deliberately not read: this
// route spans vaults and takes its scope only from `vaults`.
func parseParams(c *echo.Context) (params, error) {
	p := params{links: parseLinks(c.QueryParam("links")), limit: parseLimit(c.QueryParam("limit")), depth: 1}
	if _, ok := c.QueryParams()["focus"]; ok {
		p.focus = c.QueryParam("focus")
		if p.focus == "" || utf8.RuneCountInString(p.focus) > maxFocus {
			return params{}, errInvalidFocus
		}
		p.focus = normaliseFocus(p.focus)
		if _, ok := c.QueryParams()["limit"]; !ok {
			p.limit = focusLimit
		}
	}
	if n, err := strconv.Atoi(c.QueryParam("depth")); err == nil {
		p.depth = min(max(n, 1), maxDepth)
	}
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

// normaliseFocus maps a focus value onto the case node ids use, so a uuid copied from any
// endpoint matches in either case: a bare uuid, or a uuid after `space:` or `author:user:`,
// becomes UPPERCASE. Anything else is matched exactly as given.
func normaliseFocus(s string) string {
	if id, err := ids.Parse(s); err == nil {
		return nodeID(id)
	}
	for _, prefix := range [...]string{"space:", "author:user:"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			if id, err := ids.Parse(rest); err == nil {
				return prefix + nodeID(id)
			}
		}
	}
	return s
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
				rels = append(rels, relation{From: nodeID(l.FromID), To: nodeID(*l.ToID), Kind: "link"})
			}
		}
	}
	if p.links["source"] && len(memoryIDs) > 0 {
		srcs, err := m.q.MemorySources(ctx, db.MemorySourcesParams{VaultIds: vaultIDs, MemoryIds: memoryIDs})
		if err != nil {
			return err
		}
		for _, s := range srcs {
			rels = append(rels, relation{From: nodeID(s.MemoryID), To: nodeID(s.NoteID), Kind: "source"})
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
			rels = append(rels, relation{From: nodeID(s.FromID), To: nodeID(s.ToID), Kind: "similar", Weight: &w})
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
	if p.focus != "" {
		var found bool
		if nodes, edges, found = neighbourhood(nodes, edges, p.focus, p.depth); !found {
			return errNodeNotFound
		}
	}
	resp := BrainGraphResponse{
		GeneratedAt: wire.Time{Time: m.now()},
		Truncated:   truncated,
		Vaults:      vaults,
		Nodes:       nodes,
		Edges:       edges,
	}
	// Vault access was resolved above, so a 304 is only ever sent to a caller who may read the graph.
	tag := etag(resp)
	h := c.Response().Header()
	h.Set("ETag", tag)
	h.Set(echo.HeaderCacheControl, "private, no-cache")
	if tag != "" && matchesETag(c.Request().Header.Get("If-None-Match"), tag) {
		return c.NoContent(http.StatusNotModified)
	}
	return wire.JSON(c, http.StatusOK, resp)
}

// matchesETag reports whether an If-None-Match value names tag. It accepts a comma-separated
// list and the W/ weak prefix; `*` is not supported.
func matchesETag(header, tag string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(part), "W/") == tag {
			return true
		}
	}
	return false
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
