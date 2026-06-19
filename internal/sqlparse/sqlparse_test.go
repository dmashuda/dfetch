package sqlparse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExtractsTables(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"simple", "SELECT * FROM users", []string{"users"}},
		{"join", "SELECT * FROM users JOIN orders USING (id)", []string{"orders", "users"}},
		{"no from", "SELECT 1", nil},
		{"dedupe self join", "SELECT * FROM users a JOIN users b ON a.id = b.pid", []string{"users"}},
		{"cte excluded", "WITH t AS (SELECT * FROM real) SELECT * FROM t", []string{"real"}},
		{"cte plus base", "WITH t AS (SELECT * FROM a) SELECT * FROM t JOIN b ON t.id=b.id", []string{"a", "b"}},
		{"subquery in from", "SELECT * FROM (SELECT * FROM inner_t) x", []string{"inner_t"}},
		{"subquery in where", "SELECT * FROM a WHERE id IN (SELECT id FROM b)", []string{"a", "b"}},
		{"quoted double", `SELECT * FROM "My Table"`, []string{"My Table"}},
		{"quoted bracket", "SELECT * FROM [My Table]", []string{"My Table"}},
		{"quoted backtick", "SELECT * FROM `tbl`", []string{"tbl"}},
		{"schema qualified", "SELECT * FROM main.foo", []string{"foo"}},
		{"table function not a source", "SELECT * FROM json_each('[]')", nil},
		{"values", "VALUES (1), (2)", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.sql)
			require.NoError(t, err)
			assert.Equal(t, tc.sql, q.Raw)
			assert.Equal(t, tc.want, q.Tables)
		})
	}
}

// TestParseComplexJoins exercises multi-table and nested joins to confirm every
// referenced source table is discovered.
func TestParseComplexJoins(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			"three way inner join",
			`SELECT a.x, b.y, c.z FROM a
			   JOIN b ON a.id = b.aid
			   JOIN c ON c.bid = b.id`,
			[]string{"a", "b", "c"},
		},
		{
			"left and inner mix",
			`SELECT * FROM orders o
			   LEFT JOIN customers cu ON cu.id = o.customer_id
			   INNER JOIN items i ON i.order_id = o.id`,
			[]string{"customers", "items", "orders"},
		},
		{
			"cross join",
			"SELECT * FROM a CROSS JOIN b",
			[]string{"a", "b"},
		},
		{
			"join against subquery",
			`SELECT * FROM a
			   JOIN (SELECT id FROM b JOIN c ON b.cid = c.id) sub ON sub.id = a.id`,
			[]string{"a", "b", "c"},
		},
		{
			"cte joined with base and subquery",
			`WITH recent AS (SELECT * FROM events)
			 SELECT * FROM recent r
			   JOIN users u ON u.id = r.user_id
			   LEFT JOIN (SELECT * FROM regions) rg ON rg.id = u.region_id`,
			[]string{"events", "regions", "users"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.sql)
			require.NoError(t, err)
			assert.Equal(t, tc.want, q.Tables)
		})
	}
}

// TestParseExtractsColumns confirms referenced column names are collected from
// the projection, JOIN/USING clauses, and WHERE predicates, while SELECT *
// yields no columns.
func TestParseExtractsColumns(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"projection", "SELECT name, age FROM users", []string{"age", "name"}},
		{"qualified and where", "SELECT a.x, b.y FROM a JOIN b ON a.id = b.aid WHERE b.y > 3", []string{"aid", "id", "x", "y"}},
		{"using clause", "SELECT id FROM a JOIN b USING (id)", []string{"id"}},
		{"star has no columns", "SELECT * FROM t", nil},
		{"qualified star has no columns", "SELECT t.* FROM t", nil},
		{"group by and having", "SELECT dept, COUNT(*) FROM emp GROUP BY dept HAVING COUNT(emp_id) > 1", []string{"dept", "emp_id"}},
		{"order by", "SELECT name FROM users ORDER BY created_at DESC", []string{"created_at", "name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.sql)
			require.NoError(t, err)
			assert.Equal(t, tc.want, q.Columns)
		})
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"insert", "INSERT INTO t VALUES (1)"},
		{"update", "UPDATE t SET x = 1"},
		{"delete", "DELETE FROM t"},
		{"create", "CREATE TABLE t (a int)"},
		{"drop", "DROP TABLE t"},
		{"pragma", "PRAGMA foreign_keys = on"},
		{"attach", "ATTACH DATABASE 'x.db' AS x"},
		{"explain select", "EXPLAIN SELECT * FROM t"},
		{"multiple statements", "SELECT 1; SELECT 2"},
		{"empty", ""},
		{"syntax error", "SELECT FROM"},
		{"garbage", "not sql at all !!!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.sql)
			require.Error(t, err)
			assert.Nil(t, q)
		})
	}
}

func TestParseTrailingSemicolonAllowed(t *testing.T) {
	q, err := Parse("SELECT * FROM users;")
	require.NoError(t, err)
	assert.Equal(t, []string{"users"}, q.Tables)
}

func TestUnquoteIdent(t *testing.T) {
	cases := map[string]string{
		`"a"`:      "a",
		"`a`":      "a",
		"[a]":      "a",
		`'a'`:      "a",
		`"a""b"`:   `a"b`,
		"plain":    "plain",
		`"`:        `"`,
		"unclosed": "unclosed",
	}
	for in, want := range cases {
		assert.Equalf(t, want, unquoteIdent(in), "unquoteIdent(%q)", in)
	}
}
