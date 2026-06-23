// Package config loads dfetch's data source configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SourceConfig binds a SQL schema name to a configured connector.
type SourceConfig struct {
	// Name is the SQL schema referenced in queries (e.g. "github" in
	// github.issues).
	Name string `yaml:"name"`
	// Type selects the registered connector implementation (e.g. "github").
	Type string `yaml:"type"`
	// Params holds connector-specific options.
	Params map[string]any `yaml:"params"`
}

// SavedQuery is a reusable, named SQL query stored in the config. Params lists
// the named bind parameters the SQL references (as :name), in the positional
// order they are supplied on the command line. Columns is the default output
// projection; an empty list means every column the query produces.
type SavedQuery struct {
	// Name is the identifier used to run the query (dfetch run <name>).
	Name string `yaml:"name"`
	// Description is a short human-readable summary shown by `dfetch queries`.
	Description string `yaml:"description"`
	// Params are the named bind parameters (:name) the SQL references, in the
	// order positional CLI arguments are bound to them.
	Params []string `yaml:"params"`
	// Columns is the default set of output columns; empty means all columns.
	Columns []string `yaml:"columns"`
	// SQL is the query body, run with Params bound as SQLite parameters.
	SQL string `yaml:"sql"`
}

// Config is the top-level dfetch configuration.
type Config struct {
	Sources []SourceConfig `yaml:"sources"`
	Queries []SavedQuery   `yaml:"queries"`
}

// Query returns the saved query with the given name, if one is configured.
func (c *Config) Query(name string) (*SavedQuery, bool) {
	for i := range c.Queries {
		if c.Queries[i].Name == name {
			return &c.Queries[i], true
		}
	}
	return nil, false
}

// fileName is the per-project / per-user config filename.
const fileName = "dfetch.yaml"

// DefaultPath returns the home-directory fallback config path (~/dfetch.yaml).
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, fileName), nil
}

// discoverPath returns the default config path: ./dfetch.yaml in the current
// working directory when it exists, otherwise the home-directory fallback
// (~/dfetch.yaml). This makes config per-project by default while still allowing
// a machine-wide config.
func discoverPath() (string, error) {
	if cwd, err := os.Getwd(); err == nil {
		local := filepath.Join(cwd, fileName)
		if _, err := os.Stat(local); err == nil {
			return local, nil
		}
	}
	return DefaultPath()
}

// Load reads the config from path. If path is empty, the default path is used
// (./dfetch.yaml if present, else ~/dfetch.yaml). A missing default config
// yields an empty Config rather than an error, so dfetch can run without a
// config file present.
func Load(path string) (*Config, error) {
	usingDefault := path == ""
	if usingDefault {
		p, err := discoverPath()
		if err != nil {
			return nil, err
		}
		path = p
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if usingDefault && os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return &cfg, nil
}
