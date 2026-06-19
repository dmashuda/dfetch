package engine

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Result holds the columns and rows produced by a resolved query.
type Result struct {
	Columns []string
	Rows    [][]any
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
	if _, err := fmt.Fprintln(w, strings.Join(r.Columns, "\t")); err != nil {
		return err
	}
	for _, row := range r.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = fmt.Sprintf("%v", v)
		}
		if _, err := fmt.Fprintln(w, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return nil
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
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = fmt.Sprintf("%v", v)
		}
		if err := cw.Write(cells); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
