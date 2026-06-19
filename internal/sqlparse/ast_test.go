package sqlparse

import (
	"strconv"
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

func intValue(n int64) *Value {
	return &Value{Kind: ValueLiteral, Literal: &Literal{Kind: LiteralInteger, Raw: strconv.FormatInt(n, 10), Value: n}}
}

func intLit(n int64) Value { return *intValue(n) }

func stringValue(raw, parsed string) *Value {
	return &Value{Kind: ValueLiteral, Literal: &Literal{Kind: LiteralString, Raw: raw, Value: parsed}}
}

// firstValue parses a one-predicate WHERE and returns the comparison's value.
func firstValue(t *testing.T, sql string) *Value {
	t.Helper()
	s := mustParse(t, sql)
	require.Len(t, s.Where, 1)
	require.NotNil(t, s.Where[0].Value)
	return s.Where[0].Value
}

func TestASTLiteralTypes(t *testing.T) {
	t.Run("integer", func(t *testing.T) {
		lit := firstValue(t, "SELECT * FROM t WHERE a = 42").Literal
		require.NotNil(t, lit)
		assert.Equal(t, LiteralInteger, lit.Kind)
		assert.Equal(t, int64(42), lit.Value)
	})
	t.Run("hex integer", func(t *testing.T) {
		lit := firstValue(t, "SELECT * FROM t WHERE a = 0xFF").Literal
		assert.Equal(t, LiteralInteger, lit.Kind)
		assert.Equal(t, int64(255), lit.Value)
	})
	t.Run("float", func(t *testing.T) {
		lit := firstValue(t, "SELECT * FROM t WHERE a = 3.5").Literal
		assert.Equal(t, LiteralFloat, lit.Kind)
		assert.Equal(t, 3.5, lit.Value)
	})
	t.Run("float exponent", func(t *testing.T) {
		lit := firstValue(t, "SELECT * FROM t WHERE a = 1e3").Literal
		assert.Equal(t, LiteralFloat, lit.Kind)
		assert.Equal(t, 1000.0, lit.Value)
	})
	t.Run("string", func(t *testing.T) {
		lit := firstValue(t, "SELECT * FROM t WHERE a = 'it''s'").Literal
		assert.Equal(t, LiteralString, lit.Kind)
		assert.Equal(t, "it's", lit.Value)
	})
	t.Run("bool", func(t *testing.T) {
		lit := firstValue(t, "SELECT * FROM t WHERE a = TRUE").Literal
		assert.Equal(t, LiteralBool, lit.Kind)
		assert.Equal(t, true, lit.Value)
	})
	t.Run("blob", func(t *testing.T) {
		lit := firstValue(t, "SELECT * FROM t WHERE a = X'4142'").Literal
		assert.Equal(t, LiteralBlob, lit.Kind)
		assert.Equal(t, []byte("AB"), lit.Value)
	})
	t.Run("keyword", func(t *testing.T) {
		lit := firstValue(t, "SELECT * FROM t WHERE a = CURRENT_TIMESTAMP").Literal
		assert.Equal(t, LiteralKeyword, lit.Kind)
		assert.Equal(t, "CURRENT_TIMESTAMP", lit.Value)
	})
	t.Run("null via equality stays raw value", func(t *testing.T) {
		// `a = NULL` is a normal equality whose RHS literal is typed null.
		lit := firstValue(t, "SELECT * FROM t WHERE a = NULL").Literal
		assert.Equal(t, LiteralNull, lit.Kind)
		assert.Nil(t, lit.Value)
	})
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
		t.Run(tc.want.String()+"_"+tc.sql, func(t *testing.T) {
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

	assert.Equal(t, Predicate{Table: "t", Column: "age", Op: OpGte, Value: intValue(21), Raw: "t.age >= 21"}, s.Where[0])
	assert.Equal(t, Predicate{Column: "status", Op: OpEq, Value: stringValue("'paid'", "paid"), Raw: "status = 'paid'"}, s.Where[1])
	assert.Equal(t, Predicate{Column: "name", Op: OpLike, Value: stringValue("'A%'", "A%"), Raw: "name LIKE 'A%'"}, s.Where[2])
	assert.Equal(t, Predicate{Column: "qty", Op: OpNotEq, Value: intValue(0), Raw: "qty <> 0"}, s.Where[3])
}

func TestASTPredicateOperatorFlip(t *testing.T) {
	// value on the left flips the operator so the column leads.
	s := mustParse(t, "SELECT * FROM t WHERE 100 > t.qty")
	require.Len(t, s.Where, 1)
	assert.Equal(t, Predicate{Table: "t", Column: "qty", Op: OpLt, Value: intValue(100), Raw: "100 > t.qty"}, s.Where[0])
}

func TestASTPatternOperators(t *testing.T) {
	cases := []struct {
		sql  string
		want Operator
	}{
		{"SELECT * FROM t WHERE name LIKE 'a%'", OpLike},
		{"SELECT * FROM t WHERE name NOT LIKE 'a%'", OpNotLike}, // regression: NOT must not be dropped
		{"SELECT * FROM t WHERE name GLOB 'a*'", OpGlob},
		{"SELECT * FROM t WHERE name NOT GLOB 'a*'", OpNotGlob},
		{"SELECT * FROM t WHERE name REGEXP '^a'", OpRegexp},
		{"SELECT * FROM t WHERE name NOT REGEXP '^a'", OpNotRegexp},
		{"SELECT * FROM t WHERE name MATCH 'a'", OpMatch},
		{"SELECT * FROM t WHERE name NOT MATCH 'a'", OpNotMatch},
	}
	for _, tc := range cases {
		t.Run(tc.want.String(), func(t *testing.T) {
			s := mustParse(t, tc.sql)
			require.Len(t, s.Where, 1)
			assert.Equal(t, tc.want, s.Where[0].Op)
			assert.Equal(t, "name", s.Where[0].Column)
			assert.NotNil(t, s.Where[0].Value)
		})
	}
}

func TestASTIsOperators(t *testing.T) {
	cases := []struct {
		sql  string
		want Operator
	}{
		{"SELECT * FROM t WHERE x IS 5", OpIs},
		{"SELECT * FROM t WHERE x IS NOT 5", OpIsNot},
		{"SELECT * FROM t WHERE x IS DISTINCT FROM 5", OpIsDistinctFrom},
		{"SELECT * FROM t WHERE x IS NOT DISTINCT FROM 5", OpIsNotDistinctFrom},
	}
	for _, tc := range cases {
		t.Run(tc.want.String(), func(t *testing.T) {
			s := mustParse(t, tc.sql)
			require.Len(t, s.Where, 1)
			assert.Equal(t, Predicate{Column: "x", Op: tc.want, Value: intValue(5), Raw: s.Where[0].Raw}, s.Where[0])
		})
	}
}

func TestASTBetween(t *testing.T) {
	s := mustParse(t, "SELECT * FROM t WHERE age BETWEEN 18 AND 65")
	require.Len(t, s.Where, 1)
	assert.Equal(t, Predicate{
		Column: "age", Op: OpBetween,
		Values: []Value{intLit(18), intLit(65)},
		Raw:    "age BETWEEN 18 AND 65",
	}, s.Where[0])

	s = mustParse(t, "SELECT * FROM t WHERE age NOT BETWEEN 18 AND 65")
	assert.Equal(t, OpNotBetween, s.Where[0].Op)
	assert.Len(t, s.Where[0].Values, 2)
}

func TestASTIn(t *testing.T) {
	s := mustParse(t, "SELECT * FROM t WHERE id IN (1, 2, 3)")
	require.Len(t, s.Where, 1)
	assert.Equal(t, Predicate{
		Column: "id", Op: OpIn,
		Values: []Value{intLit(1), intLit(2), intLit(3)},
		Raw:    "id IN (1, 2, 3)",
	}, s.Where[0])

	s = mustParse(t, "SELECT * FROM t WHERE id NOT IN (1, 2)")
	assert.Equal(t, OpNotIn, s.Where[0].Op)
	assert.Len(t, s.Where[0].Values, 2)
}

func TestASTUnstructuredMultiOperand(t *testing.T) {
	// IN (subquery) and BETWEEN with non-literal bounds are preserved as raw.
	in := mustParse(t, "SELECT * FROM t WHERE id IN (SELECT id FROM b)")
	require.Len(t, in.Where, 1)
	assert.Equal(t, OpNone, in.Where[0].Op)
	assert.Empty(t, in.Where[0].Values)

	bt := mustParse(t, "SELECT * FROM t WHERE col BETWEEN a AND b")
	assert.Equal(t, OpNone, bt.Where[0].Op)
}

func TestASTPredicateBindAndNull(t *testing.T) {
	s := mustParse(t, "SELECT * FROM t WHERE t.id = :id AND t.deleted IS NULL AND t.name IS NOT NULL")
	require.Len(t, s.Where, 3)

	assert.Equal(t, Predicate{Table: "t", Column: "id", Op: OpEq, Value: &Value{Kind: ValueBind, Bind: ":id"}, Raw: "t.id = :id"}, s.Where[0])
	assert.Equal(t, Predicate{Table: "t", Column: "deleted", Op: OpIsNull, Raw: "t.deleted IS NULL"}, s.Where[1])
	assert.Equal(t, Predicate{Table: "t", Column: "name", Op: OpIsNotNull, Raw: "t.name IS NOT NULL"}, s.Where[2])
}

func TestASTUnstructuredPredicatesKeepRaw(t *testing.T) {
	// Top-level OR is not split; it is preserved as raw text rather than dropped.
	s := mustParse(t, "SELECT * FROM a JOIN b ON a.id = b.id WHERE p = 1 OR q = 2")
	require.Len(t, s.Where, 1)
	assert.Empty(t, s.Where[0].Op)
	assert.Equal(t, "p = 1 OR q = 2", s.Where[0].Raw)
}

func TestASTColumnToColumnPredicate(t *testing.T) {
	// A join key a.id = b.id is a structured column-to-column comparison.
	s := mustParse(t, "SELECT * FROM a JOIN b ON a.id = b.id")
	require.Len(t, s.Joins, 1)
	require.Len(t, s.Joins[0].On, 1)

	on := s.Joins[0].On[0]
	assert.Equal(t, OpEq, on.Op)
	assert.Equal(t, "a", on.Table)
	assert.Equal(t, "id", on.Column)
	assert.Equal(t, "b", on.RefTable)
	assert.Equal(t, "id", on.RefColumn)
	assert.Nil(t, on.Value)
	assert.Equal(t, "a.id = b.id", on.Raw)
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

func TestLiteralAccessors(t *testing.T) {
	t.Run("matching accessors return the typed value", func(t *testing.T) {
		i := firstValue(t, "SELECT * FROM t WHERE a = 42").Literal
		got, ok := i.AsInt()
		require.True(t, ok)
		assert.Equal(t, int64(42), got)

		f := firstValue(t, "SELECT * FROM t WHERE a = 3.5").Literal
		gotF, ok := f.AsFloat()
		require.True(t, ok)
		assert.Equal(t, 3.5, gotF)

		s := firstValue(t, "SELECT * FROM t WHERE a = 'hi'").Literal
		gotS, ok := s.AsString()
		require.True(t, ok)
		assert.Equal(t, "hi", gotS)

		b := firstValue(t, "SELECT * FROM t WHERE a = TRUE").Literal
		gotB, ok := b.AsBool()
		require.True(t, ok)
		assert.True(t, gotB)

		bl := firstValue(t, "SELECT * FROM t WHERE a = X'4142'").Literal
		gotBlob, ok := bl.AsBlob()
		require.True(t, ok)
		assert.Equal(t, []byte("AB"), gotBlob)

		kw := firstValue(t, "SELECT * FROM t WHERE a = CURRENT_DATE").Literal
		gotKw, ok := kw.AsKeyword()
		require.True(t, ok)
		assert.Equal(t, "CURRENT_DATE", gotKw)

		null := firstValue(t, "SELECT * FROM t WHERE a = NULL").Literal
		assert.True(t, null.IsNull())
	})

	t.Run("mismatched accessors report false", func(t *testing.T) {
		i := firstValue(t, "SELECT * FROM t WHERE a = 42").Literal
		_, ok := i.AsString()
		assert.False(t, ok)
		_, ok = i.AsFloat()
		assert.False(t, ok)
		assert.False(t, i.IsNull())
	})

	t.Run("nil literal is safe", func(t *testing.T) {
		var l *Literal
		_, ok := l.AsInt()
		assert.False(t, ok)
		assert.False(t, l.IsNull())
	})
}

func TestEnumStrings(t *testing.T) {
	assert.Equal(t, "=", OpEq.String())
	assert.Equal(t, "IS NOT NULL", OpIsNotNull.String())
	assert.Equal(t, "", OpNone.String())
	assert.Equal(t, "LEFT", JoinLeft.String())
	assert.Equal(t, "literal", ValueLiteral.String())
	assert.Equal(t, "bind", ValueBind.String())
}
