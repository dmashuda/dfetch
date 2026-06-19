package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMissingDefaultReturnsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Empty(t, cfg.Sources)
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
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Sources, 2)
	assert.Equal(t, "users", cfg.Sources[0].Table)
	assert.Equal(t, "csv", cfg.Sources[0].Type)
	assert.Equal(t, "./users.csv", cfg.Sources[0].Params["path"])
}

func TestLoadMissingExplicitPathErrors(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	assert.Error(t, err)
}
