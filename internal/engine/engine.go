// Package engine orchestrates a dfetch query: parse the SQL, resolve each
// referenced schema to a connector, fetch and load each table into a per-request
// local SQLite database (pushing down as much of the query as is safe), then
// resolve the original query against it.
package engine

import (
	"context"
	"fmt"
	"sync"

	"github.com/dmashuda/dfetch/internal/config"
	"github.com/dmashuda/dfetch/internal/localdb"
	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/source/github"
	"github.com/dmashuda/dfetch/internal/source/jaeger"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/dmashuda/dfetch/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
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
	"jaeger": jaeger.New,
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

	q, err := parseQuery(ctx, sql)
	if err != nil {
		return nil, recordErr(span, err)
	}

	db, err := localdb.Open(ctx)
	if err != nil {
		return nil, recordErr(span, fmt.Errorf("opening local database: %w", err))
	}
	defer func() { _ = db.Close() }()

	// Resolve every source and create its table up front, serially: Attach and
	// the attached-schema map are not goroutine-safe.
	resolved, err := e.resolveSources(collectSources(q.Stmt))
	if err != nil {
		return nil, recordErr(span, err)
	}
	for _, r := range resolved {
		if err := db.Attach(ctx, r.src.Schema); err != nil {
			return nil, recordErr(span, err)
		}
		if err := db.CreateTable(ctx, r.src.Schema, r.ts); err != nil {
			return nil, recordErr(span, err)
		}
	}

	// Stream each source's pages concurrently, loading every chunk as it arrives.
	if err := e.streamSources(ctx, db, q.Stmt, resolved); err != nil {
		return nil, recordErr(span, err)
	}

	res, err := db.Query(ctx, q.Raw)
	if err != nil {
		return nil, recordErr(span, fmt.Errorf("resolving query: %w", err))
	}
	return &Result{Columns: res.Columns, Rows: res.Rows}, nil
}

// parseQuery parses sql inside its own "engine.parse" span, annotated with what
// the parser understood — the referenced tables, table/column counts, and the
// query's shape (sources, joins, WHERE conjuncts, ORDER BY, LIMIT, DISTINCT, and
// whether the AST fully models the query). These make a trace useful for
// debugging why a query planned or pushed down the way it did.
func parseQuery(ctx context.Context, sql string) (*sqlparse.Query, error) {
	_, span := telemetry.Tracer().Start(ctx, "engine.parse")
	defer span.End()

	q, err := sqlparse.Parse(sql)
	if err != nil {
		return nil, recordErr(span, err)
	}

	attrs := []attribute.KeyValue{
		attribute.StringSlice("dfetch.parse.tables", q.Tables),
		attribute.Int("dfetch.parse.table_count", len(q.Tables)),
		attribute.Int("dfetch.parse.column_count", len(q.Columns)),
	}
	if s := q.Stmt; s != nil {
		attrs = append(attrs,
			attribute.Int("dfetch.parse.source_count", len(s.From)),
			attribute.Int("dfetch.parse.join_count", len(s.Joins)),
			attribute.Int("dfetch.parse.where_predicate_count", len(s.Where)),
			attribute.Int("dfetch.parse.order_by_count", len(s.OrderBy)),
			attribute.Bool("dfetch.parse.has_limit", s.Limit != nil),
			attribute.Bool("dfetch.parse.distinct", s.Distinct),
			// Complete is false when the parser dropped clauses the AST doesn't
			// model (GROUP BY/HAVING, CTEs, compound UNION/…): a useful signal that
			// push-down planning saw only part of the query.
			attribute.Bool("dfetch.parse.fully_modeled", s.Complete),
		)
	}
	span.SetAttributes(attrs...)
	return q, nil
}

// recordErr marks span as failed and returns err unchanged, for inline use.
func recordErr(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return err
}

// maxConcurrentFetches caps how many sources are fetched at once so a query over
// many tables doesn't open an unbounded number of API connections.
const maxConcurrentFetches = 8

// resolvedSource is a source matched to its connector and table schema, ready to
// be created and streamed into the local database.
type resolvedSource struct {
	src  sqlparse.Source
	conn source.Connector
	ts   source.TableSchema
}

// resolveSources maps every source to its connector and table schema, in order.
func (e *Engine) resolveSources(sources []sqlparse.Source) ([]resolvedSource, error) {
	out := make([]resolvedSource, len(sources))
	for i, src := range sources {
		conn, ts, err := e.resolveSource(src)
		if err != nil {
			return nil, err
		}
		out[i] = resolvedSource{src: src, conn: conn, ts: ts}
	}
	return out, nil
}

// streamSources scans every source concurrently and loads each emitted chunk
// into the local database as it arrives. Chunk inserts are serialized with mu
// because localdb runs on a single pinned connection. The first error cancels the
// remaining scans.
func (e *Engine) streamSources(ctx context.Context, db *localdb.DB, stmt *sqlparse.Select, resolved []resolvedSource) error {
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentFetches)

	for _, r := range resolved {
		r := r
		g.Go(func() error {
			// Wrap the connector call so its HTTP and insert spans nest under it.
			scanCtx, scanSpan := telemetry.Tracer().Start(gctx, "connector.scan",
				trace.WithAttributes(
					attribute.String("db.namespace", r.src.Schema),
					attribute.String("db.collection.name", r.src.Name),
				))
			defer scanSpan.End()

			err := r.conn.Scan(scanCtx, planScan(stmt, r.src, r.ts), func(chunk *source.Rows) error {
				mu.Lock()
				defer mu.Unlock()
				return db.Insert(scanCtx, r.src.Schema, r.src.Name, chunk.Columns, chunk.Rows)
			})
			if err != nil {
				return fmt.Errorf("scanning %s.%s: %w", r.src.Schema, r.src.Name, err)
			}
			return nil
		})
	}
	return g.Wait()
}

// resolveSource maps a schema-qualified source to its connector and table schema.
func (e *Engine) resolveSource(src sqlparse.Source) (source.Connector, source.TableSchema, error) {
	if src.Schema == "" {
		return nil, source.TableSchema{}, fmt.Errorf("table %q has no schema; qualify it with a connector schema (e.g. github.%s)", src.Name, src.Name)
	}
	conn, ok := e.connectors[src.Schema]
	if !ok {
		return nil, source.TableSchema{}, fmt.Errorf("no connector for schema %q", src.Schema)
	}
	ts, ok := tableSchema(conn, src.Name)
	if !ok {
		return nil, source.TableSchema{}, fmt.Errorf("connector %q has no table %q", src.Schema, src.Name)
	}
	return conn, ts, nil
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
