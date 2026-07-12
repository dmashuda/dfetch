// Package connectors provides dfetch's default connector set, kept out of the
// engine so a program embedding the engine links only the connectors it
// imports. The CLI composes DefaultOptions with the user's config; a library
// user can do the same, pick individual factories, or skip this package
// entirely and register its own source.Connector implementations.
package connectors

import (
	"fmt"
	"sort"

	"github.com/dmashuda/dfetch/engine"
	"github.com/dmashuda/dfetch/source"
	"github.com/dmashuda/dfetch/source/ckan"
	"github.com/dmashuda/dfetch/source/docker"
	"github.com/dmashuda/dfetch/source/files"
	"github.com/dmashuda/dfetch/source/github"
	"github.com/dmashuda/dfetch/source/jaeger"
	"github.com/dmashuda/dfetch/source/jira"
	"github.com/dmashuda/dfetch/source/newrelic"
	"github.com/dmashuda/dfetch/source/postgres"
	"github.com/dmashuda/dfetch/source/slack"
)

// Builtins returns the connector types usable without configuration. Each is
// also auto-instantiated under its own name as a schema (so New(nil) must
// work).
func Builtins() map[string]source.Factory {
	return map[string]source.Factory{
		"datagov": ckan.New,
		"docker":  docker.New,
		"files":   files.New,
		"github":  github.New,
		"jaeger":  jaeger.New,
		"slack":   slack.New,
	}
}

// ConfigOnly returns the connector types available via config (`type: <name>`)
// but not auto-instantiated, because they need params to construct (e.g. a
// Postgres DSN).
func ConfigOnly() map[string]source.Factory {
	return map[string]source.Factory{
		"jira":     jira.New,
		"newrelic": newrelic.New,
		"postgres": postgres.New,
	}
}

// DefaultRegistry returns a registry with every default connector type
// (Builtins and ConfigOnly) registered.
func DefaultRegistry() *source.Registry {
	r := source.NewRegistry()
	for typeName, factory := range Builtins() {
		r.Register(typeName, factory)
	}
	for typeName, factory := range ConfigOnly() {
		r.Register(typeName, factory)
	}
	return r
}

// NewBuiltins instantiates every builtin with nil params, keyed by its default
// schema name.
func NewBuiltins() (map[string]source.Connector, error) {
	out := make(map[string]source.Connector)
	for typeName, factory := range Builtins() {
		c, err := factory(nil)
		if err != nil {
			return nil, fmt.Errorf("initializing built-in connector %q: %w", typeName, err)
		}
		out[typeName] = c
	}
	return out, nil
}

// DefaultOptions returns engine options reproducing the stock dfetch setup:
// the default registry plus every builtin registered under its own schema
// name. Append further options (e.g. engine.WithConfig) to override or add.
func DefaultOptions() ([]engine.Option, error) {
	conns, err := NewBuiltins()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(conns))
	for name := range conns {
		names = append(names, name)
	}
	sort.Strings(names)

	opts := []engine.Option{engine.WithRegistry(DefaultRegistry())}
	for _, name := range names {
		opts = append(opts, engine.WithConnector(name, conns[name]))
	}
	return opts, nil
}
