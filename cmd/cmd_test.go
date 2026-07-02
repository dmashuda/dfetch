package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Warnings go to stderr (prefixed "warning: "), never to stdout, so piping
// --format json|csv stays clean.
func TestPrintWarningsGoesToStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	printWarnings(cmd, []string{"github.issues: stopped at the 10-page cap", "more"})

	assert.Empty(t, out.String(), "stdout must stay clean")
	assert.Contains(t, errOut.String(), "warning: github.issues: stopped at the 10-page cap")
	assert.Contains(t, errOut.String(), "warning: more")
}

func TestVersionCommand(t *testing.T) {
	SetVersion("v9.9.9-test")

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"version"})

	require.NoError(t, rootCmd.Execute())
	assert.Equal(t, "v9.9.9-test", strings.TrimSpace(out.String()))
}

// SetVersion also wires cobra's --version flag.
func TestVersionFlag(t *testing.T) {
	SetVersion("v9.9.9-test")

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"--version"})

	require.NoError(t, rootCmd.Execute())
	assert.Contains(t, out.String(), "v9.9.9-test")
}

// `dfetch query -` reads the SQL from stdin. A source-free query resolves
// entirely in the local SQLite database, so this runs offline.
func TestQueryFromStdin(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dfetch.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("{}"), 0o600))
	t.Cleanup(func() { cfgFile = "" })

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetIn(strings.NewReader("SELECT 1 AS one, 'hi' AS greeting"))
	rootCmd.SetArgs([]string{"query", "-", "--config", cfg, "--format", "csv"})

	require.NoError(t, rootCmd.Execute())
	assert.Contains(t, out.String(), "one,greeting")
	assert.Contains(t, out.String(), "1,hi")
}
