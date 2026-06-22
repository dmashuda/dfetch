// Package source defines the data-source abstraction. A Connector represents one
// external system (registered under a SQL schema name, e.g. "github") and
// exposes one or more tables. Each table is loaded into the per-request SQLite
// database; the engine pushes as much of the query as it safely can into the
// connector's Scan so the source returns less data.
package source

import (
	"context"
	"errors"
	"fmt"

	"github.com/dmashuda/dfetch/internal/sqlparse"
)

// ErrNotImplemented is returned by connectors that are not yet wired up.
var ErrNotImplemented = errors.New("not implemented")

// Column describes a single column of a connector table.
type Column struct {
	// Name is the column name as it appears in the SQLite table.
	Name string
	// Type is the SQLite type affinity (e.g. "TEXT", "INTEGER", "REAL").
	Type string
}

// TableSchema describes one table a connector serves.
type TableSchema struct {
	Name    string
	Columns []Column
}

// ColumnNames returns the schema's column names in order.
func (t TableSchema) ColumnNames() []string {
	names := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = c.Name
	}
	return names
}

// Filter is a structured WHERE conjunct the engine offers for push-down. Value
// holds the typed literal for single-value operators (string/int64/float64/
// bool/[]byte/nil); Values holds the list for IN / the [low, high] pair for
// BETWEEN. Bind parameters are not pushed down.
type Filter struct {
	Column string
	Op     sqlparse.Operator
	Value  any
	Values []any
}

// OrderTerm is one ORDER BY term offered for push-down.
type OrderTerm struct {
	Column string
	Desc   bool
}

// ScanRequest carries the pushed-down portion of a query for one table. A
// connector may honor as little or as much as it can; the engine re-applies the
// full query in SQLite, so returning a superset is always correct.
type ScanRequest struct {
	Table   string
	Columns []string // requested columns; empty means all
	Filters []Filter
	OrderBy []OrderTerm
	Limit   *int
	Offset  *int
}

// Filter returns the first filter on the named column, or false if none.
func (r ScanRequest) Filter(column string) (Filter, bool) {
	for _, f := range r.Filters {
		if f.Column == column {
			return f, true
		}
	}
	return Filter{}, false
}

// Rows is the result of a Scan: column names plus rows whose values are ordered
// to match Columns.
type Rows struct {
	Columns []string
	Rows    [][]any
}

// Connector exposes the tables of one external system.
//
// A connector with a small, fixed set of tables returns them all from Tables().
// A dynamic source (a SQL warehouse with thousands of tables) instead returns an
// empty or curated Tables() and implements the optional SchemaDescriber and
// TableLister interfaces below, so the engine resolves only the referenced tables
// per query and discovery stays lazy.
type Connector interface {
	// Tables returns the schemas of every table this connector serves. A dynamic
	// connector may return an empty slice and rely on SchemaDescriber instead.
	Tables() []TableSchema
	// Scan fetches rows for one table, pushing down what it can from req. It
	// calls emit once per chunk (e.g. per API page) so the engine can load each
	// chunk as it arrives instead of buffering the whole result; emit returns an
	// error to abort the scan early, which Scan should propagate.
	Scan(ctx context.Context, req ScanRequest, emit func(*Rows) error) error
}

// SchemaDescriber is an optional capability: a connector that resolves one
// table's columns on demand rather than enumerating every table up front. The
// engine prefers DescribeTable over the Tables() lookup when a connector
// implements it, so a dynamic source only introspects the referenced tables.
// found is false (with a nil error) when the table does not exist.
type SchemaDescriber interface {
	DescribeTable(ctx context.Context, table string) (ts TableSchema, found bool, err error)
}

// TableLister is an optional capability for discovery: it lists table names
// (without columns) so `dfetch tables <schema>` can browse a large catalog
// without loading every column. A connector that implements it need not return
// those tables from Tables().
type TableLister interface {
	ListTables(ctx context.Context, opts ListOptions) ([]string, error)
}

// ListOptions filters a TableLister.ListTables call.
type ListOptions struct {
	// Filter is a case-insensitive substring matched against the table name;
	// empty matches all.
	Filter string
	// Limit caps the number of names returned; 0 means no limit.
	Limit int
}

// Factory builds a Connector from its config params.
type Factory func(params map[string]any) (Connector, error)

// Registry maps connector type names to their factories.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register associates a connector type name with a factory. It panics if the
// type is registered twice, since registration happens at startup.
func (r *Registry) Register(typeName string, f Factory) {
	if _, exists := r.factories[typeName]; exists {
		panic(fmt.Sprintf("connector type %q already registered", typeName))
	}
	r.factories[typeName] = f
}

// Build constructs a Connector for the given type and params.
func (r *Registry) Build(typeName string, params map[string]any) (Connector, error) {
	f, ok := r.factories[typeName]
	if !ok {
		return nil, fmt.Errorf("unknown connector type %q", typeName)
	}
	return f(params)
}

// StringList reads params[key] as a list of strings, tolerating the shapes YAML
// produces: a []string, a []any of strings, or a single string. It returns nil
// when the key is absent or not string-like. Dynamic connectors use it for the
// scoping convention (e.g. params["schemas"], params["tables"]).
func StringList(params map[string]any, key string) []string {
	switch v := params[key].(type) {
	case []string:
		return v
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
