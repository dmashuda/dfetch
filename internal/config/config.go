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

// Config is the top-level dfetch configuration.
type Config struct {
	Sources []SourceConfig `yaml:"sources"`
}

// DefaultPath returns the default config path (~/.dfetch/config.yaml).
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".dfetch", "config.yaml"), nil
}

// Load reads the config from path. If path is empty, the default path is used.
// A missing default config yields an empty Config rather than an error, so
// dfetch can run without a config file present.
func Load(path string) (*Config, error) {
	usingDefault := path == ""
	if usingDefault {
		p, err := DefaultPath()
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
