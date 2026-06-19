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
  - name: github
    type: github
    params:
      base_url: https://github.example.com/api/v3
  - name: gh2
    type: github
    params: {}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Sources, 2)
	assert.Equal(t, "github", cfg.Sources[0].Name)
	assert.Equal(t, "github", cfg.Sources[0].Type)
	assert.Equal(t, "https://github.example.com/api/v3", cfg.Sources[0].Params["base_url"])
}

func TestLoadMissingExplicitPathErrors(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	assert.Error(t, err)
}
