package sqlparse

import (
	"regexp"
	"strings"
)

// This file renders the typed AST back into SQL text. It reproduces the modeled
// structure (DISTINCT, projections, FROM/JOINs, WHERE). Constructs the AST does
// not model (ORDER BY, LIMIT, GROUP BY, CTEs, compound selects) are not emitted,
// and predicates/sources the parser left unstructured are rendered from their
// preserved Raw text.

var bareIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// quoteIdent double-quotes an identifier unless it is a plain bareword that is
// not a SQL keyword. Keywords must be quoted or the rendered SQL fails to parse
// (e.g. a column named "order").
func quoteIdent(name string) string {
	if bareIdent.MatchString(name) {
		if _, isKeyword := sqlKeywords[strings.ToUpper(name)]; !isKeyword {
			return name
		}
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlKeywords is the set of SQLite keyword tokens (from grammar/SQLiteLexer.g4).
// An identifier matching one of these must be quoted when rendered.
var sqlKeywords = map[string]struct{}{
	"ABORT": {}, "ACTION": {}, "ADD": {}, "AFTER": {}, "ALL": {}, "ALTER": {},
	"ALWAYS": {}, "ANALYZE": {}, "AND": {}, "AS": {}, "ASC": {}, "ATTACH": {},
	"AUTOINCREMENT": {}, "BEFORE": {}, "BEGIN": {}, "BETWEEN": {}, "BY": {}, "CASCADE": {},
	"CASE": {}, "CAST": {}, "CHECK": {}, "COLLATE": {}, "COLUMN": {}, "COMMIT": {},
	"CONFLICT": {}, "CONSTRAINT": {}, "CREATE": {}, "CROSS": {}, "CURRENT": {}, "CURRENT_DATE": {},
	"CURRENT_TIME": {}, "CURRENT_TIMESTAMP": {}, "DATABASE": {}, "DEFAULT": {}, "DEFERRABLE": {}, "DEFERRED": {},
	"DELETE": {}, "DESC": {}, "DETACH": {}, "DISTINCT": {}, "DO": {}, "DROP": {},
	"EACH": {}, "ELSE": {}, "END": {}, "ESCAPE": {}, "EXCEPT": {}, "EXCLUDE": {},
	"EXCLUSIVE": {}, "EXISTS": {}, "EXPLAIN": {}, "FAIL": {}, "FALSE": {}, "FILTER": {},
	"FIRST": {}, "FOLLOWING": {}, "FOR": {}, "FOREIGN": {}, "FROM": {}, "FULL": {},
	"GENERATED": {}, "GLOB": {}, "GROUP": {}, "GROUPS": {}, "HAVING": {}, "IF": {},
	"IGNORE": {}, "IMMEDIATE": {}, "IN": {}, "INDEX": {}, "INDEXED": {}, "INITIALLY": {},
	"INNER": {}, "INSERT": {}, "INSTEAD": {}, "INTERSECT": {}, "INTO": {}, "IS": {},
	"ISNULL": {}, "JOIN": {}, "KEY": {}, "LAST": {}, "LEFT": {}, "LIKE": {},
	"LIMIT": {}, "MATCH": {}, "MATERIALIZED": {}, "NATURAL": {}, "NO": {}, "NOT": {},
	"NOTHING": {}, "NOTNULL": {}, "NULL": {}, "NULLS": {}, "OF": {}, "OFFSET": {},
	"ON": {}, "OR": {}, "ORDER": {}, "OTHERS": {}, "OUTER": {}, "OVER": {},
	"PARTITION": {}, "PLAN": {}, "PRAGMA": {}, "PRECEDING": {}, "PRIMARY": {}, "QUERY": {},
	"RAISE": {}, "RANGE": {}, "RECURSIVE": {}, "REFERENCES": {}, "REGEXP": {}, "REINDEX": {},
	"RELEASE": {}, "RENAME": {}, "REPLACE": {}, "RESTRICT": {}, "RETURNING": {}, "RIGHT": {},
	"ROLLBACK": {}, "ROW": {}, "ROWID": {}, "ROWS": {}, "SAVEPOINT": {}, "SELECT": {},
	"SET": {}, "STORED": {}, "STRICT": {}, "TABLE": {}, "TEMP": {}, "TEMPORARY": {},
	"THEN": {}, "TIES": {}, "TO": {}, "TRANSACTION": {}, "TRIGGER": {}, "TRUE": {},
	"UNBOUNDED": {}, "UNION": {}, "UNIQUE": {}, "UPDATE": {}, "USING": {}, "VACUUM": {},
	"VALUES": {}, "VIEW": {}, "VIRTUAL": {}, "WHEN": {}, "WHERE": {}, "WINDOW": {},
	"WITH": {}, "WITHIN": {}, "WITHOUT": {},
}

// SQL renders the query back into a SQL string.
func (q *Query) SQL() string {
	if q == nil || q.Stmt == nil {
		return ""
	}
	return q.Stmt.SQL()
}

// SQL renders the Select back into a SQL string. It reproduces only the modeled
// clauses; when Complete is false the result omits clauses that were present in
// the source query (see Select.Complete) and must not be treated as equivalent.
func (s *Select) SQL() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if s.Distinct {
		b.WriteString("DISTINCT ")
	}
	if len(s.Projections) == 0 {
		b.WriteString("*")
	} else {
		parts := make([]string, len(s.Projections))
		for i, p := range s.Projections {
			parts[i] = p.sql()
		}
		b.WriteString(strings.Join(parts, ", "))
	}
	if len(s.From) > 0 {
		b.WriteString(" FROM ")
		b.WriteString(s.From[0].sql())
		for i, j := range s.Joins {
			b.WriteString(j.sql(s.From[i+1]))
		}
	}
	if len(s.Where) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(renderPredicates(s.Where))
	}
	return b.String()
}

func (p Projection) sql() string {
	switch {
	case p.Star && p.Table != "":
		return quoteIdent(p.Table) + ".*"
	case p.Star:
		return "*"
	}

	var s string
	switch {
	case p.Column != "" && p.Table != "":
		s = quoteIdent(p.Table) + "." + quoteIdent(p.Column)
	case p.Column != "":
		s = quoteIdent(p.Column)
	default:
		s = p.Expr
	}
	if p.Alias != "" {
		s += " AS " + quoteIdent(p.Alias)
	}
	return s
}

func (s Source) sql() string {
	var base string
	switch {
	case s.Subquery != nil:
		base = "(" + s.Subquery.SQL() + ")"
	case s.Name != "":
		base = quoteIdent(s.Name)
		if s.Schema != "" {
			base = quoteIdent(s.Schema) + "." + base
		}
	default:
		base = s.Raw
	}
	if s.Alias != "" {
		base += " AS " + quoteIdent(s.Alias)
	}
	return base
}

func (j Join) sql(right Source) string {
	if j.Type == JoinComma {
		return ", " + right.sql()
	}

	var kw strings.Builder
	if j.Natural {
		kw.WriteString("NATURAL ")
	}
	switch j.Type {
	case JoinLeft:
		kw.WriteString("LEFT ")
	case JoinRight:
		kw.WriteString("RIGHT ")
	case JoinFull:
		kw.WriteString("FULL ")
	case JoinCross:
		kw.WriteString("CROSS ")
	}
	kw.WriteString("JOIN")

	out := " " + kw.String() + " " + right.sql()
	switch {
	case len(j.Using) > 0:
		cols := make([]string, len(j.Using))
		for i, c := range j.Using {
			cols[i] = quoteIdent(c)
		}
		out += " USING (" + strings.Join(cols, ", ") + ")"
	case len(j.On) > 0:
		out += " ON " + renderPredicates(j.On)
	}
	return out
}

func renderPredicates(ps []Predicate) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.sql()
	}
	return strings.Join(parts, " AND ")
}

func (p Predicate) sql() string {
	if p.Op == OpNone {
		return p.Raw
	}

	col := quoteIdent(p.Column)
	if p.Table != "" {
		col = quoteIdent(p.Table) + "." + col
	}

	switch p.Op {
	case OpIsNull, OpIsNotNull:
		return col + " " + p.Op.String()
	case OpBetween, OpNotBetween:
		if len(p.Values) == 2 {
			return col + " " + p.Op.String() + " " + p.Values[0].sql() + " AND " + p.Values[1].sql()
		}
	case OpIn, OpNotIn:
		items := make([]string, len(p.Values))
		for i, v := range p.Values {
			items[i] = v.sql()
		}
		return col + " " + p.Op.String() + " (" + strings.Join(items, ", ") + ")"
	default:
		if p.Value != nil {
			return col + " " + p.Op.String() + " " + p.Value.sql()
		}
	}
	return p.Raw
}

func (v Value) sql() string {
	if v.Kind == ValueBind {
		return v.Bind
	}
	if v.Literal != nil {
		return v.Literal.Raw
	}
	return ""
}
