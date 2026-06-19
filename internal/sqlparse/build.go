package sqlparse

import (
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/dmashuda/dfetch/internal/sqlparse/gen"
)

// buildSelect translates a parsed select_stmt into dfetch's typed AST. Only the
// first select_core is modeled; compound tails (UNION/…), WITH bodies, and the
// trailing ORDER BY/LIMIT are not yet represented (see ast.go).
func buildSelect(stmt gen.ISelect_stmtContext) *Select {
	cores := stmt.AllSelect_core()
	if len(cores) == 0 {
		return &Select{}
	}
	return buildCore(cores[0])
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

	// IS NULL / IS NOT NULL (the `expr ISNULL` / `expr NOTNULL` / `expr NOT NULL` forms).
	if len(comps) == 1 {
		if len(bin.AllISNULL_()) > 0 {
			return nullPredicate(comps[0], "IS NULL", raw)
		}
		if len(bin.AllNOTNULL_()) > 0 || len(bin.AllNULL_()) > 0 {
			return nullPredicate(comps[0], "IS NOT NULL", raw)
		}
		// No binary operator at this level: descend to the comparison level for
		// the relational operators (<, <=, >, >=).
		return buildPredicateFromComparison(comps[0], raw)
	}

	if len(comps) == 2 {
		if op := equalityOp(bin); op != "" {
			return comparison(comps[0], comps[1], op, raw)
		}
		if len(bin.AllLIKE_()) > 0 {
			return comparison(comps[0], comps[1], "LIKE", raw)
		}
		// x IS NULL / x IS NOT NULL (the `IS NULL` form; the bare ISNULL/NOTNULL
		// keywords are handled in the single-operand branch above).
		if len(bin.AllIS_()) > 0 {
			if r := classify(comps[1]); r.isVal && r.val.Kind == ValueLiteral && strings.EqualFold(r.val.Text, "null") {
				op := "IS NULL"
				if len(bin.AllNOT_()) > 0 {
					op = "IS NOT NULL"
				}
				return nullPredicate(comps[0], op, raw)
			}
		}
	}
	return Predicate{Raw: raw}
}

func buildPredicateFromComparison(comp gen.IExpr_comparisonContext, raw string) Predicate {
	bits := comp.AllExpr_bitwise()
	if op := relationalOp(comp); op != "" && len(bits) == 2 {
		return comparison(bits[0], bits[1], op, raw)
	}
	return Predicate{Raw: raw}
}

func equalityOp(bin gen.IExpr_binaryContext) string {
	switch {
	case len(bin.AllEQ()) > 0 || len(bin.AllASSIGN()) > 0:
		return "="
	case len(bin.AllNOT_EQ1()) > 0 || len(bin.AllNOT_EQ2()) > 0:
		return "<>"
	}
	return ""
}

func relationalOp(comp gen.IExpr_comparisonContext) string {
	switch {
	case len(comp.AllLT_EQ()) > 0:
		return "<="
	case len(comp.AllGT_EQ()) > 0:
		return ">="
	case len(comp.AllLT()) > 0:
		return "<"
	case len(comp.AllGT()) > 0:
		return ">"
	}
	return ""
}

func nullPredicate(node antlr.Tree, op, raw string) Predicate {
	if o := classify(node); o.isCol {
		return Predicate{Table: o.table, Column: o.col, Op: op, Raw: raw}
	}
	return Predicate{Raw: raw}
}

// comparison builds a "column op value" predicate from two operands in either
// order, flipping the operator when the column is on the right.
func comparison(left, right antlr.Tree, op, raw string) Predicate {
	l, r := classify(left), classify(right)
	switch {
	case l.isCol && r.isVal:
		return Predicate{Table: l.table, Column: l.col, Op: op, Value: r.val, Raw: raw}
	case l.isVal && r.isCol:
		return Predicate{Table: r.table, Column: r.col, Op: flipOp(op), Value: l.val, Raw: raw}
	default:
		return Predicate{Raw: raw}
	}
}

func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	default: // =, <>, LIKE are unchanged (LIKE reversed is unusual but harmless)
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
		return operand{isVal: true, val: &Value{Kind: ValueLiteral, Text: base.Literal_value().GetText()}}
	case base.BIND_PARAMETER() != nil:
		return operand{isVal: true, val: &Value{Kind: ValueBind, Text: base.BIND_PARAMETER().GetText()}}
	case base.Column_name_excluding_string() != nil:
		return operand{isCol: true, col: unquoteIdent(base.Column_name_excluding_string().GetText())}
	case base.Table_name() != nil && base.Column_name() != nil:
		return operand{isCol: true, table: unquoteIdent(base.Table_name().GetText()), col: unquoteIdent(base.Column_name().GetText())}
	}
	return operand{}
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
