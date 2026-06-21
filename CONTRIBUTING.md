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
internal/source/github  GitHub connector (issues, pulls, repos, commits, releases, workflow_runs)
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

A connector adapts one external system into SQL tables under a schema. The full
guide — the `source.Connector` contract, the query lifecycle, push-down
semantics, the table/column → SQLite mapping, streaming, registration, testing,
worked examples, and a step-by-step + checklist — lives next to the code:

**→ [`internal/source/README.md`](internal/source/README.md)**

It's detailed enough to hand to an agent as the basis for an implementation plan.
The existing `internal/source/github` and `internal/source/jaeger` connectors are
the reference implementations.
