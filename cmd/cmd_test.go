package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionCommand(t *testing.T) {
	SetVersion("v9.9.9-test")

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"version"})

	require.NoError(t, rootCmd.Execute())
	assert.Equal(t, "v9.9.9-test", strings.TrimSpace(out.String()))
}
