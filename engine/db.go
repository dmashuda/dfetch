package engine

import (
	"context"

	"github.com/dmashuda/dfetch/localdb"
	"github.com/dmashuda/dfetch/source"
)

// DB is the per-request local SQL database the engine loads sources into and
// resolves the final query against. localdb.DB is the default implementation;
// supply another via WithDB to control how the underlying database files are
// created and managed (e.g. in-memory, a fixed path, or a shared cache).
type DB interface {
	// Attach makes schema available so a schema-qualified table (e.g.
	// github.issues) can be created under it. Called once per referenced schema
	// before any CreateTable; must be idempotent.
	Attach(ctx context.Context, schema string) error
	// CreateTable creates an empty table matching ts under the attached schema.
	CreateTable(ctx context.Context, schema string, ts source.TableSchema) error
	// Insert loads one chunk of rows into a previously created table. Each row's
	// values are ordered to match cols. The engine serializes Insert calls.
	Insert(ctx context.Context, schema, table string, cols []string, rows [][]any) error
	// Query runs the original SQL against the loaded tables, binding args (e.g.
	// sql.Named values for :name parameters).
	Query(ctx context.Context, query string, args ...any) (*localdb.Result, error)
	// Close releases the database and any files backing it. Called once per run.
	Close() error
}

// OpenDBFunc creates the per-request DB. The engine calls it once per Run and
// closes the returned DB when the run completes.
type OpenDBFunc func(ctx context.Context) (DB, error)

// defaultOpenDB backs each run with localdb's temp-file SQLite database.
func defaultOpenDB(ctx context.Context) (DB, error) {
	return localdb.Open(ctx)
}

var _ DB = (*localdb.DB)(nil)
