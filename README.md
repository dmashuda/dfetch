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
cmd/                    cobra CLI: root, query, tables, version
internal/config         YAML config loading (schema -> connector)
internal/source         Connector interface + ScanRequest (push-down) + registry
internal/source/github  GitHub connector (issues, pulls, repos)
internal/sqlparse       SQL parse/validate + typed AST (incl. ORDER BY/LIMIT) + SQL rendering
internal/localdb        per-request local SQLite database (attach/create/insert/query)
internal/engine         orchestration: parse -> plan push-down -> load -> resolve
```
