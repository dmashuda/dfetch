package files

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dmashuda/dfetch/source"
)

// newCSVReader builds the reader both describe and scan use. Records may be
// ragged (FieldsPerRecord -1): scan pads/truncates each record to the declared
// columns, so a stray short row degrades to NULLs instead of failing the query.
func newCSVReader(r io.Reader, comma rune) *csv.Reader {
	cr := csv.NewReader(stripBOM(r))
	cr.Comma = comma
	cr.FieldsPerRecord = -1
	return cr
}

// describeCSV infers the schema from the header row and a sample of records.
func describeCSV(r io.Reader, comma rune) ([]source.Column, error) {
	cr := newCSVReader(r, comma)
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("empty file (a header row is required)")
	}
	if err != nil {
		return nil, err
	}
	names := headerNames(header)

	guesses := make([]typeGuess, len(names))
	for range sampleRows {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		for i := range guesses {
			if i < len(rec) {
				guesses[i].seeText(rec[i])
			}
		}
	}

	cols := make([]source.Column, len(names))
	for i, n := range names {
		cols[i] = source.Column{Name: n, Type: guesses[i].affinity()}
	}
	return cols, nil
}

// headerNames turns a header record into usable, unique column names: cells
// are trimmed, an empty cell becomes column_<n> (1-based), and a duplicate
// gets a _2/_3/… suffix.
func headerNames(header []string) []string {
	names := make([]string, len(header))
	seen := make(map[string]bool, len(header))
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" {
			h = fmt.Sprintf("column_%d", i+1)
		}
		name := h
		for n := 2; seen[name]; n++ {
			name = fmt.Sprintf("%s_%d", h, n)
		}
		seen[name] = true
		names[i] = name
	}
	return names
}

// scanCSV streams the records after the header into w, coercing each cell to
// the column's inferred affinity.
func scanCSV(r io.Reader, comma rune, cols []source.Column, w *rowWriter) error {
	cr := newCSVReader(r, comma)
	if _, err := cr.Read(); err != nil { // header
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		row := make([]any, len(cols))
		for i, col := range cols {
			if i < len(rec) {
				row[i] = textValue(rec[i], col.Type)
			}
		}
		done, err := w.add(row)
		if err != nil || done {
			return err
		}
	}
}

// textValue coerces one CSV cell to the column's affinity: numeric columns get
// typed values with the empty cell as NULL; anything unparseable (or a TEXT
// column) stays the string as written, so SQLite's verbatim re-filter matches.
func textValue(s, affinity string) any {
	switch affinity {
	case "INTEGER":
		if s == "" {
			return nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case "REAL":
		if s == "" {
			return nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return s
}

// typeGuess accumulates observations of one column's sampled values and picks
// the narrowest SQLite affinity every non-empty value fits: INTEGER, then
// REAL, else TEXT (also the default when no value was seen).
type typeGuess struct {
	seen    bool
	notInt  bool
	notReal bool
}

func (g *typeGuess) seeText(s string) {
	if s == "" {
		return
	}
	g.seen = true
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		g.notInt = true
	}
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		g.notReal = true
	}
}

func (g *typeGuess) affinity() string {
	switch {
	case !g.seen:
		return "TEXT"
	case !g.notInt:
		return "INTEGER"
	case !g.notReal:
		return "REAL"
	default:
		return "TEXT"
	}
}
