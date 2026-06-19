// Package localdb manages the per-request local SQLite database that dfetch
// loads data sources into and resolves the final query against.
package localdb

import (
	"context"
	"database/sql"
	"errors"

	"github.com/dmashuda/dfetch/internal/source"
	// Registered for its side effect: the "sqlite3" database/sql driver.
	_ "github.com/mattn/go-sqlite3"
)

// ErrNotImplemented is returned by stub methods pending implementation.
var ErrNotImplemented = errors.New("not implemented")

// DB is a per-request local SQLite database.
type DB struct {
	conn *sql.DB
}

// Result holds the columns and rows produced by a resolved query.
type Result struct {
	Columns []string
	Rows    [][]any
}

// Open creates a fresh per-request in-memory SQLite database. The schema/load
// and query methods below are still stubs.
func Open(ctx context.Context) (*DB, error) {
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &DB{conn: conn}, nil
}

// CreateTable creates a table matching the given schema. Stub.
func (db *DB) CreateTable(ctx context.Context, schema source.TableSchema) error {
	return ErrNotImplemented
}

// Insert loads rows into a previously created table. Stub.
func (db *DB) Insert(ctx context.Context, table string, rows [][]any) error {
	return ErrNotImplemented
}

// Query runs the resolved SQL against the local database. Stub.
func (db *DB) Query(ctx context.Context, sql string) (*Result, error) {
	return nil, ErrNotImplemented
}

// Close releases the database and any temp files.
func (db *DB) Close() error {
	if db.conn == nil {
		return nil
	}
	return db.conn.Close()
}
