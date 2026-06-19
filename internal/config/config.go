// Package config loads dfetch's data source configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SourceConfig maps a SQL table name to a configured data source.
type SourceConfig struct {
	// Table is the name referenced in SQL queries.
	Table string `yaml:"table"`
	// Type selects the registered source implementation (e.g. "csv", "http").
	Type string `yaml:"type"`
	// Params holds source-specific options.
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
