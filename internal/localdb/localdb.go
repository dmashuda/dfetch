// Package localdb manages the per-request local SQLite database that dfetch
// loads data sources into and resolves the final query against.
package localdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dmashuda/dfetch/internal/source"
	// Registered for its side effect: the "sqlite3" database/sql driver.
	_ "github.com/mattn/go-sqlite3"
)

// DB is a per-request local SQLite database. Every operation runs on a single
// pinned connection: with an in-memory SQLite database each connection is a
// separate database, so attachments and tables created on one connection are
// invisible to another. Pinning keeps them all in the same database.
type DB struct {
	db       *sql.DB
	conn     *sql.Conn
	attached map[string]struct{}
}

// Result holds the columns and rows produced by a resolved query.
type Result struct {
	Columns []string
	Rows    [][]any
}

// Open creates a fresh per-request in-memory SQLite database and pins a
// connection to it.
func Open(ctx context.Context) (*DB, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, err
	}
	return &DB{db: db, conn: conn, attached: map[string]struct{}{}}, nil
}

// Attach attaches a fresh in-memory database under the given schema name so a
// schema-qualified table (e.g. github.issues) can be created. It is idempotent;
// an empty schema (the default "main") is a no-op.
func (db *DB) Attach(ctx context.Context, schema string) error {
	if schema == "" || schema == "main" {
		return nil
	}
	if _, ok := db.attached[schema]; ok {
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, "ATTACH DATABASE ':memory:' AS "+quoteIdent(schema)); err != nil {
		return fmt.Errorf("attaching schema %q: %w", schema, err)
	}
	db.attached[schema] = struct{}{}
	return nil
}

// CreateTable creates a table matching the schema under the given attached
// schema name ("" for the default database).
func (db *DB) CreateTable(ctx context.Context, schema string, ts source.TableSchema) error {
	defs := make([]string, len(ts.Columns))
	for i, c := range ts.Columns {
		typ := c.Type
		if typ == "" {
			typ = "TEXT"
		}
		defs[i] = quoteIdent(c.Name) + " " + typ
	}
	stmt := fmt.Sprintf("CREATE TABLE %s (%s)", qualify(schema, ts.Name), strings.Join(defs, ", "))
	if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("creating table %s: %w", qualify(schema, ts.Name), err)
	}
	return nil
}

// Insert loads rows into a previously created table. Each row's values must be
// ordered to match cols.
func (db *DB) Insert(ctx context.Context, schema, table string, cols []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	stmtSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		qualify(schema, table), strings.Join(quoted, ", "), placeholders)

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, stmtSQL)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, row := range rows {
		if len(row) != len(cols) {
			_ = tx.Rollback()
			return fmt.Errorf("row has %d values, table %q has %d columns", len(row), table, len(cols))
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("inserting into %s: %w", qualify(schema, table), err)
		}
	}
	return tx.Commit()
}

// Query runs the resolved SQL against the local database and returns the rows,
// with []byte values normalized to strings.
func (db *DB) Query(ctx context.Context, query string) (*Result, error) {
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var out [][]any
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range cells {
			if b, ok := v.([]byte); ok {
				cells[i] = string(b)
			}
		}
		out = append(out, cells)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &Result{Columns: cols, Rows: out}, nil
}

// Close releases the pinned connection and the database.
func (db *DB) Close() error {
	var err error
	if db.conn != nil {
		err = db.conn.Close()
	}
	if db.db != nil {
		if dbErr := db.db.Close(); err == nil {
			err = dbErr
		}
	}
	return err
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func qualify(schema, table string) string {
	if schema == "" || schema == "main" {
		return quoteIdent(table)
	}
	return quoteIdent(schema) + "." + quoteIdent(table)
}
