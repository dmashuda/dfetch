# Use dfetch as a library

dfetch is importable as a Go module: the CLI is a thin wrapper over the same
public packages. `engine.New` takes functional options, carries **no default
connectors**, and every piece is swappable:

```go
import (
    "github.com/dmashuda/dfetch/engine"
    "github.com/dmashuda/dfetch/source"
)

// Your own data source: implement source.Connector (Tables + Scan) and
// register it under a schema name.
eng, err := engine.New(engine.WithConnector("mydata", myConn))
res, err := eng.Run(ctx, `SELECT * FROM mydata.items WHERE state = 'open'`)
```

**The stock connector set** lives in the `connectors` package (importing
`engine` alone doesn't link pgx or any API client). This reproduces the CLI's
setup:

```go
import "github.com/dmashuda/dfetch/connectors"

opts, err := connectors.DefaultOptions() // builtins + default registry
eng, err := engine.New(append(opts, engine.WithConnector("mydata", myConn))...)
```

**Config without YAML files** — `config.Config` is plain data, so build it in
code (or load it with `config.Load`) and pass it with `engine.WithConfig`;
typed sources are built through the registry from `engine.WithRegistry`:

```go
eng, err := engine.New(
    engine.WithRegistry(connectors.DefaultRegistry()),
    engine.WithSources(config.SourceConfig{
        Name:   "warehouse",
        Type:   "postgres",
        Params: map[string]any{"dsn": dsn, "schemas": []string{"public"}},
    }),
)
```

Options apply in order and the last registration of a schema name wins, which
is how config overrides a builtin. `WithRegistry` merges rather than replaces,
so appending your own registry after `connectors.DefaultOptions()` adds or
overrides connector types without losing the default ones.

**Credentials without env vars** — every connector secret resolves through the
same chain: a plain param, else its env var(s), else a `<x>_func` param holding
a Go function, else a `<x>_command` param naming a command to run. The function
form only exists for programmatic config (YAML can't express it) and is how an
embedding program plugs in its own secret store:

```go
eng, err := engine.New(
    engine.WithRegistry(connectors.DefaultRegistry()),
    engine.WithSources(config.SourceConfig{
        Name: "github",
        Type: "github",
        Params: map[string]any{
            "token_func": func(ctx context.Context) (string, error) {
                return mySecrets.Fetch(ctx, "github-token")
            },
        },
    }),
)
```

Resolution is lazy (first use of the schema), cached on success for the
connector's lifetime (failures retry on the next query), and race-safe. Custom connectors get the same behavior from
`source.NewCredential`.

**Custom SQLite management** — the engine drives the per-request database
through the `engine.DB` interface (`Attach`, `CreateTable`, `Insert`, `Query`,
`Close`) and opens one per run via `engine.WithDB`. The default is `localdb`
(temp files, removed on close); supply your own opener to control file
placement, caching, or lifecycle:

```go
eng, err := engine.New(
    engine.WithConnector("mydata", myConn),
    engine.WithDB(func(ctx context.Context) (engine.DB, error) {
        return myDBManager.Open(ctx) // must implement engine.DB
    }),
)
```

Tracing works the same as the CLI: spans go to the global OpenTelemetry tracer
provider, a no-op unless your program installs one.
