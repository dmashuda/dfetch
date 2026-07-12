package files

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/dmashuda/dfetch/source"
)

// newCSVReader builds the reader both describe and scan use. Records may be
// ragged (FieldsPerRecord -1): scan pads/truncates each record to the declared
// columns, so a stray short row degrades to NULLs instead of failing the query.
// ReuseRecord is safe: callers copy out every cell they keep and never retain
// the record slice.
func newCSVReader(r io.Reader, comma rune) *csv.Reader {
	cr := csv.NewReader(stripBOM(r))
	cr.Comma = comma
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
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
// gets a _2/_3/… suffix. Uniqueness is case-insensitive because SQLite column
// names are — otherwise a header like "id,ID" would build a CREATE TABLE with a
// duplicate column and fail every query.
func headerNames(header []string) []string {
	names := make([]string, len(header))
	seen := make(map[string]bool, len(header))
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" {
			h = fmt.Sprintf("column_%d", i+1)
		}
		name := h
		for n := 2; seen[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s_%d", h, n)
		}
		seen[strings.ToLower(name)] = true
		names[i] = name
	}
	return names
}

// parseSQLiteInt parses s as an int64 only when its canonical form round-trips,
// so identifier-like values with a leading zero or sign ("01234", "+5") stay
// TEXT and compare equal to the string as written rather than being rewritten.
func parseSQLiteInt(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return n, true
}

// parseSQLiteReal parses s as a float64, rejecting NaN/Inf (SQLite stores NaN
// as NULL and has no infinity literal, so coercing them would corrupt the
// value) and redundant-leading-zero forms ("01.5") that wouldn't round-trip.
func parseSQLiteReal(s string) (float64, bool) {
	if hasRedundantLeadingZero(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// hasRedundantLeadingZero reports whether s (after an optional sign) begins with
// a '0' followed by another digit — the shape of an identifier like a zip code,
// not a canonical number.
func hasRedundantLeadingZero(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	return i+1 < len(s) && s[i] == '0' && s[i+1] >= '0' && s[i+1] <= '9'
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
		ok, err := w.add(row)
		if err != nil {
			return err
		}
		if !ok {
			return nil // hit the cap
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
		if n, ok := parseSQLiteInt(s); ok {
			return n
		}
	case "REAL":
		if s == "" {
			return nil
		}
		if f, ok := parseSQLiteReal(s); ok {
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
	if _, ok := parseSQLiteInt(s); ok {
		return // a valid int is also a valid real; no need to check further
	}
	g.notInt = true
	if _, ok := parseSQLiteReal(s); !ok {
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
