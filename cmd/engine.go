package cmd

import (
	"github.com/dmashuda/dfetch/config"
	"github.com/dmashuda/dfetch/connectors"
	"github.com/dmashuda/dfetch/engine"
)

// newEngine builds the engine with the default connector set plus cfg's
// sources; a config source can override a builtin's schema name.
func newEngine(cfg *config.Config) (*engine.Engine, error) {
	opts, err := connectors.DefaultOptions()
	if err != nil {
		return nil, err
	}
	return engine.New(append(opts, engine.WithConfig(cfg))...)
}
