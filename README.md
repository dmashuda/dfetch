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
| `dfetch run <name> [args...]` | Run a saved query, binding args to its params. `--all-columns`, `--format`. |
| `dfetch queries` | List saved queries with their parameters and descriptions. |
| `dfetch tables [schema]` | List available tables and columns, optionally for one schema. |
| `dfetch version` | Print the version. |

`--config <path>` (global) points at a config file; the default is `./dfetch.yaml`
in the current directory, falling back to `~/dfetch.yaml` (see
[Configuration](#configuration)).

## Connectors

Four connectors are built in and need no configuration. To point one at a
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
| `github.artifacts` | GitHub Actions artifacts for a repo | `owner`, `repo` |

Column names mirror the GitHub API JSON fields (`created_at`, `updated_at`,
`user_login`, …). The required filters are API path parameters, so a query
without them errors before any request is made. Beyond the required ones, several
columns push down to the API: `commits` accepts `sha` (a branch/ref to start
from), `path`, and `author_login`; `workflow_runs` accepts `head_branch`,
`event`, `status`, `actor_login`, and `head_sha`; `artifacts` accepts `name` and
`workflow_run_id` (which fetches just that run's artifacts).

<!-- BEGIN EXAMPLES github -->
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

# artifacts joined to the run that produced them
dfetch query "SELECT a.name, a.size_in_bytes, r.run_number, r.conclusion
              FROM github.artifacts a
              JOIN github.workflow_runs r ON r.id = a.workflow_run_id
              WHERE a.owner='dmashuda' AND a.repo='dfetch'
                AND r.owner='dmashuda' AND r.repo='dfetch' LIMIT 10"
```
<!-- END EXAMPLES github -->

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

<!-- BEGIN EXAMPLES jaeger -->
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
<!-- END EXAMPLES jaeger -->

Notes:

- `jaeger.spans` requires a `service_name` filter (api_v3 requires it) **unless**
  you filter by `trace_id`, which looks the trace up directly.
- Push-down covers `service_name`, `operation_name`, a `start_time` range → the
  api's time window (defaulting to the **last hour** when none is given), and a
  `duration_ms` range.
- `kind` and `status_code` are the OTLP enums as readable strings
  (`internal`/`server`/`client`/…, `unset`/`ok`/`error`); `attributes` is the
  span's attribute list as a JSON object, queryable with SQLite's `json_extract`.
- `max_traces` (param, default `1000`) is the api_v3 `search_depth`: it caps how
  many traces one scan fetches **and** must stay below the Jaeger query service's
  own max-traces limit. A deployment that configures a low limit rejects the
  default with `search depth must be greater than 0 and less than max traces` —
  set `max_traces` below that limit in your config to fix it.
- Set `JAEGER_TOKEN` for a bearer-authenticated deployment.

### data.gov / CKAN — schema `datagov`

Queries [data.gov](https://data.gov)'s open-data catalog through its
[CKAN Action API](https://docs.ckan.org/en/latest/api/) and serves the catalog
metadata as tables. CKAN powers hundreds of government and research portals, so
the same connector works against any of them via a `base_url` override (see
[Configuration](#configuration)).

| table | rows | required filters |
| --- | --- | --- |
| `datagov.datasets` | datasets (packages) in the catalog | — |
| `datagov.resources` | the downloadable files within datasets | — |
| `datagov.organizations` | publishing organizations | — |
| `datagov.groups` | dataset groups | — |

<!-- BEGIN EXAMPLES datagov -->
```sh
# full-text search the catalog, most recently updated first
dfetch query "SELECT name, organization, num_resources
              FROM datagov.datasets
              WHERE q='climate' ORDER BY metadata_modified DESC LIMIT 10"

# datasets from one organization (pushed down as a Solr fq)
dfetch query "SELECT name, metadata_modified
              FROM datagov.datasets
              WHERE organization='noaa-gov'
              ORDER BY metadata_modified DESC LIMIT 10"

# CSV resources of datasets matching a topic
dfetch query "SELECT package_name, name, url
              FROM datagov.resources
              WHERE q='wildfire' AND format='CSV' LIMIT 10"

# organizations with the most datasets
dfetch query "SELECT name, title, package_count
              FROM datagov.organizations
              ORDER BY package_count DESC LIMIT 10"
```
<!-- END EXAMPLES datagov -->

Notes:

- `datasets` and `resources` share a virtual `q` column for full-text search:
  `WHERE q = 'climate'` becomes the CKAN `q` parameter. It is a search input, not
  a stored field.
- Push-down on `datasets` covers equality/`IN` on `organization`, `license_id`,
  `state`, `name`, and `tags` (CKAN Solr `fq`), a `metadata_created` /
  `metadata_modified` range → a Solr date range, `ORDER BY` on
  `metadata_modified` / `metadata_created` / `title`, and `LIMIT`/`OFFSET`. Other
  predicates are re-applied locally by SQLite.
- No required filters, but an unfiltered scan is capped (it won't page through the
  whole catalog); add a `q`/`WHERE`/`LIMIT` to narrow it.
- No API key is needed. data.gov has moved its primary endpoint behind the GSA
  `api.gsa.gov` gateway; the default `base_url` (`catalog-old.data.gov`) still
  serves the standard API. To use the gateway, set `base_url`,
  `action_path: /v3/action`, and an `api_key` (e.g. `DEMO_KEY`) in config.

### Docker — schema `docker`

Backed by the Docker Engine API over the local daemon socket
(`/var/run/docker.sock`) — no configuration or token needed.

| table | rows |
| --- | --- |
| `docker.containers` | all containers (running and stopped) |
| `docker.images` | images on the host |
| `docker.volumes` | volumes |
| `docker.networks` | networks |

Column names mirror the API fields (`image`, `state`, `created`, …). Nested data
(`ports`, `labels`, `mounts`, `repo_tags`, `ipam`, `options`, …) is stored as a
JSON string column — query it with SQLite's `json_extract`. There are no required
filters and no push-down: each scan fetches the full list and SQLite applies the
query. To point at a non-default socket use the `socket` param, or `base_url` for
a plaintext TCP daemon.

```sh
# running containers and their compose project
dfetch query "SELECT name, image,
              json_extract(labels,'\$.\"com.docker.compose.project\"') AS project
              FROM docker.containers
              WHERE state='running' ORDER BY name"

# largest images by size
dfetch query "SELECT json_extract(repo_tags,'\$[0]') AS tag, size
              FROM docker.images
              WHERE tag IS NOT NULL ORDER BY size DESC LIMIT 10"
```

### PostgreSQL — connector type `postgres`

A configured connector (no default — it needs a DSN). Each source maps **one
Postgres schema** and discovers its tables on demand from `information_schema`, so
`dfetch tables` browses them lazily rather than listing the whole catalog. It
pushes a real `SELECT` to the server.

```yaml
sources:
  - name: warehouse           # queried as warehouse.<table>
    type: postgres
    params:
      schema: public          # the Postgres schema (default: public)
      # dsn: postgres://…      # optional; overrides the env DSN
      # max_rows: 100000       # cap on a scan whose LIMIT can't be pushed
```

The DSN comes from `$DFETCH_POSTGRES_DSN` (or `$DATABASE_URL`), or a `dsn` param.
To expose more than one Postgres schema, register more sources (e.g. a second
`analytics` source with `schema: analytics`).

```sh
DFETCH_POSTGRES_DSN='postgres://user:pass@host:5432/db?sslmode=disable' \
  dfetch query "SELECT id, total FROM warehouse.orders
                WHERE status='paid' ORDER BY created_at DESC LIMIT 20"
```

Push-down: the query's referenced columns become the `SELECT` list (only those, not
`*`); equality / `<>` / `<` / `<=` / `>` / `>=` / `IN` / `BETWEEN` filters become a
parameterized `WHERE`; and `ORDER BY` + `LIMIT` are pushed when the order keys are
numeric/temporal (so Postgres and SQLite order identically). `LIKE` and other
predicates are evaluated locally by SQLite. A scan whose `LIMIT` can't be pushed is
capped at `max_rows`. Column types map to SQLite affinities (`numeric` → `REAL`,
which can lose precision on very large exact values).

## Configuration

dfetch works with no config. To point a connector at a non-default host, or to
register a connector under additional schemas, create a `dfetch.yaml` in the
directory you run dfetch from — config is per-project. dfetch looks for
`./dfetch.yaml` first and falls back to `~/dfetch.yaml`; `--config <path>`
overrides both. Each `sources` entry binds a SQL schema `name` to a connector
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

### Saved queries

Add a `queries` list to store reusable, parameterized queries. Each query has a
`name` (used by `dfetch run`), an optional `description`, an ordered list of
`params` bound as `:name` placeholders in the `sql`, and an optional `columns`
list selecting the default output columns:

```yaml
queries:
  - name: fetch-trace
    description: Spans for a trace
    params: [trace_id]               # bound positionally: dfetch run fetch-trace <trace_id>
    columns: [trace_id, name, duration]
    sql: SELECT * FROM prod-traces.spans WHERE trace_id = :trace_id
```

```sh
dfetch queries                       # list saved queries
dfetch run fetch-trace abc123        # binds :trace_id = abc123, shows the columns above
dfetch run fetch-trace abc123 --all-columns   # show every column the query produces
```

Positional arguments bind to `params` in order. Parameters are SQLite bind
values, so they substitute values (not table or column names). When `columns` is
set, output is narrowed to those columns unless `--all-columns` is passed.

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
