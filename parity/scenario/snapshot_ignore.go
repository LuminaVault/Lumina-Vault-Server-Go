package scenario

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// snapshotIgnorePrefix marks an ignore entry that applies to table snapshots
// rather than the response body. These cover behaviour Go changed on purpose
// after the Swift server was archived, where the Swift image is still the
// reference for everything else:
//
//	snapshot:<table>                    the table is not snapshotted at all
//	snapshot:<table>.<column>           the column is removed from every row
//	snapshot:<table>.<column>.<key>     the key is removed from the column's JSON
//	                                    object; an object left empty reads as null
//	snapshot:<table>[<column>~<regex>]  rows whose column (as text) matches are
//	                                    left out, for rows only Go writes
//
// Removal happens before rows are ordered and their UUIDs numbered, so rows
// that exist on one side only cannot shift the placeholders of the rest.
const snapshotIgnorePrefix = "snapshot:"

// snapshotIgnore is the parsed snapshot part of a step's ignore list, by table.
// In paths, a nil slice for a table means the whole table is ignored.
type snapshotIgnore struct {
	paths map[string][][]string
	rows  map[string][]rowFilter
}

type rowFilter struct {
	column string
	re     *regexp.Regexp
}

var rowFilterRe = regexp.MustCompile(`^([a-z0-9_]+)\[([a-z0-9_]+)~(.+)\]$`)

// splitIgnore separates the snapshot entries from the response-body paths.
func splitIgnore(ignore []string) (body []string, snaps snapshotIgnore, err error) {
	snaps = snapshotIgnore{paths: map[string][][]string{}, rows: map[string][]rowFilter{}}
	for _, entry := range ignore {
		rest, ok := strings.CutPrefix(entry, snapshotIgnorePrefix)
		if !ok {
			body = append(body, entry)
			continue
		}
		if m := rowFilterRe.FindStringSubmatch(rest); m != nil {
			re, err := regexp.Compile(m[3])
			if err != nil {
				return nil, snaps, fmt.Errorf("ignore %q: %w", entry, err)
			}
			snaps.rows[m[1]] = append(snaps.rows[m[1]], rowFilter{column: m[2], re: re})
			continue
		}
		parts := strings.Split(rest, ".")
		table := parts[0]
		if len(parts) == 1 {
			snaps.paths[table] = nil
			continue
		}
		if cols, seen := snaps.paths[table]; seen && cols == nil {
			continue // already ignored whole
		}
		snaps.paths[table] = append(snaps.paths[table], parts[1:])
	}
	return body, snaps, nil
}

// skips reports whether the whole table is ignored.
func (s snapshotIgnore) skips(table string) bool {
	cols, ok := s.paths[table]
	return ok && cols == nil
}

// strip drops the ignored rows of a table and removes the ignored columns and
// keys from the rest.
func (s snapshotIgnore) strip(table string, rows []json.RawMessage) ([]json.RawMessage, error) {
	paths, filters := s.paths[table], s.rows[table]
	if len(paths) == 0 && len(filters) == 0 {
		return rows, nil
	}
	out := rows[:0:0]
	for _, row := range rows {
		// Numbers stay exact: the row is marshalled again below.
		dec := json.NewDecoder(bytes.NewReader(row))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return nil, err
		}
		if dropped(m, filters) {
			continue
		}
		for _, p := range paths {
			removePath(m, p)
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func dropped(row map[string]any, filters []rowFilter) bool {
	for _, f := range filters {
		v, ok := row[f.column]
		if !ok || v == nil {
			continue
		}
		if f.re.MatchString(fmt.Sprint(v)) {
			return true
		}
	}
	return false
}

// removePath deletes path from m. A JSON object stored in a text or jsonb
// column arrives as a map; one emptied by the removal becomes null, which is
// how a row with no such object reads.
func removePath(m map[string]any, path []string) {
	if len(path) == 1 {
		delete(m, path[0])
		return
	}
	child, ok := m[path[0]].(map[string]any)
	if !ok {
		return
	}
	removePath(child, path[1:])
	if len(child) == 0 {
		m[path[0]] = nil
	}
}
