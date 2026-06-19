// Package engine orchestrates a dfetch query: parse the SQL, resolve each
// referenced schema to a connector, fetch and load each table into a per-request
// local SQLite database (pushing down as much of the query as is safe), then
// resolve the original query against it.
package engine

import (
	"context"
	"fmt"

	"github.com/dmashuda/dfetch/internal/config"
	"github.com/dmashuda/dfetch/internal/localdb"
	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/source/github"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/dmashuda/dfetch/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// Engine resolves dfetch queries against configured connectors, keyed by the SQL
// schema they serve (e.g. "github").
type Engine struct {
	connectors map[string]source.Connector
}

// builtins are connector types that are available without configuration. Each
// is also registered under its own name as a schema.
var builtins = map[string]source.Factory{
	"github": github.New,
}

// New builds an Engine: the built-in connectors plus any declared in config.
func New(cfg *config.Config) (*Engine, error) {
	registry := source.NewRegistry()
	for typeName, factory := range builtins {
		registry.Register(typeName, factory)
	}

	connectors := make(map[string]source.Connector)
	for typeName, factory := range builtins {
		c, err := factory(nil)
		if err != nil {
			return nil, fmt.Errorf("initializing built-in connector %q: %w", typeName, err)
		}
		connectors[typeName] = c
	}

	for _, sc := range cfg.Sources {
		c, err := registry.Build(sc.Type, sc.Params)
		if err != nil {
			return nil, fmt.Errorf("connector %q: %w", sc.Name, err)
		}
		connectors[sc.Name] = c // config can override or add schemas
	}

	return &Engine{connectors: connectors}, nil
}

// Schemas returns each connector schema and the tables it serves, for
// discovery (e.g. the `dfetch tables` command).
func (e *Engine) Schemas() map[string][]source.TableSchema {
	out := make(map[string][]source.TableSchema, len(e.connectors))
	for schema, conn := range e.connectors {
		out[schema] = conn.Tables()
	}
	return out
}

// Run executes the full pipeline for a SQL query (SQLite syntax).
func (e *Engine) Run(ctx context.Context, sql string) (*Result, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "engine.Run",
		trace.WithAttributes(semconv.DBQueryText(sql)))
	defer span.End()

	q, err := sqlparse.Parse(sql)
	if err != nil {
		return nil, recordErr(span, err)
	}

	db, err := localdb.Open(ctx)
	if err != nil {
		return nil, recordErr(span, fmt.Errorf("opening local database: %w", err))
	}
	defer func() { _ = db.Close() }()

	for _, src := range collectSources(q.Stmt) {
		if err := e.loadSource(ctx, db, q.Stmt, src); err != nil {
			return nil, recordErr(span, err)
		}
	}

	res, err := db.Query(ctx, q.Raw)
	if err != nil {
		return nil, recordErr(span, fmt.Errorf("resolving query: %w", err))
	}
	return &Result{Columns: res.Columns, Rows: res.Rows}, nil
}

// recordErr marks span as failed and returns err unchanged, for inline use.
func recordErr(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return err
}

// loadSource fetches one source table (pushing down what it can) and loads it
// into the local database under its schema.
func (e *Engine) loadSource(ctx context.Context, db *localdb.DB, stmt *sqlparse.Select, src sqlparse.Source) error {
	ctx, span := telemetry.Tracer().Start(ctx, "engine.loadSource",
		trace.WithAttributes(
			attribute.String("db.namespace", src.Schema),
			attribute.String("db.collection.name", src.Name),
		))
	defer span.End()

	if src.Schema == "" {
		return recordErr(span, fmt.Errorf("table %q has no schema; qualify it with a connector schema (e.g. github.%s)", src.Name, src.Name))
	}
	conn, ok := e.connectors[src.Schema]
	if !ok {
		return recordErr(span, fmt.Errorf("no connector for schema %q", src.Schema))
	}
	ts, ok := tableSchema(conn, src.Name)
	if !ok {
		return recordErr(span, fmt.Errorf("connector %q has no table %q", src.Schema, src.Name))
	}

	// Wrap the connector call so its HTTP client spans nest under connector.scan.
	scanCtx, scanSpan := telemetry.Tracer().Start(ctx, "connector.scan",
		trace.WithAttributes(attribute.String("db.collection.name", src.Name)))
	rows, err := conn.Scan(scanCtx, planScan(stmt, src, ts))
	scanSpan.End()
	if err != nil {
		return recordErr(span, fmt.Errorf("scanning %s.%s: %w", src.Schema, src.Name, err))
	}

	if err := db.Attach(ctx, src.Schema); err != nil {
		return recordErr(span, err)
	}
	if err := db.CreateTable(ctx, src.Schema, ts); err != nil {
		return recordErr(span, err)
	}
	return db.Insert(ctx, src.Schema, src.Name, rows.Columns, rows.Rows)
}

// tableSchema finds the named table among a connector's tables.
func tableSchema(conn source.Connector, name string) (source.TableSchema, bool) {
	for _, ts := range conn.Tables() {
		if ts.Name == name {
			return ts, true
		}
	}
	return source.TableSchema{}, false
}

// collectSources returns the distinct base-table sources referenced anywhere in
// the statement (including subqueries), in a stable order.
func collectSources(s *sqlparse.Select) []sqlparse.Source {
	seen := map[[2]string]bool{}
	var out []sqlparse.Source
	var walk func(*sqlparse.Select)
	walk = func(sel *sqlparse.Select) {
		if sel == nil {
			return
		}
		for _, src := range sel.From {
			if src.Subquery != nil {
				walk(src.Subquery)
				continue
			}
			if src.Name == "" {
				continue // table-valued functions / raw sources
			}
			key := [2]string{src.Schema, src.Name}
			if !seen[key] {
				seen[key] = true
				out = append(out, src)
			}
		}
	}
	walk(s)
	return out
}
