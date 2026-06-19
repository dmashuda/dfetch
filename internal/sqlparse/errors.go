package sqlparse

import (
	"fmt"

	"github.com/antlr4-go/antlr/v4"
)

// errorCollector captures ANTLR lexer/parser syntax errors instead of printing
// them to stderr, so Parse can surface them as a single Go error.
type errorCollector struct {
	*antlr.DefaultErrorListener
	msgs []string
}

func newErrorCollector() *errorCollector {
	return &errorCollector{DefaultErrorListener: antlr.NewDefaultErrorListener()}
}

func (e *errorCollector) SyntaxError(_ antlr.Recognizer, _ any, line, column int, msg string, _ antlr.RecognitionException) {
	e.msgs = append(e.msgs, fmt.Sprintf("line %d:%d %s", line, column, msg))
}
