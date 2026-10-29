package scenario

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Difference is one mismatch between the Swift and Go transcripts.
type Difference struct {
	Scenario string `json:"scenario"`
	Step     string `json:"step"`
	Field    string `json:"field"` // "status", "content-type", "$.body.path" or "snapshot:<table>$.path"
	Swift    string `json:"swift"`
	Go       string `json:"go"`
}

// Diff compares two transcripts of the same scenario step by step.
func Diff(swift, goT Transcript) []Difference {
	var out []Difference
	add := func(step, field string, a, b any) {
		out = append(out, Difference{Scenario: swift.Scenario, Step: step, Field: field, Swift: show(a), Go: show(b)})
	}
	if len(swift.Steps) != len(goT.Steps) {
		add("", "steps", len(swift.Steps), len(goT.Steps))
	}
	for i := range min(len(swift.Steps), len(goT.Steps)) {
		a, b := swift.Steps[i], goT.Steps[i]
		if a.Status != b.Status {
			add(a.Name, "status", a.Status, b.Status)
		}
		if a.ContentType != b.ContentType {
			add(a.Name, "content-type", a.ContentType, b.ContentType)
		}
		walk("$", a.Body, b.Body, func(path string, x, y any) { add(a.Name, path, x, y) })
		tables := map[string]bool{}
		for t := range a.Snapshots {
			tables[t] = true
		}
		for t := range b.Snapshots {
			tables[t] = true
		}
		for _, t := range sortedKeys(tables) {
			walk("snapshot:"+t+"$", a.Snapshots[t], b.Snapshots[t], func(path string, x, y any) { add(a.Name, path, x, y) })
		}
	}
	return out
}

// walk reports each leaf-level mismatch with its JSON path.
func walk(path string, a, b any, report func(path string, a, b any)) {
	am, aIsMap := a.(map[string]any)
	bm, bIsMap := b.(map[string]any)
	if aIsMap && bIsMap {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for _, k := range sortedKeys(keys) {
			av, aok := am[k]
			bv, bok := bm[k]
			switch {
			case !aok:
				report(path+"."+k, "<absent>", bv)
			case !bok:
				report(path+"."+k, av, "<absent>")
			default:
				walk(path+"."+k, av, bv, report)
			}
		}
		return
	}
	as, aIsList := a.([]any)
	bs, bIsList := b.([]any)
	if aIsList && bIsList {
		if len(as) != len(bs) {
			report(path+".length", len(as), len(bs))
		}
		for i := range min(len(as), len(bs)) {
			walk(fmt.Sprintf("%s[%d]", path, i), as[i], bs[i], report)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		report(path, a, b)
	}
}

func show(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
