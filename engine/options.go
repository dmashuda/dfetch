package engine

import (
	"github.com/dmashuda/dfetch/config"
	"github.com/dmashuda/dfetch/source"
)

// Option configures the Engine built by New.
type Option func(*settings)

// settings accumulates the options passed to New. Connector registrations are
// kept in order so the last registration of a schema name wins, regardless of
// whether it came from WithConnector or a config source.
type settings struct {
	registry *source.Registry
	ops      []connectorOp
	openDB   OpenDBFunc
}

// connectorOp is one connector registration: either a prebuilt connector
// (conn != nil) or a config source to build via the registry at New time.
type connectorOp struct {
	name string
	conn source.Connector
	src  config.SourceConfig
}

// WithConnector registers a caller-built connector under a schema name, so its
// tables resolve as <name>.<table>. This is how a program plugs in its own
// Connector implementation.
func WithConnector(name string, conn source.Connector) Option {
	return func(s *settings) {
		s.ops = append(s.ops, connectorOp{name: name, conn: conn})
	}
}

// WithSources declares config-style sources; New builds each via the registry
// (see WithRegistry) from its Type and Params and registers it under its Name.
func WithSources(sources ...config.SourceConfig) Option {
	return func(s *settings) {
		for _, sc := range sources {
			s.ops = append(s.ops, connectorOp{src: sc})
		}
	}
}

// WithConfig registers every source declared in cfg, equivalent to
// WithSources(cfg.Sources...). A nil cfg is a no-op.
func WithConfig(cfg *config.Config) Option {
	return func(s *settings) {
		if cfg == nil {
			return
		}
		for _, sc := range cfg.Sources {
			s.ops = append(s.ops, connectorOp{src: sc})
		}
	}
}

// WithRegistry sets the factory registry used to build WithSources/WithConfig
// entries. Without it the registry is empty, so any typed source fails at New
// with "unknown connector type". The connectors package provides
// DefaultRegistry() with every built-in type.
func WithRegistry(reg *source.Registry) Option {
	return func(s *settings) {
		if reg != nil {
			s.registry = reg
		}
	}
}

// WithDB sets how the per-request local database is created. The default opens
// localdb's temp-file SQLite database.
func WithDB(open OpenDBFunc) Option {
	return func(s *settings) {
		if open != nil {
			s.openDB = open
		}
	}
}
