// Package engine orchestrates a dfetch query: parse the SQL, resolve each
// referenced schema to a connector, fetch and load each table into a per-request
// local SQLite database (pushing down as much of the query as is safe), then
// resolve the original query against it.
package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/dmashuda/dfetch/internal/telemetry"
	"github.com/dmashuda/dfetch/source"
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
	openDB     OpenDBFunc
}

// New builds an Engine from the given options. Options apply in order, and the
// last registration of a schema name wins — so registering the default set
// first and a config's sources after lets config override a built-in schema.
//
// New carries no default connectors: the connectors package provides dfetch's
// built-in set (connectors.DefaultOptions). An engine with no connectors is
// valid; every query then fails with "no connector for schema". Typed sources
// (WithSources/WithConfig) are built at New time via the WithRegistry registry.
func New(opts ...Option) (*Engine, error) {
	s := settings{registry: source.NewRegistry(), openDB: defaultOpenDB}
	for _, o := range opts {
		o(&s)
	}

	connectors := make(map[string]source.Connector)
	for _, op := range s.ops {
		if op.conn != nil {
			connectors[op.name] = op.conn
			continue
		}
		c, err := s.registry.Build(op.src.Type, op.src.Params)
		if err != nil {
			return nil, fmt.Errorf("connector %q: %w", op.src.Name, err)
		}
		connectors[op.src.Name] = c
	}

	return &Engine{connectors: connectors, openDB: s.openDB}, nil
}

// SchemaSummary is one schema's entry in the top-level `dfetch tables` listing.
type SchemaSummary struct {
	Schema     string
	TableCount int  // number of tables, or -1 when a dynamic source couldn't be listed
	Dynamic    bool // true when the connector lists/describes tables on demand
}

// SchemaSummaries returns one summary per connector schema (sorted), for the
// top-level `dfetch tables` view. A dynamic source's count comes from listing its
// table names; if that fails (e.g. the source is unreachable) the count is -1
// rather than failing the whole listing.
func (e *Engine) SchemaSummaries(ctx context.Context) []SchemaSummary {
	out := make([]SchemaSummary, 0, len(e.connectors))
	for schema, conn := range e.connectors {
		s := SchemaSummary{Schema: schema}
		if lister, ok := conn.(source.TableLister); ok {
			s.Dynamic = true
			names, err := lister.ListTables(ctx, source.ListOptions{})
			if err != nil {
				s.TableCount = -1
			} else {
				s.TableCount = len(names)
			}
		} else {
			s.TableCount = len(conn.Tables())
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Schema < out[j].Schema })
	return out
}

// ListTables returns the table names served under schema, filtered by a
// case-insensitive substring. A dynamic connector (TableLister) lists on demand;
// a static one lists from Tables().
func (e *Engine) ListTables(ctx context.Context, schema, filter string) ([]string, error) {
	conn, ok := e.connectors[schema]
	if !ok {
		return nil, fmt.Errorf("no connector for schema %q", schema)
	}
	if lister, ok := conn.(source.TableLister); ok {
		return lister.ListTables(ctx, source.ListOptions{Filter: filter})
	}
	var names []string
	for _, ts := range conn.Tables() {
		if filter == "" || strings.Contains(strings.ToLower(ts.Name), strings.ToLower(filter)) {
			names = append(names, ts.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// DescribeTable returns the column schema of one table, resolving it on demand
// for dynamic connectors (SchemaDescriber) and from Tables() otherwise.
func (e *Engine) DescribeTable(ctx context.Context, schema, table string) (source.TableSchema, error) {
	conn, ok := e.connectors[schema]
	if !ok {
		return source.TableSchema{}, fmt.Errorf("no connector for schema %q", schema)
	}
	return resolveTable(ctx, conn, schema, table)
}

// Run executes the full pipeline for a SQL query (SQLite syntax).
func (e *Engine) Run(ctx context.Context, query string) (*Result, error) {
	return e.RunWithParams(ctx, query, nil)
}

// RunWithParams executes the full pipeline for a SQL query (SQLite syntax),
// binding params as named SQLite parameters (referenced as :name in the SQL).
// A nil or empty params map runs the query with no bound parameters.
//
// The params are also handed to the push-down planner: the planner resolves a
// bind-parameter RHS (e.g. `service_name = :service`) to its value so the filter
// can be pushed to the connector, while the final query keeps the :name bind for
// SQLite. Without this, connectors that require a filter value at fetch time
// (jaeger.spans needs service_name or trace_id, github.pulls needs owner/repo)
// would never see the value, since a bind is opaque until SQLite executes.
func (e *Engine) RunWithParams(ctx context.Context, query string, params map[string]any) (*Result, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "engine.Run",
		trace.WithAttributes(semconv.DBQueryText(query)))
	defer span.End()

	q, err := parseQuery(ctx, query)
	if err != nil {
		return nil, recordErr(span, fmt.Errorf("parsing SQL: %w", err))
	}

	db, err := e.openDB(ctx)
	if err != nil {
		return nil, recordErr(span, fmt.Errorf("opening local database: %w", err))
	}
	defer func() { _ = db.Close() }()

	// Resolve every source and create its table up front, serially: Attach and
	// the attached-schema map are not goroutine-safe.
	resolved, err := e.resolveSources(ctx, collectSources(q.Stmt))
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
	warnings, err := e.streamSources(ctx, db, q.Stmt, resolved, params)
	if err != nil {
		return nil, recordErr(span, err)
	}

	res, err := db.Query(ctx, q.Raw, namedArgs(params)...)
	if err != nil {
		return nil, recordErr(span, fmt.Errorf("executing query on the local database: %w", err))
	}
	return &Result{Columns: res.Columns, Rows: res.Rows, Warnings: warnings}, nil
}

// namedArgs converts a params map into sql.Named bind arguments. The order is
// irrelevant: named parameters bind by name, not position. Returns nil for an
// empty map so Query is called with no extra args.
func namedArgs(params map[string]any) []any {
	if len(params) == 0 {
		return nil
	}
	args := make([]any, 0, len(params))
	for name, val := range params {
		args = append(args, sql.Named(name, val))
	}
	return args
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
		attrs = append(
			attrs,
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
func (e *Engine) resolveSources(ctx context.Context, sources []sqlparse.Source) ([]resolvedSource, error) {
	out := make([]resolvedSource, len(sources))
	for i, src := range sources {
		conn, ts, err := e.resolveSource(ctx, src)
		if err != nil {
			return nil, err
		}
		out[i] = resolvedSource{src: src, conn: conn, ts: ts}
	}
	return out, nil
}

// streamSources scans every source concurrently and loads each emitted chunk
// into the local database as it arrives. Chunk inserts are serialized with mu
// because the DB is not required to be safe for concurrent writes (localdb
// runs on a single pinned connection). The first error cancels the remaining
// scans.
func (e *Engine) streamSources(ctx context.Context, db DB, stmt *sqlparse.Select, resolved []resolvedSource, params map[string]any) ([]string, error) {
	var mu sync.Mutex
	var warnings []string
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

			err := r.conn.Scan(scanCtx, planScan(stmt, r.src, r.ts, params), func(chunk *source.Rows) error {
				mu.Lock()
				defer mu.Unlock()
				if len(chunk.Warnings) > 0 {
					warnings = append(warnings, chunk.Warnings...)
					for _, w := range chunk.Warnings {
						scanSpan.AddEvent("result.truncated", trace.WithAttributes(attribute.String("warning", w)))
					}
				}
				if len(chunk.Rows) == 0 {
					return nil // warning-only or empty chunk: nothing to insert
				}
				return db.Insert(scanCtx, r.src.Schema, r.src.Name, chunk.Columns, chunk.Rows)
			})
			if err != nil {
				return fmt.Errorf("fetching %s.%s: %w", r.src.Schema, r.src.Name, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return warnings, nil
}

// resolveSource maps a schema-qualified source to its connector and table schema.
func (e *Engine) resolveSource(ctx context.Context, src sqlparse.Source) (source.Connector, source.TableSchema, error) {
	if src.Schema == "" {
		return nil, source.TableSchema{}, fmt.Errorf("table %q has no schema; qualify it with a connector schema (e.g. github.%s)", src.Name, src.Name)
	}
	conn, ok := e.connectors[src.Schema]
	if !ok {
		return nil, source.TableSchema{}, fmt.Errorf("no connector for schema %q", src.Schema)
	}
	ts, err := resolveTable(ctx, conn, src.Schema, src.Name)
	if err != nil {
		return nil, source.TableSchema{}, err
	}
	return conn, ts, nil
}

// resolveTable returns the column schema of conn's named table, preferring an
// on-demand SchemaDescriber.DescribeTable (dynamic connectors) and falling back
// to the Tables() listing (static connectors, or a dynamic connector that also
// serves curated static tables).
func resolveTable(ctx context.Context, conn source.Connector, schema, name string) (source.TableSchema, error) {
	if d, ok := conn.(source.SchemaDescriber); ok {
		ts, found, err := d.DescribeTable(ctx, name)
		if err != nil {
			return source.TableSchema{}, fmt.Errorf("describing %s.%s: %w", schema, name, err)
		}
		if found {
			return ts, nil
		}
	}
	if ts, ok := tableSchema(conn, name); ok {
		return ts, nil
	}
	return source.TableSchema{}, fmt.Errorf("connector %q has no table %q", schema, name)
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
