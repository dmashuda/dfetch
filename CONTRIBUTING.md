# Contributing to dfetch

## Prerequisites

- **Go** — the toolchain version in [`go.mod`](go.mod).
- **A C compiler + `CGO_ENABLED=1`.** The local query engine uses
  [`mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3), which is cgo. The
  Makefile sets `CGO_ENABLED=1` for you.
- **Java** — only to regenerate the SQL parser (`make generate`); normal builds
  don't need it (the generated code is committed).

Alternatively, use Nix:

```sh
# with direnv (recommended):
# optional but highly recommended: also install `nix-direnv`
# which provides better/smarter caching for Nix derivations
# https://github.com/nix-community/nix-direnv
echo "use flake" > .envrc
direnv allow

# with just nix:
nix develop
# or if you use a different shell like Fish you can do
nix develop --command fish
```

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

## Pull requests

Work on a branch and open a PR against `main`; don't push to `main` directly.
Keep PRs focused, make sure `make test`, `make lint`, and `make coverage` pass
locally, and let CI go green before merging. Commit messages use
conventional-commit prefixes — `feat:`, `fix:`, `docs:`, `chore:`, `build:`,
`test:` — which also drive the generated release notes.

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

### README query examples

The query examples in the README's GitHub and Jaeger connector sections are
**generated** from [`examples.yaml`](examples.yaml) — don't edit those blocks by
hand. Edit `examples.yaml` (each entry is a `desc` + raw-SQL `query`), then:

```sh
make examples        # regenerate the README blocks from examples.yaml
make examples-check  # offline: fail if the README has drifted
make examples-test   # run every example query against the live services
```

`make examples-test` uses `$GITHUB_TOKEN` (or `gh auth token`) for the GitHub
queries and skips the Jaeger group when no local Jaeger is reachable; mark an
example `run: false` if it can't run as-is (e.g. it contains a placeholder). The
generator/runner live in [`tools/examples`](tools/examples) over the tested
[`internal/examples`](internal/examples) package.

## Project layout

```
cmd/                    cobra CLI: root, query, tables, version
config         YAML config loading (schema -> connector)
source         Connector interface + ScanRequest (push-down) + registry
source/github  GitHub connector (issues, pulls, repos, commits, releases, workflow_runs, artifacts)
source/jaeger  Jaeger connector (spans, services, operations)
source/ckan    CKAN/data.gov connector (datasets, resources, organizations, groups)
source/docker  Docker connector (containers, images, volumes, networks), local unix socket
source/slack   Slack connector (channels, users, messages, search), Web API
source/newrelic New Relic connector (dynamic NRDB event types + accounts/entities/alerts/issues), NerdGraph, config-only via `type: newrelic`
source/postgres Postgres connector (dynamic; SQL push-down), config-only via `type: postgres`
source/jira    Jira Cloud connector (issues via JQL push-down, projects, comments), REST v3, config-only via `type: jira`
internal/sqlparse       SQL parse/validate + typed AST (ORDER BY/LIMIT) + SQL rendering
localdb        per-request local SQLite database (attach/create/insert/query)
engine         orchestration: parse -> plan push-down -> load -> resolve
internal/telemetry      OpenTelemetry setup (env-gated; no-op when off)
internal/examples       render/check README query examples from examples.yaml
tools/examples          dev CLI: gen/check/run the examples (make examples*)
```

### How a query runs

A connector is registered under a SQL schema (e.g. `github`) and exposes tables
(`github.issues`). `engine.Run`:

1. **parse** the SQL into a typed AST (`internal/sqlparse`);
2. for each schema-qualified source, **plan** a push-down `source.ScanRequest`
   (filters / `ORDER BY` / `LIMIT`) — see `engine/plan.go`;
3. **attach** an in-memory schema, **create** the table, and **scan** the
   connector, loading each emitted chunk into SQLite as it arrives (sources are
   fetched concurrently; writes are serialized onto localdb's single connection);
4. run the **original SQL verbatim** against SQLite — the source of truth, so a
   connector returning a superset is always corrected here.

## Adding a new connector

A connector adapts one external system into SQL tables under a schema. The full
guide — the `source.Connector` contract, the query lifecycle, push-down
semantics, the table/column → SQLite mapping, streaming, registration, testing,
worked examples, and a step-by-step + checklist — lives next to the code:

**→ [`source/README.md`](source/README.md)**

It's detailed enough to hand to an agent as the basis for an implementation plan.
The existing `source/github` and `source/jaeger` connectors are
the reference implementations.

A connector with a large/unknown table set (a SQL warehouse) returns an empty
`Tables()` and instead implements the optional `SchemaDescriber` and `TableLister`
capabilities, so the engine resolves only the referenced tables per query and
`dfetch tables` browses lazily (counts → names → columns). See the "Dynamic
sources" section of the connector guide.
