# CLAUDE.md

Guidance for working in the dfetch repo.

## What dfetch is

A Go library + CLI that takes a SQL query (SQLite syntax), parses/validates it,
fetches the referenced data sources (each exposed as a SQLite table), loads them
into a per-request local SQLite database, and resolves the query against it.

## Layout

```
cmd/                 cobra CLI: root, query, version
internal/config      YAML config loading (table -> source mapping)
internal/source      Source interface + type registry (csv, ...)
internal/sqlparse    SQL parse/validate + table/column extraction (ANTLR)
internal/localdb     per-request local SQLite database (mattn/go-sqlite3, cgo)
internal/engine      orchestration: parse -> fetch -> load -> resolve
```

## Conventions

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
make generate   # regenerate the ANTLR parser (requires Java)
```

## CI

`.github/workflows/ci.yaml` runs build, `make coverage`, and golangci-lint on
push/PR to `main`. Releases build per-OS natively (cgo can't cross-compile
cleanly from one runner) — see `.github/workflows/release.yaml`.
