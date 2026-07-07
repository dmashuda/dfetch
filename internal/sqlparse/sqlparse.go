// Package sqlparse lexes, parses, and validates the incoming SQL (SQLite
// syntax) and reports the external tables a query references, so the engine
// knows which data sources to fetch. It performs syntactic validation only;
// authoritative semantic validation happens when the query runs against the
// per-request SQLite database (see localdb).
package sqlparse

//go:generate ./scripts/gen-parser.sh

import (
	"sort"

	"github.com/antlr4-go/antlr/v4"
	"github.com/dmashuda/dfetch/internal/sqlparse/gen"
)

// Query is the validated result of parsing an incoming SQL statement.
type Query struct {
	// Raw is the original SQL, run verbatim against the local database later.
	Raw string
	// Tables are the external base tables the query references: referenced
	// tables minus CTE names and table-valued functions, deduped and sorted,
	// with identifiers unquoted.
	Tables []string
	// Columns are the column names the query references, deduped and sorted,
	// with identifiers unquoted. This is a flat set across the whole query;
	// mapping each column to a specific table is future work. SELECT * yields
	// no columns (the set of columns is not known syntactically).
	Columns []string
	// Stmt is the structured AST of the query, built for planning what to fetch
	// per source and which filters/projections can be pushed down. Tables and
	// Columns above are exhaustive flat views; Stmt models the query structure
	// (see ast.go for what is and isn't yet represented).
	Stmt *Select
}

// Parse lexes, parses, and validates a single read-only SELECT statement and
// returns the external tables it references. Syntax errors, non-SELECT
// statements, and multiple statements yield an error.
func Parse(raw string) (*Query, error) {
	errs := newErrorCollector()

	lexer := gen.NewSQLiteLexer(antlr.NewInputStream(raw))
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(errs)

	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := gen.NewSQLiteParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(errs)

	tree := p.Parse()
	if len(errs.errs) > 0 {
		// Return the first syntax error: it is the most precise, and later
		// ANTLR errors are usually cascading noise from recovery.
		return nil, errs.errs[0]
	}

	sel, err := validateReadOnlySelect(tree)
	if err != nil {
		return nil, err
	}

	c := newTableCollector()
	antlr.NewParseTreeWalker().Walk(c, tree)

	q := &Query{Raw: raw, Stmt: buildSelect(sel)}
	if tables := c.external(); len(tables) > 0 {
		sort.Strings(tables)
		q.Tables = tables
	}
	if cols := c.columns(); len(cols) > 0 {
		sort.Strings(cols)
		q.Columns = cols
	}
	return q, nil
}

// validateReadOnlySelect ensures the parse tree holds exactly one statement and
// that it is a plain (non-EXPLAIN) SELECT, returning that select statement.
// It reports where the offending construct is when one exists.
func validateReadOnlySelect(tree gen.IParseContext) (gen.ISelect_stmtContext, error) {
	list := tree.Sql_stmt_list()
	if list == nil {
		return nil, &Error{Msg: "no SQL statement found"}
	}
	stmts := list.AllSql_stmt()
	switch len(stmts) {
	case 0:
		return nil, &Error{Msg: "no SQL statement found"}
	case 1:
		// ok
	default:
		// Point at the start of the second statement.
		return nil, tokenError(stmts[1].GetStart(), "only a single statement is supported, found %d", len(stmts))
	}

	st := stmts[0]
	if ex := st.EXPLAIN_(); ex != nil {
		return nil, tokenError(ex.GetSymbol(), "EXPLAIN is not supported; only read-only SELECT statements are allowed")
	}
	sel := st.Select_stmt()
	if sel == nil {
		return nil, tokenError(st.GetStart(), "only read-only SELECT statements are supported")
	}
	return sel, nil
}
