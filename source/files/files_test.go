package files

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmashuda/dfetch/engine"
	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newConn writes the given files (path -> content) into a temp root and
// returns a connector rooted there.
func newConn(t *testing.T, fileset map[string]string, params map[string]any) *Connector {
	t.Helper()
	dir := t.TempDir()
	for name, content := range fileset {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	if params == nil {
		params = map[string]any{}
	}
	params["root"] = dir
	c, err := New(params)
	require.NoError(t, err)
	return c.(*Connector)
}

// collectScan drives Scan and accumulates every chunk's rows and warnings.
func collectScan(t *testing.T, c *Connector, req source.ScanRequest) (rows [][]any, cols []string, warnings []string) {
	t.Helper()
	err := c.Scan(context.Background(), req, func(r *source.Rows) error {
		if len(r.Columns) > 0 {
			cols = r.Columns
		}
		rows = append(rows, r.Rows...)
		warnings = append(warnings, r.Warnings...)
		return nil
	})
	require.NoError(t, err)
	return rows, cols, warnings
}

// scanChunkSizes drives Scan and records the size of each emitted data chunk.
func scanChunkSizes(t *testing.T, c *Connector, req source.ScanRequest) []int {
	t.Helper()
	var sizes []int
	err := c.Scan(context.Background(), req, func(r *source.Rows) error {
		if len(r.Rows) > 0 {
			sizes = append(sizes, len(r.Rows))
		}
		return nil
	})
	require.NoError(t, err)
	return sizes
}

func describe(t *testing.T, c *Connector, table string) source.TableSchema {
	t.Helper()
	ts, found, err := c.DescribeTable(context.Background(), table)
	require.NoError(t, err)
	require.True(t, found, "table %q should resolve", table)
	return ts
}

func TestNewDefaults(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	fc := c.(*Connector)
	assert.Equal(t, ".", fc.root)
	assert.Equal(t, defaultMaxRows, fc.maxRows)
	assert.Nil(t, c.Tables(), "dynamic source declares no static tables")
}

func TestDescribeCSVTypes(t *testing.T) {
	c := newConn(t, map[string]string{
		"orders.csv": "id,amount,note,when,empty\n" +
			"1,9.5,hello,2024-01-01,\n" +
			"2,10,,2024-01-02,\n",
	}, nil)
	ts := describe(t, c, "orders.csv")
	assert.Equal(t, "orders.csv", ts.Name)
	assert.Equal(t, []source.Column{
		{Name: "id", Type: "INTEGER"},
		{Name: "amount", Type: "REAL"}, // 9.5 forces REAL even though 10 is integral
		{Name: "note", Type: "TEXT"},
		{Name: "when", Type: "TEXT"},
		{Name: "empty", Type: "TEXT"}, // no values seen -> TEXT
	}, ts.Columns)
}

func TestDescribeCSVHeaderSanitizing(t *testing.T) {
	c := newConn(t, map[string]string{
		"h.csv": "\ufeffid, id ,,id\n1,2,3,4\n",
	}, nil)
	ts := describe(t, c, "h.csv")
	names := ts.ColumnNames()
	assert.Equal(t, []string{"id", "id_2", "column_3", "id_3"}, names,
		"BOM stripped, cells trimmed, empties named, duplicates suffixed")
}

func TestDescribeTSV(t *testing.T) {
	c := newConn(t, map[string]string{
		"t.tsv": "a\tb\n1\tx\n",
	}, nil)
	ts := describe(t, c, "t.tsv")
	assert.Equal(t, []source.Column{
		{Name: "a", Type: "INTEGER"},
		{Name: "b", Type: "TEXT"},
	}, ts.Columns)
}

func TestDescribeJSON(t *testing.T) {
	c := newConn(t, map[string]string{
		"e.json": `[
			{"b": 1, "a": 1.5, "ok": true, "tags": ["x"], "note": null},
			{"b": 2, "extra": "later"}
		]`,
	}, nil)
	ts := describe(t, c, "e.json")
	assert.Equal(t, []source.Column{
		{Name: "b", Type: "INTEGER"},
		{Name: "a", Type: "REAL"},
		{Name: "ok", Type: "INTEGER"}, // bool stored as 0/1
		{Name: "tags", Type: "TEXT"},  // nested -> JSON text
		{Name: "note", Type: "TEXT"},  // only null seen -> TEXT
		{Name: "extra", Type: "TEXT"}, // key union in first-seen order
	}, ts.Columns)
}

func TestDescribeJSONL(t *testing.T) {
	c := newConn(t, map[string]string{
		"l.jsonl":  "{\"n\": 1}\n{\"n\": 2, \"s\": \"x\"}\n",
		"n.ndjson": "{\"n\": 1}\n",
	}, nil)
	ts := describe(t, c, "l.jsonl")
	assert.Equal(t, []source.Column{
		{Name: "n", Type: "INTEGER"},
		{Name: "s", Type: "TEXT"},
	}, ts.Columns)
	describe(t, c, "n.ndjson") // .ndjson resolves too
}

func TestDescribeNotFound(t *testing.T) {
	c := newConn(t, nil, nil)
	_, found, err := c.DescribeTable(context.Background(), "missing.csv")
	require.NoError(t, err)
	assert.False(t, found)
	// Unsupported extension on a nonexistent file: also just "no such table".
	_, found, err = c.DescribeTable(context.Background(), "missing.parquet")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestDescribeUnsupportedExistingFile(t *testing.T) {
	c := newConn(t, map[string]string{"data.txt": "hello"}, nil)
	_, found, err := c.DescribeTable(context.Background(), "data.txt")
	assert.False(t, found)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported file type")
}

func TestDescribeRejectsEscapingPaths(t *testing.T) {
	c := newConn(t, nil, nil)
	for _, table := range []string{"../evil.csv", "/etc/passwd.csv", "a/../../b.csv"} {
		_, _, err := c.DescribeTable(context.Background(), table)
		require.Error(t, err, "path %q must be rejected", table)
		assert.Contains(t, err.Error(), "relative path")
	}
}

func TestDescribeParseErrors(t *testing.T) {
	c := newConn(t, map[string]string{
		"empty.csv":  "",
		"empty.json": "[]",
		"obj.json":   `{"not": "an array"}`,
	}, nil)
	_, _, err := c.DescribeTable(context.Background(), "empty.csv")
	require.ErrorContains(t, err, "header row is required")
	_, _, err = c.DescribeTable(context.Background(), "empty.json")
	require.ErrorContains(t, err, "no keys")
	_, _, err = c.DescribeTable(context.Background(), "obj.json")
	require.ErrorContains(t, err, "top-level array")
}

func TestListTables(t *testing.T) {
	c := newConn(t, map[string]string{
		"a.csv":            "x\n1\n",
		"sub/dir/b.jsonl":  "{\"x\": 1}\n",
		"sub/readme.md":    "not data",
		".hidden/c.csv":    "x\n1\n",
		"sub/.hidden.json": "[{\"x\": 1}]",
	}, nil)

	names, err := c.ListTables(context.Background(), source.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a.csv", "sub/dir/b.jsonl"}, names,
		"hidden files/dirs and unsupported extensions are skipped")

	names, err = c.ListTables(context.Background(), source.ListOptions{Filter: "B.JSON"})
	require.NoError(t, err)
	assert.Equal(t, []string{"sub/dir/b.jsonl"}, names, "filter is a case-insensitive substring")

	names, err = c.ListTables(context.Background(), source.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Len(t, names, 1)
}

func TestScanCSV(t *testing.T) {
	c := newConn(t, map[string]string{
		"d.csv": "id,amount,note\n" +
			"1,9.5,hello\n" +
			"2,,short\n" + // empty numeric cell -> NULL
			"3,1.25\n", // ragged short row -> trailing NULL
	}, nil)
	rows, cols, warnings := collectScan(t, c, source.ScanRequest{Table: "d.csv"})
	assert.Equal(t, []string{"id", "amount", "note"}, cols)
	assert.Empty(t, warnings)
	assert.Equal(t, [][]any{
		{int64(1), 9.5, "hello"},
		{int64(2), nil, "short"},
		{int64(3), 1.25, nil},
	}, rows)
}

// A value the inferred affinity can't parse (possible when it first appears
// beyond the sample window) stays the string as written, so SQLite's verbatim
// re-filter still matches it.
func TestTextValueFallback(t *testing.T) {
	assert.Equal(t, int64(7), textValue("7", "INTEGER"))
	assert.Equal(t, "x", textValue("x", "INTEGER"))
	assert.Equal(t, 2.5, textValue("2.5", "REAL"))
	assert.Equal(t, "n/a", textValue("n/a", "REAL"))
	assert.Nil(t, textValue("", "INTEGER"))
	assert.Equal(t, "", textValue("", "TEXT"))
	assert.Equal(t, "9", textValue("9", "TEXT"))
}

func TestScanJSON(t *testing.T) {
	c := newConn(t, map[string]string{
		"e.json": `[
			{"id": 1, "meta": {"k": "v"}, "ok": true},
			{"id": 2, "later": "unioned"}
		]`,
	}, nil)
	rows, cols, _ := collectScan(t, c, source.ScanRequest{Table: "e.json"})
	assert.Equal(t, []string{"id", "meta", "ok", "later"}, cols)
	assert.Equal(t, [][]any{
		{int64(1), `{"k":"v"}`, true, nil}, // missing keys -> NULL
		{int64(2), nil, nil, "unioned"},
	}, rows)
}

func TestScanJSONL(t *testing.T) {
	c := newConn(t, map[string]string{
		"l.jsonl": "{\"n\": 1, \"f\": 2.5}\n{\"n\": 2}\n",
	}, nil)
	rows, _, _ := collectScan(t, c, source.ScanRequest{Table: "l.jsonl"})
	assert.Equal(t, [][]any{
		{int64(1), 2.5},
		{int64(2), nil},
	}, rows)
}

func csvOfSize(n int) string {
	var sb strings.Builder
	sb.WriteString("id\n")
	for i := range n {
		fmt.Fprintf(&sb, "%d\n", i)
	}
	return sb.String()
}

func TestScanChunking(t *testing.T) {
	c := newConn(t, map[string]string{"big.csv": csvOfSize(2500)}, nil)
	sizes := scanChunkSizes(t, c, source.ScanRequest{Table: "big.csv"})
	assert.Equal(t, []int{1000, 1000, 500}, sizes, "one chunk per batch, remainder flushed")
}

func TestScanLimitPushdown(t *testing.T) {
	c := newConn(t, map[string]string{"d.csv": csvOfSize(100)}, nil)
	limit, offset := 5, 3

	// No filters, no ordering: reading stops at limit+offset.
	rows, _, warnings := collectScan(t, c, source.ScanRequest{Table: "d.csv", Limit: &limit, Offset: &offset})
	assert.Len(t, rows, limit+offset)
	assert.Empty(t, warnings)

	// A filter the connector can't apply: the limit must not truncate the read.
	rows, _, _ = collectScan(t, c, source.ScanRequest{
		Table:   "d.csv",
		Limit:   &limit,
		Filters: []source.Filter{{Column: "id", Op: source.OpGt, Value: int64(90)}},
	})
	assert.Len(t, rows, 100, "filtered scans return the whole file for SQLite to re-filter")

	// An ORDER BY the file can't honor: same.
	rows, _, _ = collectScan(t, c, source.ScanRequest{
		Table:   "d.csv",
		Limit:   &limit,
		OrderBy: []source.OrderTerm{{Column: "id", Desc: true}},
	})
	assert.Len(t, rows, 100)
}

func TestScanMaxRowsWarns(t *testing.T) {
	c := newConn(t, map[string]string{"d.csv": csvOfSize(15)}, map[string]any{"max_rows": 10})
	rows, _, warnings := collectScan(t, c, source.ScanRequest{Table: "d.csv"})
	assert.Len(t, rows, 10)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "max_rows=10")

	// A pushed LIMIT below the cap is a complete answer: no warning.
	limit := 5
	rows, _, warnings = collectScan(t, c, source.ScanRequest{Table: "d.csv", Limit: &limit})
	assert.Len(t, rows, 5)
	assert.Empty(t, warnings)
}

func TestScanMissingFile(t *testing.T) {
	c := newConn(t, nil, nil)
	err := c.Scan(context.Background(), source.ScanRequest{Table: "nope.csv"}, func(*source.Rows) error {
		t.Fatal("emit must not be called")
		return nil
	})
	require.ErrorContains(t, err, "no file")
}

// The whole point of path-as-table-name: a quoted relative path parses,
// resolves, loads, and queries end to end through the engine.
func TestEngineEndToEnd(t *testing.T) {
	c := newConn(t, map[string]string{
		"data/orders.csv": "id,region,amount\n" +
			"1,east,10\n" +
			"2,west,20\n" +
			"3,east,30\n",
	}, nil)
	e, err := engine.New(engine.WithConnector("files", c))
	require.NoError(t, err)

	res, err := e.Run(context.Background(),
		`SELECT id, amount FROM files."data/orders.csv" WHERE region = 'east' ORDER BY amount DESC LIMIT 1`)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, int64(3), res.Rows[0][0])
	assert.Equal(t, int64(30), res.Rows[0][1])
}
