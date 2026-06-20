# Contributing to dfetch

## Prerequisites

- **Go** — the toolchain version in [`go.mod`](go.mod).
- **A C compiler + `CGO_ENABLED=1`.** The local query engine uses
  [`mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3), which is cgo. The
  Makefile sets `CGO_ENABLED=1` for you.
- **Java** — only to regenerate the SQL parser (`make generate`); normal builds
  don't need it (the generated code is committed).

## Build and test

```sh
make build      # build ./bin/dfetch
make test       # go test ./...
make coverage   # tests + coverage gate (excludes generated code)
make lint       # golangci-lint
make vet        # go vet (excludes generated code)
make generate   # regenerate the ANTLR SQLite parser (requires Java)
```

CI (`.github/workflows/ci.yaml`) runs build, `make coverage`, and golangci-lint
on every push/PR to `main`. Releases are tag-triggered (`v*`) and build each OS
on its native runner, since cgo can't be cleanly cross-compiled
(`.github/workflows/release.yaml`).

### Testing conventions

Use [testify](https://github.com/stretchr/testify) — `require` for fatal
assertions, `assert` for non-fatal ones. Don't write bare
`if got != want { t.Fatalf(...) }` checks in new tests.

### The generated parser

`internal/sqlparse/gen` is ANTLR-generated and committed — **do not hand-edit
it.** The grammar lives in `internal/sqlparse/grammar`; regenerate with
`make generate` (pinned to ANTLR 4.13.1, matching the `antlr4-go/antlr/v4`
runtime in `go.mod`). golangci-lint skips generated files automatically, and the
`vet`/`coverage` targets exclude `internal/sqlparse/gen`.

### Profiling a query

`make profile` runs a query end-to-end and writes CPU + memory profiles;
`make pprof` opens one in the pprof web UI. No profiling code ships in the
binary — a gated benchmark (`BenchmarkProfileQuery`) drives the query under
`go test -cpuprofile/-memprofile`.

```sh
make profile PROFILE_QUERY="SELECT ... FROM github.issues WHERE ..."
make pprof            # memory profile  →  http://localhost:8081
make pprof PROF=cpu   # CPU profile
```

## Project layout

```
cmd/                    cobra CLI: root, query, tables, version
internal/config         YAML config loading (schema -> connector)
internal/source         Connector interface + ScanRequest (push-down) + registry
internal/source/github  GitHub connector (issues, pulls, repos)
internal/source/jaeger  Jaeger connector (spans, services, operations)
internal/sqlparse       SQL parse/validate + typed AST (ORDER BY/LIMIT) + SQL rendering
internal/localdb        per-request local SQLite database (attach/create/insert/query)
internal/engine         orchestration: parse -> plan push-down -> load -> resolve
internal/telemetry      OpenTelemetry setup (env-gated; no-op when off)
```

### How a query runs

A connector is registered under a SQL schema (e.g. `github`) and exposes tables
(`github.issues`). `engine.Run`:

1. **parse** the SQL into a typed AST (`internal/sqlparse`);
2. for each schema-qualified source, **plan** a push-down `source.ScanRequest`
   (filters / `ORDER BY` / `LIMIT`) — see `internal/engine/plan.go`;
3. **attach** an in-memory schema, **create** the table, and **scan** the
   connector, loading each emitted chunk into SQLite as it arrives (sources are
   fetched concurrently; writes are serialized onto localdb's single connection);
4. run the **original SQL verbatim** against SQLite — the source of truth, so a
   connector returning a superset is always corrected here.

## Adding a new connector

A connector is any type that implements `source.Connector`
(`internal/source/source.go`):

```go
type Connector interface {
    // Tables returns the schema of every table this connector serves.
    Tables() []TableSchema
    // Scan fetches rows for one table, emitting one chunk per page so the engine
    // can load each as it arrives. Returning an error from emit aborts the scan.
    Scan(ctx context.Context, req ScanRequest, emit func(*Rows) error) error
}

// New is the factory the registry calls; params come from config (or nil for a builtin).
type Factory func(params map[string]any) (Connector, error)
```

The existing `internal/source/github` and `internal/source/jaeger` connectors
are the reference implementations — copy their shape. Steps:

### 1. Create the package

`internal/source/<name>/<name>.go` with a `Connector` struct and a `New`
factory. Read connector-specific options from `params` and secrets from the
environment:

```go
func New(params map[string]any) (source.Connector, error) {
    c := &Connector{
        client:  &http.Client{Timeout: 30 * time.Second,
            // otelhttp gives one client span per request (no-op until tracing is on).
            Transport: otelhttp.NewTransport(http.DefaultTransport)},
        baseURL: defaultBaseURL,
        token:   os.Getenv("MYSVC_TOKEN"),
    }
    if bu, ok := params["base_url"].(string); ok && bu != "" {
        c.baseURL = strings.TrimSuffix(bu, "/")
    }
    return c, nil
}
```

Accepting a `base_url` param is what lets tests point the connector at an
`httptest.NewServer`.

### 2. Declare tables and columns

Each `Column.Type` is a SQLite affinity (`TEXT`, `INTEGER`, `REAL`; empty
defaults to `TEXT`) and becomes the column's type in the generated `CREATE
TABLE` verbatim. Name columns after the source's fields so queries read
naturally.

```go
var widgetsCols = []source.Column{
    {Name: "id", Type: "INTEGER"}, {Name: "name", Type: "TEXT"},
    {Name: "created_at", Type: "TEXT"},
}

func (c *Connector) Tables() []source.TableSchema {
    return []source.TableSchema{{Name: "widgets", Columns: widgetsCols}}
}
```

### 3. Implement `Scan`

Dispatch on `req.Table`, fetch the data, and `emit` one `*source.Rows` per page.
Each row's values must be in the **same order** as the table's columns. **You may
honor as much or as little of the push-down as you like** — the engine re-runs
the full SQL in SQLite, so returning a superset is always correct.

```go
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
    switch req.Table {
    case "widgets":
        return c.scanWidgets(ctx, req, emit)
    default:
        return fmt.Errorf("<name>: unknown table %q", req.Table)
    }
}
```

The push-down lives in `req` (`internal/source/source.go`):

- `req.Filters` — `WHERE` conjuncts offered for push-down: `Column`, `Op`
  (`sqlparse.Operator`, e.g. `OpEq`, `OpGt`, `OpBetween`, `OpIn`), and a typed
  `Value` (or `Values` for `IN`/`BETWEEN`). Bind parameters are not pushed.
  `req.Filter("col")` returns the first filter on a column.
- `req.OrderBy`, `req.Limit`, `req.Offset` — translate into your API's params
  **only when the result will still be exactly what the query wants**; otherwise
  skip them and let SQLite finish the job. If a filter is a required API
  parameter (like GitHub's `owner`/`repo`), return a helpful error when it's
  missing rather than fetching everything.

The github connector's `stringEq` / `requireStringEq` / `pageLimit` / `limitSafe`
helpers are a good template for safe push-down.

### 4. Register it

Add the factory to the `builtins` map in `internal/engine/engine.go` (and import
the package). That single line makes it appear in `dfetch tables` and queryable
as `<name>.<table>`:

```go
var builtins = map[string]source.Factory{
    "github": github.New,
    "jaeger": jaeger.New,
    "<name>": myconnector.New,
}
```

Connectors are also usable without being builtins: a user can register any
registered `type` under a schema `name` via `~/.dfetch/config.yaml`.

### 5. Test it

Copy the `httptest.NewServer` pattern from `github_test.go`/`jaeger_test.go`:
pass `params{"base_url": srv.URL}` to `New`, drive `Scan` with a collecting
`emit`, and assert on both the rows returned and the outbound request (to verify
push-down). Cover at least: a `Tables()` test, a happy-path scan, push-down
assertions, missing-required-filter errors, and API-error handling.

### 6. Document it

Add the connector (its tables, required filters, and an example) to the
[Connectors](README.md#connectors) section of the README, and a one-line entry
to the layout above.

### Checklist

- [ ] `internal/source/<name>/` package with `New`, `Tables`, `Scan`
- [ ] Registered in `engine.go` `builtins`
- [ ] Tests (httptest + testify), `make coverage` passes
- [ ] `make lint` and `make vet` clean
- [ ] README + layout updated
