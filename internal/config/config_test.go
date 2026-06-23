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
	t.Chdir(t.TempDir()) // no dfetch.yaml in cwd or home

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

// A dfetch.yaml in the working directory is the default config, taking
// precedence over the home-directory fallback.
func TestLoadPrefersCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "dfetch.yaml"),
		[]byte("sources:\n  - name: home\n    type: github\n"), 0o600))

	cwd := t.TempDir()
	t.Chdir(cwd)
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "dfetch.yaml"),
		[]byte("sources:\n  - name: local\n    type: github\n"), 0o600))

	cfg, err := Load("")
	require.NoError(t, err)
	require.Len(t, cfg.Sources, 1)
	assert.Equal(t, "local", cfg.Sources[0].Name)
}

// With no dfetch.yaml in cwd, Load falls back to ~/dfetch.yaml.
func TestLoadFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "dfetch.yaml"),
		[]byte("sources:\n  - name: home\n    type: github\n"), 0o600))

	t.Chdir(t.TempDir()) // empty cwd, no local dfetch.yaml

	cfg, err := Load("")
	require.NoError(t, err)
	require.Len(t, cfg.Sources, 1)
	assert.Equal(t, "home", cfg.Sources[0].Name)
}

func TestLoadParsesQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dfetch.yaml")
	content := `
queries:
  - name: fetch-trace
    description: Spans for a trace
    params: [trace_id]
    columns: [trace_id, name, duration]
    sql: SELECT * FROM jaeger.spans WHERE trace_id = :trace_id
  - name: all-services
    sql: SELECT name FROM jaeger.services
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Queries, 2)

	q, ok := cfg.Query("fetch-trace")
	require.True(t, ok)
	assert.Equal(t, "Spans for a trace", q.Description)
	assert.Equal(t, []string{"trace_id"}, q.Params)
	assert.Equal(t, []string{"trace_id", "name", "duration"}, q.Columns)
	assert.Contains(t, q.SQL, ":trace_id")

	_, ok = cfg.Query("nope")
	assert.False(t, ok)
}
