package sqlparse

import (
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/dmashuda/dfetch/internal/sqlparse/gen"
)

// buildSelect translates a parsed select_stmt into dfetch's typed AST. Only the
// first select_core is modeled; compound tails (UNION/…), WITH bodies, and the
// trailing ORDER BY/LIMIT are not yet represented (see ast.go). Select.Complete
// records whether any such clause was dropped.
func buildSelect(stmt gen.ISelect_stmtContext) *Select {
	cores := stmt.AllSelect_core()
	if len(cores) == 0 {
		return &Select{}
	}
	s := buildCore(cores[0])
	// The query is fully represented only if the core modeled everything it saw
	// and the statement has no WITH, compound tail, ORDER BY, or LIMIT.
	s.Complete = s.Complete &&
		stmt.With_clause() == nil &&
		len(cores) == 1 &&
		stmt.Order_clause() == nil &&
		stmt.Limit_clause() == nil
	return s
}

func buildCore(core gen.ISelect_coreContext) *Select {
	s := &Select{Distinct: core.DISTINCT_() != nil}

	for _, rc := range core.AllResult_column() {
		s.Projections = append(s.Projections, buildProjection(rc))
	}
	if jc := core.Join_clause(); jc != nil {
		s.From, s.Joins = buildJoinClause(jc)
	}
	if w := core.GetWhere_expr(); w != nil {
		s.Where = buildPredicates(w)
	}
	// GROUP BY / HAVING / WINDOW are not modeled; their presence makes the AST
	// an incomplete representation of the query. An incomplete subquery source
	// also makes this select incomplete, since rendering it would drop the
	// subquery's unmodeled clauses.
	s.Complete = core.GROUP_() == nil && core.HAVING_() == nil && core.WINDOW_() == nil
	for _, src := range s.From {
		if src.Subquery != nil && !src.Subquery.Complete {
			s.Complete = false
		}
	}
	return s
}

func buildProjection(rc gen.IResult_columnContext) Projection {
	if rc.STAR() != nil {
		p := Projection{Star: true}
		if tn := rc.Table_name(); tn != nil {
			p.Table = unquoteIdent(tn.GetText())
		}
		return p
	}

	p := Projection{}
	if ca := rc.Column_alias(); ca != nil {
		p.Alias = unquoteIdent(ca.GetText())
	}
	if e := rc.Expr(); e != nil {
		if op := classify(e); op.isCol {
			p.Table, p.Column = op.table, op.col
		} else {
			p.Expr = origText(e)
		}
	}
	return p
}

// buildJoinClause walks the join_clause children in order so each optional
// join_constraint is attached to the correct join operator.
func buildJoinClause(jc gen.IJoin_clauseContext) ([]Source, []Join) {
	var (
		sources []Source
		joins   []Join
		pending *Join
	)
	flush := func() {
		if pending != nil {
			joins = append(joins, *pending)
			pending = nil
		}
	}
	for _, ch := range jc.GetChildren() {
		switch c := ch.(type) {
		case gen.ITable_or_subqueryContext:
			sources = append(sources, buildSource(c))
		case gen.IJoin_operatorContext:
			flush()
			j := buildJoinOperator(c)
			pending = &j
		case gen.IJoin_constraintContext:
			if pending != nil {
				applyJoinConstraint(pending, c)
			}
		}
	}
	flush()
	return sources, joins
}

func buildSource(tos gen.ITable_or_subqueryContext) Source {
	switch {
	case tos.Table_name() != nil && tos.Table_function_name() == nil:
		src := Source{Name: unquoteIdent(tos.Table_name().GetText()), Alias: sourceAlias(tos)}
		if sc := tos.Schema_name(); sc != nil {
			src.Schema = unquoteIdent(sc.GetText())
		}
		return src
	case tos.Select_stmt() != nil:
		return Source{Subquery: buildSelect(tos.Select_stmt()), Alias: sourceAlias(tos)}
	default:
		// Table-valued functions and parenthesized joins are preserved as text.
		return Source{Raw: origText(tos), Alias: sourceAlias(tos)}
	}
}

// sourceAlias returns a source's alias, which the grammar expresses either as
// `AS alias` (table_alias) or as a bare `alias` (table_alias_excluding_joins).
func sourceAlias(tos gen.ITable_or_subqueryContext) string {
	if al := tos.Table_alias(); al != nil {
		return unquoteIdent(al.GetText())
	}
	if al := tos.Table_alias_excluding_joins(); al != nil {
		return unquoteIdent(al.GetText())
	}
	return ""
}

func buildJoinOperator(op gen.IJoin_operatorContext) Join {
	j := Join{Type: JoinInner, Natural: op.NATURAL_() != nil}
	switch {
	case op.COMMA() != nil:
		j.Type = JoinComma
	case op.LEFT_() != nil:
		j.Type = JoinLeft
	case op.RIGHT_() != nil:
		j.Type = JoinRight
	case op.FULL_() != nil:
		j.Type = JoinFull
	case op.CROSS_() != nil:
		j.Type = JoinCross
	}
	return j
}

func applyJoinConstraint(j *Join, jc gen.IJoin_constraintContext) {
	switch {
	case jc.ON_() != nil && jc.Expr() != nil:
		j.On = buildPredicates(jc.Expr())
	case jc.USING_() != nil:
		for _, cn := range jc.AllColumn_name() {
			j.Using = append(j.Using, unquoteIdent(cn.GetText()))
		}
	}
}

// buildPredicates splits a WHERE/ON expression into its top-level AND-ed
// conjuncts, structuring each as a simple comparison when possible. If the
// expression is a top-level OR, it is kept whole as a single raw predicate.
func buildPredicates(expr gen.IExprContext) []Predicate {
	or := expr.Expr_or()
	if or == nil {
		return nil
	}
	ands := or.AllExpr_and()
	if len(ands) != 1 {
		return []Predicate{{Raw: origText(expr)}}
	}
	conj := ands[0].AllExpr_not()
	out := make([]Predicate, 0, len(conj))
	for _, n := range conj {
		out = append(out, buildPredicateFromNot(n))
	}
	return out
}

func buildPredicateFromNot(n gen.IExpr_notContext) Predicate {
	raw := origText(n)
	if len(n.AllNOT_()) > 0 {
		return Predicate{Raw: raw}
	}
	return buildPredicateFromBinary(n.Expr_binary(), raw)
}

func buildPredicateFromBinary(bin gen.IExpr_binaryContext, raw string) Predicate {
	comps := bin.AllExpr_comparison()
	// A single leading NOT inside expr_binary negates LIKE/GLOB/REGEXP/MATCH/IN/
	// BETWEEN/IS. (The prefix NOT of `NOT (expr)` lives at expr_not, not here.)
	not := len(bin.AllNOT_()) > 0

	// Postfix null tests: `x ISNULL`, `x NOTNULL`, `x NOT NULL`.
	if len(comps) == 1 {
		switch {
		case len(bin.AllISNULL_()) > 0:
			return nullPredicate(comps[0], OpIsNull, raw)
		case len(bin.AllNOTNULL_()) > 0, len(bin.AllNULL_()) > 0:
			return nullPredicate(comps[0], OpIsNotNull, raw)
		}
		// No operator at this level: descend for the relational operators.
		return buildPredicateFromComparison(comps[0], raw)
	}

	// Equality (=, <>).
	if op := equalityOp(bin); op != OpNone && len(comps) == 2 {
		return comparison(comps[0], comps[1], op, raw)
	}

	// Pattern match: LIKE / GLOB / REGEXP / MATCH (+ NOT). An ESCAPE clause adds
	// a third operand; only the simple two-operand form is structured.
	if op := patternOp(bin, not); op != OpNone {
		if len(comps) == 2 {
			return comparison(comps[0], comps[1], op, raw)
		}
		return Predicate{Raw: raw}
	}

	// IS / IS NOT / IS NULL / IS [NOT] DISTINCT FROM.
	if len(bin.AllIS_()) > 0 && len(comps) == 2 {
		return isPredicate(bin, comps[0], comps[1], not, raw)
	}

	// BETWEEN / NOT BETWEEN: [tested, low, high].
	if len(bin.AllBETWEEN_()) > 0 && len(comps) == 3 {
		return betweenPredicate(comps[0], comps[1], comps[2], not, raw)
	}

	// IN / NOT IN (value-list form only; subquery/table forms stay raw).
	if len(bin.AllIN_()) > 0 {
		return inPredicate(bin, comps, not, raw)
	}

	return Predicate{Raw: raw}
}

// ifNot picks the negated operator when not is set.
func ifNot(not bool, affirmative, negated Operator) Operator {
	if not {
		return negated
	}
	return affirmative
}

func patternOp(bin gen.IExpr_binaryContext, not bool) Operator {
	switch {
	case len(bin.AllLIKE_()) > 0:
		return ifNot(not, OpLike, OpNotLike)
	case len(bin.AllGLOB_()) > 0:
		return ifNot(not, OpGlob, OpNotGlob)
	case len(bin.AllREGEXP_()) > 0:
		return ifNot(not, OpRegexp, OpNotRegexp)
	case len(bin.AllMATCH_()) > 0:
		return ifNot(not, OpMatch, OpNotMatch)
	}
	return OpNone
}

func isPredicate(bin gen.IExpr_binaryContext, lhs, rhs antlr.Tree, not bool, raw string) Predicate {
	distinct := len(bin.AllDISTINCT_()) > 0 && len(bin.AllFROM_()) > 0
	if !distinct {
		// `x IS NULL` / `x IS NOT NULL`.
		if r := classify(rhs); r.isVal && r.val.Literal.IsNull() {
			return nullPredicate(lhs, ifNot(not, OpIsNull, OpIsNotNull), raw)
		}
	}
	op := ifNot(not, OpIs, OpIsNot)
	if distinct {
		op = ifNot(not, OpIsDistinctFrom, OpIsNotDistinctFrom)
	}
	return comparison(lhs, rhs, op, raw)
}

func betweenPredicate(tested, low, high antlr.Tree, not bool, raw string) Predicate {
	t, lo, hi := classify(tested), classify(low), classify(high)
	if t.isCol && lo.isVal && hi.isVal {
		return Predicate{
			Table:  t.table,
			Column: t.col,
			Op:     ifNot(not, OpBetween, OpNotBetween),
			Values: []Value{*lo.val, *hi.val},
			Raw:    raw,
		}
	}
	return Predicate{Raw: raw}
}

func inPredicate(bin gen.IExpr_binaryContext, comps []gen.IExpr_comparisonContext, not bool, raw string) Predicate {
	// Only the parenthesized value-list form is structured; IN (subquery),
	// IN table, and IN table_function(...) are left raw.
	if len(bin.AllOPEN_PAR()) == 0 ||
		len(bin.AllSelect_stmt()) > 0 ||
		len(bin.AllTable_name()) > 0 ||
		len(bin.AllTable_function_name()) > 0 ||
		len(comps) == 0 {
		return Predicate{Raw: raw}
	}
	tested := classify(comps[0])
	if !tested.isCol {
		return Predicate{Raw: raw}
	}
	values := make([]Value, 0, len(comps)-1)
	for _, item := range comps[1:] {
		c := classify(item)
		if !c.isVal {
			return Predicate{Raw: raw}
		}
		values = append(values, *c.val)
	}
	return Predicate{
		Table:  tested.table,
		Column: tested.col,
		Op:     ifNot(not, OpIn, OpNotIn),
		Values: values,
		Raw:    raw,
	}
}

func buildPredicateFromComparison(comp gen.IExpr_comparisonContext, raw string) Predicate {
	bits := comp.AllExpr_bitwise()
	if op := relationalOp(comp); op != OpNone && len(bits) == 2 {
		return comparison(bits[0], bits[1], op, raw)
	}
	return Predicate{Raw: raw}
}

func equalityOp(bin gen.IExpr_binaryContext) Operator {
	switch {
	case len(bin.AllEQ()) > 0 || len(bin.AllASSIGN()) > 0:
		return OpEq
	case len(bin.AllNOT_EQ1()) > 0 || len(bin.AllNOT_EQ2()) > 0:
		return OpNotEq
	}
	return OpNone
}

func relationalOp(comp gen.IExpr_comparisonContext) Operator {
	switch {
	case len(comp.AllLT_EQ()) > 0:
		return OpLte
	case len(comp.AllGT_EQ()) > 0:
		return OpGte
	case len(comp.AllLT()) > 0:
		return OpLt
	case len(comp.AllGT()) > 0:
		return OpGt
	}
	return OpNone
}

func nullPredicate(node antlr.Tree, op Operator, raw string) Predicate {
	if o := classify(node); o.isCol {
		return Predicate{Table: o.table, Column: o.col, Op: op, Raw: raw}
	}
	return Predicate{Raw: raw}
}

// comparison builds a "column op value" predicate from two operands in either
// order, flipping the operator when the column is on the right.
func comparison(left, right antlr.Tree, op Operator, raw string) Predicate {
	l, r := classify(left), classify(right)
	switch {
	case l.isCol && r.isVal:
		return Predicate{Table: l.table, Column: l.col, Op: op, Value: r.val, Raw: raw}
	case l.isVal && r.isCol:
		// The column is on the right: structuring it requires putting the column
		// first, which is only valid for operators whose operands commute (after
		// mirroring relational ones). Non-commutative pattern operators
		// (LIKE/GLOB/REGEXP/MATCH) would be inverted, so keep them as raw text.
		if !commutable(op) {
			return Predicate{Raw: raw}
		}
		return Predicate{Table: r.table, Column: r.col, Op: flipOp(op), Value: l.val, Raw: raw}
	default:
		return Predicate{Raw: raw}
	}
}

// commutable reports whether an operator can be rewritten with its operands
// swapped (relational ops via flipOp, equality/identity ops unchanged). Pattern
// operators are not commutative and must not be flipped.
func commutable(op Operator) bool {
	switch op {
	case OpLike, OpNotLike, OpGlob, OpNotGlob, OpRegexp, OpNotRegexp, OpMatch, OpNotMatch:
		return false
	default:
		return true
	}
}

func flipOp(op Operator) Operator {
	switch op {
	case OpLt:
		return OpGt
	case OpLte:
		return OpGte
	case OpGt:
		return OpLt
	case OpGte:
		return OpLte
	default: // symmetric ops (=, <>, IS, IS [NOT] DISTINCT FROM) are unchanged
		return op
	}
}

// operand is a classified comparison operand: a column reference or a value.
type operand struct {
	isCol bool
	table string
	col   string
	isVal bool
	val   *Value
}

// classify descends through the single-child precedence chain of an expression
// operand to its base and reports whether it is a plain column or a literal/bind
// value. Anything else (function calls, arithmetic, parenthesized exprs) yields
// the zero operand.
func classify(node antlr.Tree) operand {
	base, ok := soleBase(node)
	if !ok {
		return operand{}
	}
	switch {
	case base.Literal_value() != nil:
		return operand{isVal: true, val: &Value{Kind: ValueLiteral, Literal: buildLiteral(base.Literal_value())}}
	case base.BIND_PARAMETER() != nil:
		return operand{isVal: true, val: &Value{Kind: ValueBind, Bind: base.BIND_PARAMETER().GetText()}}
	case base.Column_name_excluding_string() != nil:
		return operand{isCol: true, col: unquoteIdent(base.Column_name_excluding_string().GetText())}
	case base.Table_name() != nil && base.Column_name() != nil:
		return operand{isCol: true, table: unquoteIdent(base.Table_name().GetText()), col: unquoteIdent(base.Column_name().GetText())}
	}
	return operand{}
}

// buildLiteral classifies a literal_value node and parses it into a typed Go
// value (see Literal).
func buildLiteral(lv gen.ILiteral_valueContext) *Literal {
	raw := lv.GetText()
	switch {
	case lv.NULL_() != nil:
		return &Literal{Kind: LiteralNull, Raw: raw}
	case lv.TRUE_() != nil:
		return &Literal{Kind: LiteralBool, Raw: raw, Value: true}
	case lv.FALSE_() != nil:
		return &Literal{Kind: LiteralBool, Raw: raw, Value: false}
	case lv.STRING_LITERAL() != nil:
		return &Literal{Kind: LiteralString, Raw: raw, Value: unquoteString(raw)}
	case lv.BLOB_LITERAL() != nil:
		return &Literal{Kind: LiteralBlob, Raw: raw, Value: decodeBlob(raw)}
	case lv.NUMERIC_LITERAL() != nil:
		kind, val := parseNumeric(raw)
		return &Literal{Kind: kind, Raw: raw, Value: val}
	default: // CURRENT_TIME / CURRENT_DATE / CURRENT_TIMESTAMP
		return &Literal{Kind: LiteralKeyword, Raw: raw, Value: strings.ToUpper(raw)}
	}
}

// parseNumeric parses a SQLite numeric literal into an int64 or float64.
func parseNumeric(s string) (LiteralKind, any) {
	if ls := strings.ToLower(s); strings.HasPrefix(ls, "0x") {
		if n, err := strconv.ParseInt(ls[2:], 16, 64); err == nil {
			return LiteralInteger, n
		}
		if u, err := strconv.ParseUint(ls[2:], 16, 64); err == nil {
			return LiteralInteger, int64(u)
		}
	}
	if !strings.ContainsAny(s, ".eE") {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return LiteralInteger, n
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return LiteralFloat, f
	}
	return LiteralFloat, nil
}

// unquoteString turns a SQL string literal ('it”s') into its Go value (it's).
func unquoteString(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

// decodeBlob decodes a SQL blob literal (X'4142') into raw bytes.
func decodeBlob(s string) []byte {
	if len(s) >= 3 && (s[0] == 'x' || s[0] == 'X') && s[1] == '\'' && s[len(s)-1] == '\'' {
		if b, err := hex.DecodeString(s[2 : len(s)-1]); err == nil {
			return b
		}
	}
	return nil
}

// soleBase follows the single-child precedence chain (expr_or → expr_and → … →
// expr_base) down to the base node. If any level has more than one child (an
// operator, NOT, or parenthesized list), the operand is not a plain column or
// value and soleBase reports false.
func soleBase(t antlr.Tree) (*gen.Expr_baseContext, bool) {
	for {
		if b, ok := t.(*gen.Expr_baseContext); ok {
			return b, true
		}
		if t.GetChildCount() != 1 {
			return nil, false
		}
		t = t.GetChild(0)
	}
}

// origText returns the original source text of a parser rule, preserving the
// spacing between tokens (unlike GetText, which concatenates them).
func origText(ctx antlr.ParserRuleContext) string {
	start, stop := ctx.GetStart(), ctx.GetStop()
	if start == nil || stop == nil {
		return ctx.GetText()
	}
	cs := start.GetInputStream()
	if cs == nil {
		return ctx.GetText()
	}
	return cs.GetTextFromInterval(antlr.NewInterval(start.GetStart(), stop.GetStop()))
}
