package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingDefaultReturnsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") returned error: %v", err)
	}
	if len(cfg.Sources) != 0 {
		t.Fatalf("expected empty config, got %d sources", len(cfg.Sources))
	}
}

func TestLoadParsesSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
sources:
  - table: users
    type: csv
    params:
      path: ./users.csv
  - table: orders
    type: http
    params:
      url: https://example.com/orders.json
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(cfg.Sources))
	}
	if cfg.Sources[0].Table != "users" || cfg.Sources[0].Type != "csv" {
		t.Fatalf("unexpected first source: %+v", cfg.Sources[0])
	}
	if got := cfg.Sources[0].Params["path"]; got != "./users.csv" {
		t.Fatalf("unexpected param path: %v", got)
	}
}

func TestLoadMissingExplicitPathErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing explicit config path")
	}
}
