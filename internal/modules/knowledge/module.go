// Package knowledge serves the SQL half of /v1/knowledge: the Go port of
// LuminaVaultServer Sources/App/Knowledge (KnowledgeGraphController,
// KnowledgeGraphService). /v1/knowledge/reason and /reason/stream call the
// model and are the knowledge-reason module (group 4F).
// KnowledgeExtractionWorker fills these tables and stays in Swift.
package knowledge

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/knowledge/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/knowledge/db"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// KnowledgeGraphService's limits.
const (
	defaultLimit = 800
	maxLimit     = 2000
	maxDepth     = 4
)

var (
	errBadEdgeID      = wire.NewError(http.StatusBadRequest, "invalid edge id")
	errSameNode       = wire.NewError(http.StatusBadRequest, "choose two different nodes")
	errNotReviewable  = wire.NewError(http.StatusNotFound, "reviewable inference not found")
	errPathNotHydrate = wire.NewError(http.StatusInternalServerError, "connection path could not be hydrated")
)

// Module is the route group. Swift stacks jwtAuthenticator, the memoryQuery
// entitlement and captureByUser on it (App+build.swift "knowledgeGroup"). Each
// handler resolves vault access itself, after decoding its body, so with
// enforcement off a bad body is a 400 before a missing token is a 401.
type Module struct {
	q        *db.Queries
	access   *vaultaccess.Service
	identify echo.MiddlewareFunc
	gate     echo.MiddlewareFunc
	limits   *ratelimit.Store
	now      func() time.Time
}

// New builds the module. identify is auth.Identify, gate the memoryQuery
// entitlement.Middleware, and limits the process's shared rate-limit store.
func New(pool *pgxpool.Pool, identify, gate echo.MiddlewareFunc, limits *ratelimit.Store) *Module {
	return &Module{
		q:        db.New(pool),
		access:   vaultaccess.New(pool),
		identify: identify,
		gate:     gate,
		limits:   limits,
		now:      time.Now,
	}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	limit := ratelimit.TieredMiddleware(m.limits, ratelimit.CaptureByUser, ratelimit.UserOrIPKey, m.callerTier)
	mw := []echo.MiddlewareFunc{m.identify, m.gate, limit}
	r.GET("/v1/knowledge/graph", m.graph, mw...)
	r.POST("/v1/knowledge/connections/explain", m.explain, mw...)
	r.POST("/v1/knowledge/edges/:id/confirm", m.confirm, mw...)
	r.POST("/v1/knowledge/edges/:id/dismiss", m.dismiss, mw...)
}

// callerTier is `context.identity?.tierEnum`. The bucket is shared with capture.
func (m *Module) callerTier(c *echo.Context) (string, bool) {
	id, ok := auth.Identity(c)
	if !ok {
		return "", false
	}
	tier, err := m.q.UserTier(c.Request().Context(), id)
	if err != nil {
		return "", false
	}
	return tier, true
}

// MARK: - GET /v1/knowledge/graph

func (m *Module) graph(c *echo.Context) error {
	acc, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	limit := defaultLimit
	if n, ok := wire.QueryInt(c, "limit"); ok {
		limit = n
	}
	limit = min(max(limit, 1), maxLimit)
	var confidence float64
	if v, ok := wire.QueryDouble(c, "minimumConfidence"); ok {
		confidence = v
	}
	// A NaN survives both, as in Swift, and then matches no row.
	confidence = min(max(confidence, 0), 1)
	kinds := knownValues(wire.Query(c, "kinds"), knownKinds)
	predicates := knownValues(wire.Query(c, "predicates"), knownPredicates)
	states := knownValues(wire.Query(c, "states"), knownStates)
	if len(states) == 0 {
		states = []string{"asserted", "suggested", "confirmed"}
	}

	ctx := c.Request().Context()
	nodeRows, err := m.q.GraphNodes(ctx, db.GraphNodesParams{
		TenantID: acc.VaultID, MinimumConfidence: confidence, Kinds: kinds, RowLimit: int32(limit), //nolint:gosec // at most maxLimit
	})
	if err != nil {
		return err
	}
	out := KnowledgeGraphResponse{Nodes: []KnowledgeNodeDTO{}, Edges: []KnowledgeEdgeDTO{}, GeneratedAt: wire.Time{Time: m.now()}}
	if len(nodeRows) == 0 {
		return wire.JSON(c, http.StatusOK, out)
	}
	nodeIDs := make([]uuid.UUID, len(nodeRows))
	for i, n := range nodeRows {
		nodeIDs[i] = n.ID
	}
	edgeRows, err := m.q.GraphEdges(ctx, db.GraphEdgesParams{
		TenantID: acc.VaultID, NodeIds: nodeIDs, States: states, MinimumConfidence: confidence,
		Predicates: predicates, RowLimit: maxLimit * 8,
	})
	if err != nil {
		return err
	}
	edgeIDs := make([]uuid.UUID, len(edgeRows))
	for i, e := range edgeRows {
		edgeIDs[i] = e.ID
	}
	ev, err := m.loadEvidence(ctx, acc.VaultID, nodeIDs, edgeIDs)
	if err != nil {
		return err
	}
	for _, n := range nodeRows {
		out.Nodes = append(out.Nodes, nodeDTO(db.NodesByIDRow(n), ev.nodes))
	}
	for _, e := range edgeRows {
		out.Edges = append(out.Edges, edgeDTO(db.EdgesByIDRow(e), ev.edges))
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: - POST /v1/knowledge/connections/explain

func (m *Module) explain(c *echo.Context) error {
	body, err := wire.Bind[ConnectionExplanationRequest](c)
	if err != nil {
		return err
	}
	acc, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	from, to := body.FromNodeID.UUID, body.ToNodeID.UUID
	if from == to {
		return errSameNode
	}
	requested := maxDepth
	if body.MaxDepth != nil {
		requested = *body.MaxDepth
	}
	depth := min(max(1, requested), maxDepth)
	ctx := c.Request().Context()
	rows, err := m.q.ConnectionPaths(ctx, db.ConnectionPathsParams{
		FromNode: from, ToNode: to, TenantID: acc.VaultID, MaxDepth: int32(depth), //nolint:gosec // at most maxDepth
	})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return wire.JSON(c, http.StatusOK, ConnectionExplanationResponse{
			Explanation: "No evidence-backed connection was found within " + strconv.Itoa(depth) + " steps.",
			Paths:       []KnowledgePathDTO{},
			Confidence:  0,
			Caveats:     []string{"The graph may still be processing recent memories."},
		})
	}
	paths, err := m.hydratePaths(ctx, acc.VaultID, rows)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errPathNotHydrate
	}
	return wire.JSON(c, http.StatusOK, explanation(paths))
}

// explanation words the strongest path, which is the first one.
func explanation(paths []KnowledgePathDTO) ConnectionExplanationResponse {
	best := paths[0]
	steps := make([]string, len(best.Edges))
	inferred := false
	for i, edge := range best.Edges {
		steps[i] = displayName(edge.Predicate, edge.From != best.Nodes[i].Id) + " " + best.Nodes[i+1].Label
		inferred = inferred || edge.State == Suggested
	}
	caveats := []string{}
	if inferred {
		caveats = []string{"This path includes a machine-inferred connection that has not been confirmed."}
	}
	return ConnectionExplanationResponse{
		Explanation: best.Nodes[0].Label + " connects to " + best.Nodes[len(best.Nodes)-1].Label +
			" because it " + strings.Join(steps, ", then ") + ".",
		Paths:      paths,
		Confidence: best.Confidence,
		Caveats:    caveats,
	}
}

// hydratePaths drops a path with a node or edge outside the tenant: nothing
// stops an edge from naming another tenant's node.
func (m *Module) hydratePaths(ctx context.Context, tenant uuid.UUID, rows []db.ConnectionPathsRow) ([]KnowledgePathDTO, error) {
	var nodeIDs, edgeIDs []uuid.UUID
	for _, r := range rows {
		nodeIDs = append(nodeIDs, r.NodeIds...)
		edgeIDs = append(edgeIDs, r.EdgeIds...)
	}
	nodeRows, err := m.q.NodesByID(ctx, db.NodesByIDParams{TenantID: tenant, Ids: nodeIDs})
	if err != nil {
		return nil, err
	}
	edgeRows, err := m.q.EdgesByID(ctx, db.EdgesByIDParams{TenantID: tenant, Ids: edgeIDs})
	if err != nil {
		return nil, err
	}
	ev, err := m.loadEvidence(ctx, tenant, nodeIDs, edgeIDs)
	if err != nil {
		return nil, err
	}
	nodes := make(map[uuid.UUID]KnowledgeNodeDTO, len(nodeRows))
	for _, n := range nodeRows {
		nodes[n.ID] = nodeDTO(n, ev.nodes)
	}
	edges := make(map[uuid.UUID]KnowledgeEdgeDTO, len(edgeRows))
	for _, e := range edgeRows {
		edges[e.ID] = edgeDTO(e, ev.edges)
	}
	paths := make([]KnowledgePathDTO, 0, len(rows))
row:
	for _, r := range rows {
		path := KnowledgePathDTO{
			Id:         wire.UUID{UUID: uuid.New()},
			Nodes:      make([]KnowledgeNodeDTO, 0, len(r.NodeIds)),
			Edges:      make([]KnowledgeEdgeDTO, 0, len(r.EdgeIds)),
			Confidence: wire.Float64(r.PathConfidence),
		}
		for _, id := range r.NodeIds {
			n, ok := nodes[id]
			if !ok {
				continue row
			}
			path.Nodes = append(path.Nodes, n)
		}
		for _, id := range r.EdgeIds {
			e, ok := edges[id]
			if !ok {
				continue row
			}
			path.Edges = append(path.Edges, e)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// MARK: - POST /v1/knowledge/edges/{id}/confirm and /dismiss

func (m *Module) confirm(c *echo.Context) error { return m.review(c, "confirmed") }
func (m *Module) dismiss(c *echo.Context) error { return m.review(c, "dismissed") }

func (m *Module) review(c *echo.Context, state string) error {
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return errBadEdgeID
	}
	body, err := wire.Bind[InferenceReviewRequest](c)
	if err != nil {
		return err
	}
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	row, err := m.q.ReviewEdge(ctx, db.ReviewEdgeParams{State: state, Note: body.Note, TenantID: acc.VaultID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotReviewable
	}
	if err != nil {
		return err
	}
	ev, err := m.loadEvidence(ctx, acc.VaultID, nil, []uuid.UUID{id})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, edgeDTO(db.EdgesByIDRow(row), ev.edges))
}

// MARK: - Shared

type evidence struct {
	nodes, edges map[uuid.UUID][]KnowledgeEvidenceDTO
}

func (m *Module) loadEvidence(ctx context.Context, tenant uuid.UUID, nodeIDs, edgeIDs []uuid.UUID) (evidence, error) {
	out := evidence{nodes: map[uuid.UUID][]KnowledgeEvidenceDTO{}, edges: map[uuid.UUID][]KnowledgeEvidenceDTO{}}
	if len(nodeIDs) == 0 && len(edgeIDs) == 0 {
		return out, nil
	}
	rows, err := m.q.Evidence(ctx, db.EvidenceParams{TenantID: tenant, NodeIds: nodeIDs, EdgeIds: edgeIDs})
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		dto := KnowledgeEvidenceDTO{
			Id: wire.UUID{UUID: r.ID}, MemoryID: wire.UUID{UUID: r.MemoryID}, Quote: r.Quote,
			StartOffset: intPtr(r.StartOffset), EndOffset: intPtr(r.EndOffset),
		}
		if r.SourceVaultFileID != nil {
			dto.SourceVaultFileID = &wire.UUID{UUID: *r.SourceVaultFileID}
		}
		if r.NodeID != nil {
			out.nodes[*r.NodeID] = append(out.nodes[*r.NodeID], dto)
		}
		if r.EdgeID != nil {
			out.edges[*r.EdgeID] = append(out.edges[*r.EdgeID], dto)
		}
	}
	return out, nil
}

func intPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func nodeDTO(r db.NodesByIDRow, ev map[uuid.UUID][]KnowledgeEvidenceDTO) KnowledgeNodeDTO {
	dto := KnowledgeNodeDTO{
		Id:         wire.UUID{UUID: r.ID},
		Kind:       KnowledgeNodeKindDTO(r.Kind),
		Label:      r.Label,
		Summary:    r.Summary,
		Confidence: wire.Float64(r.Confidence),
		Evidence:   evidenceOf(ev, r.ID),
	}
	if !dto.Kind.Valid() {
		dto.Kind = Claim
	}
	if r.OccurredAt != nil {
		dto.OccurredAt = &wire.Time{Time: *r.OccurredAt}
	}
	return dto
}

func edgeDTO(r db.EdgesByIDRow, ev map[uuid.UUID][]KnowledgeEvidenceDTO) KnowledgeEdgeDTO {
	dto := KnowledgeEdgeDTO{
		Id:              wire.UUID{UUID: r.ID},
		From:            wire.UUID{UUID: r.FromNodeID},
		To:              wire.UUID{UUID: r.ToNodeID},
		Predicate:       KnowledgeEdgePredicateDTO(r.Predicate),
		State:           KnowledgeEdgeStateDTO(r.State),
		Confidence:      wire.Float64(r.Confidence),
		Rationale:       r.Rationale,
		CounterEvidence: r.CounterEvidence,
		Evidence:        evidenceOf(ev, r.ID),
	}
	if !dto.Predicate.Valid() {
		dto.Predicate = RelatedTo
	}
	if !dto.State.Valid() {
		dto.State = Suggested
	}
	return dto
}

func evidenceOf(ev map[uuid.UUID][]KnowledgeEvidenceDTO, id uuid.UUID) []KnowledgeEvidenceDTO {
	if rows := ev[id]; rows != nil {
		return rows
	}
	return []KnowledgeEvidenceDTO{}
}

var (
	knownKinds      = map[string]bool{"claim": true, "entity": true, "event": true}
	knownPredicates = map[string]bool{
		"mentions": true, "about": true, "supports": true, "contradicts": true,
		"causes": true, "precedes": true, "related_to": true, "derived_from": true,
	}
	knownStates = map[string]bool{"asserted": true, "suggested": true, "confirmed": true, "dismissed": true, "stale": true}
)

// knownValues is `Set(csv(value).compactMap(Enum.init(rawValue:)))`. Never nil:
// a NULL array would match no row, where empty means no filter.
func knownValues(csv string, known map[string]bool) []string {
	out := []string{}
	for part := range strings.SplitSeq(csv, ",") {
		if known[part] {
			out = append(out, part)
		}
	}
	return out
}

// displayName is KnowledgeEdgePredicateDTO.displayName(reversed:).
func displayName(p KnowledgeEdgePredicateDTO, reversed bool) string {
	if reversed {
		switch p {
		case Mentions:
			return "is mentioned by"
		case About:
			return "is the subject of"
		case Supports:
			return "is supported by"
		case Contradicts:
			return "contradicts"
		case Causes:
			return "is caused by"
		case Precedes:
			return "follows"
		case RelatedTo:
			return "relates to"
		case DerivedFrom:
			return "is a source for"
		}
	}
	switch p {
	case Mentions:
		return "mentions"
	case About:
		return "is about"
	case Supports:
		return "supports"
	case Contradicts:
		return "contradicts"
	case Causes:
		return "causes"
	case Precedes:
		return "precedes"
	case RelatedTo:
		return "relates to"
	case DerivedFrom:
		return "is derived from"
	}
	return "relates to"
}
