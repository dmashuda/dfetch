package sqlparse

import (
	"strings"

	"github.com/dmashuda/dfetch/internal/sqlparse/gen"
)

// tableCollector walks a parsed statement and records the base tables it
// references along with the CTE names it defines and the columns it references.
// The external tables to fetch are the referenced tables minus the CTE names
// (CTEs are defined inline) and minus table-valued functions (e.g. json_each),
// which are not data sources.
type tableCollector struct {
	*gen.BaseSQLiteParserListener
	refs map[string]struct{}
	cte  map[string]struct{}
	cols map[string]struct{}
}

func newTableCollector() *tableCollector {
	return &tableCollector{
		BaseSQLiteParserListener: &gen.BaseSQLiteParserListener{},
		refs:                     map[string]struct{}{},
		cte:                      map[string]struct{}{},
		cols:                     map[string]struct{}{},
	}
}

func (c *tableCollector) EnterCommon_table_expression(ctx *gen.Common_table_expressionContext) {
	if n := ctx.Cte_table_name(); n != nil {
		if tn := n.Table_name(); tn != nil {
			c.cte[unquoteIdent(tn.GetText())] = struct{}{}
		}
	}
}

func (c *tableCollector) EnterTable_or_subquery(ctx *gen.Table_or_subqueryContext) {
	// Only the plain-table alternative is a data source. Skip table-valued
	// functions, parenthesized joins, and subqueries.
	if ctx.Table_name() != nil && ctx.Table_function_name() == nil {
		c.refs[unquoteIdent(ctx.Table_name().GetText())] = struct{}{}
	}
}

// The grammar uses two column-reference rules: qualified refs (table.column)
// and join/USING lists use column_name, while bare columns inside expressions
// use column_name_excluding_string. Collect both.

func (c *tableCollector) EnterColumn_name(ctx *gen.Column_nameContext) {
	c.cols[unquoteIdent(ctx.GetText())] = struct{}{}
}

func (c *tableCollector) EnterColumn_name_excluding_string(ctx *gen.Column_name_excluding_stringContext) {
	c.cols[unquoteIdent(ctx.GetText())] = struct{}{}
}

// external returns the referenced tables that are not CTE-defined.
func (c *tableCollector) external() []string {
	out := make([]string, 0, len(c.refs))
	for name := range c.refs {
		if _, isCTE := c.cte[name]; isCTE {
			continue
		}
		out = append(out, name)
	}
	return out
}

// columns returns the referenced column names. This is a flat set across the
// whole query; mapping each column to a specific table is future work.
func (c *tableCollector) columns() []string {
	out := make([]string, 0, len(c.cols))
	for name := range c.cols {
		out = append(out, name)
	}
	return out
}

// unquoteIdent strips SQLite identifier quoting: "x", `x`, [x], and 'x'.
// Doubled quote characters inside a quoted identifier are unescaped.
func unquoteIdent(s string) string {
	if len(s) < 2 {
		return s
	}
	switch q := s[0]; q {
	case '"', '`', '\'':
		if s[len(s)-1] == q {
			inner := s[1 : len(s)-1]
			return strings.ReplaceAll(inner, string([]byte{q, q}), string(q))
		}
	case '[':
		if s[len(s)-1] == ']' {
			return s[1 : len(s)-1]
		}
	}
	return s
}
