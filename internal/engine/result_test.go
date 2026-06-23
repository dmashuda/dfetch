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

func TestResultProjectNarrowsAndReorders(t *testing.T) {
	got, err := sampleResult().Project([]string{"name"})
	require.NoError(t, err)
	assert.Equal(t, []string{"name"}, got.Columns)
	require.Len(t, got.Rows, 2)
	assert.Equal(t, "alice", got.Rows[0][0])
	assert.Equal(t, "bob", got.Rows[1][0])

	// Order follows the requested column list, not the original.
	got, err = sampleResult().Project([]string{"name", "id"})
	require.NoError(t, err)
	assert.Equal(t, []string{"name", "id"}, got.Columns)
	assert.Equal(t, "alice", got.Rows[0][0])
	assert.Equal(t, 1, got.Rows[0][1])
}

func TestResultProjectEmptyReturnsAll(t *testing.T) {
	r := sampleResult()
	got, err := r.Project(nil)
	require.NoError(t, err)
	assert.Same(t, r, got)
	assert.Equal(t, []string{"id", "name"}, got.Columns)
}

func TestResultProjectUnknownColumnErrors(t *testing.T) {
	_, err := sampleResult().Project([]string{"id", "nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
	assert.Contains(t, err.Error(), "id, name") // lists available columns
}
