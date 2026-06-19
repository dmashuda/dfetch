// Package source defines the data source abstraction. Each source is exposed
// as a single SQLite table that dfetch loads into a per-request local database.
package source

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotImplemented is returned by stub sources that are not yet wired up.
var ErrNotImplemented = errors.New("not implemented")

// Column describes a single column of a source-backed table.
type Column struct {
	// Name is the column name as it appears in the SQLite table.
	Name string
	// Type is the SQLite type affinity (e.g. "TEXT", "INTEGER", "REAL").
	Type string
}

// TableSchema describes the SQLite table a source backs.
type TableSchema struct {
	Name    string
	Columns []Column
}

// Source produces the schema and rows for a single table.
type Source interface {
	// Schema describes the SQLite table this source backs.
	Schema(ctx context.Context) (TableSchema, error)
	// Fetch yields the rows to load into the local SQLite table. Each inner
	// slice is one row, with values ordered to match Schema().Columns.
	Fetch(ctx context.Context) (rows [][]any, err error)
}

// Factory builds a Source from a table name and source-specific params.
type Factory func(table string, params map[string]any) (Source, error)

// Registry maps source type names to their factories.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register associates a source type name with a factory. It panics if the
// type is registered twice, since registration happens at startup.
func (r *Registry) Register(typeName string, f Factory) {
	if _, exists := r.factories[typeName]; exists {
		panic(fmt.Sprintf("source type %q already registered", typeName))
	}
	r.factories[typeName] = f
}

// Build constructs a Source for the given type, table, and params.
func (r *Registry) Build(typeName, table string, params map[string]any) (Source, error) {
	f, ok := r.factories[typeName]
	if !ok {
		return nil, fmt.Errorf("unknown source type %q", typeName)
	}
	return f(table, params)
}

// DefaultRegistry returns a registry with all built-in source types registered.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("csv", NewCSVSource)
	return r
}
