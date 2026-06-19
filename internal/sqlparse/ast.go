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
// WITH/CTE bodies, and GROUP BY/HAVING are not yet modeled on the AST (their
// presence sets Select.Complete = false). ORDER BY and LIMIT are modeled.
// Query.Tables / Query.Columns remain the exhaustive flat views used for fetch
// planning.

// Select is the structured form of a single SELECT query.
type Select struct {
	// Complete reports whether the AST fully represents the source query. It is
	// false when the parser dropped clauses the AST does not model (ORDER BY,
	// LIMIT, GROUP BY/HAVING, WINDOW, WITH/CTE, or a compound UNION/EXCEPT tail),
	// meaning SQL() reconstructs only part of the query and must not be treated
	// as an equivalent rewrite. The zero value (false) is the safe default.
	Complete    bool
	Distinct    bool
	Projections []Projection
	// From holds the FROM/JOIN sources in order. From[0] is the first source;
	// each later source From[i] is connected by Joins[i-1].
	From []Source
	// Joins[i] describes how From[i+1] joins the preceding sources.
	Joins []Join
	// Where holds the top-level AND-ed conjuncts of the WHERE clause.
	Where []Predicate
	// OrderBy holds the ORDER BY terms in order, empty if none.
	OrderBy []OrderTerm
	// Limit holds the LIMIT/OFFSET clause, nil if none.
	Limit *Limit
}

// OrderTerm is one ORDER BY term. Column (with optional Table qualifier) is set
// for a simple column; otherwise Expr holds the raw text of the ordering
// expression. Desc is true for DESC, false for ASC/unspecified.
type OrderTerm struct {
	Table  string
	Column string
	Expr   string
	Desc   bool
}

// Limit is a LIMIT clause with an optional OFFSET. Count and Offset are the
// literal or bind values as written (use Count.Literal.AsInt() for the number).
type Limit struct {
	Count  *Value
	Offset *Value
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

// JoinType identifies the kind of join connecting two sources. It is an enum so
// downstream consumers switch on the named constants exhaustively rather than
// matching strings. The zero value is JoinInner.
type JoinType int

const (
	JoinInner JoinType = iota // INNER / plain JOIN
	JoinLeft                  // LEFT [OUTER] JOIN
	JoinRight                 // RIGHT [OUTER] JOIN
	JoinFull                  // FULL [OUTER] JOIN
	JoinCross                 // CROSS JOIN
	JoinComma                 // implicit comma join
)

func (j JoinType) String() string {
	switch j {
	case JoinInner:
		return "INNER"
	case JoinLeft:
		return "LEFT"
	case JoinRight:
		return "RIGHT"
	case JoinFull:
		return "FULL"
	case JoinCross:
		return "CROSS"
	case JoinComma:
		return "COMMA"
	default:
		return "UNKNOWN"
	}
}

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

// Operator is a comparison operator in a structured predicate. It is an enum so
// downstream consumers (e.g. a push-down planner) handle the full closed set via
// an exhaustive switch rather than matching operator strings. The zero value,
// OpNone, means the predicate is not a structured comparison (only Raw applies).
type Operator int

const (
	OpNone Operator = iota // not a structured comparison

	OpEq    // =
	OpNotEq // <>
	OpLt    // <
	OpLte   // <=
	OpGt    // >
	OpGte   // >=

	OpLike      // LIKE
	OpNotLike   // NOT LIKE
	OpGlob      // GLOB
	OpNotGlob   // NOT GLOB
	OpRegexp    // REGEXP
	OpNotRegexp // NOT REGEXP
	OpMatch     // MATCH
	OpNotMatch  // NOT MATCH

	OpIs                // IS
	OpIsNot             // IS NOT
	OpIsDistinctFrom    // IS DISTINCT FROM
	OpIsNotDistinctFrom // IS NOT DISTINCT FROM
	OpIsNull            // IS NULL
	OpIsNotNull         // IS NOT NULL

	OpBetween    // BETWEEN (Values = [low, high])
	OpNotBetween // NOT BETWEEN (Values = [low, high])
	OpIn         // IN (Values = list)
	OpNotIn      // NOT IN (Values = list)
)

// String returns the SQL form of the operator (empty for OpNone).
func (o Operator) String() string {
	switch o {
	case OpEq:
		return "="
	case OpNotEq:
		return "<>"
	case OpLt:
		return "<"
	case OpLte:
		return "<="
	case OpGt:
		return ">"
	case OpGte:
		return ">="
	case OpLike:
		return "LIKE"
	case OpNotLike:
		return "NOT LIKE"
	case OpGlob:
		return "GLOB"
	case OpNotGlob:
		return "NOT GLOB"
	case OpRegexp:
		return "REGEXP"
	case OpNotRegexp:
		return "NOT REGEXP"
	case OpMatch:
		return "MATCH"
	case OpNotMatch:
		return "NOT MATCH"
	case OpIs:
		return "IS"
	case OpIsNot:
		return "IS NOT"
	case OpIsDistinctFrom:
		return "IS DISTINCT FROM"
	case OpIsNotDistinctFrom:
		return "IS NOT DISTINCT FROM"
	case OpIsNull:
		return "IS NULL"
	case OpIsNotNull:
		return "IS NOT NULL"
	case OpBetween:
		return "BETWEEN"
	case OpNotBetween:
		return "NOT BETWEEN"
	case OpIn:
		return "IN"
	case OpNotIn:
		return "NOT IN"
	default:
		return ""
	}
}

// Predicate is one conjunct of a WHERE or ON clause. When Op != OpNone it is a
// structured comparison on "<Table>.<Column>" that may be pushable to a source.
// Otherwise only Raw is meaningful (the original text of the conjunct).
//
// The right-hand side depends on Op:
//   - single-value ops (=, <>, <, <=, >, >=, LIKE, GLOB, REGEXP, MATCH and their
//     NOT forms, IS, IS NOT, IS [NOT] DISTINCT FROM): Value is set, Values nil.
//   - IN / NOT IN: Values holds the list, Value nil.
//   - BETWEEN / NOT BETWEEN: Values holds [low, high], Value nil.
//   - IS NULL / IS NOT NULL: both Value and Values are nil.
type Predicate struct {
	Table  string // column's table qualifier ("" if unqualified)
	Column string
	Op     Operator
	Value  *Value  // single right-hand value (see above)
	Values []Value // multiple right-hand values: IN list, or BETWEEN [low, high]
	Raw    string  // always set: original text of the conjunct
}

// ValueKind distinguishes a literal from a bind parameter. It is an enum so
// consumers switch on the named constants rather than matching strings.
type ValueKind int

const (
	ValueLiteral ValueKind = iota // a literal value (e.g. 5, 'abc')
	ValueBind                     // a bind parameter (e.g. ?, :id)
)

func (v ValueKind) String() string {
	switch v {
	case ValueLiteral:
		return "literal"
	case ValueBind:
		return "bind"
	default:
		return "unknown"
	}
}

// Value is the right-hand side of a simple comparison predicate: either a typed
// literal or a bind parameter.
type Value struct {
	Kind    ValueKind
	Literal *Literal // set when Kind == ValueLiteral
	Bind    string   // bind parameter token (e.g. ?, :id, @x, $y) when Kind == ValueBind
}

// LiteralKind classifies a SQL literal so consumers switch on its type rather
// than reparsing text.
type LiteralKind int

const (
	LiteralNull    LiteralKind = iota // NULL
	LiteralInteger                    // integer (incl. hex 0x…)
	LiteralFloat                      // floating point
	LiteralString                     // 'text'
	LiteralBool                       // TRUE / FALSE
	LiteralBlob                       // X'4142'
	LiteralKeyword                    // CURRENT_TIME / CURRENT_DATE / CURRENT_TIMESTAMP
)

func (l LiteralKind) String() string {
	switch l {
	case LiteralNull:
		return "null"
	case LiteralInteger:
		return "integer"
	case LiteralFloat:
		return "float"
	case LiteralString:
		return "string"
	case LiteralBool:
		return "bool"
	case LiteralBlob:
		return "blob"
	case LiteralKeyword:
		return "keyword"
	default:
		return "unknown"
	}
}

// Literal is a typed SQL literal value.
type Literal struct {
	Kind LiteralKind
	Raw  string // original source text (e.g. 21, 'paid', X'4142', CURRENT_TIMESTAMP)
	// Value is the parsed Go value matching Kind:
	//   LiteralInteger -> int64    LiteralFloat   -> float64
	//   LiteralString  -> string   LiteralBool    -> bool
	//   LiteralBlob    -> []byte   LiteralNull    -> nil
	//   LiteralKeyword -> string (the upper-cased keyword)
	//
	// Prefer the typed accessors (AsInt, AsString, …) over asserting Value.
	Value any
}

// IsNull reports whether the literal is SQL NULL.
func (l *Literal) IsNull() bool { return l != nil && l.Kind == LiteralNull }

// AsInt returns the value when the literal is an integer.
func (l *Literal) AsInt() (int64, bool) {
	v, ok := literalValue[int64](l)
	return v, ok
}

// AsFloat returns the value when the literal is a float.
func (l *Literal) AsFloat() (float64, bool) {
	v, ok := literalValue[float64](l)
	return v, ok
}

// AsString returns the value when the literal is a string.
func (l *Literal) AsString() (string, bool) {
	if l == nil || l.Kind != LiteralString {
		return "", false
	}
	v, ok := l.Value.(string)
	return v, ok
}

// AsBool returns the value when the literal is a boolean.
func (l *Literal) AsBool() (bool, bool) {
	v, ok := literalValue[bool](l)
	return v, ok
}

// AsBlob returns the bytes when the literal is a blob.
func (l *Literal) AsBlob() ([]byte, bool) {
	v, ok := literalValue[[]byte](l)
	return v, ok
}

// AsKeyword returns the keyword when the literal is one of CURRENT_TIME,
// CURRENT_DATE, or CURRENT_TIMESTAMP.
func (l *Literal) AsKeyword() (string, bool) {
	if l == nil || l.Kind != LiteralKeyword {
		return "", false
	}
	v, ok := l.Value.(string)
	return v, ok
}

func literalValue[T any](l *Literal) (T, bool) {
	var zero T
	if l == nil {
		return zero, false
	}
	v, ok := l.Value.(T)
	return v, ok
}
