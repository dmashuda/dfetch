# Connectors

Six connectors are built in and need no configuration. To point one at a
non-default host (e.g. an enterprise or remote instance), or to register the same
connector under several schemas, see [Configuration](README.md#configuration).

## GitHub — schema `github`

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

**Authentication:** a bare token (sent as `Bearer <token>`) from `GITHUB_TOKEN`
(or `GH_TOKEN`); else `params.token_func` (a Go function, programmatic config
only); else `params.token_command` (an argv whose stdout is the token).
Resolution is lazy — the command/function runs only when a `github.*` table is
actually queried. Unauthenticated requests work with GitHub's lower rate
limits.

```sh
GITHUB_TOKEN=ghp_… dfetch query "SELECT * FROM github.repos WHERE owner='golang'"
```

```yaml
sources:
  - name: github
    type: github
    params:
      token_command: ["gh", "auth", "token"]
```

## Jaeger — schema `jaeger`

Queries a Jaeger instance through its
[api_v3 query service](https://www.jaegertracing.io/docs/apis/) and serves your
traces as tables. Defaults to `http://localhost:16686` — the same Jaeger the
bundled `docker compose` runs (see [Tracing](README.md#tracing)) — so dfetch can query
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
- `max_traces` (param) sets the api_v3 `search_depth` — an explicit cap on how
  many traces one scan fetches. **By default it's omitted**, so the Jaeger query
  service applies its own limit (the time window already bounds the scan) and
  dfetch works against any server out of the box. api_v3 rejects a `search_depth`
  that isn't strictly below the server's own max-traces limit (`search depth must
  be greater than 0 and less than max traces`); set `max_traces` below that limit
  only if you want a deterministic cap.
- **Authentication:** none for a local Jaeger. For a bearer-authenticated
  deployment, a bare token (sent as `Bearer <token>`) comes from
  `JAEGER_TOKEN`; else `params.token_func` (a Go function, programmatic config
  only); else `params.token_command` (an argv whose stdout is the token).
  Resolution is lazy — on first use of a `jaeger.*` table.

## data.gov / CKAN — schema `datagov`

Queries [data.gov](https://data.gov)'s open-data catalog through its
[CKAN Action API](https://docs.ckan.org/en/latest/api/) and serves the catalog
metadata as tables. CKAN powers hundreds of government and research portals, so
the same connector works against any of them via a `base_url` override (see
[Configuration](README.md#configuration)).

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
- **Authentication:** none for the default endpoint. data.gov has moved its
  primary endpoint behind the GSA `api.gsa.gov` gateway; the default `base_url`
  (`catalog-old.data.gov`) still serves the standard API. To use the gateway,
  set `base_url`, `action_path: /v3/action`, and a key (e.g. `DEMO_KEY`, sent
  as the `api_key` query parameter). The key comes from `params.api_key`; else
  `CKAN_API_KEY`; else `params.api_key_func` (a Go function, programmatic
  config only); else `params.api_key_command` (an argv whose stdout is the
  key). Resolution is lazy — on first use of a `datagov.*` table.

## Docker — schema `docker`

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

## Slack — schema `slack`

Backed by the [Slack Web API](https://api.slack.com/web). Serves your workspace's
channels, users, and message history as tables, plus full-text `search`.

| table | rows | required filters |
| --- | --- | --- |
| `slack.channels` | conversations (public + private) | — |
| `slack.users` | workspace members | — |
| `slack.messages` | a channel's message history | `channel` |
| `slack.search` | full-text message search | `query` |

Column names mirror the API fields. Nested objects are flattened (`topic`,
`purpose` to their text; user `real_name`/`display_name`/`email`/`title` from the
profile); `reactions` is kept as a JSON string queryable with SQLite's
`json_extract`. Push-down covers: `messages` accepts a `ts` range → the API's
`oldest`/`latest` window; `channels` maps `is_archived = 0` to `exclude_archived`.
`messages` requires `channel = '<id>'` and `search` requires `query = '...'`, so a
query without them errors before any request is made. An unbounded scan is capped
(it won't page through the whole workspace); add a `LIMIT` or narrower filters.

```sh
# most active public + private channels
dfetch query "SELECT name, num_members, is_private
              FROM slack.channels
              WHERE is_archived = 0
              ORDER BY num_members DESC LIMIT 10"

# people on the workspace (excluding bots and deactivated accounts)
dfetch query "SELECT name, real_name, tz
              FROM slack.users
              WHERE is_bot = 0 AND deleted = 0 LIMIT 20"

# recent messages in a channel, newest first
dfetch query "SELECT ts, user, text
              FROM slack.messages
              WHERE channel = 'C0123456789'
              ORDER BY ts DESC LIMIT 50"

# search messages (needs a user token; see Authentication)
dfetch query "SELECT ts, channel_name, username, text
              FROM slack.search
              WHERE query = 'deploy' LIMIT 20"
```

**Authentication:** Slack requires an `Authorization: Bearer <token>` header.
The header comes from `SLACK_TOKEN` (a bare token, sent as `Bearer <token>`);
else `params.auth_header_func` (a Go function, programmatic config only); else
`params.auth_header_command` — an argv whose stdout is used **verbatim** as the
header value, so it can emit any scheme. Resolution is lazy — on first use of a
`slack.*` table.

```yaml
sources:
  - name: slack
    type: slack
    params:
      # full header value, used as-is (e.g. "Bearer xoxb-…")
      auth_header_command: ["cat", "/run/secrets/slack-auth-header"]
```

Browser session tokens (`xoxc-…`) additionally require Slack's `d` cookie: set
`SLACK_COOKIE` to the bare cookie value (sent as `Cookie: d=<value>`); else
`params.cookie_func`; else `params.cookie_command` (stdout used **verbatim** as
the Cookie header). Same lazy resolution and precedence as the auth header.

`slack.search` requires a **user token** (`xoxp-…`); bot tokens (`xoxb-…`) cannot
call `search.messages` and the API returns `not_allowed_token_type`.

## Local files — schema `files`

Serves data files on disk — CSV, TSV, JSON, and JSONL/NDJSON — as tables, so
ad-hoc data can be trimmed, aggregated, and joined against every other source
with plain SQL. It is a dynamic source: the **table name is the file's path**
relative to the working directory, quoted as a SQL identifier:

```sql
SELECT * FROM files."data/orders.csv" LIMIT 10
```

| format          | file becomes a table of                                            |
| --------------- | ------------------------------------------------------------------ |
| `.csv` / `.tsv` | records under the header row (the header names the columns)        |
| `.json`         | the objects of a top-level JSON array (keys become columns)        |
| `.jsonl` / `.ndjson` | one object per line (keys become columns)                     |

The schema is inferred from a sample of the first 1000 rows/objects: a column
every sampled value fits as an integer is `INTEGER`, as a number `REAL`,
otherwise `TEXT`. JSON columns are the key union of the sampled objects in
first-seen order; nested objects/arrays are stored as JSON text (query with
`json_extract`). Empty CSV cells in numeric columns load as NULL; empty or
duplicate header cells are renamed (`column_3`, `id_2`). `dfetch tables files`
walks the working directory for supported files (hidden files and directories
are skipped).

There is no filter push-down — the file is the API — but a `LIMIT` (plus
`OFFSET`) with no `WHERE`/`ORDER BY` stops reading early. Anything else reads
the whole file, capped at `max_rows` (default 1000000) with a warning when the
cap truncates.

Table paths are confined to the root: absolute paths and `..` escapes are
rejected. The builtin is rooted at the working directory; a config source can
re-root it (and several sources can expose several directories):

```yaml
sources:
  - name: exports
    type: files
    params:
      root: /data/exports # queried as exports."2026-07/usage.csv"
      max_rows: 5000000
```

```sh
# peek at a CSV
dfetch query 'SELECT * FROM files."data/orders.csv" LIMIT 10'

# aggregate it
dfetch query 'SELECT region, SUM(amount) AS total
              FROM files."data/orders.csv"
              GROUP BY region ORDER BY total DESC'

# join local data against an API source
dfetch query "SELECT i.number, i.title, t.priority
              FROM files.\"triage.csv\" t
              JOIN github.issues i ON i.number = t.number
              WHERE i.owner='golang' AND i.repo='go'"

# reach into nested JSON with json_extract
dfetch query "SELECT json_extract(payload, '\$.status') AS status, COUNT(*) AS n
              FROM files.\"events.jsonl\" GROUP BY status"
```

## PostgreSQL — connector type `postgres`

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

**Authentication:** the credential lives in the DSN. It comes from
`params.dsn`; else `$DFETCH_POSTGRES_DSN` (or `$DATABASE_URL`); else
`params.dsn_func` (a Go function, programmatic config only); else
`params.dsn_command` (an argv whose stdout is the DSN). Resolution is lazy —
the pool opens on first use of a `warehouse.*` table, and a missing DSN errors
there rather than at startup. To expose more than one Postgres schema, register
more sources (e.g. a second `analytics` source with `schema: analytics`).

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

## New Relic — connector type `newrelic`

A configured connector backed by [NerdGraph](https://docs.newrelic.com/docs/apis/nerdgraph/get-started/introduction-new-relic-nerdgraph/),
New Relic's GraphQL API (no default — it needs an account id). It is a dynamic
source: **NRDB event types** (`Transaction`, `Span`, `Log`, custom types) are
discovered on demand and scanned by translating your query into NRQL, alongside
six curated tables served from NerdGraph object queries:

| table                       | rows                                          | required filters |
| --------------------------- | --------------------------------------------- | ---------------- |
| `newrelic.<EventType>`      | NRDB events (dynamic; see `dfetch tables`)    | —                |
| `newrelic.accounts`         | accounts visible to the API key               | —                |
| `newrelic.entities`         | monitored entities (APM, hosts, monitors, …)  | —                |
| `newrelic.alert_policies`   | alert policies in the account                 | —                |
| `newrelic.alert_conditions` | NRQL alert conditions                         | —                |
| `newrelic.issues`           | AI issues (what's alerting)                   | —                |
| `newrelic.incidents`        | incidents underlying the issues               | —                |

```yaml
sources:
  - name: newrelic            # queried as newrelic.<table>
    type: newrelic
    params:
      account_id: 1234567     # required: your New Relic account id
      # region: EU            # api.eu.newrelic.com for EU-datacenter accounts
      # max_rows: 5000        # cap on an event scan whose LIMIT can't be pushed
      # nrql_timeout: 70      # NRQL timeout in seconds
```

```sh
NEW_RELIC_API_KEY='NRAK-…' \
  dfetch query "SELECT timestamp, appName, duration FROM newrelic.Transaction
                WHERE appName = 'billing' AND timestamp >= 1750000000000
                ORDER BY timestamp DESC LIMIT 20"
```

Event columns come from NRQL's `keyset()` and mirror the attribute names
(`host.name`, `db.query.text`, …); `timestamp` is epoch **milliseconds**
(INTEGER), so numeric comparisons and `ORDER BY timestamp` behave exactly like
the NR UI. Push-down: equality / `!=` / `IN` (and numeric ranges) become NRQL
`WHERE` clauses; `timestamp` ranges also set the `SINCE`/`UNTIL` window; `ORDER
BY timestamp` + `LIMIT` ride the query when every filter was pushed. `LIKE` is
evaluated locally (NRQL's is case-insensitive). Without a `timestamp` lower
bound only the **last hour** is searched (with a warning), and no event scan
returns more than **5000 rows** (NRQL's hard cap) — narrow the window or push a
LIMIT for complete results.

**Authentication:** a **User API key** from `$NEW_RELIC_API_KEY` (or
`$DFETCH_NEWRELIC_API_KEY`); else `params.api_key_func` (a Go function,
programmatic config only); else `params.api_key_command` (an argv whose stdout
is the key). Unlike datagov's public `api_key`, a plain `api_key` param is
**not** read — keep real secrets out of config files. Resolution is lazy — on
first use of a `newrelic.*` table, so a
declared source without a key doesn't break unrelated commands. NerdGraph does
not accept the ingest license key used for sending telemetry — create a key of
type "User" under one.newrelic.com → API keys.

## Jira — connector type `jira`

A configured connector backed by the
[Jira Cloud REST API](https://developer.atlassian.com/cloud/jira/platform/rest/v3/)
(no default host — every site is its own `https://<yoursite>.atlassian.net`).

| table             | rows                               | required filters |
| ----------------- | ----------------------------------- | ----------------- |
| `jira.issues`      | issues, translated into a JQL search | —                |
| `jira.projects`    | projects visible to the API key/user | —                |
| `jira.comments`    | one issue's comments                 | `issue_key`       |

```yaml
sources:
  - name: jira # queried as jira.<table>
    type: jira
    params:
      base_url: https://yoursite.atlassian.net
```

```sh
JIRA_EMAIL='me@example.com' JIRA_API_TOKEN='ATATT...' \
  dfetch query "SELECT key, summary, status, assignee_display_name
                FROM jira.issues
                WHERE project_key = 'PROJ' AND status = 'In Progress'
                ORDER BY updated DESC LIMIT 20"

dfetch query "SELECT key, name, project_type_key FROM jira.projects ORDER BY key"

dfetch query "SELECT author_display_name, created, body
              FROM jira.comments
              WHERE issue_key = 'PROJ-123'
              ORDER BY created DESC"
```

`jira.issues` translates the query's `WHERE`/`ORDER BY` into JQL for the
enhanced search endpoint (`/rest/api/3/search/jql`): equality on `key`,
`project_key` (→ `project`), `issue_type`, `status`, `priority`, `resolution`,
`assignee_account_id` (→ `assignee`), `reporter_account_id` (→ `reporter`);
`IN` on `key`/`project_key`; range filters on `created`/`updated` (widened by a
day in each direction — Jira interprets JQL datetime literals in the
authenticated user's timezone, which dfetch can't know, and the exact predicate
is re-applied locally — then rounded outward to the minute, JQL's datetime
granularity); and `ORDER BY` on `created`, `updated`, `due_date` (→ `duedate`)
when every term maps (`key`, `priority`, and `status` are not pushed: JQL sorts
them semantically — issue keys numerically, priority/status by rank — which
differs from SQLite's text ordering). A `LIMIT` rides the search only when
every filter translated into JQL **exactly** and the ordering was fully
honored — a widened range filter therefore blocks it. **Without any
translatable restriction, the search defaults to `created >= -30d`** (JQL
rejects an unbounded query), so a bare `SELECT * FROM jira.issues LIMIT 10`
still runs; the default emits a warning, blocks `LIMIT` push-down, and is
replaced by any pushed filter that bounds the query.
`description` is rendered from Atlassian Document Format to plain text.

`jira.projects` pushes `key` equality/`IN` to the `keys` query param and a
single-column `ORDER BY` (`key` or `name`) to `orderBy`. `jira.comments`
requires `issue_key = '...'` (or `issue_key IN (...)`, one request per key) and
pushes a single-column `ORDER BY created`; `body` is also rendered from ADF to
plain text.

**Authentication (required):** HTTP Basic with `$JIRA_EMAIL` + `$JIRA_API_TOKEN`
(create a token at
[id.atlassian.com/manage-profile/security/api-tokens](https://id.atlassian.com/manage-profile/security/api-tokens));
else `params.auth_header_func` (a Go function, programmatic config only); else
`params.auth_header_command` — an argv whose stdout is used **verbatim** as the
Authorization header. Resolution is lazy — on first use of a `jira.*` table.
With nothing configured, a jira query fails with a message naming the options;
a `401` response is reported with the same hint.
