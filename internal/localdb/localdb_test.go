package localdb

import (
	"context"
	"database/sql"
	"strconv"
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

// TestInsertBulkAcrossBatches inserts more rows than a single batch can hold
// (with a 13-column schema the batch is well under 1500), exercising the
// multi-row/multi-batch path and confirming every row lands in order.
func TestInsertBulkAcrossBatches(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	// 13 columns => batchRows = 999/13 = 76, so 1500 rows spans many batches.
	cols := make([]source.Column, 13)
	for i := range cols {
		cols[i] = source.Column{Name: "c" + strconv.Itoa(i), Type: "INTEGER"}
	}
	ts := source.TableSchema{Name: "wide", Columns: cols}
	require.NoError(t, db.CreateTable(ctx, "", ts))

	const n = 1500
	rows := make([][]any, n)
	for i := range rows {
		row := make([]any, len(cols))
		row[0] = int64(i) // c0 = row index
		for j := 1; j < len(cols); j++ {
			row[j] = int64(0)
		}
		rows[i] = row
	}
	require.NoError(t, db.Insert(ctx, "", ts.Name, ts.ColumnNames(), rows))

	res, err := db.Query(ctx, `SELECT COUNT(*), MIN(c0), MAX(c0) FROM wide`)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, int64(n), res.Rows[0][0])
	assert.Equal(t, int64(0), res.Rows[0][1])
	assert.Equal(t, int64(n-1), res.Rows[0][2])

	// Spot-check a row in a later batch resolves correctly.
	res, err = db.Query(ctx, `SELECT c0 FROM wide WHERE c0 = 1234`)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, int64(1234), res.Rows[0][0])
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

// TestQueryNamedBindParam confirms named bind parameters (:name) reach the
// driver and filter rows, the mechanism saved queries use to bind their args.
func TestQueryNamedBindParam(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	ts := issuesSchema()
	require.NoError(t, db.CreateTable(ctx, "", ts))
	require.NoError(t, db.Insert(ctx, "", ts.Name, ts.ColumnNames(), [][]any{
		{int64(1), "first", "open"},
		{int64(2), "second", "closed"},
	}))

	res, err := db.Query(ctx,
		"SELECT number, title FROM issues WHERE state = :state ORDER BY number",
		sql.Named("state", "closed"))
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, int64(2), res.Rows[0][0])
	assert.Equal(t, "second", res.Rows[0][1])
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
