package localdb

import (
	"context"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// open returns an open DB whose Close is checked at test cleanup.
func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}

func issuesSchema() source.TableSchema {
	return source.TableSchema{
		Name: "issues",
		Columns: []source.Column{
			{Name: "number", Type: "INTEGER"},
			{Name: "title", Type: "TEXT"},
			{Name: "state", Type: "TEXT"},
		},
	}
}

func TestOpenClose(t *testing.T) {
	db, err := Open(context.Background())
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func TestCreateInsertQuery(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	ts := issuesSchema()
	require.NoError(t, db.CreateTable(ctx, "", ts))
	rows := [][]any{
		{int64(1), "first", "open"},
		{int64(2), "second", "closed"},
	}
	require.NoError(t, db.Insert(ctx, "", ts.Name, ts.ColumnNames(), rows))

	res, err := db.Query(ctx, "SELECT number, title FROM issues WHERE state = 'open' ORDER BY number")
	require.NoError(t, err)
	assert.Equal(t, []string{"number", "title"}, res.Columns)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, int64(1), res.Rows[0][0])
	assert.Equal(t, "first", res.Rows[0][1])
}

// TestAttachedSchemaQualifiedTable is the core engine scenario: a schema-
// qualified table (github.issues) created under an attached schema and queried
// with its qualified name on the same pinned connection.
func TestAttachedSchemaQualifiedTable(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	require.NoError(t, db.Attach(ctx, "github"))
	require.NoError(t, db.Attach(ctx, "github")) // idempotent

	ts := issuesSchema()
	require.NoError(t, db.CreateTable(ctx, "github", ts))
	require.NoError(t, db.Insert(ctx, "github", ts.Name, ts.ColumnNames(), [][]any{
		{int64(10), "bug", "open"},
		{int64(11), "feat", "open"},
	}))

	res, err := db.Query(ctx, "SELECT number FROM github.issues ORDER BY number DESC LIMIT 1")
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, int64(11), res.Rows[0][0])
}

func TestInsertEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	ts := issuesSchema()
	require.NoError(t, db.CreateTable(ctx, "", ts))
	require.NoError(t, db.Insert(ctx, "", ts.Name, ts.ColumnNames(), nil))

	res, err := db.Query(ctx, "SELECT COUNT(*) FROM issues")
	require.NoError(t, err)
	assert.Equal(t, int64(0), res.Rows[0][0])
}

func TestInsertWrongArity(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	ts := issuesSchema()
	require.NoError(t, db.CreateTable(ctx, "", ts))
	err := db.Insert(ctx, "", ts.Name, ts.ColumnNames(), [][]any{{int64(1), "only-two"}})
	assert.Error(t, err)
}

func TestQuerySyntaxError(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	_, err := db.Query(ctx, "SELECT FROM nope")
	assert.Error(t, err)
}
