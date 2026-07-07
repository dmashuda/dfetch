package engine

import (
	"context"
	"testing"

	"github.com/dmashuda/dfetch/internal/config"
	"github.com/dmashuda/dfetch/internal/source"
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

func (f *fakeConn) Scan(_ context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	f.got = req
	if f.err != nil {
		return f.err
	}
	return emit(f.rows)
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

// TestRunPushesJoinLimitToDrivingSource end-to-ends the join LIMIT pushdown: the
// driving table gets LIMIT, the pinned lookup does not, and the result is the
// correct top-N (the fake connectors return supersets, so SQLite still trims).
func TestRunPushesJoinLimitToDrivingSource(t *testing.T) {
	drv := &fakeConn{
		tables: []source.TableSchema{{Name: "t", Columns: []source.Column{
			{Name: "owner"}, {Name: "fk"}, {Name: "ts", Type: "INTEGER"},
		}}},
		rows: &source.Rows{Columns: []string{"owner", "fk", "ts"}, Rows: [][]any{
			{"x", "k", int64(3)}, {"x", "k", int64(1)}, {"x", "k", int64(2)},
		}},
	}
	dim := &fakeConn{
		tables: []source.TableSchema{{Name: "u", Columns: []source.Column{{Name: "key"}, {Name: "label"}}}},
		rows:   &source.Rows{Columns: []string{"key", "label"}, Rows: [][]any{{"k", "X"}}},
	}
	e := engineWith(map[string]source.Connector{"drv": drv, "dim": dim})

	res, err := e.Run(context.Background(),
		"SELECT d.ts, u.label FROM drv.t d JOIN dim.u u ON u.key = d.fk WHERE d.owner='x' AND d.fk='k' ORDER BY d.ts DESC LIMIT 2")
	require.NoError(t, err)

	require.NotNil(t, drv.got.Limit, "LIMIT pushed to the driving source")
	assert.Equal(t, 2, *drv.got.Limit)
	assert.Nil(t, dim.got.Limit, "LIMIT not pushed to the lookup source")

	require.Len(t, res.Rows, 2)
	assert.Equal(t, int64(3), res.Rows[0][0])
	assert.Equal(t, int64(2), res.Rows[1][0])
}

func engineWith(conns map[string]source.Connector) *Engine {
	return &Engine{connectors: conns}
}

// TestRunWithParamsBindsNamedParam end-to-ends a saved-query bind: a :title
// parameter flows through parse, load, and the final SQLite query, filtering to
// the matching row. The bound value is also resolved during planning and pushed
// to the connector as a filter, so sources that require a filter value at fetch
// time still receive it.
func TestRunWithParamsBindsNamedParam(t *testing.T) {
	conn := issuesConn()
	e := engineWith(map[string]source.Connector{"github": conn})

	res, err := e.RunWithParams(context.Background(),
		"SELECT number, title FROM github.issues WHERE title = :title",
		map[string]any{"title": "b"})
	require.NoError(t, err)

	// The bound value reached the connector as a pushed-down filter.
	assert.Contains(t, conn.got.Filters,
		source.Filter{Column: "title", Op: source.OpEq, Value: "b"})

	// And the final result is filtered to the matching row.
	require.Len(t, res.Rows, 1)
	assert.Equal(t, int64(2), res.Rows[0][0])
	assert.Equal(t, "b", res.Rows[0][1])
}

// Warnings emitted by a connector reach Result.Warnings.
func TestRunCollectsConnectorWarnings(t *testing.T) {
	conn := issuesConn()
	conn.rows.Warnings = []string{"github.issues: stopped at the 10-page cap"}
	e := engineWith(map[string]source.Connector{"github": conn})

	res, err := e.Run(context.Background(),
		"SELECT number FROM github.issues WHERE owner='golang' AND repo='go'")
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, "github.issues: stopped at the 10-page cap")
}

// A parse failure is labeled as such.
func TestRunParseErrorIsLabeled(t *testing.T) {
	e := engineWith(map[string]source.Connector{})
	_, err := e.Run(context.Background(), "SELECT FROM")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing SQL:")
}

// A SQLite error from the final query is labeled as a local-database failure.
func TestRunLocalQueryErrorIsLabeled(t *testing.T) {
	conn := issuesConn()
	e := engineWith(map[string]source.Connector{"github": conn})
	_, err := e.Run(context.Background(),
		"SELECT no_such_column FROM github.issues WHERE owner='golang' AND repo='go'")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "executing query on the local database:")
}

func TestRunPushdownAndResolve(t *testing.T) {
	conn := issuesConn()
	e := engineWith(map[string]source.Connector{"github": conn})

	res, err := e.Run(context.Background(),
		"SELECT number, title FROM github.issues WHERE owner='golang' AND repo='go' AND state='open' ORDER BY updated_at DESC LIMIT 2")
	require.NoError(t, err)

	// Push-down: filters, order, and limit reached the connector.
	assert.ElementsMatch(t, []source.Filter{
		{Column: "owner", Op: source.OpEq, Value: "golang"},
		{Column: "repo", Op: source.OpEq, Value: "go"},
		{Column: "state", Op: source.OpEq, Value: "open"},
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
