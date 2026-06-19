// Package sqlparse lexes, parses, and validates the incoming SQL (SQLite
// syntax) and reports the external tables a query references, so the engine
// knows which data sources to fetch. It performs syntactic validation only;
// authoritative semantic validation happens when the query runs against the
// per-request SQLite database (see internal/localdb).
package sqlparse

//go:generate ./scripts/gen-parser.sh

import (
	"errors"
	"fmt"
	"sort"
	"strings"

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
	if len(errs.msgs) > 0 {
		return nil, fmt.Errorf("invalid SQL: %s", strings.Join(errs.msgs, "; "))
	}

	if err := validateReadOnlySelect(tree); err != nil {
		return nil, err
	}

	c := newTableCollector()
	antlr.NewParseTreeWalker().Walk(c, tree)

	q := &Query{Raw: raw}
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
// that it is a plain (non-EXPLAIN) SELECT.
func validateReadOnlySelect(tree gen.IParseContext) error {
	list := tree.Sql_stmt_list()
	if list == nil {
		return errors.New("no SQL statement found")
	}
	stmts := list.AllSql_stmt()
	switch len(stmts) {
	case 0:
		return errors.New("no SQL statement found")
	case 1:
		// ok
	default:
		return errors.New("only a single statement is supported")
	}

	st := stmts[0]
	if st.EXPLAIN_() != nil || st.Select_stmt() == nil {
		return errors.New("only read-only SELECT statements are supported")
	}
	return nil
}
