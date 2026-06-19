package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleResult() *Result {
	return &Result{
		Columns: []string{"id", "name"},
		Rows:    [][]any{{1, "alice"}, {2, "bob"}},
	}
}

func TestResultWriteTable(t *testing.T) {
	var sb strings.Builder
	require.NoError(t, sampleResult().Write(&sb, "table"))
	out := sb.String()
	assert.Contains(t, out, "id\tname")
	assert.Contains(t, out, "1\talice")
}

func TestResultWriteJSON(t *testing.T) {
	var sb strings.Builder
	require.NoError(t, sampleResult().Write(&sb, "json"))
	assert.Contains(t, sb.String(), `"name": "alice"`)
}

func TestResultWriteCSV(t *testing.T) {
	var sb strings.Builder
	require.NoError(t, sampleResult().Write(&sb, "csv"))
	out := sb.String()
	assert.Contains(t, out, "id,name")
	assert.Contains(t, out, "2,bob")
}

func TestResultWriteUnknownFormat(t *testing.T) {
	assert.Error(t, sampleResult().Write(&strings.Builder{}, "xml"))
}
