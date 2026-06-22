package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runTables executes `dfetch tables <args...>` against a config-less engine and
// returns combined stdout/stderr.
func runTables(t *testing.T, args ...string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // no config file -> builtins only

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"tables"}, args...))
	require.NoError(t, rootCmd.Execute())
	return out.String()
}

// No args: schemas with table counts, not individual tables or columns.
func TestTablesSummaries(t *testing.T) {
	got := runTables(t)
	assert.Contains(t, got, "github")
	assert.Contains(t, got, "tables)")       // e.g. "github  (7 tables)"
	assert.NotContains(t, got, "updated_at") // no columns at the summary level
}

// `tables <schema>`: schema-qualified table names, no columns.
func TestTablesListsNames(t *testing.T) {
	got := runTables(t, "github")
	assert.Contains(t, got, "github.issues")
	assert.Contains(t, got, "github.pulls")
	assert.Contains(t, got, "github.repos")
	assert.NotContains(t, got, "updated_at") // columns are one level deeper
}

// `tables <schema> <filter>`: only tables whose name contains the filter.
func TestTablesListsNamesFiltered(t *testing.T) {
	got := runTables(t, "github", "issues")
	assert.Contains(t, got, "github.issues")
	assert.NotContains(t, got, "github.pulls")
	assert.NotContains(t, got, "github.repos")
}

// `tables <schema>.<table>`: the table's columns.
func TestTablesDescribesColumns(t *testing.T) {
	got := runTables(t, "github.issues")
	assert.Contains(t, got, "github.issues")
	assert.Contains(t, got, "owner")
	assert.Contains(t, got, "updated_at")
}

func TestTablesUnknownSchema(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"tables", "nope"})

	assert.Error(t, rootCmd.Execute())
}

func TestTablesUnknownTable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"tables", "github.nope"})

	assert.Error(t, rootCmd.Execute())
}
