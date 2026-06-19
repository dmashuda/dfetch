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

```sh
dfetch query "SELECT * FROM users JOIN orders USING (user_id)"
dfetch query "SELECT 1" --format json   # formats: table | json | csv
dfetch version
```

## Configuration

By default `dfetch` reads `~/.dfetch/config.yaml` (override with `--config`):

```yaml
sources:
  - table: users
    type: csv
    params:
      path: ./users.csv
  - table: orders
    type: http
    params:
      url: https://example.com/orders.json
```

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
cmd/                 cobra CLI: root, query, version
internal/config      YAML config loading
internal/source      Source interface + type registry (csv, ...)
internal/sqlparse    SQL parse/validate + table/column extraction + typed AST + SQL rendering
internal/localdb     per-request local SQLite database
internal/engine      orchestration: parse -> fetch -> load -> resolve
```
