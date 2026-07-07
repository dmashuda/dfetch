//go:build integration

// Integration tests against a real Postgres. Run with:
//
//	docker compose up -d postgres
//	DFETCH_TEST_POSTGRES_DSN='postgres://dfetch:dfetch@localhost:5432/dfetch?sslmode=disable' \
//	    go test -tags integration ./internal/source/postgres/
//
// They are skipped (the build tag excludes the file) from the default `go test`.
package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testConnector(t *testing.T) *Connector {
	t.Helper()
	dsn := os.Getenv("DFETCH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DFETCH_TEST_POSTGRES_DSN to run Postgres integration tests")
	}
	c, err := New(map[string]any{"dsn": dsn})
	require.NoError(t, err)
	return c.(*Connector)
}

func collectScan(c source.Connector, req source.ScanRequest) (*source.Rows, error) {
	rows := &source.Rows{}
	err := c.Scan(context.Background(), req, func(chunk *source.Rows) error {
		if rows.Columns == nil {
			rows.Columns = chunk.Columns
		}
		rows.Rows = append(rows.Rows, chunk.Rows...)
		return nil
	})
	return rows, err
}

func TestIntegrationDiscoverAndScan(t *testing.T) {
	c := testConnector(t)
	ctx := context.Background()

	_, err := c.db.ExecContext(ctx, `DROP TABLE IF EXISTS dfetch_orders`)
	require.NoError(t, err)
	_, err = c.db.ExecContext(ctx, `CREATE TABLE dfetch_orders (
		id integer PRIMARY KEY, status text, total numeric, created_at timestamptz)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = c.db.ExecContext(ctx, `DROP TABLE dfetch_orders`) })
	_, err = c.db.ExecContext(ctx, `INSERT INTO dfetch_orders VALUES
		(1,'paid',  10.0,'2026-01-01T00:00:00Z'),
		(2,'paid',  20.0,'2026-03-01T00:00:00Z'),
		(3,'open',  30.0,'2026-02-01T00:00:00Z')`)
	require.NoError(t, err)

	// ListTables
	names, err := c.ListTables(ctx, source.ListOptions{Filter: "dfetch_ord"})
	require.NoError(t, err)
	assert.Contains(t, names, "dfetch_orders")

	// DescribeTable
	ts, found, err := c.DescribeTable(ctx, "dfetch_orders")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []string{"id", "status", "total", "created_at"}, ts.ColumnNames())
	_, found, err = c.DescribeTable(ctx, "does_not_exist")
	require.NoError(t, err)
	assert.False(t, found)

	// Scan with pushed WHERE + ORDER BY timestamp + LIMIT.
	limit := 1
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "dfetch_orders",
		Columns: []string{"id", "created_at"},
		Filters: []source.Filter{{Column: "status", Op: source.OpEq, Value: "paid"}},
		OrderBy: []source.OrderTerm{{Column: "created_at", Desc: true}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"id", "created_at"}, rows.Columns)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, int64(2), rows.Rows[0][0]) // newest paid order
}
