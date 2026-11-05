package memory

import (
	"context"
	"errors"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// MemoryGraphService's defaults, and its fixed edge weights. Weight drives only visual
// alpha; capping priority comes from edge-kind precedence in mergeAndCap.
const (
	graphDefaultLimit      = 500
	graphMaxLimit          = 2000
	graphDefaultSimilarity = 0.78
	graphDefaultMaxEdges   = 8
	graphMaxMaxEdges       = 50

	wikilinkWeight = 1.0
	tagWeight      = 1.0
	spaceWeight    = 0.45
	temporalWeight = 0.30
	wikiNodeScore  = 1.0
	spaceHubScore  = 8.0

	edgeWikilink = "wikilink"
	edgeTag      = "tag"
	edgeSpace    = "space"
	edgeSemantic = "semantic"
	edgeTemporal = "temporal"
)

var epoch = time.Unix(0, 0).UTC()

type graphFilter struct {
	providers, models, sources  map[string]bool
	createdAfter, createdBefore *time.Time
}

type memoryNodeRow struct {
	id                             uuid.UUID
	content                        string
	tags                           []string
	createdAt                      *time.Time
	score                          float64
	spaceID, sourceVaultFileID     *uuid.UUID
	lastAccessedAt                 *time.Time
	graphX, graphY, graphZ         *float64
	originKind                     string
	originSourceID, originProvider *string
	originModel                    *string
}

type wikiNodeRow struct {
	id        uuid.UUID
	path      string
	title     *string
	spaceID   *uuid.UUID
	createdAt *time.Time
}

// nodeMeta is NodeMeta, what the chain and hub builders read.
type nodeMeta struct {
	id        uuid.UUID
	spaceID   *uuid.UUID
	createdAt *time.Time
}

// edge is MemoryGraphEdgeDTO before the merge. from < to always holds.
type edge struct {
	from, to   uuid.UUID
	kind       string
	tag        *string
	similarity *float64
	weight     float64
}

func (f *graphFilter) matchesMemory(r *memoryNodeRow) bool {
	if len(f.providers) > 0 && !f.providers[derefOr(r.originProvider)] {
		return false
	}
	if len(f.models) > 0 && !f.models[derefOr(r.originModel)] {
		return false
	}
	if len(f.sources) > 0 && !f.sources[memorystore.ParseSourceKind(r.originKind)] {
		return false
	}
	return f.matchesDate(r.createdAt)
}

func (f *graphFilter) matchesWiki(r *wikiNodeRow) bool {
	if len(f.providers) > 0 || len(f.models) > 0 {
		return false
	}
	if len(f.sources) > 0 && !f.sources[memorystore.SourceVault] {
		return false
	}
	return f.matchesDate(r.createdAt)
}

func (f *graphFilter) matchesDate(date *time.Time) bool {
	if date == nil {
		return f.createdAfter == nil && f.createdBefore == nil
	}
	if f.createdAfter != nil && date.Before(*f.createdAfter) {
		return false
	}
	return f.createdBefore == nil || !date.After(*f.createdBefore)
}

func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// graph serves GET /v1/memory/graph. Unlike its sibling routes Swift does not run this
// response through ModelDisclosurePolicy, so a managed tenant sees origin models here.
func (m *Module) graph(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	limit, ok := wire.QueryInt(c, "limit")
	if !ok {
		limit = graphDefaultLimit
	}
	limit = max(1, min(graphMaxLimit, limit))
	similarity := graphDefaultSimilarity
	if raw, present := wire.QueryParam(c, "similarityThreshold"); present {
		if parsed, ok := parseSwiftDouble(raw); ok {
			similarity = parsed
		}
	}
	similarity = clampSwift(similarity, 0, 1)
	maxEdges, ok := wire.QueryInt(c, "maxEdgesPerNode")
	if !ok {
		maxEdges = graphDefaultMaxEdges
	}
	maxEdges = max(1, min(graphMaxMaxEdges, maxEdges))
	includeWiki := true
	if raw, present := wire.QueryParam(c, "includeWikiPages"); present {
		includeWiki = raw == "true" || raw == "1"
	}
	kinds := parseEdgeKinds(wire.Query(c, "kinds"))
	filter, err := parseGraphFilter(c)
	if err != nil {
		return err
	}
	out, err := m.buildGraph(c.Request().Context(), a.VaultID, limit, similarity, maxEdges, includeWiki, kinds, filter)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

// parseSwiftDouble is Double(String). A value too large for a Double is infinite and
// one too small is zero, not an error, unlike strconv.
func parseSwiftDouble(s string) (float64, bool) {
	if strings.ContainsRune(s, '_') {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// clampSwift is clampDouble with Swift.max and Swift.min's comparisons: NaN lands on hi.
func clampSwift(v, lo, hi float64) float64 {
	inner := v
	if math.IsNaN(v) || hi < v {
		inner = hi
	}
	if inner >= lo {
		return inner
	}
	return lo
}

func parseEdgeKinds(csv string) map[string]bool {
	all := map[string]bool{edgeWikilink: true, edgeTag: true, edgeSpace: true, edgeSemantic: true, edgeTemporal: true}
	if csv == "" {
		return all
	}
	parsed := map[string]bool{}
	for part := range strings.SplitSeq(csv, ",") {
		if name := swiftstr.TrimWhitespaces(part); all[name] {
			parsed[name] = true
		}
	}
	if len(parsed) == 0 {
		return all
	}
	return parsed
}

func csvSet(raw string) map[string]bool {
	out := map[string]bool{}
	for part := range strings.SplitSeq(raw, ",") {
		if part != "" {
			out[part] = true
		}
	}
	return out
}

func parseGraphFilter(c *echo.Context) (*graphFilter, error) {
	f := &graphFilter{providers: csvSet(wire.Query(c, "providers")), models: csvSet(wire.Query(c, "models")), sources: map[string]bool{}}
	rawSources := wire.Query(c, "sources")
	for name := range csvSet(rawSources) {
		// MemorySourceKindDTO(rawValue:): an unknown name is dropped here, not read as
		// legacy, which only the decoder does.
		if memorystore.ParseSourceKind(name) == name {
			f.sources[name] = true
		}
	}
	if rawSources != "" && len(f.sources) == 0 {
		return nil, wire.NewError(http.StatusBadRequest, "sources contains no recognized values")
	}
	var err error
	if f.createdAfter, err = parseGraphDate(c, "createdAfter"); err != nil {
		return nil, err
	}
	if f.createdBefore, err = parseGraphDate(c, "createdBefore"); err != nil {
		return nil, err
	}
	return f, nil
}

func parseGraphDate(c *echo.Context, name string) (*time.Time, error) {
	raw := wire.Query(c, name)
	if raw == "" {
		return nil, nil
	}
	t, ok := parseInternetDate(raw)
	if !ok {
		return nil, wire.NewError(http.StatusBadRequest, name+" must be ISO-8601")
	}
	return &t, nil
}

func (m *Module) buildGraph(ctx context.Context, tenant uuid.UUID, limit int, similarity float64, maxEdges int, includeWiki bool, kinds map[string]bool, filter *graphFilter) (*graphResponse, error) {
	allMemories, err := m.fetchMemoryNodes(ctx, tenant)
	if err != nil {
		return nil, err
	}
	memRows := make([]memoryNodeRow, 0, min(limit, len(allMemories)))
	for i := range allMemories {
		if len(memRows) < limit && filter.matchesMemory(&allMemories[i]) {
			memRows = append(memRows, allMemories[i])
		}
	}
	var wikiRows []wikiNodeRow
	if includeWiki {
		allWiki, err := m.fetchWikiNodes(ctx, tenant)
		if err != nil {
			return nil, err
		}
		for i := range allWiki {
			if len(wikiRows) < limit && filter.matchesWiki(&allWiki[i]) {
				wikiRows = append(wikiRows, allWiki[i])
			}
		}
	}
	now := m.now()
	out := &graphResponse{Nodes: []graphNode{}, Edges: []graphEdge{}}
	if len(memRows) == 0 && len(wikiRows) == 0 {
		out.GeneratedAt = wire.Time{Time: now}
		return out, nil
	}

	memoryIDs := make([]uuid.UUID, len(memRows))
	metas := make([]nodeMeta, 0, len(memRows)+len(wikiRows))
	for i := range memRows {
		memoryIDs[i] = memRows[i].id
		metas = append(metas, nodeMeta{memRows[i].id, memRows[i].spaceID, memRows[i].createdAt})
	}
	wikiIDs := make(map[uuid.UUID]bool, len(wikiRows))
	for i := range wikiRows {
		wikiIDs[wikiRows[i].id] = true
		metas = append(metas, nodeMeta{wikiRows[i].id, wikiRows[i].spaceID, wikiRows[i].createdAt})
	}

	var tagEdges, semanticEdges, wikilinkEdges, temporalEdges, spaceEdges []edge
	if kinds[edgeTag] && len(memoryIDs) > 0 {
		if tagEdges, err = m.computeTagEdges(ctx, tenant, memoryIDs); err != nil {
			return nil, err
		}
	}
	if kinds[edgeSemantic] && len(memoryIDs) > 0 {
		if semanticEdges, err = m.computeSemanticEdges(ctx, tenant, memoryIDs, similarity, maxEdges); err != nil {
			return nil, err
		}
	}
	if kinds[edgeWikilink] {
		wikilinkEdges = lineageEdges(memRows, wikiIDs)
	}
	if kinds[edgeTemporal] {
		temporalEdges = temporalChainEdges(metas)
	}

	var spaceNodes []graphNode
	hubIDs := map[uuid.UUID]bool{}
	if kinds[edgeSpace] {
		names, err := m.fetchSpaceNames(ctx, tenant, memberSpaces(metas))
		if err != nil {
			return nil, err
		}
		// Swift maps a Dictionary here, so its hubs come in a different order on each
		// process. Go sorts them by id.
		hubs := make([]uuid.UUID, 0, len(names))
		for id := range names {
			hubs = append(hubs, id)
			hubIDs[id] = true
		}
		sort.Slice(hubs, func(i, j int) bool { return memorystore.CompareIDs(hubs[i], hubs[j]) < 0 })
		for _, id := range hubs {
			spaceNodes = append(spaceNodes, graphNode{
				ID: wire.UUID{UUID: id}, Title: names[id], Tags: []string{},
				CreatedAt: wire.Time{Time: epoch}, Score: spaceHubScore, Kind: "space",
				SpaceID: wireUUIDPtr(&id),
			})
		}
		spaceEdges = spaceStarEdges(metas, hubIDs)
	}

	merged := mergeAndCap([][]edge{wikilinkEdges, tagEdges, spaceEdges, semanticEdges, temporalEdges}, maxEdges, hubIDs)
	for i := range merged {
		e := &merged[i]
		ge := graphEdge{From: wire.UUID{UUID: e.from}, To: wire.UUID{UUID: e.to}, Kind: e.kind, Tag: e.tag, Weight: wire.Double(e.weight)}
		ge.Similarity = doublePtr(e.similarity)
		out.Edges = append(out.Edges, ge)
	}
	for i := range memRows {
		out.Nodes = append(out.Nodes, memoryNode(&memRows[i], now))
	}
	for i := range wikiRows {
		out.Nodes = append(out.Nodes, wikiNode(&wikiRows[i]))
	}
	out.Nodes = append(out.Nodes, spaceNodes...)
	out.GeneratedAt = wire.Time{Time: m.now()}
	return out, nil
}

func memoryNode(r *memoryNodeRow, now time.Time) graphNode {
	created := epoch
	if r.createdAt != nil {
		created = *r.createdAt
	}
	tags := r.tags
	if tags == nil {
		tags = []string{}
	}
	lastAccessed := r.lastAccessedAt
	if lastAccessed == nil {
		lastAccessed = r.createdAt
	}
	activity := wire.Double(memorystore.GraphActivity(r.score, lastAccessed, now))
	node := graphNode{
		ID: wire.UUID{UUID: r.id}, Title: memorystore.GraphTitle(r.content), Tags: tags,
		CreatedAt: wire.Time{Time: created}, Score: wire.Double(r.score), Kind: "memory",
		SpaceID: wireUUIDPtr(r.spaceID), Activity: &activity,
		Provenance: summaryDTO(memorystore.OriginSummary(r.id, r.originKind, r.originSourceID, r.originProvider, r.originModel, r.createdAt)),
	}
	if r.graphX != nil && r.graphY != nil && r.graphZ != nil {
		node.Position = &position3D{X: wire.Double(*r.graphX), Y: wire.Double(*r.graphY), Z: wire.Double(*r.graphZ)}
	}
	return node
}

func wikiNode(r *wikiNodeRow) graphNode {
	created := epoch
	if r.createdAt != nil {
		created = *r.createdAt
	}
	return graphNode{
		ID: wire.UUID{UUID: r.id}, Title: titleFromWiki(r.title, r.path), Tags: []string{},
		CreatedAt: wire.Time{Time: created}, Score: wikiNodeScore, Kind: "wikiPage",
		SpaceID: wireUUIDPtr(r.spaceID),
	}
}

// titleFromWiki prefers the metadata title, else the file's basename without its
// extension, else the raw path.
func titleFromWiki(title *string, path string) string {
	if title != nil && swiftstr.TrimWhitespaces(*title) != "" {
		return *title
	}
	base := vaultfiles.LastPathComponent(path)
	noExt := base
	if ext := vaultfiles.PathExtension(base); ext != "" {
		noExt = base[:len(base)-len(ext)-1]
	}
	if noExt == "" {
		return path
	}
	return noExt
}

func (m *Module) fetchMemoryNodes(ctx context.Context, tenant uuid.UUID) ([]memoryNodeRow, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id, content, tags, created_at, score, space_id, source_vault_file_id,
		       last_accessed_at, graph_x, graph_y, graph_z,
		       origin_kind, origin_source_id, origin_provider, origin_model
		FROM memories
		WHERE tenant_id = $1
		ORDER BY score DESC, last_accessed_at DESC NULLS LAST, id ASC
		LIMIT $2`, tenant, graphMaxLimit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (memoryNodeRow, error) {
		var r memoryNodeRow
		err := row.Scan(&r.id, &r.content, &r.tags, &r.createdAt, &r.score, &r.spaceID, &r.sourceVaultFileID,
			&r.lastAccessedAt, &r.graphX, &r.graphY, &r.graphZ,
			&r.originKind, &r.originSourceID, &r.originProvider, &r.originModel)
		return r, err
	})
}

// fetchWikiNodes is one node per text-like or image vault file.
func (m *Module) fetchWikiNodes(ctx context.Context, tenant uuid.UUID) ([]wikiNodeRow, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id, path, metadata->>'title' AS title, space_id, created_at
		FROM vault_files
		WHERE tenant_id = $1
		  AND (content_type LIKE 'text/%' OR content_type LIKE 'image/%')
		ORDER BY created_at DESC NULLS LAST, id ASC
		LIMIT $2`, tenant, graphMaxLimit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (wikiNodeRow, error) {
		var r wikiNodeRow
		err := row.Scan(&r.id, &r.path, &r.title, &r.spaceID, &r.createdAt)
		return r, err
	})
}

func memberSpaces(metas []nodeMeta) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	out := []uuid.UUID{}
	for i := range metas {
		if s := metas[i].spaceID; s != nil && !seen[*s] {
			seen[*s] = true
			out = append(out, *s)
		}
	}
	return out
}

func (m *Module) fetchSpaceNames(ctx context.Context, tenant uuid.UUID, spaces []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(spaces) == 0 {
		return out, nil
	}
	rows, err := m.pool.Query(ctx, `SELECT id, name FROM spaces WHERE tenant_id = $1 AND id = ANY($2::uuid[])`, tenant, spaces)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   uuid.UUID
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// computeTagEdges is one edge per pair sharing a tag; the smallest shared tag names it.
func (m *Module) computeTagEdges(ctx context.Context, tenant uuid.UUID, memoryIDs []uuid.UUID) ([]edge, error) {
	rows, err := m.pool.Query(ctx, `
		WITH node_tags AS (
			SELECT id, unnest(tags) AS tag
			FROM memories
			WHERE tenant_id = $1 AND id = ANY($2::uuid[])
		)
		SELECT a.id AS from_id, b.id AS to_id, MIN(a.tag) AS tag
		FROM node_tags a
		JOIN node_tags b ON a.tag = b.tag AND a.id < b.id
		GROUP BY a.id, b.id`, tenant, memoryIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (edge, error) {
		e := edge{kind: edgeTag, weight: tagWeight}
		var tag string
		err := row.Scan(&e.from, &e.to, &tag)
		e.tag = &tag
		return e, err
	})
}

// computeSemanticEdges is each node's nearest neighbours, collapsed to undirected
// edges keeping the higher similarity.
func (m *Module) computeSemanticEdges(ctx context.Context, tenant uuid.UUID, memoryIDs []uuid.UUID, similarity float64, maxEdges int) ([]edge, error) {
	rows, err := m.pool.Query(ctx, `
		WITH neighbors AS (
			SELECT a.id AS from_id,
			       b.id AS to_id,
			       1 - (a.embedding <=> b.embedding) AS similarity,
			       ROW_NUMBER() OVER (PARTITION BY a.id ORDER BY a.embedding <=> b.embedding) AS rn
			FROM memories a
			JOIN memories b ON a.tenant_id = b.tenant_id AND a.id <> b.id
			WHERE a.tenant_id = $1
			  AND a.id = ANY($2::uuid[])
			  AND b.id = ANY($2::uuid[])
			  AND a.embedding IS NOT NULL
			  AND b.embedding IS NOT NULL
		)
		SELECT LEAST(from_id, to_id) AS from_id,
		       GREATEST(from_id, to_id) AS to_id,
		       MAX(similarity) AS similarity
		FROM neighbors
		WHERE rn <= $3 AND similarity >= $4
		GROUP BY LEAST(from_id, to_id), GREATEST(from_id, to_id)`, tenant, memoryIDs, int64(maxEdges), similarity)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (edge, error) {
		e := edge{kind: edgeSemantic}
		var s float64
		err := row.Scan(&e.from, &e.to, &s)
		e.similarity, e.weight = &s, s
		return e, err
	})
}

// ordered is ordered(_:_:), the undirected invariant from < to.
func ordered(a, b uuid.UUID) (uuid.UUID, uuid.UUID) {
	if memorystore.CompareIDs(a, b) < 0 {
		return a, b
	}
	return b, a
}

// lineageEdges links a memory to its source page, only when the page is a node.
func lineageEdges(memRows []memoryNodeRow, wikiIDs map[uuid.UUID]bool) []edge {
	var out []edge
	for i := range memRows {
		source := memRows[i].sourceVaultFileID
		if source == nil || !wikiIDs[*source] {
			continue
		}
		from, to := ordered(memRows[i].id, *source)
		out = append(out, edge{from: from, to: to, kind: edgeWikilink, weight: wikilinkWeight})
	}
	return out
}

// spaceStarEdges is one edge from each member to its Space hub.
func spaceStarEdges(metas []nodeMeta, hubIDs map[uuid.UUID]bool) []edge {
	var out []edge
	for i := range metas {
		s := metas[i].spaceID
		if s == nil || !hubIDs[*s] {
			continue
		}
		from, to := ordered(metas[i].id, *s)
		out = append(out, edge{from: from, to: to, kind: edgeSpace, weight: spaceWeight})
	}
	return out
}

// temporalChainEdges is chainEdges over day buckets: within a day, nodes newest first
// are linked in a line, which gives O(n) edges rather than a clique. A node with no
// created_at has no bucket.
func temporalChainEdges(metas []nodeMeta) []edge {
	buckets := map[int64][]nodeMeta{}
	for i := range metas {
		if at := metas[i].createdAt; at != nil {
			// Int(timeIntervalSince1970 / 86400) truncates toward zero.
			day := int64(float64(at.UnixMicro()) / 1e6 / 86400)
			buckets[day] = append(buckets[day], metas[i])
		}
	}
	return chainEdges(buckets, edgeTemporal, temporalWeight)
}

func chainEdges(buckets map[int64][]nodeMeta, kind string, weight float64) []edge {
	var out []edge
	for _, members := range buckets {
		if len(members) < 2 {
			continue
		}
		sort.SliceStable(members, func(i, j int) bool {
			a, b := members[i], members[j]
			da, db := epoch, epoch
			if a.createdAt != nil {
				da = *a.createdAt
			}
			if b.createdAt != nil {
				db = *b.createdAt
			}
			if !da.Equal(db) {
				return da.After(db)
			}
			return memorystore.CompareIDs(a.id, b.id) < 0
		})
		for i := 0; i+1 < len(members); i++ {
			from, to := ordered(members[i].id, members[i+1].id)
			out = append(out, edge{from: from, to: to, kind: kind, weight: weight})
		}
	}
	return out
}

// mergeAndCap dedupes by undirected pair with precedence, the first group an edge
// appears in winning, then prunes greedily so each node keeps at most maxEdges. Hubs
// in uncapped connect to all their members; the cap still applies to the other end.
func mergeAndCap(groups [][]edge, maxEdges int, uncapped map[uuid.UUID]bool) []edge {
	type pair struct{ from, to uuid.UUID }
	type ranked struct {
		rank int
		edge edge
	}
	byPair := map[pair]ranked{}
	for rank, group := range groups {
		for _, e := range group {
			key := pair{e.from, e.to}
			if _, taken := byPair[key]; !taken {
				byPair[key] = ranked{rank, e}
			}
		}
	}
	all := make([]ranked, 0, len(byPair))
	for _, r := range byPair {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.edge.weight != b.edge.weight {
			return a.edge.weight > b.edge.weight
		}
		if c := memorystore.CompareIDs(a.edge.from, b.edge.from); c != 0 {
			return c < 0
		}
		return memorystore.CompareIDs(a.edge.to, b.edge.to) < 0
	})
	degree := map[uuid.UUID]int{}
	kept := make([]edge, 0, len(all))
	for _, r := range all {
		e := r.edge
		if (!uncapped[e.from] && degree[e.from] >= maxEdges) || (!uncapped[e.to] && degree[e.to] >= maxEdges) {
			continue
		}
		kept = append(kept, e)
		if !uncapped[e.from] {
			degree[e.from]++
		}
		if !uncapped[e.to] {
			degree[e.to]++
		}
	}
	return kept
}
