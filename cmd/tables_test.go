package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTablesCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no config file

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"tables", "github"})

	require.NoError(t, rootCmd.Execute())
	got := out.String()
	assert.Contains(t, got, "github.issues")
	assert.Contains(t, got, "github.pulls")
	assert.Contains(t, got, "github.repos")
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
