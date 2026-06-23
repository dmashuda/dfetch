package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueriesLists(t *testing.T) {
	withConfig(t, savedQueriesConfig)
	out, err := execRoot(t, "queries")
	require.NoError(t, err)
	assert.Contains(t, out, "NAME")
	assert.Contains(t, out, "fetch-trace")
	assert.Contains(t, out, "trace_id")
	assert.Contains(t, out, "Spans for a trace")
	// A query with no params renders a "-" placeholder.
	assert.Contains(t, out, "all-services")
}

func TestQueriesEmpty(t *testing.T) {
	withConfig(t, "sources: []\n")
	out, err := execRoot(t, "queries")
	require.NoError(t, err)
	assert.Contains(t, out, "no saved queries configured")
}
