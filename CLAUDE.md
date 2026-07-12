# CLAUDE.md

Guidance for working in the dfetch repo.

## What dfetch is

A Go library + CLI that takes a SQL query (SQLite syntax), parses/validates it,
fetches the referenced data sources (each exposed as a SQLite table), loads them
into a per-request local SQLite database, and resolves the query against it.

## Layout

```
cmd/                    cobra CLI: root, query, tables, version
                        (cmd/engine.go wires connectors.DefaultOptions + config into engine.New)
config                  YAML config loading (schema -> connector; saved queries)
                        loaded from ./dfetch.yaml, then $XDG_CONFIG_HOME/dfetch/dfetch.yaml, then ~/dfetch.yaml
source                  Connector interface + ScanRequest (push-down) + registry
                        (see source/README.md to add a new connector)
source/github           GitHub connector (issues/pulls/repos/commits/releases/workflow_runs/artifacts), stdlib net/http
source/jaeger           Jaeger connector (spans/services/operations), api_v3
source/ckan             CKAN/data.gov connector (datasets/resources/organizations/groups), Action API
source/docker           Docker connector (containers/images/volumes/networks), Engine API over the local unix socket, stdlib net/http
source/files            Local files connector (CSV/TSV/JSON/JSONL as tables; dynamic, path = table name, rooted at cwd or params.root)
source/slack            Slack connector (channels/users/messages/search), Web API, stdlib net/http; auth via $SLACK_TOKEN or params.auth_header_func/auth_header_command (full header, verbatim); browser xoxc tokens also need the "d" cookie via $SLACK_COOKIE (bare value) or params.cookie_func/cookie_command (full Cookie header, verbatim)
source/newrelic         New Relic connector (dynamic NRDB event types via NRQL + curated accounts/entities/alerts/issues tables), NerdGraph GraphQL, config-only; auth via $NEW_RELIC_API_KEY (User key)
source/postgres         Postgres connector (dynamic; SQL push-down via database/sql + pgx), config-only
source/jira             Jira Cloud connector (issues via JQL push-down, projects, comments), REST v3, config-only; auth via $JIRA_EMAIL + $JIRA_API_TOKEN (Basic) or params.auth_header_func/auth_header_command (full header, verbatim)
connectors              default connector set (Builtins/ConfigOnly/DefaultRegistry/DefaultOptions); engine never imports it
internal/sqlparse       SQL parse/validate + typed AST (incl. ORDER BY/LIMIT) (ANTLR)
localdb                 per-request local SQLite database (mattn/go-sqlite3, cgo); default engine.DB implementation
engine                  orchestration: parse -> plan push-down -> load -> resolve; functional-options New (WithConnector/WithSources/WithConfig/WithRegistry/WithDB)
internal/telemetry      OpenTelemetry setup (env-gated; no-op when off)
internal/examples       render/check connectors.md examples from examples.yaml (tested)
tools/examples          dev CLI behind `make examples`/`examples-check`/`examples-test`
```

## Library vs. CLI

Everything outside `internal/` is public library API: `engine` (options-based
`New`, the `DB` interface for custom SQLite management), `source` (the
`Connector` contract + `Operator` enum), `config` (programmatic or YAML), and
`localdb` (the default `engine.DB`). The engine has **no default connectors**;
the `connectors` package holds dfetch's stock set, and the CLI composes
`connectors.DefaultOptions()` + `engine.WithConfig(cfg)` in `cmd/engine.go`.
Adding a builtin means editing `connectors/connectors.go`, not the engine. See
the "Use as a library" section of README.md.

## How a query runs

A connector is registered under a SQL schema (e.g. `github`) and exposes tables
(`github.issues`). `engine.Run`: parse → for each schema-qualified source, plan a
push-down `source.ScanRequest` (filters/ORDER BY/LIMIT) and `Scan` the connector
→ `ATTACH ':memory:' AS <schema>`, create the table, load the rows → run the
original SQL **verbatim** against SQLite (the source of truth; connectors may
return a superset). `Scan` streams rows via an `emit` callback (one chunk per API
page); the engine creates all tables up front, then fetches sources concurrently
(`errgroup`) and loads each chunk **as it arrives**, serializing the writes with a
mutex onto localdb's single pinned connection. LIMIT is pushed to a source
when it's single-source, or when it's the driving source of a join the LIMIT can
safely ride (ordering entirely on it, and the join can't drop its rows — every
other source pinned to constants, or a leftmost source with only LEFT/CROSS
joins); see `limitSafeForJoin` in `engine/plan.go`. The connector
additionally refuses to push unless it consumed every filter and honored the
order.

Most connectors declare every table in `Tables()`. A **dynamic** source (a SQL
warehouse) instead returns an empty `Tables()` and implements the optional
`source.SchemaDescriber` / `source.TableLister` interfaces; the engine resolves a
referenced table's columns on demand (`resolveTable` prefers `DescribeTable`) and
`dfetch tables` browses tiered (schemas+counts → names → columns). See the
"Dynamic sources" section of `source/README.md`.

## Debugging with traces

dfetch emits OpenTelemetry traces (`internal/telemetry`). Tracing is **off
unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set**, so it never interferes with normal
runs. To inspect what a query does:

```sh
docker compose up -d           # otel-collector (:4318) -> Jaeger (UI on :16686)
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
go run . query "<sql>"
# open http://localhost:16686, service "dfetch"
```

dfetch exports to the local OpenTelemetry Collector, which forwards to Jaeger
(pipeline in `otel-collector.yaml`) — add exporters/processors there rather
than pointing dfetch elsewhere.

One CLI invocation = one trace, rooted in a `cli.<command>` span started by
`cmd.Execute` (every subcommand, not just query):
`cli.query → engine.Run → engine.loadSource → connector.scan → HTTP
GET` (one per API page, via `otelhttp`) plus the SQLite `ATTACH/CREATE/INSERT/
SELECT` spans (via `XSAM/otelsql`). Use it to see API-call count, where time went
(API vs. local SQL), and which step errored. Instrumentation lives in the library
wraps (`github.New` transport, `localdb.Open` driver) and engine app-spans; new
code in those layers should keep `context` threaded so spans nest correctly.

## Conventions

- **Credentials:** connector secrets resolve via `source.Credential`
  (`source/credential.go`) with fixed precedence: plain param > env var >
  `<x>_func` (Go func, programmatic config only) > `<x>_command` (argv, no
  shell). Resolution is lazy (first use, never at construction) and race-safe;
  success is cached for the connector's lifetime, failures are retried on the
  next use. New connectors build one with `source.NewCredential` and resolve
  on first use — typically `Get` at the top of `Scan`; a dynamic connector may
  defer to its request/DB layer instead (newrelic's `gqlPost`, postgres's
  `getDB`). Auth tests must clear the relevant env vars with `t.Setenv` (env
  wins, so ambient credentials shadow the source under test).
- **Testing:** use [testify](https://github.com/stretchr/testify) — `require` for
  fatal assertions, `assert` for non-fatal. Do not write bare
  `if got != want { t.Fatalf(...) }` checks in new tests.
- **cgo:** the local SQLite engine uses `mattn/go-sqlite3`, so builds need
  `CGO_ENABLED=1` and a C compiler. The Makefile sets this for you.
- **Generated parser:** `internal/sqlparse/gen` is ANTLR-generated and committed.
  Do not hand-edit it. Regenerate with `make generate` (needs Java; pinned to
  ANTLR 4.13.1, matching the `antlr4-go/antlr/v4` runtime in go.mod). The grammar
  lives in `internal/sqlparse/grammar`.
- **Lint/coverage with generated code:** golangci-lint skips generated files
  automatically; the `vet` and `coverage` make targets exclude
  `internal/sqlparse/gen` (it has no tests and would otherwise sink the coverage
  gate and trip vet's unreachable analyzer).

## Common commands

```sh
make build      # build ./bin/dfetch
make test       # go test ./...
make coverage   # tests + coverage gate (excludes generated code)
make lint       # golangci-lint
make vet        # go vet (excludes generated code)
make fmt        # format Markdown/YAML/JSON docs + config with Prettier (needs Node)
make fmt-check  # offline: fail if any doc/config is unformatted (run in CI's lint job)
make generate   # regenerate the ANTLR parser (requires Java)
make examples   # regenerate connectors.md query examples from examples.yaml
make examples-check  # offline: fail if connectors.md drifted from examples.yaml
make examples-test   # run every example query against live services
```

Connector query examples are generated from `examples.yaml` into `connectors.md`
(see `internal/examples` / `tools/examples`) — edit the YAML, not the marked
example blocks. The target doc is the Makefile's `EXAMPLES_DOC` (`connectors.md`).

## CI

`.github/workflows/ci.yaml` runs build, `make coverage`, golangci-lint, and
`make fmt-check` (Prettier) on push/PR to `main`. Releases build per-OS natively
(cgo can't cross-compile cleanly from one runner) — see
`.github/workflows/release.yaml`.
