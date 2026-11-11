package brain

import (
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
	it := item{ID: r.ID, VaultID: r.VaultID, SpaceID: r.SpaceID, Kind: r.Kind, CreatedAt: r.CreatedAt}
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
