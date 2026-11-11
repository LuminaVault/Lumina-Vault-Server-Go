package brain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"lumina/internal/modules/brain/db"
	"lumina/internal/wire"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxLabelRunes = 80

// item is a note or a memory, the nodes that count against `limit`.
type item struct {
	ID, VaultID uuid.UUID
	SpaceID     *uuid.UUID
	Kind        string
	Label       string
	CreatedAt   *time.Time
	Pos         *[3]float64
	// Tags and CreatedBy feed the tag and author hubs; memories carry tags, notes do not.
	Tags      []string
	CreatedBy *uuid.UUID
}

// relation is an edge candidate between two node ids.
type relation struct {
	From, To string
	Kind     string
	Weight   *float64
}

// hub is a node shared by several items (tag, author, entity, space).
type hub struct {
	ID, Kind, Label string
	VaultID         *uuid.UUID
	Refs            []BrainGraphRef
}

// minHubMembers is how many items must point at a hub for it to be returned.
const minHubMembers = 2

// defaultLinks are the edge kinds returned when `links` names none that is valid.
var defaultLinks = []string{"link", "source", "tag", "author", "entity", "space"}

var allLinks = map[string]bool{"link": true, "source": true, "tag": true, "author": true, "entity": true, "space": true, "similar": true}

// parseLinks reads the `links` CSV. Unknown values are ignored; nothing valid
// means the defaults (everything except similar).
func parseLinks(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if k := strings.TrimSpace(part); allLinks[k] {
			out[k] = true
		}
	}
	if len(out) == 0 {
		for _, k := range defaultLinks {
			out[k] = true
		}
	}
	return out
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// memoryLabel is the first non-empty line of the content, at most 80 runes.
func memoryLabel(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateRunes(line, maxLabelRunes)
		}
	}
	return "Untitled"
}

// noteLabel is the file name of a vault path without its .md extension.
func noteLabel(p string) string {
	base := strings.TrimSuffix(path.Base(p), ".md")
	if base == "" || base == "." || base == "/" {
		return "Untitled"
	}
	return truncateRunes(base, maxLabelRunes)
}

// itemFromRow maps an Items row to an item. The memory body is already cut to 400 characters by SQL.
func itemFromRow(r db.ItemsRow) item {
	it := item{ID: r.ID, VaultID: r.VaultID, SpaceID: r.SpaceID, Kind: r.Kind, CreatedAt: r.CreatedAt, Tags: r.Tags, CreatedBy: r.CreatedByUserID}
	if r.Kind == "memory" {
		it.Label = memoryLabel(r.Body)
		if r.GraphX != nil && r.GraphY != nil && r.GraphZ != nil {
			it.Pos = &[3]float64{*r.GraphX, *r.GraphY, *r.GraphZ}
		}
	} else {
		it.Label = noteLabel(r.Body)
	}
	return it
}

// normaliseTag trims and lowercases a tag. An empty result means the tag is skipped.
func normaliseTag(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// tagHubs makes one hub per normalised tag and one edge from each tagged item.
func tagHubs(items []item) ([]hub, []relation) {
	var hubs []hub
	var rels []relation
	seen := map[string]bool{}
	for _, it := range items {
		for _, raw := range it.Tags {
			tag := normaliseTag(raw)
			if tag == "" {
				continue
			}
			id := "tag:" + tag
			if !seen[id] {
				seen[id] = true
				hubs = append(hubs, hub{ID: id, Kind: "tag", Label: tag})
			}
			rels = append(rels, relation{From: it.ID.String(), To: id, Kind: "tag"})
		}
	}
	return hubs, rels
}

// spaceHubs makes a hub per Space row. Items whose Space is not in rows get no edge.
func spaceHubs(items []item, spaces []db.SpacesRow) ([]hub, []relation) {
	hubs := make([]hub, 0, len(spaces))
	known := make(map[uuid.UUID]bool, len(spaces))
	for _, s := range spaces {
		known[s.ID] = true
		vault := s.TenantID
		hubs = append(hubs, hub{ID: "space:" + s.ID.String(), Kind: "space", Label: s.Name, VaultID: &vault})
	}
	var rels []relation
	for _, it := range items {
		if it.SpaceID != nil && known[*it.SpaceID] {
			rels = append(rels, relation{From: it.ID.String(), To: "space:" + it.SpaceID.String(), Kind: "space"})
		}
	}
	return hubs, rels
}

// userAuthorHubs makes a hub per user found in users. An item whose creator is not in
// users gets no edge.
func userAuthorHubs(items []item, users []db.AuthorsRow) ([]hub, []relation) {
	hubs := make([]hub, 0, len(users))
	known := make(map[uuid.UUID]bool, len(users))
	for _, u := range users {
		known[u.ID] = true
		hubs = append(hubs, hub{ID: "author:user:" + u.ID.String(), Kind: "author", Label: u.Username})
	}
	var rels []relation
	for _, it := range items {
		if it.CreatedBy != nil && known[*it.CreatedBy] {
			rels = append(rels, relation{From: it.ID.String(), To: "author:user:" + it.CreatedBy.String(), Kind: "author"})
		}
	}
	return hubs, rels
}

// modelAuthorHubs makes a hub per provider/model pair. A null provider is the empty string.
func modelAuthorHubs(rows []db.ModelAuthorsRow) ([]hub, []relation) {
	var hubs []hub
	var rels []relation
	seen := map[string]bool{}
	for _, r := range rows {
		provider := ""
		if r.Provider != nil {
			provider = *r.Provider
		}
		id := "author:model:" + provider + "/" + r.Model
		if !seen[id] {
			seen[id] = true
			hubs = append(hubs, hub{ID: id, Kind: "author", Label: r.Model})
		}
		rels = append(rels, relation{From: r.MemoryID.String(), To: id, Kind: "author"})
	}
	return hubs, rels
}

// entityHubs keys hubs by canonical_key across vaults. Per vault the highest-confidence row
// stands for the entity; the label comes from the best row overall (ties go to the smallest id)
// and refs hold one entry per vault, sorted by (vaultID, id).
func entityHubs(rows []db.EntitiesRow) ([]hub, []relation) {
	better := func(a, b db.EntitiesRow) bool {
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		return a.ID.String() < b.ID.String()
	}
	type key struct {
		canonical string
		vault     uuid.UUID
	}
	perVault := map[key]db.EntitiesRow{}
	var rels []relation
	for _, r := range rows {
		k := key{r.CanonicalKey, r.TenantID}
		if cur, ok := perVault[k]; !ok || better(r, cur) {
			perVault[k] = r
		}
		rels = append(rels, relation{From: r.MemoryID.String(), To: "entity:" + r.CanonicalKey, Kind: "entity"})
	}
	byKey := map[string][]db.EntitiesRow{}
	for k, r := range perVault {
		byKey[k.canonical] = append(byKey[k.canonical], r)
	}
	hubs := make([]hub, 0, len(byKey))
	for canonical, rs := range byKey {
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].TenantID != rs[j].TenantID {
				return rs[i].TenantID.String() < rs[j].TenantID.String()
			}
			return rs[i].ID.String() < rs[j].ID.String()
		})
		best := rs[0]
		refs := make([]BrainGraphRef, 0, len(rs))
		for _, r := range rs {
			if better(r, best) {
				best = r
			}
			refs = append(refs, BrainGraphRef{VaultID: wire.UUID{UUID: r.TenantID}, Id: wire.UUID{UUID: r.ID}})
		}
		hubs = append(hubs, hub{ID: "entity:" + canonical, Kind: "entity", Label: best.Label, Refs: refs})
	}
	return hubs, rels
}

func undirected(kind string) bool { return kind == "link" || kind == "similar" }

// assemble turns items, hubs and relations into the wire nodes and edges. Edges only ever
// join nodes that are returned.
func assemble(items []item, hubs []hub, rels []relation) ([]BrainGraphNode, []BrainGraphEdge) {
	itemIDs := make(map[string]bool, len(items))
	for _, it := range items {
		itemIDs[it.ID.String()] = true
	}
	hubByID := make(map[string]hub, len(hubs))
	for _, h := range hubs {
		hubByID[h.ID] = h
	}
	present := func(id string) bool { _, isHub := hubByID[id]; return itemIDs[id] || isHub }

	// Drop self-loops and edges with a missing endpoint, then count hub members.
	valid := make([]relation, 0, len(rels))
	members := map[string]map[string]bool{}
	for _, r := range rels {
		if r.From == r.To || !present(r.From) || !present(r.To) {
			continue
		}
		valid = append(valid, r)
		for _, end := range [2]string{r.To, r.From} {
			if _, isHub := hubByID[end]; isHub {
				if members[end] == nil {
					members[end] = map[string]bool{}
				}
				members[end][r.From+"|"+r.To] = true
			}
		}
	}
	keep := map[string]bool{}
	for id, m := range members {
		if len(m) >= minHubMembers {
			keep[id] = true
		}
	}

	// Sorted first so that the direction kept for an unordered pair does not depend on input order.
	sort.SliceStable(valid, func(i, j int) bool {
		a, b := valid[i], valid[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	seen := map[[3]string]bool{}
	edges := make([]BrainGraphEdge, 0, len(valid))
	weight := map[string]int{}
	for _, r := range valid {
		if _, isHub := hubByID[r.To]; isHub && !keep[r.To] {
			continue
		}
		if _, isHub := hubByID[r.From]; isHub && !keep[r.From] {
			continue
		}
		// Keep the direction of the first relation seen; only the dedupe key is unordered.
		from, to := r.From, r.To
		ka, kb := from, to
		if undirected(r.Kind) && ka > kb {
			ka, kb = kb, ka
		}
		key := [3]string{r.Kind, ka, kb}
		if seen[key] {
			continue
		}
		seen[key] = true
		e := BrainGraphEdge{From: from, To: to, Kind: BrainGraphEdgeKind(r.Kind)}
		if r.Weight != nil {
			w := wire.Float64(*r.Weight)
			e.Weight = &w
		}
		edges = append(edges, e)
		weight[from]++
		weight[to]++
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})

	nodes := make([]BrainGraphNode, 0, len(items)+len(keep))
	for _, it := range items {
		id := it.ID.String()
		n := BrainGraphNode{Id: id, Kind: BrainGraphNodeKind(it.Kind), Label: it.Label, Weight: weight[id]}
		v := wire.UUID{UUID: it.VaultID}
		n.VaultID = &v
		if it.SpaceID != nil {
			s := wire.UUID{UUID: *it.SpaceID}
			n.SpaceID = &s
		}
		if it.CreatedAt != nil {
			c := wire.Time{Time: *it.CreatedAt}
			n.CreatedAt = &c
		}
		if it.Pos != nil {
			n.Pos = &BrainGraphPosition{X: wire.Float64(it.Pos[0]), Y: wire.Float64(it.Pos[1]), Z: wire.Float64(it.Pos[2])}
		}
		nodes = append(nodes, n)
	}
	for _, h := range hubs {
		if !keep[h.ID] {
			continue
		}
		n := BrainGraphNode{Id: h.ID, Kind: BrainGraphNodeKind(h.Kind), Label: h.Label, Weight: weight[h.ID]}
		if h.VaultID != nil {
			v := wire.UUID{UUID: *h.VaultID}
			n.VaultID = &v
		}
		if len(h.Refs) > 0 {
			refs := h.Refs
			n.Refs = &refs
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		return nodes[i].Id < nodes[j].Id
	})
	return nodes, edges
}

// neighbourhood keeps the nodes within depth hops of focus, treating edges as undirected, and
// the edges between kept nodes. weight is recomputed for the kept subgraph. The hub minimum is
// not re-applied: a local graph still shows the hub its focus belongs to. The bool is false
// when focus is not a node. Input order (kind, id) and (kind, from, to) is preserved.
func neighbourhood(nodes []BrainGraphNode, edges []BrainGraphEdge, focus string, depth int) ([]BrainGraphNode, []BrainGraphEdge, bool) {
	exists := false
	for _, n := range nodes {
		if n.Id == focus {
			exists = true
			break
		}
	}
	if !exists {
		return nil, nil, false
	}
	adj := map[string][]string{}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
		adj[e.To] = append(adj[e.To], e.From)
	}
	kept := map[string]bool{focus: true}
	frontier := []string{focus}
	for range depth {
		var next []string
		for _, id := range frontier {
			for _, nb := range adj[id] {
				if !kept[nb] {
					kept[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	outEdges := make([]BrainGraphEdge, 0)
	weight := map[string]int{}
	for _, e := range edges {
		if kept[e.From] && kept[e.To] {
			outEdges = append(outEdges, e)
			weight[e.From]++
			weight[e.To]++
		}
	}
	outNodes := make([]BrainGraphNode, 0, len(kept))
	for _, n := range nodes {
		if kept[n.Id] {
			n.Weight = weight[n.Id]
			outNodes = append(outNodes, n)
		}
	}
	return outNodes, outEdges, true
}

// etag is a strong validator: the quoted hex SHA-256 of the response JSON with generatedAt
// zeroed, so the tag changes only when the graph does. A response that cannot be marshalled
// has no tag; the handler's own marshal then fails the request.
func etag(resp BrainGraphResponse) string {
	resp.GeneratedAt = wire.Time{}
	b, err := json.Marshal(resp)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
