# Connectors (`source`)

A **connector** is a data source adapter: it exposes one external system (the
GitHub API, a Jaeger instance, …) as one or more SQL tables under a schema name.
This package defines the connector contract; each connector lives in its own
subpackage (`source/github`, `source/jaeger`).

This document is the reference for **writing a new connector**. It is detailed on
purpose — enough that you can hand it to an agent and have it produce a sound
implementation plan. Read it top to bottom before starting; the
[Step-by-step](#step-by-step) and [Checklist](#checklist) sections at the end are
the actionable summary.

## Contents

- [Where a connector fits](#where-a-connector-fits)
- [The contract](#the-contract)
- [The query lifecycle in detail](#the-query-lifecycle-in-detail)
- [Tables and columns → SQLite](#tables-and-columns--sqlite)
- [Push-down](#push-down)
- [Streaming with `emit`](#streaming-with-emit)
- [Registration and configuration](#registration-and-configuration)
- [Observability](#observability)
- [Testing](#testing)
- [Reference implementations](#reference-implementations)
- [Invariants and gotchas](#invariants-and-gotchas)
- [Step-by-step](#step-by-step)
- [Checklist](#checklist)

## Where a connector fits

dfetch resolves a query like this (see `engine/engine.go`):

1. **Parse** the SQL into a typed AST (`internal/sqlparse`).
2. **Resolve** each schema-qualified table (`github.issues`) to the connector
   registered under that schema, and to that connector's `TableSchema`.
3. For each source: **attach** an on-disk SQLite database under the schema name
   and **create** the table from the connector's declared columns.
4. **Scan** every connector concurrently, **loading each emitted chunk** into its
   SQLite table as it arrives.
5. Run the **original SQL verbatim** against the loaded SQLite database and return
   the result.

The crucial consequence of step 5: **SQLite is the source of truth.** A connector
only has to return a _superset_ of the rows the query needs — the engine re-applies
the full `WHERE`/`JOIN`/`ORDER BY`/`LIMIT` locally. Push-down is therefore a pure
optimization (fetch less), never a correctness requirement.

## The contract

Everything is in `source/source.go`.

```go
// Connector exposes the tables of one external system.
type Connector interface {
    // Tables returns the schema of every table this connector serves.
    Tables() []TableSchema

    // Scan fetches rows for one table (req.Table), pushing down what it can from
    // req. It calls emit once per chunk (e.g. per API page) so the engine can
    // load each chunk as it arrives instead of buffering the whole result.
    // Returning an error from emit aborts the scan; Scan should propagate it.
    Scan(ctx context.Context, req ScanRequest, emit func(*Rows) error) error
}

// Factory builds a Connector from its config params (nil for a builtin).
type Factory func(params map[string]any) (Connector, error)
```

Supporting types:

```go
type Column struct {
    Name string // column name in the SQLite table
    Type string // SQLite affinity: "TEXT", "INTEGER", "REAL" (empty → TEXT)
}

type TableSchema struct {
    Name    string
    Columns []Column
}
func (t TableSchema) ColumnNames() []string // names in order

type ScanRequest struct {
    Table   string       // which table to scan (you serve one per Scan call)
    Columns []string     // projected columns; empty means all. Optional to honor.
    Filters []Filter     // WHERE conjuncts attributable to this source
    OrderBy []OrderTerm  // ORDER BY terms attributable to this source
    Limit   *int         // LIMIT, when safe to push (nil otherwise)
    Offset  *int         // OFFSET, when safe to push (nil otherwise)
}
func (r ScanRequest) Filter(column string) (Filter, bool) // first filter on column

type Filter struct {
    Column string
    Op     Operator // OpEq, OpGt, OpBetween, OpIn, … (see below)
    Value  any      // single-value ops: string/int64/float64/bool/[]byte/nil
    Values []any    // IN list, or [low, high] for BETWEEN
}

type OrderTerm struct {
    Column string
    Desc   bool
}

// Rows is one emitted chunk: column names plus rows whose values are ordered to
// match Columns.
type Rows struct {
    Columns []string
    Rows    [][]any
}
```

A connector typically also exposes a `New(params map[string]any) (source.Connector, error)`
that matches `Factory`.

## Dynamic sources (lazy schema)

Connectors with a small, fixed set of tables (the API connectors here) just
return them all from `Tables()`. A **dynamic source** — a SQL warehouse like
Postgres or Snowflake with hundreds of tables — should _not_ enumerate its whole
catalog to run one query or to power `dfetch tables`. Such a connector returns an
empty (or curated) `Tables()` and implements two optional capabilities, which the
engine detects by type assertion:

```go
// Resolve one table's columns on demand. The engine prefers this over the
// Tables() lookup, so only the tables a query references are introspected.
// found == false (nil error) means "no such table".
type SchemaDescriber interface {
    DescribeTable(ctx context.Context, table string) (ts TableSchema, found bool, err error)
}

// List table names (no columns) for discovery, optionally filtered.
type TableLister interface {
    ListTables(ctx context.Context, opts ListOptions) ([]string, error)
}

type ListOptions struct {
    Filter string // case-insensitive substring on the table name; "" = all
    Limit  int    // cap on names returned; 0 = no limit
}
```

The engine still creates each referenced table up front, so `DescribeTable` must
return the **full column list** for the table being resolved (it runs during
resolution, before `Scan`). `dfetch tables` uses these for its tiered view:
schemas + counts → `ListTables` names → `DescribeTable` columns.

**Scoping via config.** Bound what a dynamic source exposes through `params`
(no special config schema — just documented keys your `New` reads). The
conventional keys are `schemas` (namespaces to expose) and `tables` (an explicit
allowlist); parse them with the `source.StringList(params, key)` helper, which
tolerates a YAML list or a single string:

```yaml
sources:
  - name: warehouse
    type: postgres
    params:
      schemas: [public, analytics]
      tables: [public.orders, public.users] # optional allowlist
```

## The query lifecycle in detail

What the engine does around your connector (`engine/engine.go`,
`plan.go`):

- **Resolution** (`resolveSource`): the schema qualifier picks the connector;
  the table name is matched against `conn.Tables()`. An unqualified table, an
  unknown schema, or an unknown table is a user-facing error before any scan.
- **Table creation** happens up front and **serially** (attach + the
  attached-schema map are not goroutine-safe), via `localdb.CreateTable`.
- **Scanning** runs all sources **concurrently** in an `errgroup` (cap 8). Each
  `Scan` runs inside a `connector.scan` span. The `emit` callback writes the
  chunk with `localdb.Insert`, serialized by a mutex because localdb uses a
  single pinned connection. The first error cancels the rest.
- **Planning** (`planScan`) builds the `ScanRequest` for each source — see
  [Push-down](#push-down).

You implement `Tables()` and `Scan()`. You do **not** touch localdb, the engine,
or SQLite directly.

## Tables and columns → SQLite

`localdb.CreateTable` turns your `TableSchema` into `CREATE TABLE
"schema"."table" ("col" TYPE, …)`:

- `Column.Type` is used **verbatim** as the SQLite column type; empty defaults to
  `TEXT`. Use `TEXT`, `INTEGER`, or `REAL` (SQLite type affinities). No
  constraints, PKs, or `NOT NULL` are emitted.
- Pick column names that mirror the source's own field names so queries read like
  the upstream API/docs (`created_at`, `user_login`, `duration_ms`).

`localdb.Insert` loads each emitted chunk:

- Every row's value count **must equal** the table's column count, in the **same
  order** as the declared columns — a mismatch is an error. Build rows positionally.
- Accepted Go value types: `string`, `int64`, `float64`, `bool` (stored as 0/1),
  `[]byte`, and `nil` (SQL NULL). Use `nil` for absent/null fields. Prefer `int64`
  over `int` for integer columns.
- On read-back, `[]byte` results are normalized to strings.

There is no schema migration: each request creates fresh tables, so changing your
columns is free.

## Push-down

`planScan` (`engine/plan.go`) decides what to offer each source in the
`ScanRequest`. You then decide what to actually honor.

**What the planner offers:**

- **Filters** whose column belongs to the table and is _attributable_ to this
  source (qualified with the source's alias/name, or unqualified in a
  single-source query). Structured comparisons are converted to `Filter`;
  `OpNone` (unparsed), `IS NULL`, and `IS NOT NULL` are **not** offered. Bind
  parameters (`?`, `:id`) are **never** pushed. The planner also infers equality
  filters across equi-joins (e.g. `r.name = p.repo` + `p.repo = 'go'` ⇒
  `r.name = 'go'`).
- **OrderBy** terms attributable to the source.
- **Limit/Offset** only when this source drives the result (single-source query,
  or a join where the LIMIT can safely ride the driving source).

**Operators** (`source.Operator`, see `source/operator.go`): `OpEq`,
`OpNotEq`, `OpLt`, `OpLte`, `OpGt`, `OpGte`, `OpLike`/`OpNotLike`,
`OpGlob`/`OpNotGlob`, `OpRegexp`/`OpNotRegexp`, `OpMatch`/`OpNotMatch`,
`OpIs`/`OpIsNot`/`OpIsDistinctFrom`/`OpIsNotDistinctFrom`,
`OpBetween`/`OpNotBetween` (`Values = [low, high]`), `OpIn`/`OpNotIn`
(`Values = list`). For single-value ops read `Filter.Value`; for `IN`/`BETWEEN`
read `Filter.Values`.

**The golden rule:** only translate a filter/order/limit into an upstream request
parameter when doing so still returns _exactly the rows the query wants, or a
superset_. If the API can't honor something precisely, **ignore it** and let
SQLite finish the job. Concretely:

- **Equality on a column the API filters on** → safe to push.
- **A LIMIT** is only safe to push when the API returns exactly the filtered +
  ordered set — i.e. every filter was consumed by the API _and_ the ordering was
  fully honored. A multi-key `ORDER BY` the API can't reproduce must **not** push
  LIMIT, or you'll truncate rows the real order would keep. With an `OFFSET`,
  fetch `limit+offset` rows so SQLite can apply the offset.
- **A required API parameter** (GitHub's `owner`/`repo`, Jaeger's `service_name`):
  if it's missing from the filters, return a helpful error rather than fetching
  the world. Use the `requireStringEq`-style pattern.

When push-down is unsafe, cap unbounded fetches some other way (e.g. a max-pages
limit) so a broad query doesn't pull an entire dataset.

## Streaming with `emit`

`Scan` should call `emit` **once per natural chunk** — typically one API page —
rather than buffering everything. The engine inserts each chunk as it arrives, so
streaming reduces peak memory and overlaps network with local inserts.

- Emit `&source.Rows{Columns: <table column names>, Rows: <[][]any>}`. Keep
  `Columns` consistent across chunks (the engine uses the first chunk's columns).
- If `emit` returns an error, stop and propagate it (the engine is cancelling —
  e.g. another source failed).
- Skip empty chunks (don't emit a `Rows` with zero rows) — the one exception is a
  warning-only chunk from `source.Warn(...)`, which carries `Warnings` and no rows
  to tell the user the result may be incomplete (e.g. a pagination/result cap was
  hit). The engine collects its `Warnings` and skips the (no-op) insert.
- Thread `ctx` into every outbound call for cancellation and tracing.

## Registration and configuration

The engine itself carries no connectors; dfetch's default set lives in the
`connectors` package, and the CLI wires it up with `connectors.DefaultOptions()`
plus the user's config. Three independent ways a connector becomes available:

**Builtin** — available with no config, under a fixed schema name. Add it to
`Builtins()` in `connectors/connectors.go` (or `ConfigOnly()` if it cannot
construct without params, e.g. a database DSN):

```go
func Builtins() map[string]source.Factory {
    return map[string]source.Factory{
        "github": github.New,
        "jaeger": jaeger.New,
        "<name>": myconnector.New, // now queryable as <name>.<table>
    }
}
```

Builtins are constructed with `factory(nil)`, so `New` must work with `nil`
params and sensible defaults.

**Config** — a user binds any registered connector `type` to a schema `name` in
`dfetch.yaml` (`./dfetch.yaml`, falling back to `~/dfetch.yaml`; see
`config`). This can add new schemas or override a builtin, and lets one
connector type serve several hosts:

```yaml
sources:
  - name: gh-enterprise # schema; queried as gh-enterprise.issues
    type: github # registered connector type
    params:
      base_url: https://github.example.com/api/v3
```

The CLI registers the default set first, then each config source, and the last
registration of a schema name wins — so config overrides a builtin.

**Library** — an external Go program never touches this repo: it implements
`source.Connector` and registers an instance directly with
`engine.WithConnector("name", conn)`, or registers a `source.Factory` in its own
`source.Registry` and declares typed sources via `engine.WithSources`/
`engine.WithConfig`. See [library.md](../library.md) at the repo root.

**Params and secrets:** read structured options from `params` (type-assert, e.g.
`params["base_url"].(string)`). Secrets go through `source.Credential` — the
standard lazy resolver every connector uses. Build one in `New`:

```go
token, err := source.NewCredential("myconn", "token", params, "",
    source.EnvFirst("MYCONN_TOKEN"))
```

and call `token.Get(ctx)` at the start of `Scan`. That one line gives your
connector the full standard trio with fixed precedence: a static param (when
you name one), else the environment, else `params["token_func"]` (a Go
function, programmatic config only), else `params["token_command"]` (an argv
run without a shell, 5s timeout). Resolution is lazy (never at construction —
connectors are built eagerly for every query), resolves once on success — a failure is
returned without being cached, so the next use retries — and is race-safe
under concurrent Scans. When the env value needs shaping (a
`"Bearer "` prefix, a Basic pair), pass your own closure instead of `EnvFirst`
— see slack/jira. Always accept a `base_url`-style override — it's what lets
tests point at a local `httptest` server.

## Observability

- Build your HTTP client with `otelhttp.NewTransport(http.DefaultTransport)` so
  each request becomes a client span (a no-op until a tracer provider is
  installed). See `github.New`.
- Thread the `ctx` from `Scan` through every request (`http.NewRequestWithContext`)
  so spans nest under the engine's `connector.scan` span and cancellation works.
- You generally don't start your own spans; the engine wraps `Scan`. Add spans
  only for expensive internal phases if useful.

## Testing

Follow `github_test.go` / `jaeger_test.go` (testify: `require` for fatal,
`assert` otherwise):

- A `newTestConnector(t, http.HandlerFunc)` helper spins up `httptest.NewServer`
  and calls `New(map[string]any{"base_url": srv.URL})`.
- A `collectScan(conn, req)` helper drives `Scan` with an `emit` that accumulates
  chunks, so you can assert on the full result; a `scanChunkSizes` variant asserts
  the streaming/pagination shape.
- The handler both **returns canned JSON** and **captures the inbound request**
  (`r.URL.Path`, `r.URL.Query()`), so you can assert push-down landed in the
  outbound call.

Cover at least: `Tables()`; a happy-path scan (row mapping, types, column order);
push-down assertions; LIMIT-not-pushed when ordering can't be honored;
missing-required-filter returns an error **without** calling the API; pagination/
streaming; and API-error surfacing. `make coverage` enforces a coverage gate.

**Database connectors** can't use `httptest`. Follow `source/postgres`
instead: factor the request-building into **pure functions** (`buildSelect`, type
mapping, value normalization) and unit-test those directly (asserting the generated
SQL + args is the analog of capturing the outbound HTTP request) so they count
toward coverage; then add a **gated integration test** (`//go:build integration`)
that runs against a real database from `$DFETCH_TEST_POSTGRES_DSN` and `t.Skip`s
when it's unset. CI provides the database as a service container.

## Reference implementations

**`source/github`** — REST over `net/http`. Shows: required path-param
filters (`requireStringEq`), mapping `ORDER BY` to the API's sort param
(`orderParam`), safe LIMIT push-down accounting for OFFSET (`pageLimit`,
`limitSafe`, `consumedAll`), `Link`-header pagination (`nextLink`), one chunk
emitted per page, and preserving the caller's `owner` casing so SQLite's
case-sensitive `WHERE` still matches.

**`source/jaeger`** — Jaeger api_v3 returning the OTLP model. Shows:
required `service_name`, deriving a time window from range filters on a `start_time`
column (`timeBounds`, defaulting to the last hour) and a duration window
(`durationBounds`), a `trace_id` equality short-circuiting to a by-id endpoint
(no service/window needed), flattening nested JSON into rows, an `attributes` JSON
column, OTLP enums rendered as readable strings, and decoding a streamed grpc-gateway
response with a `json.Decoder` loop (one chunk per decoded object).

**`source/postgres`** — the reference **dynamic / SQL** connector
(`database/sql` + `jackc/pgx`). Shows: an empty `Tables()` with `SchemaDescriber`/
`TableLister` backed by `information_schema`; mapping Postgres types to SQLite
affinities; and translating a `ScanRequest` into a real parameterized
`SELECT … WHERE … ORDER BY … LIMIT` (`buildSelect`) — pushing only operators
Postgres and SQLite evaluate identically (no `LIKE`), pushing `ORDER BY`+`LIMIT`
only for order-safe column types with NULL placement aligned to SQLite
(`orderPushSafe`), and honoring the engine-supplied column projection
(`ScanRequest.Columns`). It is config-only (registered for `type: postgres`, never
auto-instantiated), so it has no `base_url`-style builtin default.

## Invariants and gotchas

- **Superset is fine; wrong rows are not.** When unsure whether the API honors a
  predicate exactly, don't push it.
- **Row arity and order must match the declared columns**, every chunk.
- **Case sensitivity:** SQLite's default collation is case-sensitive and the
  engine runs the user's `WHERE` verbatim. If you store a value the user filtered
  on, store it as they wrote it (see github's `owner` handling), or the local
  re-filter may drop the row.
- **`nil` for NULL**, `int64` for integers, `bool` for `INTEGER` 0/1.
- **Required filters → error, not full fetch.**
- **Cap unbounded scans** when LIMIT can't be pushed.
- **`New(nil)` must work** for builtins.
- **No global state** — connectors are constructed once and used concurrently
  across sources; keep `Scan` safe for concurrent calls (a shared `*http.Client`
  is fine).

## Step-by-step

1. **Create the package** `source/<name>/<name>.go`: a `Connector`
   struct (HTTP client with `otelhttp` transport, base URL, optional
   `*source.Credential`) and a `New(params) (source.Connector, error)` factory
   reading `params["base_url"]` and building the credential with
   `source.NewCredential` (see [Params and secrets](#registration-and-configuration)),
   defaulting so `New(nil)` works.
2. **Declare tables/columns** (often in a `tables.go`): `[]source.Column` per
   table with SQLite affinities, and `Tables()` returning them.
3. **Implement `Scan`**: dispatch on `req.Table` to a per-table `scanX`; build the
   upstream request from safe push-down; page through results and `emit` one
   `*source.Rows` per page with values in column order.
4. **Add push-down helpers** as needed (equality extraction, range→window,
   order/limit mapping) — copy github/jaeger patterns; only push what's safe.
5. **Register** in `connectors/connectors.go` — `Builtins()` for a
   works-with-`nil`-params connector, `ConfigOnly()` otherwise.
6. **Test** with `httptest` + testify (see [Testing](#testing)).
7. **Document**: add the connector to [connectors.md](../../connectors.md) (and an
   example group in `examples.yaml`, rendered via `make examples`), and a layout
   line in [CONTRIBUTING.md](../../CONTRIBUTING.md).

## Checklist

- [ ] `source/<name>/` package with `New`, `Tables`, `Scan`
- [ ] `New(nil)` works with sensible defaults; `base_url` override supported
- [ ] Secrets resolve via `source.NewCredential` (env / `<x>_func` / `<x>_command`), lazily at `Scan`
- [ ] Columns use SQLite affinities; rows match column count/order; `nil`/`int64`/`bool` used correctly
- [ ] Push-down only when safe; required filters error; unbounded scans capped
- [ ] One chunk emitted per page; `ctx` threaded; `emit` errors propagated
- [ ] Registered in `connectors/connectors.go` (`Builtins()` or `ConfigOnly()`)
- [ ] Tests (httptest + testify) cover tables, scan, push-down, errors; `make coverage` passes
- [ ] `make lint` and `make vet` clean
- [ ] README Connectors section + CONTRIBUTING layout updated
