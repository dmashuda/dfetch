package sqlparse

import (
	"reflect"
	"testing"
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
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.sql, err)
			}
			if q.Raw != tc.sql {
				t.Errorf("Raw = %q, want %q", q.Raw, tc.sql)
			}
			want := tc.want
			if len(want) == 0 {
				want = nil
			}
			if got := q.Tables; !reflect.DeepEqual(got, want) {
				t.Errorf("Tables = %#v, want %#v", got, want)
			}
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
			if q, err := Parse(tc.sql); err == nil {
				t.Fatalf("Parse(%q) = %#v, want error", tc.sql, q)
			}
		})
	}
}

func TestParseTrailingSemicolonAllowed(t *testing.T) {
	q, err := Parse("SELECT * FROM users;")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(q.Tables, []string{"users"}) {
		t.Fatalf("Tables = %#v, want [users]", q.Tables)
	}
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
		if got := unquoteIdent(in); got != want {
			t.Errorf("unquoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
