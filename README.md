# dfetch

Query and join data across any data source with SQL, on demand.

`dfetch` takes a SQL query (SQLite syntax), validates it, fetches each referenced
data source (exposed as a SQLite table), loads it into a **per-request local
SQLite database**, and runs your query against that database. You get the full
power of SQLite — joins across sources, aggregates, JSON functions — over live
data from APIs.

```sh
dfetch query "SELECT number, title, state FROM github.issues
              WHERE owner='golang' AND repo='go' AND state='open'
              ORDER BY updated_at DESC LIMIT 10"
```

## Install

Download a prebuilt binary from the
[latest release](https://github.com/dmashuda/dfetch/releases/latest)
(`linux/amd64`, `darwin/arm64`, `windows/amd64`), then put it on your `PATH`:

```sh
tar xzf dfetch_linux_amd64.tar.gz && sudo mv dfetch /usr/local/bin/
dfetch version
```

Or install with Go:

```sh
go install github.com/dmashuda/dfetch@latest
```

dfetch links against [`mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3)
(cgo), so installing with Go needs a C compiler and `CGO_ENABLED=1`. To build
from source, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Quick start

```sh
# what can I query?
dfetch tables

# run a query — default output is an aligned table; --format json|csv also work
dfetch query "SELECT number, title FROM github.issues
              WHERE owner='octocat' AND repo='Hello-World'" --format json
```

A data source is a **connector** that exposes one or more tables under a SQL
schema (e.g. `github.issues`). dfetch pushes filters, `ORDER BY`, and `LIMIT`
down to each source where it safely can, then resolves the *full* query locally
in SQLite — so the result is always correct even when a connector returns a
superset of the rows.

## Commands

| command | description |
| --- | --- |
| `dfetch query "<sql>"` | Run a SQL query. `--format table\|json\|csv` (default `table`). |
| `dfetch tables [schema]` | List available tables and columns, optionally for one schema. |
| `dfetch version` | Print the version. |

`--config <path>` (global) points at a config file; the default is
`~/.dfetch/config.yaml` (see [Configuration](#configuration)).

## Connectors

Two connectors are built in and need no configuration. To point one at a
non-default host (e.g. an enterprise or remote instance), or to register the same
connector under several schemas, see [Configuration](#configuration).

### GitHub — schema `github`

Backed by the GitHub REST API.

| table | rows | required filters |
| --- | --- | --- |
| `github.issues` | issues for a repo | `owner`, `repo` |
| `github.pulls` | pull requests for a repo | `owner`, `repo` |
| `github.repos` | a repo, or an owner's repos | `owner` |
| `github.commits` | commit history for a repo | `owner`, `repo` |
| `github.releases` | releases for a repo | `owner`, `repo` |
| `github.workflow_runs` | GitHub Actions runs for a repo | `owner`, `repo` |

Column names mirror the GitHub API JSON fields (`created_at`, `updated_at`,
`user_login`, …). The required filters are API path parameters, so a query
without them errors before any request is made. Beyond the required ones, several
columns push down to the API: `commits` accepts `sha` (a branch/ref to start
from), `path`, and `author_login`; `workflow_runs` accepts `head_branch`,
`event`, `status`, `actor_login`, and `head_sha`.

```sh
# issues for a repo, newest first
dfetch query "SELECT number, title, state, comments
              FROM github.issues
              WHERE owner='golang' AND repo='go' AND state='open'
              ORDER BY updated_at DESC LIMIT 10"

# join pulls to their repo
dfetch query "SELECT p.number, p.title, r.full_name, r.stars
              FROM github.pulls p
              JOIN github.repos r ON r.owner=p.owner AND r.name=p.repo
              WHERE p.owner='golang' AND p.repo='go' AND p.state='open'
              ORDER BY p.created_at DESC LIMIT 20"

# did the latest CI on main pass?
dfetch query "SELECT run_number, name, status, conclusion, created_at
              FROM github.workflow_runs
              WHERE owner='dmashuda' AND repo='dfetch' AND head_branch='main'
              ORDER BY created_at DESC LIMIT 5"

# recent commits touching one file
dfetch query "SELECT sha, author_login, message
              FROM github.commits
              WHERE owner='golang' AND repo='go' AND path='src/runtime/proc.go'
              LIMIT 10"
```

**Authentication:** set `GITHUB_TOKEN` (or `GH_TOKEN`) to authenticate;
unauthenticated requests work but are rate-limited to 60/hour.

```sh
GITHUB_TOKEN=ghp_… dfetch query "SELECT * FROM github.repos WHERE owner='golang'"
```

### Jaeger — schema `jaeger`

Queries a Jaeger instance through its
[api_v3 query service](https://www.jaegertracing.io/docs/apis/) and serves your
traces as tables. Defaults to `http://localhost:16686` — the same Jaeger the
bundled `docker compose` runs (see [Tracing](#tracing)) — so dfetch can query
its own traces.

| table | rows | required filters |
| --- | --- | --- |
| `jaeger.spans` | one row per span | `service_name` |
| `jaeger.services` | service names | — |
| `jaeger.operations` | operations for a service | `service_name` |

```sh
# slowest spans for a service in the last hour
dfetch query "SELECT operation_name, duration_ms, status_code
              FROM jaeger.spans
              WHERE service_name='dfetch'
              ORDER BY duration_ms DESC LIMIT 10"

# every span of one trace (no service_name or time window needed for a trace_id lookup)
dfetch query "SELECT span_id, parent_span_id, operation_name, service_name, duration_ms
              FROM jaeger.spans WHERE trace_id='<TRACE_ID>'
              ORDER BY start_time_unix_nano"

# error spans in a window, reading a span attribute out of the JSON column
dfetch query "SELECT operation_name,
                     json_extract(attributes, '\$.\"db.statement\"') AS sql
              FROM jaeger.spans
              WHERE service_name='dfetch' AND status_code='error'
                AND start_time >= '2026-06-01T00:00:00Z'"
```

Notes:

- `jaeger.spans` requires a `service_name` filter (api_v3 requires it) **unless**
  you filter by `trace_id`, which looks the trace up directly.
- Push-down covers `service_name`, `operation_name`, a `start_time` range → the
  api's time window (defaulting to the **last hour** when none is given), and a
  `duration_ms` range.
- `kind` and `status_code` are the OTLP enums as readable strings
  (`internal`/`server`/`client`/…, `unset`/`ok`/`error`); `attributes` is the
  span's attribute list as a JSON object, queryable with SQLite's `json_extract`.
- Set `JAEGER_TOKEN` for a bearer-authenticated deployment.

## Configuration

dfetch works with no config. To point a connector at a non-default host, or to
register a connector under additional schemas, create `~/.dfetch/config.yaml`
(or pass `--config <path>`). Each entry binds a SQL schema `name` to a connector
`type`, with connector-specific `params`:

```yaml
sources:
  - name: gh-enterprise        # queried as gh-enterprise.issues
    type: github
    params:
      base_url: https://github.example.com/api/v3
  - name: prod-traces          # queried as prod-traces.spans
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
├─ engine.parse               → what the parser understood (tables, joins, limit, …)
├─ engine.loadSource (github.issues)
│  ├─ connector.scan          → one HTTP GET span per API page (otelhttp)
│  └─ ATTACH / CREATE / INSERT (otelsql)
└─ SELECT                      (the local resolve; otelsql)
```

Use it to see how many API calls a query made (pagination shows as multiple `GET`
spans), where latency went (API vs. local SQL), and which step failed (failed
spans are marked with the error). Set `OTEL_SERVICE_NAME` or other standard
`OTEL_*` vars to customize; `OTEL_SDK_DISABLED=true` forces tracing off.

Because the Jaeger connector queries Jaeger, you can analyze these traces with
dfetch itself — see the [Jaeger connector](#jaeger--schema-jaeger).

## Contributing

Building from source, running the tests, and **writing a new connector** are
covered in [CONTRIBUTING.md](CONTRIBUTING.md).
