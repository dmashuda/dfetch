# dfetch

Connect and join data across any data source and query it with SQL on demand.

`dfetch` takes a SQL query (SQLite syntax), parses and validates it, fetches the data from the
referenced data sources (each exposed as a SQLite table), loads it into a **per-request local
SQLite database**, then resolves the query against that database.

> **Status:** early scaffolding. The package structure, interfaces, CLI, and CI/release
> automation are in place; the query engine is stubbed and returns `not implemented`.

## How it works

1. Parse and validate the SQL, extracting the referenced table names.
2. Resolve each table to a configured data source.
3. Open a fresh local SQLite database for the request.
4. Fetch each source and load it into its table.
5. Resolve the original query against the local database and return the result.

## Install

```sh
go install github.com/dmashuda/dfetch@latest
```

`dfetch` links against [`mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3), so a C
compiler and `CGO_ENABLED=1` are required to build.

## Usage

A data source is a *connector* exposing tables under a SQL schema. The built-in
`github` connector serves `github.issues`, `github.pulls`, and `github.repos`
from the GitHub REST API. dfetch pushes filters, `ORDER BY`, and `LIMIT` down to
the API where it safely can, then resolves the full query locally in SQLite.

```sh
# issues for a repo, newest first
dfetch query "SELECT number, title, state, comments
              FROM github.issues
              WHERE owner = 'golang' AND repo = 'go' AND state = 'open'
              ORDER BY updated_at DESC LIMIT 10"

dfetch query "SELECT number, title FROM github.issues
              WHERE owner='octocat' AND repo='Hello-World'" --format json

# discover tables and columns
dfetch tables           # all schemas
dfetch tables github    # one schema

dfetch version
```

Column names mirror the GitHub API JSON fields (e.g. `created_at`, `updated_at`,
`user_login`). `github.issues`/`github.pulls` require `owner` and `repo` filters
(they're path parameters); `github.repos` requires `owner`.

### Authentication

Set `GITHUB_TOKEN` (or `GH_TOKEN`) to authenticate; unauthenticated requests work
but are rate-limited to 60/hour.

```sh
GITHUB_TOKEN=ghp_… dfetch query "SELECT * FROM github.repos WHERE owner='golang'"
```

### Querying Jaeger traces

The built-in `jaeger` connector queries a local Jaeger instance through its
[api_v3 query service](https://www.jaegertracing.io/docs/apis/) and serves
`jaeger.spans` (one row per span), `jaeger.services`, and `jaeger.operations`. It
defaults to `http://localhost:16686` — the same Jaeger the bundled
`docker compose` runs (see [Tracing](#tracing)) — so dfetch can query its own
traces.

```sh
# slowest spans for a service in the last hour
dfetch query "SELECT operation_name, duration_ms, status_code
              FROM jaeger.spans
              WHERE service_name = 'dfetch'
              ORDER BY duration_ms DESC LIMIT 10"

# error spans in a window, reading a span attribute out of the JSON column
dfetch query "SELECT operation_name,
                     json_extract(attributes, '\$.\"db.statement\"') AS sql
              FROM jaeger.spans
              WHERE service_name = 'dfetch' AND status_code = 'error'
                AND start_time >= '2026-06-01T00:00:00Z'"

dfetch query "SELECT name, span_kind FROM jaeger.operations WHERE service_name = 'dfetch'"
dfetch query "SELECT name FROM jaeger.services"
```

`jaeger.spans` requires a `service_name` filter (api_v3 requires it). Push-down
covers `service_name`, `operation_name`, a `start_time` range → the api's time
window (defaulting to the **last hour** when none is given), and a `duration_ms`
range. `kind` and `status_code` are the OTLP enums rendered as readable strings
(`internal`/`server`/`client`/…, `unset`/`ok`/`error`); `attributes` is the span's
attribute list as a JSON object, queryable with SQLite's `json_extract`. Point at
a non-default host with the `base_url` param (see [Configuration](#configuration));
set `JAEGER_TOKEN` for a bearer-authenticated deployment.

## Configuration

dfetch works with no config (the `github` connector is built in). To add or
override connectors, create `~/.dfetch/config.yaml` (override the path with
`--config`). Each entry binds a SQL schema `name` to a connector `type`:

```yaml
sources:
  - name: gh-enterprise        # referenced as gh-enterprise.issues
    type: github
    params:
      base_url: https://github.example.com/api/v3
  - name: prod-traces          # referenced as prod-traces.spans
    type: jaeger
    params:
      base_url: http://jaeger.example.com:16686
```

## Tracing

dfetch is instrumented with OpenTelemetry. Tracing is **off unless an OTLP
endpoint is configured** — without it there's no exporter and effectively no
overhead. To capture traces for debugging, run the bundled Jaeger and point
dfetch at it:

```sh
docker compose up -d                                   # Jaeger (UI on :16686)
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
dfetch query "SELECT number, title FROM github.issues
              WHERE owner='golang' AND repo='go' AND state='open'
              ORDER BY updated_at DESC LIMIT 5"
# open http://localhost:16686 and pick service "dfetch"
```

Each query is one trace:

```
engine.Run (db.query.text=<sql>)
├─ engine.loadSource (github.issues)
│  ├─ connector.scan          → one HTTP GET span per API page (otelhttp)
│  └─ ATTACH / CREATE / INSERT (otelsql)
└─ SELECT                      (the local resolve; otelsql)
```

Use it to see how many GitHub API calls a query made (pagination shows as
multiple `GET` spans), where latency went (API vs. local SQL), and which step
failed (failed spans are marked with the error). Set `OTEL_SERVICE_NAME` or other
standard `OTEL_*` vars to customize; `OTEL_SDK_DISABLED=true` forces tracing off.

## Development

```sh
make build      # build ./bin/dfetch
make test       # run tests
make coverage   # run tests with the coverage gate
make lint       # golangci-lint
make generate   # regenerate the ANTLR SQLite parser (requires Java)
```

### Testing

Tests use [testify](https://github.com/stretchr/testify) (`require` for fatal
assertions, `assert` for non-fatal ones). Use it for new tests rather than
hand-written `if got != want { t.Fatalf(...) }` checks.

The SQL parser under `internal/sqlparse` is generated with ANTLR; the generated
code under `internal/sqlparse/gen` is committed, so normal builds and CI need no
Java. golangci-lint skips it automatically (generated-file detection), and the
`vet`/`coverage` make targets exclude it.

## Project layout

```
cmd/                    cobra CLI: root, query, tables, version
internal/config         YAML config loading (schema -> connector)
internal/source         Connector interface + ScanRequest (push-down) + registry
internal/source/github  GitHub connector (issues, pulls, repos)
internal/source/jaeger  Jaeger connector (spans, services, operations)
internal/sqlparse       SQL parse/validate + typed AST (incl. ORDER BY/LIMIT) + SQL rendering
internal/localdb        per-request local SQLite database (attach/create/insert/query)
internal/engine         orchestration: parse -> plan push-down -> load -> resolve
internal/telemetry      OpenTelemetry setup (env-gated; no-op when off)
```
