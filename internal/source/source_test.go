package source

import (
	"context"
	"testing"

	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeConnector struct{}

func (fakeConnector) Tables() []TableSchema { return []TableSchema{{Name: "t"}} }
func (fakeConnector) Scan(_ context.Context, _ ScanRequest, emit func(*Rows) error) error {
	return emit(&Rows{})
}

func TestRegistryBuild(t *testing.T) {
	r := NewRegistry()
	r.Register("fake", func(map[string]any) (Connector, error) {
		return fakeConnector{}, nil
	})

	c, err := r.Build("fake", nil)
	require.NoError(t, err)
	require.Len(t, c.Tables(), 1)
	assert.Equal(t, "t", c.Tables()[0].Name)
}

func TestRegistryBuildUnknown(t *testing.T) {
	_, err := NewRegistry().Build("nope", nil)
	assert.Error(t, err)
}

func TestRegistryRegisterDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	f := func(map[string]any) (Connector, error) { return fakeConnector{}, nil }
	r.Register("x", f)
	assert.Panics(t, func() { r.Register("x", f) })
}

func TestTableSchemaColumnNames(t *testing.T) {
	ts := TableSchema{Columns: []Column{{Name: "a"}, {Name: "b"}}}
	assert.Equal(t, []string{"a", "b"}, ts.ColumnNames())
}

func TestScanRequestFilter(t *testing.T) {
	req := ScanRequest{Filters: []Filter{
		{Column: "state", Op: sqlparse.OpEq, Value: "open"},
		{Column: "owner", Op: sqlparse.OpEq, Value: "golang"},
	}}

	f, ok := req.Filter("owner")
	require.True(t, ok)
	assert.Equal(t, "golang", f.Value)
	assert.Equal(t, sqlparse.OpEq, f.Op)

	_, ok = req.Filter("missing")
	assert.False(t, ok)
}
