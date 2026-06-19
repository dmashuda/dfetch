// Package engine orchestrates a dfetch query: parse/validate the SQL, resolve
// referenced tables to configured sources, fetch and load each into a
// per-request local SQLite database, then resolve the query against it.
package engine

import (
	"context"
	"errors"

	"github.com/dmashuda/dfetch/internal/config"
	"github.com/dmashuda/dfetch/internal/source"
)

// ErrNotImplemented is returned while the engine is a stub.
var ErrNotImplemented = errors.New("not implemented")

// Engine resolves dfetch queries against configured data sources.
type Engine struct {
	cfg      *config.Config
	registry *source.Registry
}

// New builds an Engine from config.
func New(cfg *config.Config) (*Engine, error) {
	return &Engine{cfg: cfg, registry: source.NewRegistry()}, nil
}

// Run executes the full pipeline for a SQL query (SQLite syntax):
//
//  1. Parse and validate the SQL, extracting referenced table names.
//  2. Resolve each referenced table to a configured source.
//  3. Open a fresh per-request local SQLite database.
//  4. For each source: create its table and load its rows.
//  5. Resolve the original query against the local database.
//
// It is currently a stub.
func (e *Engine) Run(ctx context.Context, sql string) (*Result, error) {
	return nil, ErrNotImplemented
}
