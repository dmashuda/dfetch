// Package localdb manages the per-request local SQLite database that dfetch
// loads data sources into and resolves the final query against.
package localdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/XSAM/otelsql"
	"github.com/dmashuda/dfetch/internal/source"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	// Registered for its side effect: the "sqlite3" database/sql driver.
	_ "github.com/mattn/go-sqlite3"
)

// DB is a per-request local SQLite database. It is backed by temporary files on
// disk (under a per-request temp directory) rather than memory, so large result
// sets spill to disk instead of consuming RAM; the directory is removed on Close.
//
// Every operation runs on a single pinned connection: attached databases are a
// per-connection property in SQLite, so the schemas attached on one connection
// are invisible to another. Pinning keeps them all on the same connection.
type DB struct {
	db       *sql.DB
	conn     *sql.Conn
	dir      string // per-request temp dir holding the db files; removed on Close
	attached map[string]struct{}
}

// Result holds the columns and rows produced by a resolved query.
type Result struct {
	Columns []string
	Rows    [][]any
}

// Open creates a fresh per-request SQLite database backed by a temporary file on
// disk and pins a connection to it. The temp file (and any attached-schema files)
// live under a per-request directory that Close removes.
func Open(ctx context.Context) (*DB, error) {
	dir, err := os.MkdirTemp("", "dfetch-localdb-")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir for local db: %w", err)
	}

	// otelsql wraps the sqlite3 driver so each statement becomes a span (a no-op
	// until a tracer provider is installed).
	db, err := otelsql.Open("sqlite3", filepath.Join(dir, "main.db"),
		otelsql.WithAttributes(semconv.DBSystemNameKey.String("sqlite")))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	db.SetMaxOpenConns(1)

	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		_ = db.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &DB{db: db, conn: conn, dir: dir, attached: map[string]struct{}{}}, nil
}

// Attach attaches a fresh database, backed by a temporary file in the per-request
// directory, under the given schema name so a schema-qualified table (e.g.
// github.issues) can be created. It is idempotent; an empty schema (the default
// "main") is a no-op.
func (db *DB) Attach(ctx context.Context, schema string) error {
	if schema == "" || schema == "main" {
		return nil
	}
	if _, ok := db.attached[schema]; ok {
		return nil
	}
	// Name the file by attach index, not the schema, so arbitrary schema names
	// never have to be valid (or unique) filenames.
	path := filepath.Join(db.dir, fmt.Sprintf("schema_%d.db", len(db.attached)))
	stmt := fmt.Sprintf("ATTACH DATABASE '%s' AS %s", strings.ReplaceAll(path, "'", "''"), quoteIdent(schema))
	if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
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

// maxBindParams is a conservative cap on bind parameters per statement
// (SQLite's classic SQLITE_MAX_VARIABLE_NUMBER), used to size insert batches.
const maxBindParams = 999

// Insert loads rows into a previously created table in batched multi-row INSERTs.
// Each row's values must be ordered to match cols.
func (db *DB) Insert(ctx context.Context, schema, table string, cols []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		if len(row) != len(cols) {
			return fmt.Errorf("row has %d values, table %q has %d columns", len(row), table, len(cols))
		}
	}

	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}
	prefix := fmt.Sprintf("INSERT INTO %s (%s) VALUES ", qualify(schema, table), strings.Join(quoted, ", "))
	rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"

	// As many rows per statement as fit under the bind-parameter cap.
	batchRows := maxBindParams / len(cols)
	if batchRows < 1 {
		batchRows = 1
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for start := 0; start < len(rows); start += batchRows {
		end := min(start+batchRows, len(rows))
		batch := rows[start:end]

		values := strings.TrimSuffix(strings.Repeat(rowPlaceholder+",", len(batch)), ",")
		args := make([]any, 0, len(batch)*len(cols))
		for _, row := range batch {
			args = append(args, row...)
		}
		if _, err := tx.ExecContext(ctx, prefix+values, args...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("inserting into %s: %w", qualify(schema, table), err)
		}
	}
	return tx.Commit()
}

// Query runs the resolved SQL against the local database and returns the rows,
// with []byte values normalized to strings. Optional args are bound as query
// parameters (e.g. sql.Named for :name binds).
func (db *DB) Query(ctx context.Context, query string, args ...any) (*Result, error) {
	rows, err := db.conn.QueryContext(ctx, query, args...)
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

// Close releases the pinned connection and the database, then removes the
// temporary directory holding the on-disk db files.
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
	if db.dir != "" {
		if rmErr := os.RemoveAll(db.dir); err == nil {
			err = rmErr
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
