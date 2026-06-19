package engine

import (
	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
)

// planScan extracts the part of the query that can be offered to one source for
// push-down: WHERE filters and ORDER BY terms attributable to it, plus LIMIT/
// OFFSET when it is safe to push (single-source query). The connector decides
// what it can actually honor; the local SQLite re-applies the full query, so an
// over-broad result is always corrected.
func planScan(stmt *sqlparse.Select, src sqlparse.Source, ts source.TableSchema) source.ScanRequest {
	req := source.ScanRequest{Table: src.Name}
	single := len(collectSources(stmt)) == 1
	cols := columnSet(ts)
	have := map[string]bool{}

	for _, p := range stmt.Where {
		if !attributable(p.Table, src, single) || !cols[p.Column] {
			continue
		}
		if f, ok := toFilter(p); ok {
			req.Filters = append(req.Filters, f)
			have[p.Column] = true
		}
	}

	// Infer equality filters across equi-joins: if this source's column is joined
	// to another column that has a literal equality filter, that literal also
	// applies here (e.g. `r.name = i.repo` + `i.repo = 'go'` ⇒ `r.name = 'go'`).
	req.Filters = append(req.Filters, inferJoinFilters(stmt, src, cols, have)...)

	for _, o := range stmt.OrderBy {
		if o.Column == "" || !attributable(o.Table, src, single) || !cols[o.Column] {
			continue
		}
		req.OrderBy = append(req.OrderBy, source.OrderTerm{Column: o.Column, Desc: o.Desc})
	}

	// LIMIT/OFFSET only push for a single-source query; otherwise a join could
	// drop rows after the source already truncated. The connector additionally
	// refuses to push unless it consumed every filter and honored the order.
	if single && stmt.Limit != nil {
		if n, ok := intValue(stmt.Limit.Count); ok {
			req.Limit = &n
		}
		if n, ok := intValue(stmt.Limit.Offset); ok {
			req.Offset = &n
		}
	}
	return req
}

// colRef identifies a qualified column (qualifier.column).
type colRef struct{ table, column string }

// inferJoinFilters derives equality filters for src by propagating literal
// equalities through equi-join keys. For every `A = B` join key where one side
// is a column of src and the other side has a literal `= value` filter, the
// value is pushed to src too. have records src columns that already carry a
// filter, so inference never duplicates one.
func inferJoinFilters(stmt *sqlparse.Select, src sqlparse.Source, cols, have map[string]bool) []source.Filter {
	literals := literalEqualities(stmt)
	pairs := equiJoinPairs(stmt)

	onSrc := func(r colRef) bool {
		return (r.table == src.Alias || r.table == src.Name) && cols[r.column]
	}

	var out []source.Filter
	add := func(column string, v *sqlparse.Value) {
		if have[column] {
			return
		}
		val, ok := literalValue(v)
		if !ok {
			return
		}
		have[column] = true
		out = append(out, source.Filter{Column: column, Op: sqlparse.OpEq, Value: val})
	}

	for _, p := range pairs {
		a, b := p[0], p[1]
		if onSrc(a) {
			if v, ok := literals[b]; ok {
				add(a.column, v)
			}
		}
		if onSrc(b) {
			if v, ok := literals[a]; ok {
				add(b.column, v)
			}
		}
	}
	return out
}

// literalEqualities maps each qualified column with a `= literal` predicate to
// its value (only qualified predicates participate in cross-source inference).
func literalEqualities(stmt *sqlparse.Select) map[colRef]*sqlparse.Value {
	out := map[colRef]*sqlparse.Value{}
	collect := func(ps []sqlparse.Predicate) {
		for i := range ps {
			p := &ps[i]
			if p.Op == sqlparse.OpEq && p.Value != nil && p.Table != "" {
				out[colRef{p.Table, p.Column}] = p.Value
			}
		}
	}
	collect(stmt.Where)
	for i := range stmt.Joins {
		collect(stmt.Joins[i].On)
	}
	return out
}

// equiJoinPairs returns the column-to-column equality pairs from JOIN ON clauses
// and the WHERE clause.
func equiJoinPairs(stmt *sqlparse.Select) [][2]colRef {
	var out [][2]colRef
	collect := func(ps []sqlparse.Predicate) {
		for i := range ps {
			p := &ps[i]
			if p.Op == sqlparse.OpEq && p.RefColumn != "" && p.Table != "" && p.RefTable != "" {
				out = append(out, [2]colRef{{p.Table, p.Column}, {p.RefTable, p.RefColumn}})
			}
		}
	}
	collect(stmt.Where)
	for i := range stmt.Joins {
		collect(stmt.Joins[i].On)
	}
	return out
}

// attributable reports whether a predicate/order term written with the given
// table qualifier refers to src: it matches the source's alias or name, or it is
// unqualified in a single-source query.
func attributable(qualifier string, src sqlparse.Source, single bool) bool {
	switch qualifier {
	case "":
		return single
	case src.Alias, src.Name:
		return true
	default:
		return false
	}
}

func columnSet(ts source.TableSchema) map[string]bool {
	set := make(map[string]bool, len(ts.Columns))
	for _, c := range ts.Columns {
		set[c.Name] = true
	}
	return set
}

// toFilter converts a structured predicate with literal values into a Filter.
// Bind parameters and unstructured predicates are not pushable.
func toFilter(p sqlparse.Predicate) (source.Filter, bool) {
	switch p.Op {
	case sqlparse.OpNone, sqlparse.OpIsNull, sqlparse.OpIsNotNull:
		return source.Filter{}, false
	case sqlparse.OpIn, sqlparse.OpNotIn, sqlparse.OpBetween, sqlparse.OpNotBetween:
		vals := make([]any, 0, len(p.Values))
		for i := range p.Values {
			v, ok := literalValue(&p.Values[i])
			if !ok {
				return source.Filter{}, false
			}
			vals = append(vals, v)
		}
		return source.Filter{Column: p.Column, Op: p.Op, Values: vals}, true
	default:
		v, ok := literalValue(p.Value)
		if !ok {
			return source.Filter{}, false
		}
		return source.Filter{Column: p.Column, Op: p.Op, Value: v}, true
	}
}

// literalValue returns the Go value of a literal (non-bind) value.
func literalValue(v *sqlparse.Value) (any, bool) {
	if v == nil || v.Kind != sqlparse.ValueLiteral || v.Literal == nil {
		return nil, false
	}
	return v.Literal.Value, true
}

// intValue extracts an int from an integer-literal value.
func intValue(v *sqlparse.Value) (int, bool) {
	if v == nil || v.Literal == nil {
		return 0, false
	}
	n, ok := v.Literal.AsInt()
	return int(n), ok
}
