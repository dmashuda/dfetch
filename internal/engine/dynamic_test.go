package engine

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDynamicConn models a SQL-warehouse-style connector: it returns no tables
// from Tables() and instead resolves/lists them on demand via SchemaDescriber
// and TableLister. It counts calls so tests can prove resolution stays lazy.
type fakeDynamicConn struct {
	cols      map[string][]source.Column // the "catalog": table -> columns
	rows      *source.Rows
	describeN int
	listN     int
	tablesN   int
}

func (f *fakeDynamicConn) Tables() []source.TableSchema { f.tablesN++; return nil }

func (f *fakeDynamicConn) Scan(_ context.Context, _ source.ScanRequest, emit func(*source.Rows) error) error {
	if f.rows == nil {
		return nil
	}
	return emit(f.rows)
}

func (f *fakeDynamicConn) DescribeTable(_ context.Context, table string) (source.TableSchema, bool, error) {
	f.describeN++
	cols, ok := f.cols[table]
	if !ok {
		return source.TableSchema{}, false, nil
	}
	return source.TableSchema{Name: table, Columns: cols}, true, nil
}

func (f *fakeDynamicConn) ListTables(_ context.Context, opts source.ListOptions) ([]string, error) {
	f.listN++
	var names []string
	for name := range f.cols {
		if opts.Filter == "" || strings.Contains(strings.ToLower(name), strings.ToLower(opts.Filter)) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// A query against a dynamic source resolves via DescribeTable and never
// enumerates the whole catalog with Tables().
func TestRunResolvesViaDescribeTable(t *testing.T) {
	dyn := &fakeDynamicConn{
		cols: map[string][]source.Column{
			"events": {{Name: "id", Type: "INTEGER"}, {Name: "kind", Type: "TEXT"}},
		},
		rows: &source.Rows{Columns: []string{"id", "kind"}, Rows: [][]any{
			{int64(1), "click"}, {int64(2), "view"},
		}},
	}
	e := engineWith(map[string]source.Connector{"warehouse": dyn})

	res, err := e.Run(context.Background(), "SELECT id, kind FROM warehouse.events WHERE kind='click'")
	require.NoError(t, err)
	assert.Positive(t, dyn.describeN, "resolved the table via DescribeTable")
	assert.Zero(t, dyn.tablesN, "did not enumerate the whole catalog")
	require.Len(t, res.Rows, 1) // SQLite applied WHERE kind='click' to the superset
	assert.Equal(t, int64(1), res.Rows[0][0])
}

// DescribeTable reporting found=false surfaces the usual not-found error.
func TestRunUnknownTableDynamic(t *testing.T) {
	dyn := &fakeDynamicConn{cols: map[string][]source.Column{"events": {{Name: "id"}}}}
	e := engineWith(map[string]source.Connector{"warehouse": dyn})

	_, err := e.Run(context.Background(), "SELECT * FROM warehouse.missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no table")
}

func TestSchemaSummariesMixed(t *testing.T) {
	dyn := &fakeDynamicConn{cols: map[string][]source.Column{"a": nil, "b": nil, "c": nil}}
	e := engineWith(map[string]source.Connector{"warehouse": dyn, "github": issuesConn()})

	byName := map[string]SchemaSummary{}
	for _, s := range e.SchemaSummaries(context.Background()) {
		byName[s.Schema] = s
	}
	assert.False(t, byName["github"].Dynamic)
	assert.Equal(t, 1, byName["github"].TableCount) // issuesConn serves one table
	assert.True(t, byName["warehouse"].Dynamic)
	assert.Equal(t, 3, byName["warehouse"].TableCount)
}

func TestListTablesDynamicFilter(t *testing.T) {
	dyn := &fakeDynamicConn{cols: map[string][]source.Column{"orders": nil, "order_items": nil, "users": nil}}
	e := engineWith(map[string]source.Connector{"warehouse": dyn})

	names, err := e.ListTables(context.Background(), "warehouse", "order")
	require.NoError(t, err)
	assert.Equal(t, []string{"order_items", "orders"}, names)
}

func TestDescribeTableDynamic(t *testing.T) {
	dyn := &fakeDynamicConn{cols: map[string][]source.Column{"events": {{Name: "id", Type: "INTEGER"}}}}
	e := engineWith(map[string]source.Connector{"warehouse": dyn})

	ts, err := e.DescribeTable(context.Background(), "warehouse", "events")
	require.NoError(t, err)
	assert.Equal(t, "events", ts.Name)
	require.Len(t, ts.Columns, 1)
	assert.Equal(t, "id", ts.Columns[0].Name)
}
