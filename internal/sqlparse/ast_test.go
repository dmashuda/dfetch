package sqlparse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParse(t *testing.T, sql string) *Select {
	t.Helper()
	q, err := Parse(sql)
	require.NoError(t, err)
	require.NotNil(t, q.Stmt)
	return q.Stmt
}

func TestASTProjections(t *testing.T) {
	s := mustParse(t, "SELECT a.x, b.y AS yy, COUNT(*), * FROM a JOIN b ON a.id = b.id")
	require.Len(t, s.Projections, 4)

	assert.Equal(t, Projection{Table: "a", Column: "x"}, s.Projections[0])
	assert.Equal(t, Projection{Table: "b", Column: "y", Alias: "yy"}, s.Projections[1])
	assert.Equal(t, Projection{Expr: "COUNT(*)"}, s.Projections[2])
	assert.Equal(t, Projection{Star: true}, s.Projections[3])
}

func TestASTQualifiedStarProjection(t *testing.T) {
	s := mustParse(t, "SELECT t.* FROM t")
	require.Len(t, s.Projections, 1)
	assert.Equal(t, Projection{Star: true, Table: "t"}, s.Projections[0])
}

func TestASTSourcesAndAliases(t *testing.T) {
	s := mustParse(t, "SELECT * FROM main.events e")
	require.Len(t, s.From, 1)
	assert.Equal(t, Source{Schema: "main", Name: "events", Alias: "e"}, s.From[0])
	assert.False(t, s.From[0].IsSubquery())
}

func TestASTSubquerySource(t *testing.T) {
	s := mustParse(t, "SELECT * FROM a JOIN (SELECT id FROM b) sub USING (id)")
	require.Len(t, s.From, 2)

	assert.Equal(t, "a", s.From[0].Name)

	sub := s.From[1]
	assert.True(t, sub.IsSubquery())
	assert.Equal(t, "sub", sub.Alias)
	require.NotNil(t, sub.Subquery)
	require.Len(t, sub.Subquery.From, 1)
	assert.Equal(t, "b", sub.Subquery.From[0].Name)

	require.Len(t, s.Joins, 1)
	assert.Equal(t, JoinInner, s.Joins[0].Type)
	assert.Equal(t, []string{"id"}, s.Joins[0].Using)
}

func TestASTJoinTypes(t *testing.T) {
	cases := []struct {
		sql  string
		want JoinType
	}{
		{"SELECT * FROM a JOIN b ON a.id=b.id", JoinInner},
		{"SELECT * FROM a INNER JOIN b ON a.id=b.id", JoinInner},
		{"SELECT * FROM a LEFT JOIN b ON a.id=b.id", JoinLeft},
		{"SELECT * FROM a LEFT OUTER JOIN b ON a.id=b.id", JoinLeft},
		{"SELECT * FROM a CROSS JOIN b", JoinCross},
		{"SELECT * FROM a, b", JoinComma},
	}
	for _, tc := range cases {
		t.Run(string(tc.want)+"_"+tc.sql, func(t *testing.T) {
			s := mustParse(t, tc.sql)
			require.Len(t, s.Joins, 1)
			assert.Equal(t, tc.want, s.Joins[0].Type)
		})
	}
}

func TestASTJoinConstraintAlignment(t *testing.T) {
	// A CROSS JOIN has no constraint; the following ON must attach to the
	// correct (second) join, not the cross join.
	s := mustParse(t, "SELECT * FROM a CROSS JOIN b JOIN c ON b.id = c.bid")
	require.Len(t, s.From, 3)
	require.Len(t, s.Joins, 2)

	assert.Equal(t, JoinCross, s.Joins[0].Type)
	assert.Empty(t, s.Joins[0].On)

	assert.Equal(t, JoinInner, s.Joins[1].Type)
	require.Len(t, s.Joins[1].On, 1)
	assert.Equal(t, "b.id = c.bid", s.Joins[1].On[0].Raw)
}

func TestASTWherePredicates(t *testing.T) {
	s := mustParse(t, "SELECT * FROM t WHERE t.age >= 21 AND status = 'paid' AND name LIKE 'A%' AND qty <> 0")
	require.Len(t, s.Where, 4)

	assert.Equal(t, Predicate{Table: "t", Column: "age", Op: ">=", Value: &Value{Kind: ValueLiteral, Text: "21"}, Raw: "t.age >= 21"}, s.Where[0])
	assert.Equal(t, Predicate{Column: "status", Op: "=", Value: &Value{Kind: ValueLiteral, Text: "'paid'"}, Raw: "status = 'paid'"}, s.Where[1])
	assert.Equal(t, Predicate{Column: "name", Op: "LIKE", Value: &Value{Kind: ValueLiteral, Text: "'A%'"}, Raw: "name LIKE 'A%'"}, s.Where[2])
	assert.Equal(t, Predicate{Column: "qty", Op: "<>", Value: &Value{Kind: ValueLiteral, Text: "0"}, Raw: "qty <> 0"}, s.Where[3])
}

func TestASTPredicateOperatorFlip(t *testing.T) {
	// value on the left flips the operator so the column leads.
	s := mustParse(t, "SELECT * FROM t WHERE 100 > t.qty")
	require.Len(t, s.Where, 1)
	assert.Equal(t, Predicate{Table: "t", Column: "qty", Op: "<", Value: &Value{Kind: ValueLiteral, Text: "100"}, Raw: "100 > t.qty"}, s.Where[0])
}

func TestASTPredicateBindAndNull(t *testing.T) {
	s := mustParse(t, "SELECT * FROM t WHERE t.id = :id AND t.deleted IS NULL AND t.name IS NOT NULL")
	require.Len(t, s.Where, 3)

	assert.Equal(t, Predicate{Table: "t", Column: "id", Op: "=", Value: &Value{Kind: ValueBind, Text: ":id"}, Raw: "t.id = :id"}, s.Where[0])
	assert.Equal(t, Predicate{Table: "t", Column: "deleted", Op: "IS NULL", Raw: "t.deleted IS NULL"}, s.Where[1])
	assert.Equal(t, Predicate{Table: "t", Column: "name", Op: "IS NOT NULL", Raw: "t.name IS NOT NULL"}, s.Where[2])
}

func TestASTUnstructuredPredicatesKeepRaw(t *testing.T) {
	// Top-level OR is not split; column-to-column and function predicates are
	// preserved as raw text rather than dropped.
	s := mustParse(t, "SELECT * FROM a JOIN b ON a.id = b.id WHERE p = 1 OR q = 2")
	require.Len(t, s.Where, 1)
	assert.Empty(t, s.Where[0].Op)
	assert.Equal(t, "p = 1 OR q = 2", s.Where[0].Raw)

	require.Len(t, s.Joins, 1)
	require.Len(t, s.Joins[0].On, 1)
	assert.Empty(t, s.Joins[0].On[0].Op) // a.id = b.id is column-to-column
	assert.Equal(t, "a.id = b.id", s.Joins[0].On[0].Raw)
}

func TestASTDistinct(t *testing.T) {
	assert.True(t, mustParse(t, "SELECT DISTINCT x FROM t").Distinct)
	assert.False(t, mustParse(t, "SELECT x FROM t").Distinct)
}

func TestASTMoreJoinTypes(t *testing.T) {
	cases := []struct {
		sql     string
		want    JoinType
		natural bool
	}{
		{"SELECT * FROM a RIGHT JOIN b ON a.id=b.id", JoinRight, false},
		{"SELECT * FROM a FULL OUTER JOIN b ON a.id=b.id", JoinFull, false},
		{"SELECT * FROM a NATURAL JOIN b", JoinInner, true},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			s := mustParse(t, tc.sql)
			require.Len(t, s.Joins, 1)
			assert.Equal(t, tc.want, s.Joins[0].Type)
			assert.Equal(t, tc.natural, s.Joins[0].Natural)
		})
	}
}

func TestASTTableFunctionSourceKeptRaw(t *testing.T) {
	s := mustParse(t, "SELECT * FROM json_each('[1,2]') AS j")
	require.Len(t, s.From, 1)
	src := s.From[0]
	assert.Empty(t, src.Name)
	assert.False(t, src.IsSubquery())
	assert.Equal(t, "j", src.Alias)
	assert.Contains(t, src.Raw, "json_each")
}

func TestASTValuesHasNoSources(t *testing.T) {
	s := mustParse(t, "VALUES (1), (2)")
	assert.Empty(t, s.From)
	assert.Empty(t, s.Projections)
}
