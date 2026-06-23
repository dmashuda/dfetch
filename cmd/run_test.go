package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withConfig writes a ./dfetch.yaml in a temp working directory and points HOME
// at an empty dir, so config.Load("") discovers exactly this config.
func withConfig(t *testing.T, content string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dfetch.yaml"), []byte(content), 0o600))
}

// execRoot runs the root command with args and returns combined output and any error.
func execRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return out.String(), err
}

const savedQueriesConfig = `
queries:
  - name: fetch-trace
    description: Spans for a trace
    params: [trace_id]
    columns: [trace_id, name]
    sql: SELECT * FROM jaeger.spans WHERE trace_id = :trace_id
  - name: all-services
    sql: SELECT name FROM jaeger.services
`

func TestRunUnknownQuery(t *testing.T) {
	withConfig(t, savedQueriesConfig)
	_, err := execRoot(t, "run", "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

func TestRunWrongArgCount(t *testing.T) {
	withConfig(t, savedQueriesConfig)
	// fetch-trace expects exactly one param; pass none.
	_, err := execRoot(t, "run", "fetch-trace")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expects 1 argument")
	assert.Contains(t, err.Error(), "<trace_id>")
}

func TestRunRequiresName(t *testing.T) {
	withConfig(t, savedQueriesConfig)
	_, err := execRoot(t, "run")
	require.Error(t, err) // cobra.MinimumNArgs(1)
}
