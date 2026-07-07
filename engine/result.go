package engine

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Result holds the columns and rows produced by a resolved query, plus any
// non-fatal warnings gathered while fetching (e.g. a connector truncated at a
// cap, so the result may be incomplete).
type Result struct {
	Columns  []string
	Rows     [][]any
	Warnings []string
}

// Project returns a copy of the result narrowed to cols, in the given order.
// An empty cols list returns the result unchanged (all columns). It errors if a
// requested column is not present, listing the columns that are available, so a
// stale saved-query projection fails loudly rather than silently dropping data.
func (r *Result) Project(cols []string) (*Result, error) {
	if len(cols) == 0 {
		return r, nil
	}

	idx := make(map[string]int, len(r.Columns))
	for i, c := range r.Columns {
		idx[c] = i
	}

	picks := make([]int, len(cols))
	for i, c := range cols {
		j, ok := idx[c]
		if !ok {
			return nil, fmt.Errorf("column %q not in result; available: %s", c, strings.Join(r.Columns, ", "))
		}
		picks[i] = j
	}

	rows := make([][]any, len(r.Rows))
	for i, row := range r.Rows {
		out := make([]any, len(picks))
		for k, j := range picks {
			if j < len(row) {
				out[k] = row[j]
			}
		}
		rows[i] = out
	}

	projected := make([]string, len(cols))
	copy(projected, cols)
	return &Result{Columns: projected, Rows: rows, Warnings: r.Warnings}, nil
}

// Write renders the result to w in the requested format: "table", "json", or "csv".
func (r *Result) Write(w io.Writer, format string) error {
	switch format {
	case "", "table":
		return r.writeTable(w)
	case "json":
		return r.writeJSON(w)
	case "csv":
		return r.writeCSV(w)
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
}

func (r *Result) writeTable(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(r.Columns, "\t")); err != nil {
		return err
	}
	for _, row := range r.Rows {
		if _, err := fmt.Fprintln(tw, strings.Join(cells(row), "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// cells renders a row's values for text output; SQL NULL becomes an empty cell
// rather than Go's "<nil>".
func cells(row []any) []string {
	out := make([]string, len(row))
	for i, v := range row {
		if v == nil {
			continue // leave the cell empty
		}
		out[i] = fmt.Sprintf("%v", v)
	}
	return out
}

func (r *Result) writeJSON(w io.Writer) error {
	records := make([]map[string]any, 0, len(r.Rows))
	for _, row := range r.Rows {
		rec := make(map[string]any, len(r.Columns))
		for i, col := range r.Columns {
			if i < len(row) {
				rec[col] = row[i]
			}
		}
		records = append(records, rec)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(records)
}

func (r *Result) writeCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(r.Columns); err != nil {
		return err
	}
	for _, row := range r.Rows {
		if err := cw.Write(cells(row)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
