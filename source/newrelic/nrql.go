package newrelic

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmashuda/dfetch/source"
)

// This file is the pure core of the dynamic NRDB path: translating a pushed-down
// ScanRequest into NRQL, and parsing NRQL results back into schema/rows. No I/O.

// nrqlPlan is the rendered NRQL for one NRDB scan plus what the builder decided,
// so the scanner knows which columns it will emit and which warnings apply.
type nrqlPlan struct {
	NRQL        string
	Cols        []source.Column // emitted columns in order (projection applied)
	PushedLimit bool            // the user LIMIT rode the NRQL LIMIT
	Capped      bool            // the maxRows cap bounded the scan instead
	Windowed    bool            // default SINCE window applied (no timestamp lower bound)
}

// buildNRQL renders the NRQL for one NRDB scan. Pure: now is injected so the
// default window is deterministic in tests. boolCols marks columns keyset
// reported as boolean (locally INTEGER 0/1), so equality filters on them are
// translated to NRQL's true/false.
//
// The push-down contract: every translated clause is exact (NRQL and SQLite
// agree on its semantics), so a filter is either fully consumed or not pushed
// at all — there is no widened-but-pushed middle ground outside the SINCE/UNTIL
// window, which is always redundant with an exact timestamp WHERE clause.
func buildNRQL(req source.ScanRequest, cols []source.Column, boolCols map[string]bool, maxRows int, window time.Duration, now time.Time) nrqlPlan {
	plan := nrqlPlan{Cols: projectCols(req.Columns, cols)}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	if len(req.Columns) == 0 {
		sb.WriteString("*")
	} else {
		names := make([]string, len(plan.Cols))
		for i, c := range plan.Cols {
			names[i] = quoteAttr(c.Name)
		}
		sb.WriteString(strings.Join(names, ", "))
	}
	sb.WriteString(" FROM " + quoteAttr(req.Table))

	consumed := true
	var clauses []string
	for _, f := range req.Filters {
		clause, ok := translateNRQLFilter(f, boolCols)
		if !ok {
			consumed = false
			continue
		}
		clauses = append(clauses, clause)
	}
	if len(clauses) > 0 {
		sb.WriteString(" WHERE " + strings.Join(clauses, " AND "))
	}

	orderClause, orderOK := orderHonored(req.OrderBy)
	if orderClause != "" {
		sb.WriteString(" " + orderClause)
	}

	// Push LIMIT only when the API returns exactly the filtered+ordered set and
	// the whole window (limit+offset) fits under NRQL's hard cap — otherwise a
	// pushed LIMIT could truncate rows SQLite would keep.
	n := 0
	if req.Limit != nil {
		n = *req.Limit
		if req.Offset != nil {
			n += *req.Offset
		}
	}
	if req.Limit != nil && consumed && orderOK && n <= nrqlHardCap {
		fmt.Fprintf(&sb, " LIMIT %d", n)
		plan.PushedLimit = true
	} else {
		fmt.Fprintf(&sb, " LIMIT %d", maxRows)
		plan.Capped = true
	}

	since, until, hasUntil, windowed := timestampBounds(req, now, window)
	plan.Windowed = windowed
	fmt.Fprintf(&sb, " SINCE %d", since)
	if hasUntil {
		fmt.Fprintf(&sb, " UNTIL %d", until)
	}

	plan.NRQL = sb.String()
	return plan
}

// projectCols returns the emitted columns: the schema columns narrowed and
// ordered by the requested projection (empty projection = all columns).
func projectCols(requested []string, cols []source.Column) []source.Column {
	if len(requested) == 0 {
		return cols
	}
	byName := make(map[string]source.Column, len(cols))
	for _, c := range cols {
		byName[c.Name] = c
	}
	out := make([]source.Column, 0, len(requested))
	for _, name := range requested {
		if c, ok := byName[name]; ok {
			out = append(out, c)
		}
	}
	return out
}

// translateNRQLFilter renders one filter as an exact NRQL clause, or ok=false
// when it cannot be pushed (and therefore does not count as consumed):
//
//   - =, != on strings/numbers: NRQL string equality is case-sensitive like
//     SQLite's, so these are exact.
//   - <, <=, >, >= and BETWEEN (expanded to a >= AND <= pair): numeric values
//     only — NRQL relational operators are numeric.
//   - IN / NOT IN with a non-empty list.
//   - boolean columns (keyset booleanKeys): stored locally as INTEGER 0/1, so
//     `= 1` / `= 0` translate to `= true` / `= false`; any other value is not
//     pushed.
//   - never pushed: LIKE (NRQL's is case-insensitive — pushing it could drop
//     rows SQLite's case-sensitive LIKE keeps), GLOB/REGEXP/MATCH, IS/IS NOT,
//     NOT BETWEEN, blobs, NULLs.
func translateNRQLFilter(f source.Filter, boolCols map[string]bool) (string, bool) {
	col := quoteAttr(f.Column)
	switch f.Op {
	case source.OpEq, source.OpNotEq:
		lit, ok := renderValue(f.Column, f.Value, boolCols)
		if !ok {
			return "", false
		}
		op := "="
		if f.Op == source.OpNotEq {
			op = "!="
		}
		return col + " " + op + " " + lit, true
	case source.OpLt, source.OpLte, source.OpGt, source.OpGte:
		lit, ok := renderNumber(f.Value)
		if !ok {
			return "", false
		}
		return col + " " + relOp(f.Op) + " " + lit, true
	case source.OpIn, source.OpNotIn:
		if len(f.Values) == 0 {
			return "", false
		}
		lits := make([]string, 0, len(f.Values))
		for _, v := range f.Values {
			lit, ok := renderValue(f.Column, v, boolCols)
			if !ok {
				return "", false
			}
			lits = append(lits, lit)
		}
		op := "IN"
		if f.Op == source.OpNotIn {
			op = "NOT IN"
		}
		return col + " " + op + " (" + strings.Join(lits, ", ") + ")", true
	case source.OpBetween:
		if len(f.Values) != 2 {
			return "", false
		}
		lo, ok1 := renderNumber(f.Values[0])
		hi, ok2 := renderNumber(f.Values[1])
		if !ok1 || !ok2 {
			return "", false
		}
		return "(" + col + " >= " + lo + " AND " + col + " <= " + hi + ")", true
	default:
		return "", false
	}
}

func relOp(op source.Operator) string {
	switch op {
	case source.OpLt:
		return "<"
	case source.OpLte:
		return "<="
	case source.OpGt:
		return ">"
	default:
		return ">="
	}
}

// renderValue renders a filter value as an NRQL literal. On a boolean column
// only 0/1 (or a bool) translate — to false/true; anything else is unpushable.
func renderValue(column string, v any, boolCols map[string]bool) (string, bool) {
	if boolCols[column] {
		switch n := v.(type) {
		case bool:
			return strconv.FormatBool(n), true
		case int64:
			switch n {
			case 0:
				return "false", true
			case 1:
				return "true", true
			}
		}
		return "", false
	}
	switch n := v.(type) {
	case string:
		return "'" + escapeNRQLString(n) + "'", true
	case int64, float64:
		return renderNumber(v)
	default:
		return "", false
	}
}

// renderNumber renders an int64/float64 as an NRQL numeric literal.
func renderNumber(v any) (string, bool) {
	switch n := v.(type) {
	case int64:
		return strconv.FormatInt(n, 10), true
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64), true
	default:
		return "", false
	}
}

// escapeNRQLString escapes a string for a single-quoted NRQL literal:
// backslashes first, then quotes.
func escapeNRQLString(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// quoteAttr backticks an attribute or event-type name so dotted names
// (host.name) parse. Names containing a backtick are rejected upstream
// (DescribeTable) and never reach here.
func quoteAttr(name string) string {
	return "`" + name + "`"
}

// orderHonored maps the pushed ORDER BY terms to an NRQL clause. Only zero
// terms or exactly `timestamp ASC|DESC` are honorable: timestamp is the one
// column guaranteed present on every event (no NULL-placement divergence) and
// numeric (no collation divergence), and NRQL orders by a single attribute.
func orderHonored(terms []source.OrderTerm) (clause string, ok bool) {
	switch {
	case len(terms) == 0:
		return "", true
	case len(terms) == 1 && terms[0].Column == "timestamp":
		dir := "ASC"
		if terms[0].Desc {
			dir = "DESC"
		}
		return "ORDER BY " + quoteAttr("timestamp") + " " + dir, true
	default:
		return "", false
	}
}

// timestampBounds derives the SINCE/UNTIL window (epoch ms) from the numeric
// timestamp filters. The window is widened one millisecond outward on each
// bounded side so unknown SINCE/UNTIL boundary inclusivity can never exclude a
// row the exact timestamp WHERE clauses keep — the window is a fetch superset;
// the WHERE clauses are the source of truth.
//
// With no lower bound the default window (now-window) applies and windowed is
// true so the scanner warns; an upper-bound-only query anchors the window to
// the upper bound rather than now (a historical UNTIL with a now-anchored SINCE
// would invert the window and return nothing).
func timestampBounds(req source.ScanRequest, now time.Time, window time.Duration) (since, until int64, hasUntil, windowed bool) {
	var lowers, uppers []int64
	for _, f := range req.Filters {
		if f.Column != "timestamp" {
			continue
		}
		switch f.Op {
		case source.OpGt, source.OpGte:
			if v, ok := msValue(f.Value); ok {
				lowers = append(lowers, v)
			}
		case source.OpLt, source.OpLte:
			if v, ok := msValue(f.Value); ok {
				uppers = append(uppers, v)
			}
		case source.OpEq:
			if v, ok := msValue(f.Value); ok {
				lowers = append(lowers, v)
				uppers = append(uppers, v)
			}
		case source.OpBetween:
			if len(f.Values) == 2 {
				if lo, ok := msValue(f.Values[0]); ok {
					lowers = append(lowers, lo)
				}
				if hi, ok := msValue(f.Values[1]); ok {
					uppers = append(uppers, hi)
				}
			}
		case source.OpIn:
			// IN is a disjunction: the filter as a whole bounds the window by
			// the smallest and largest listed values — appending each value to
			// both sides would AND them and invert the window. If any value is
			// unparseable the filter can't bound the window at all.
			vals := make([]int64, 0, len(f.Values))
			for _, v := range f.Values {
				ms, ok := msValue(v)
				if !ok {
					vals = nil
					break
				}
				vals = append(vals, ms)
			}
			if len(vals) > 0 {
				lowers = append(lowers, slicesMin(vals))
				uppers = append(uppers, slicesMax(vals))
			}
		}
	}

	// The WHERE clauses are ANDed, so the effective bounds are the tightest
	// ones; widening those by 1ms keeps the window a superset.
	if len(uppers) > 0 {
		until = slicesMin(uppers) + 1
		hasUntil = true
	}
	switch {
	case len(lowers) > 0:
		since = slicesMax(lowers) - 1
	case hasUntil:
		since = until - window.Milliseconds()
		windowed = true
	default:
		since = now.UnixMilli() - window.Milliseconds()
		windowed = true
	}
	return since, until, hasUntil, windowed
}

// msValue extracts an epoch-ms value from a numeric filter literal. Fractional
// values are floored: the caller widens every bound 1ms outward, and
// floor(v)-1 < v < floor(v)+1, so the window stays a superset. Rejecting them
// instead would silently fall back to the default window — which is NOT a
// superset of a consumed WHERE clause older than the window.
func msValue(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		if math.Abs(n) < 1<<53 {
			return int64(math.Floor(n)), true
		}
	}
	return 0, false
}

func slicesMin(v []int64) int64 {
	m := v[0]
	for _, x := range v[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func slicesMax(v []int64) int64 {
	m := v[0]
	for _, x := range v[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

// parseKeyset maps a `SELECT keyset()` result to columns, handling both
// plausible response shapes defensively (nil columns = unknown/idle type):
//
//	(a) one row with array fields: {"stringKeys": [...], "numericKeys": [...],
//	    "booleanKeys": [...]} (optionally "allKeys", which is ignored)
//	(b) one row per attribute: {"key": "duration", "type": "numeric"}
//
// Type mapping: string→TEXT, numeric→REAL, boolean→INTEGER, unknown→TEXT.
// timestamp is always forced to INTEGER (epoch ms) and injected when keyset
// omits it; it sorts first, the rest alphabetically, for a stable schema.
// Keys containing a backtick are dropped (unquotable in NRQL).
func parseKeyset(results []map[string]any) (cols []source.Column, boolCols map[string]bool) {
	types := map[string]string{} // attr name -> nrql type name

	addKeys := func(row map[string]any, field, typ string) {
		list, ok := row[field].([]any)
		if !ok {
			return
		}
		for _, k := range list {
			if s, ok := k.(string); ok {
				types[s] = typ
			}
		}
	}

	for _, row := range results {
		// Shape (b): {"key": ..., "type": ...}.
		if k, ok := row["key"].(string); ok {
			typ, _ := row["type"].(string)
			types[k] = typ
			continue
		}
		// Shape (a): array fields.
		addKeys(row, "stringKeys", "string")
		addKeys(row, "numericKeys", "numeric")
		addKeys(row, "booleanKeys", "boolean")
	}
	if len(types) == 0 {
		return nil, nil
	}

	boolCols = map[string]bool{}
	names := make([]string, 0, len(types))
	for name, typ := range types {
		if name == "timestamp" || strings.Contains(name, "`") {
			continue
		}
		names = append(names, name)
		if typ == "boolean" {
			boolCols[name] = true
		}
	}
	sort.Strings(names)

	cols = make([]source.Column, 0, len(names)+1)
	cols = append(cols, source.Column{Name: "timestamp", Type: "INTEGER"})
	for _, name := range names {
		affinity := "TEXT"
		switch types[name] {
		case "numeric":
			affinity = "REAL"
		case "boolean":
			affinity = "INTEGER"
		}
		cols = append(cols, source.Column{Name: name, Type: affinity})
	}
	return cols, boolCols
}

// parseEventTypes extracts event-type names from a SHOW EVENT TYPES result:
// the documented field is "eventType", with a defensive fallback to any row
// whose sole value is a string.
func parseEventTypes(results []map[string]any) []string {
	var out []string
	for _, row := range results {
		if s, ok := row["eventType"].(string); ok {
			out = append(out, s)
			continue
		}
		if len(row) == 1 {
			for _, v := range row {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// normalizeNRDB coerces a decoded NRQL result value into a localdb-accepted
// type. Every NRQL number decodes as float64; integral values headed for an
// INTEGER column (timestamp, booleans-as-0/1) become int64 so SQLite stores
// them as integers. Nested attributes become JSON text (json_extract-able).
func normalizeNRDB(v any, affinity string) any {
	switch x := v.(type) {
	case nil, bool, string:
		return x
	case float64:
		if affinity == "INTEGER" && x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
		return x
	case map[string]any, []any:
		if b, err := json.Marshal(x); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", x)
	default:
		return fmt.Sprintf("%v", x)
	}
}
