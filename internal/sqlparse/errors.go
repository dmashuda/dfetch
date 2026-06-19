package sqlparse

import (
	"fmt"

	"github.com/antlr4-go/antlr/v4"
)

// Position is a location in the source SQL. Line is 1-based and Column is
// 0-based, following ANTLR's convention.
type Position struct {
	Line   int
	Column int
}

// Error describes why a query failed to parse or validate. Pos points at the
// offending location when one is known (syntax errors and most validation
// errors), and is nil otherwise (e.g. an empty query).
type Error struct {
	Pos *Position
	Msg string
}

func (e *Error) Error() string {
	if e.Pos != nil {
		return fmt.Sprintf("line %d:%d: %s", e.Pos.Line, e.Pos.Column, e.Msg)
	}
	return e.Msg
}

// posErrorf builds a positioned Error.
func posErrorf(line, column int, format string, args ...any) *Error {
	return &Error{
		Pos: &Position{Line: line, Column: column},
		Msg: fmt.Sprintf(format, args...),
	}
}

// tokenError builds a positioned Error anchored at an ANTLR token.
func tokenError(tok antlr.Token, format string, args ...any) *Error {
	return posErrorf(tok.GetLine(), tok.GetColumn(), format, args...)
}

// errorCollector captures ANTLR lexer/parser syntax errors as positioned
// Errors instead of printing them to stderr, so Parse can surface exactly
// where and why a query is malformed.
type errorCollector struct {
	*antlr.DefaultErrorListener
	errs []*Error
}

func newErrorCollector() *errorCollector {
	return &errorCollector{DefaultErrorListener: antlr.NewDefaultErrorListener()}
}

func (e *errorCollector) SyntaxError(_ antlr.Recognizer, _ any, line, column int, msg string, _ antlr.RecognitionException) {
	e.errs = append(e.errs, posErrorf(line, column, "%s", msg))
}
