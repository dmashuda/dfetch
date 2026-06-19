// Package sqlparse parses and validates SQLite-syntax queries and reports the
// table names a query references, so the engine knows which sources to fetch.
package sqlparse

import "errors"

// ErrNotImplemented is returned until real parsing is wired up.
var ErrNotImplemented = errors.New("not implemented")

// ParseAndValidate validates the SQL (SQLite syntax) and returns the set of
// referenced table names. It is a stub pending implementation.
func ParseAndValidate(sql string) (tables []string, err error) {
	return nil, ErrNotImplemented
}
