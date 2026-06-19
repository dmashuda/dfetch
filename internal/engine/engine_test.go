package engine

import (
	"context"
	"testing"

	"github.com/dmashuda/dfetch/internal/config"
	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConn records the ScanRequest it received and returns canned rows.
type fakeConn struct {
	tables []source.TableSchema
	got    source.ScanRequest
	rows   *source.Rows
	err    error
}

func (f *fakeConn) Tables() []source.TableSchema { return f.tables }

func (f *fakeConn) Scan(_ context.Context, req source.ScanRequest) (*source.Rows, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func issuesConn() *fakeConn {
	ts := source.TableSchema{Name: "issues", Columns: []source.Column{
		{Name: "owner", Type: "TEXT"},
		{Name: "repo", Type: "TEXT"},
		{Name: "number", Type: "INTEGER"},
		{Name: "title", Type: "TEXT"},
		{Name: "state", Type: "TEXT"},
		{Name: "updated_at", Type: "TEXT"},
	}}
	return &fakeConn{
		tables: []source.TableSchema{ts},
		rows: &source.Rows{
			Columns: ts.ColumnNames(),
			Rows: [][]any{
				{"golang", "go", int64(1), "a", "open", "2024-03"},
				{"golang", "go", int64(2), "b", "open", "2024-01"},
				{"golang", "go", int64(3), "c", "open", "2024-02"},
			},
		},
	}
}

func engineWith(conns map[string]source.Connector) *Engine {
	return &Engine{connectors: conns}
}

func TestRunPushdownAndResolve(t *testing.T) {
	conn := issuesConn()
	e := engineWith(map[string]source.Connector{"github": conn})

	res, err := e.Run(context.Background(),
		"SELECT number, title FROM github.issues WHERE owner='golang' AND repo='go' AND state='open' ORDER BY updated_at DESC LIMIT 2")
	require.NoError(t, err)

	// Push-down: filters, order, and limit reached the connector.
	assert.ElementsMatch(t, []source.Filter{
		{Column: "owner", Op: sqlparse.OpEq, Value: "golang"},
		{Column: "repo", Op: sqlparse.OpEq, Value: "go"},
		{Column: "state", Op: sqlparse.OpEq, Value: "open"},
	}, conn.got.Filters)
	assert.Equal(t, []source.OrderTerm{{Column: "updated_at", Desc: true}}, conn.got.OrderBy)
	require.NotNil(t, conn.got.Limit)
	assert.Equal(t, 2, *conn.got.Limit)

	// Resolution: SQLite applied ORDER BY updated_at DESC LIMIT 2 over the rows.
	assert.Equal(t, []string{"number", "title"}, res.Columns)
	require.Len(t, res.Rows, 2)
	assert.Equal(t, int64(1), res.Rows[0][0]) // 2024-03
	assert.Equal(t, int64(3), res.Rows[1][0]) // 2024-02
}

func TestRunResolvesWhereInSQLite(t *testing.T) {
	// The connector returns a superset (a 'closed' row); SQLite's WHERE filters it.
	conn := issuesConn()
	conn.rows.Rows = append(conn.rows.Rows, []any{"golang", "go", int64(4), "d", "closed", "2024-05"})
	e := engineWith(map[string]source.Connector{"github": conn})

	res, err := e.Run(context.Background(),
		"SELECT number FROM github.issues WHERE owner='golang' AND repo='go' AND state='open'")
	require.NoError(t, err)
	assert.Len(t, res.Rows, 3) // the closed row is excluded by SQLite
}

func TestRunUnknownSchema(t *testing.T) {
	e := engineWith(map[string]source.Connector{})
	_, err := e.Run(context.Background(), "SELECT * FROM nope.things WHERE owner='x'")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no connector for schema")
}

func TestRunUnqualifiedTableErrors(t *testing.T) {
	e := engineWith(map[string]source.Connector{})
	_, err := e.Run(context.Background(), "SELECT * FROM bare")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no schema")
}

func TestRunUnknownTable(t *testing.T) {
	e := engineWith(map[string]source.Connector{"github": issuesConn()})
	_, err := e.Run(context.Background(), "SELECT * FROM github.pulls WHERE owner='x' AND repo='y'")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no table")
}

func TestNewBuiltinGithub(t *testing.T) {
	e, err := New(&config.Config{})
	require.NoError(t, err)
	_, ok := e.connectors["github"]
	assert.True(t, ok)
}
