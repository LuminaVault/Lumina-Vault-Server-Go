package notionimport

import (
	"bytes"
	"encoding/csv"
	"errors"
	"lumina/internal/vaultprops"
	"regexp"
	"strconv"
	"strings"
)

// Table is a database's CSV: the header and the rows, each as long as the header.
type Table struct {
	Columns []string
	Rows    [][]string
}

// ParseCSV reads a Notion database CSV. The BOM Notion writes is dropped, and
// short rows are padded.
func ParseCSV(data []byte) (Table, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil {
		return Table{}, err
	}
	if len(records) == 0 {
		return Table{}, errors.New("the CSV has no header")
	}
	t := Table{Columns: records[0], Rows: make([][]string, 0, len(records)-1)}
	for at := range t.Columns {
		t.Columns[at] = strings.TrimSpace(t.Columns[at])
	}
	for _, rec := range records[1:] {
		row := make([]string, len(t.Columns))
		copy(row, rec)
		t.Rows = append(t.Rows, row)
	}
	return t, nil
}

// Column returns one column's cells.
func (t Table) Column(at int) []string {
	out := make([]string, len(t.Rows))
	for n, row := range t.Rows {
		out[n] = row[at]
	}
	return out
}

// The bounds of spec D6's list rules.
const (
	maxMultiSelectItems = 50
	maxSelectValues     = 20
	// maxListItemLen keeps prose with a comma in it from reading as a list.
	maxListItemLen = 100
)

var numberCell = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][+-]?\d+)?$`)

// Inference is InferColumn's result.
type Inference struct {
	Type vaultprops.Type
	// Ranges is true when a date column held "A → B" cells; only the start
	// is kept.
	Ranges bool
	// Options are a select's or multiselect's distinct values, in first-seen
	// order.
	Options []string
}

// InferColumn picks a column's type from all of its cells (spec D6). The
// rules run in order, and one applies only when every non-empty cell fits:
// Yes/No → checkbox; numbers → number; Notion dates → date, or datetime when
// any has a time; http(s) URLs → url; ", "-separated lists with at most 50
// distinct items → multiselect; at most 20 distinct values with one repeated
// → select; otherwise text. A column with no values is text.
func InferColumn(cells []string) Inference {
	var values []string
	for _, c := range cells {
		if c = strings.TrimSpace(c); c != "" {
			values = append(values, c)
		}
	}
	if len(values) == 0 {
		return Inference{Type: vaultprops.Text}
	}
	if every(values, func(v string) bool { return v == "Yes" || v == "No" }) {
		return Inference{Type: vaultprops.Checkbox}
	}
	if every(values, numberCell.MatchString) {
		return Inference{Type: vaultprops.Number}
	}
	if out, ok := inferDates(values); ok {
		return out
	}
	if every(values, isURL) {
		return Inference{Type: vaultprops.URL}
	}
	if items, ok := listItems(values); ok {
		return Inference{Type: vaultprops.MultiSelect, Options: items}
	}
	distinct, repeated := distinctValues(values)
	if len(distinct) <= maxSelectValues && repeated && every(distinct, func(v string) bool { return !strings.Contains(v, "\n") }) {
		return Inference{Type: vaultprops.Select, Options: distinct}
	}
	return Inference{Type: vaultprops.Text}
}

func every(values []string, fits func(string) bool) bool {
	for _, v := range values {
		if !fits(v) {
			return false
		}
	}
	return true
}

func inferDates(values []string) (Inference, bool) {
	out := Inference{Type: vaultprops.Date}
	for _, v := range values {
		d, ok := ParseNotionDate(v)
		if !ok {
			return Inference{}, false
		}
		if d.HasTime {
			out.Type = vaultprops.DateTime
		}
		out.Ranges = out.Ranges || d.Range
	}
	return out, true
}

func isURL(s string) bool {
	return (strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) && !strings.ContainsAny(s, " \n")
}

// SplitList splits a multi-select cell on ", ".
func SplitList(cell string) []string {
	var out []string
	for _, item := range strings.Split(cell, ", ") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// listItems is the multiselect rule: at least one cell is a list, no cell is
// a date, items are short and on one line, and there are at most 50 distinct ones.
func listItems(values []string) ([]string, bool) {
	var all []string
	anyList := false
	for _, v := range values {
		if strings.Contains(v, "\n") {
			return nil, false
		}
		// "November 18, 2026" is a date among other text, not a two-item list.
		if _, isDate := ParseNotionDate(v); isDate {
			return nil, false
		}
		items := SplitList(v)
		anyList = anyList || len(items) > 1
		for _, item := range items {
			if len(item) > maxListItemLen {
				return nil, false
			}
		}
		all = append(all, items...)
	}
	distinct, _ := distinctValues(all)
	if !anyList || len(distinct) > maxMultiSelectItems {
		return nil, false
	}
	return distinct, true
}

func distinctValues(values []string) (distinct []string, repeated bool) {
	seen := map[string]bool{}
	for _, v := range values {
		if seen[v] {
			repeated = true
			continue
		}
		seen[v] = true
		distinct = append(distinct, v)
	}
	return distinct, repeated
}

// Convert turns one cell into the frontmatter value of type t: a bool, a
// float64, an ISO date string, a []any of strings, or the text itself. ok is
// false when the cell doesn't fit t; the caller then writes it as text (spec
// D6, registry conflicts). An empty cell is nil, true: no property.
func Convert(t vaultprops.Type, cell string) (value any, ok bool) {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return nil, true
	}
	switch t {
	case vaultprops.Checkbox:
		if cell == "Yes" || cell == "No" {
			return cell == "Yes", true
		}
		return nil, false
	case vaultprops.Number:
		if !numberCell.MatchString(cell) {
			return nil, false
		}
		f, err := strconv.ParseFloat(cell, 64)
		return f, err == nil
	case vaultprops.Date, vaultprops.DateTime:
		d, ok := ParseNotionDate(cell)
		if !ok {
			return nil, false
		}
		if t == vaultprops.Date {
			return d.Time.Format("2006-01-02"), true
		}
		return d.ISO(), true
	case vaultprops.URL:
		return cell, isURL(cell)
	case vaultprops.MultiSelect:
		items := SplitList(cell)
		out := make([]any, len(items))
		for n, item := range items {
			out[n] = item
		}
		return out, true
	}
	return cell, true
}
