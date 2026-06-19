package sqlparse

// This file defines dfetch's own typed representation of a parsed SELECT query.
// It is a pragmatic, push-down-oriented model rather than a faithful copy of the
// full SQL grammar: constructs dfetch understands (sources, joins, projections,
// simple WHERE/ON comparisons) are typed so a future planner can decide what to
// fetch per source and which filters/projections to push down. Anything dfetch
// does not model is preserved as raw SQL text (Predicate.Raw, Projection.Expr,
// Source.Raw) so nothing is silently dropped.
//
// Current limitations (tracked as future work): compound selects (UNION/…),
// WITH/CTE bodies, GROUP BY/HAVING, ORDER BY, and LIMIT are not yet modeled on
// the AST. Query.Tables / Query.Columns remain the exhaustive flat views used
// for fetch planning.

// Select is the structured form of a single SELECT query.
type Select struct {
	Distinct    bool
	Projections []Projection
	// From holds the FROM/JOIN sources in order. From[0] is the first source;
	// each later source From[i] is connected by Joins[i-1].
	From []Source
	// Joins[i] describes how From[i+1] joins the preceding sources.
	Joins []Join
	// Where holds the top-level AND-ed conjuncts of the WHERE clause.
	Where []Predicate
}

// Source is a single FROM/JOIN source: a base table, a subquery, or a form
// dfetch does not model (table-valued function, parenthesized join), preserved
// in Raw.
type Source struct {
	Schema   string  // optional schema qualifier (base tables only)
	Name     string  // base table name; empty for subquery / Raw sources
	Subquery *Select // non-nil when the source is a (SELECT …) subquery
	Alias    string  // empty when unaliased
	Raw      string  // original text for unmodeled sources (table functions, paren joins)
}

// IsSubquery reports whether the source is a derived table (subquery).
func (s Source) IsSubquery() bool { return s.Subquery != nil }

// JoinType identifies the kind of join connecting two sources.
type JoinType string

const (
	JoinInner JoinType = "INNER"
	JoinLeft  JoinType = "LEFT"
	JoinRight JoinType = "RIGHT"
	JoinFull  JoinType = "FULL"
	JoinCross JoinType = "CROSS"
	JoinComma JoinType = "COMMA" // implicit comma join
)

// Join describes a join operator and its constraint.
type Join struct {
	Type    JoinType
	Natural bool
	On      []Predicate // ON conjuncts (structured where possible); nil for USING/none
	Using   []string    // USING (col, …) columns; nil for ON/none
}

// Projection is one entry of the SELECT list.
type Projection struct {
	Star   bool   // SELECT * (Table=="") or SELECT t.* (Table set)
	Table  string // qualifier for t.* or for a qualified column
	Column string // column name for a simple column projection
	Alias  string // AS alias, if any
	Expr   string // raw text for non-column expressions (e.g. COUNT(*))
}

// Predicate is one conjunct of a WHERE or ON clause. When Op != "" it is a
// simple comparison "<Table>.<Column> Op <Value>" that may be pushable to a
// source. Otherwise only Raw is meaningful (the original text of the conjunct).
type Predicate struct {
	Table  string // column's table qualifier ("" if unqualified)
	Column string
	Op     string // =, <>, <, <=, >, >=, LIKE, IS NULL, IS NOT NULL
	Value  *Value // nil for IS NULL / IS NOT NULL and for unstructured predicates
	Raw    string // always set: original text of the conjunct
}

// ValueKind distinguishes a literal from a bind parameter.
type ValueKind string

const (
	ValueLiteral ValueKind = "literal"
	ValueBind    ValueKind = "bind"
)

// Value is the right-hand side of a simple comparison predicate.
type Value struct {
	Kind ValueKind
	Text string // literal text (e.g. 5, 'abc') or bind parameter (e.g. ?, :id)
}
