// Package postgres is a dfetch Connector backed by a PostgreSQL database over
// database/sql (jackc/pgx). It is a dynamic source: rather than declaring tables
// up front it lists/describes them on demand from information_schema, and it
// pushes a real SELECT (filters, ordering, LIMIT, and column projection) to the
// server. One connector maps one Postgres schema (default "public"); expose more
// schemas by registering more sources. It is config-only — there is no default
// DSN — so it is registered for `type: postgres` but never auto-instantiated.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/dmashuda/dfetch/internal/source"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

const (
	defaultSchema  = "public"
	defaultMaxRows = 100000 // cap on a scan whose LIMIT can't be pushed safely
	batchSize      = 1000   // rows per emitted chunk
)

// Connector queries one schema of a PostgreSQL database.
type Connector struct {
	db      *sql.DB
	schema  string
	maxRows int

	// colCache memoizes information_schema.columns lookups so a single query
	// doesn't pay two round-trips (the engine's DescribeTable at planning time
	// and Scan's own lookup). dfetch builds a connector per process and table
	// shapes are stable within a run, so a process-lifetime cache is safe.
	mu       sync.Mutex
	colCache map[string][]columnInfo
}

// New builds a Postgres connector. The DSN comes from params["dsn"], else
// $DFETCH_POSTGRES_DSN or $DATABASE_URL (a missing DSN is an error — this
// connector is only built from config). Other params: "schema" (default
// "public") and "max_rows" (cap on an un-pushable-LIMIT scan; default 100000).
func New(params map[string]any) (source.Connector, error) {
	dsn := firstEnv("DFETCH_POSTGRES_DSN", "DATABASE_URL")
	if v, ok := params["dsn"].(string); ok && v != "" {
		dsn = v
	}
	if dsn == "" {
		return nil, fmt.Errorf("postgres: no DSN configured; set params.dsn or $DFETCH_POSTGRES_DSN")
	}

	// otelsql wraps the pgx driver so each statement becomes a span (a no-op
	// until a tracer provider is installed) — same as internal/localdb.
	db, err := otelsql.Open("pgx", dsn,
		otelsql.WithAttributes(semconv.DBSystemNameKey.String("postgresql")))
	if err != nil {
		return nil, fmt.Errorf("postgres: opening database: %w", err)
	}

	c := &Connector{db: db, schema: defaultSchema, maxRows: defaultMaxRows, colCache: map[string][]columnInfo{}}
	if s, ok := params["schema"].(string); ok && s != "" {
		c.schema = s
	}
	if n, ok := intParam(params["max_rows"]); ok && n > 0 {
		c.maxRows = n
	}
	return c, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func intParam(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// Tables returns nil: tables are discovered on demand (ListTables/DescribeTable).
func (c *Connector) Tables() []source.TableSchema { return nil }

// ListTables returns the base table and view names in the connector's schema,
// optionally filtered by a case-insensitive substring.
func (c *Connector) ListTables(ctx context.Context, opts source.ListOptions) ([]string, error) {
	q := `SELECT table_name FROM information_schema.tables
	      WHERE table_schema = $1 AND table_type IN ('BASE TABLE', 'VIEW')`
	args := []any{c.schema}
	if opts.Filter != "" {
		q += ` AND table_name ILIKE '%' || $2 || '%'`
		args = append(args, opts.Filter)
	}
	q += ` ORDER BY table_name`
	if opts.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, opts.Limit) //nolint:gosec // G202: LIMIT is an int, not a user-controlled string
	}

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: listing tables in %q: %w", c.schema, err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// DescribeTable resolves one table's columns from information_schema. found is
// false (nil error) when the table does not exist in the connector's schema.
func (c *Connector) DescribeTable(ctx context.Context, table string) (source.TableSchema, bool, error) {
	cols, err := c.columns(ctx, table)
	if err != nil {
		return source.TableSchema{}, false, err
	}
	if len(cols) == 0 {
		return source.TableSchema{}, false, nil
	}
	return source.TableSchema{Name: table, Columns: cols}, true, nil
}

// columnInfo is one column's name and its raw Postgres data_type.
type columnInfo struct {
	name   string
	pgType string
}

// columnInfos returns the table's columns in ordinal order with their raw
// Postgres types (used both for affinity mapping and ORDER BY safety). Results
// are memoized so resolving then scanning a table is a single round-trip.
func (c *Connector) columnInfos(ctx context.Context, table string) ([]columnInfo, error) {
	c.mu.Lock()
	cached, ok := c.colCache[table]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	cols, err := c.fetchColumnInfos(ctx, table)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.colCache[table] = cols
	c.mu.Unlock()
	return cols, nil
}

func (c *Connector) fetchColumnInfos(ctx context.Context, table string) ([]columnInfo, error) {
	const q = `SELECT column_name, data_type FROM information_schema.columns
	           WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`
	rows, err := c.db.QueryContext(ctx, q, c.schema, table)
	if err != nil {
		return nil, fmt.Errorf("postgres: describing %s.%s: %w", c.schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var cols []columnInfo
	for rows.Next() {
		var ci columnInfo
		if err := rows.Scan(&ci.name, &ci.pgType); err != nil {
			return nil, err
		}
		cols = append(cols, ci)
	}
	return cols, rows.Err()
}

// columns maps the table's columns to dfetch columns with SQLite affinities.
func (c *Connector) columns(ctx context.Context, table string) ([]source.Column, error) {
	infos, err := c.columnInfos(ctx, table)
	if err != nil {
		return nil, err
	}
	cols := make([]source.Column, len(infos))
	for i, ci := range infos {
		cols[i] = source.Column{Name: ci.name, Type: pgTypeToAffinity(ci.pgType)}
	}
	return cols, nil
}

// Scan runs the pushed-down SELECT and emits rows in batches.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	cols, err := c.columnInfos(ctx, req.Table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf("postgres: no table %q in schema %q", req.Table, c.schema)
	}
	colType := make(map[string]string, len(cols))
	for _, col := range cols {
		colType[col.name] = col.pgType
	}

	query, args, capped := buildSelect(c.schema, req, colType, c.maxRows)
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("postgres: scanning %s.%s: %w", c.schema, req.Table, err)
	}
	defer func() { _ = rows.Close() }()

	colNames, err := rows.Columns()
	if err != nil {
		return err
	}

	batch := make([][]any, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := emit(&source.Rows{Columns: colNames, Rows: batch}); err != nil {
			return err
		}
		batch = make([][]any, 0, batchSize)
		return nil
	}
	total := 0
	for rows.Next() {
		cells := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i := range cells {
			cells[i] = normalize(cells[i])
		}
		batch = append(batch, cells)
		total++
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	// Only warn when the maxRows safety cap (not a pushed user LIMIT) bounded the
	// scan and the result filled it — a strong sign more rows were left behind.
	if capped && total >= c.maxRows {
		return emit(source.Warn("postgres.%s: capped at max_rows=%d; raise the max_rows param or add a LIMIT/filters for the rest", req.Table, c.maxRows))
	}
	return nil
}

// pgTypeToAffinity maps a Postgres data_type to a SQLite column affinity. numeric/
// decimal map to REAL, which can lose precision for very large exact numbers.
func pgTypeToAffinity(dataType string) string {
	switch strings.ToLower(dataType) {
	case "smallint", "integer", "bigint", "boolean":
		return "INTEGER"
	case "real", "double precision", "numeric", "decimal":
		return "REAL"
	default:
		return "TEXT"
	}
}

// normalize coerces a database/sql scanned value into a type localdb accepts
// (string/int64/float64/bool/[]byte/nil), turning time.Time into an RFC3339 string.
func normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, int64, float64, string:
		return x
	case []byte:
		return string(x)
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float32:
		return float64(x)
	case time.Time:
		// Normalize to UTC in a fixed-width format so the text sorts
		// chronologically; this is what makes pushing ORDER BY on a timestamp
		// column safe (see orderPushSafe). Lexical == chronological needs both a
		// constant offset AND a constant width: RFC3339Nano trims trailing zeros,
		// making "…05Z" sort after "…05.5Z" ('Z' > '.'). Six fractional digits
		// cover Postgres's microsecond precision.
		return x.UTC().Format("2006-01-02T15:04:05.000000Z")
	default:
		return fmt.Sprintf("%v", x)
	}
}

// --- SQL building (pure, unit-tested) ---

// buildSelect renders the upstream SELECT and its positional args. It honors the
// superset contract: it pushes only predicates Postgres evaluates identically to
// SQLite, and pushes LIMIT only when every filter was consumed AND the ordering is
// safe (otherwise a skipped filter or divergent order could drop wanted rows).
// capped reports whether the maxRows safety cap was applied (rather than a pushed
// user LIMIT), so the caller can warn only when the cap could have truncated.
func buildSelect(schema string, req source.ScanRequest, colType map[string]string, maxRows int) (string, []any, bool) {
	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(selectList(req.Columns))
	sb.WriteString(" FROM ")
	sb.WriteString(quoteIdent(schema) + "." + quoteIdent(req.Table))

	var args []any
	where, consumedAll := buildWhere(req.Filters, &args)
	if where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(where)
	}

	order, orderOK := buildOrderBy(req.OrderBy, colType)
	pushLimit := req.Limit != nil && orderOK && consumedAll

	if pushLimit {
		if order != "" {
			sb.WriteString(" ORDER BY ")
			sb.WriteString(order)
		}
		n := *req.Limit
		if req.Offset != nil {
			n += *req.Offset // fetch limit+offset; SQLite re-applies OFFSET verbatim
		}
		fmt.Fprintf(&sb, " LIMIT %d", n)
	} else {
		fmt.Fprintf(&sb, " LIMIT %d", maxRows) // cap an otherwise unbounded scan
	}
	return sb.String(), args, !pushLimit
}

func selectList(cols []string) string {
	if len(cols) == 0 {
		return "*"
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}
	return strings.Join(quoted, ", ")
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// buildWhere translates the pushable filters into a parameterized WHERE clause and
// reports whether every filter was consumed (untranslatable ones are left for
// SQLite, which makes pushing LIMIT unsafe).
func buildWhere(filters []source.Filter, args *[]any) (clause string, consumedAll bool) {
	consumedAll = true
	var clauses []string
	for _, f := range filters {
		c, ok := translateFilter(f, args)
		if !ok {
			consumedAll = false
			continue
		}
		clauses = append(clauses, c)
	}
	return strings.Join(clauses, " AND "), consumedAll
}

// translateFilter renders one filter as SQL, or ok=false when it can't be pushed
// safely. Only operators Postgres and SQLite evaluate identically are pushed:
// LIKE differs in case-sensitivity, and GLOB/REGEXP/MATCH have no PG equivalent,
// so those are left for SQLite.
func translateFilter(f source.Filter, args *[]any) (string, bool) {
	col := quoteIdent(f.Column)
	ph := func(v any) string {
		*args = append(*args, v)
		return fmt.Sprintf("$%d", len(*args))
	}
	switch f.Op {
	case source.OpEq:
		return col + " = " + ph(f.Value), true
	case source.OpNotEq:
		return col + " <> " + ph(f.Value), true
	case source.OpLt:
		return col + " < " + ph(f.Value), true
	case source.OpLte:
		return col + " <= " + ph(f.Value), true
	case source.OpGt:
		return col + " > " + ph(f.Value), true
	case source.OpGte:
		return col + " >= " + ph(f.Value), true
	case source.OpIn, source.OpNotIn:
		if len(f.Values) == 0 {
			return "", false
		}
		ps := make([]string, len(f.Values))
		for i, v := range f.Values {
			ps[i] = ph(v)
		}
		op := "IN"
		if f.Op == source.OpNotIn {
			op = "NOT IN"
		}
		return col + " " + op + " (" + strings.Join(ps, ", ") + ")", true
	case source.OpBetween, source.OpNotBetween:
		if len(f.Values) != 2 {
			return "", false
		}
		op := "BETWEEN"
		if f.Op == source.OpNotBetween {
			op = "NOT BETWEEN"
		}
		return col + " " + op + " " + ph(f.Values[0]) + " AND " + ph(f.Values[1]), true
	default:
		return "", false
	}
}

// buildOrderBy renders the ORDER BY clause and reports whether LIMIT may ride it.
// colType maps a column to its raw Postgres type. LIMIT is safe only when every
// key is an order-safe type (so Postgres and SQLite order identically) with NULL
// placement aligned to SQLite's (NULLs sort lowest: ASC -> NULLS FIRST, DESC ->
// NULLS LAST). An order-unsafe (text/numeric/unknown) key makes it unsafe, so the
// caller falls back to the row cap and lets SQLite sort.
func buildOrderBy(terms []source.OrderTerm, colType map[string]string) (clause string, ok bool) {
	if len(terms) == 0 {
		return "", true
	}
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		if !orderPushSafe(colType[t.Column]) {
			return "", false
		}
		dir, nulls := "ASC", "NULLS FIRST"
		if t.Desc {
			dir, nulls = "DESC", "NULLS LAST"
		}
		parts = append(parts, quoteIdent(t.Column)+" "+dir+" "+nulls)
	}
	return strings.Join(parts, ", "), true
}

// orderPushSafe reports whether ORDER BY on a column of this Postgres type can ride
// a pushed LIMIT — i.e. Postgres orders it identically to how SQLite orders the
// value dfetch stores. Safe: integers (stored INTEGER), float (REAL), booleans,
// and temporal types (stored as UTC RFC3339 text, whose lexical order matches
// chronological). Unsafe and therefore excluded: text/varchar/char and uuid
// (collation differs from SQLite's BINARY), numeric/decimal (REAL can lose the
// precision that decides a tie), and anything unknown.
func orderPushSafe(pgType string) bool {
	switch strings.ToLower(pgType) {
	case "smallint", "integer", "bigint",
		"real", "double precision",
		"boolean",
		"timestamp", "timestamp without time zone", "timestamp with time zone",
		"date", "time", "time without time zone", "time with time zone":
		return true
	default:
		return false
	}
}
