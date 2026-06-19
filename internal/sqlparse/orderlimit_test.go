package sqlparse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestASTOrderBy(t *testing.T) {
	s := mustParse(t, "SELECT * FROM t WHERE a = 1 ORDER BY updated DESC, t.name, created ASC")
	require.Len(t, s.OrderBy, 3)
	assert.Equal(t, OrderTerm{Column: "updated", Desc: true}, s.OrderBy[0])
	assert.Equal(t, OrderTerm{Table: "t", Column: "name", Desc: false}, s.OrderBy[1])
	assert.Equal(t, OrderTerm{Column: "created", Desc: false}, s.OrderBy[2])
}

func TestASTOrderByExpression(t *testing.T) {
	// A non-column ordering expression is preserved as raw text.
	s := mustParse(t, "SELECT * FROM t ORDER BY length(name) DESC")
	require.Len(t, s.OrderBy, 1)
	assert.Empty(t, s.OrderBy[0].Column)
	assert.Equal(t, "length(name)", s.OrderBy[0].Expr)
	assert.True(t, s.OrderBy[0].Desc)
}

func TestASTLimit(t *testing.T) {
	t.Run("count only", func(t *testing.T) {
		s := mustParse(t, "SELECT * FROM t LIMIT 10")
		require.NotNil(t, s.Limit)
		n, ok := s.Limit.Count.Literal.AsInt()
		require.True(t, ok)
		assert.Equal(t, int64(10), n)
		assert.Nil(t, s.Limit.Offset)
	})

	t.Run("count and offset", func(t *testing.T) {
		s := mustParse(t, "SELECT * FROM t LIMIT 10 OFFSET 5")
		require.NotNil(t, s.Limit)
		c, _ := s.Limit.Count.Literal.AsInt()
		o, _ := s.Limit.Offset.Literal.AsInt()
		assert.Equal(t, int64(10), c)
		assert.Equal(t, int64(5), o)
	})

	t.Run("comma form is offset, count", func(t *testing.T) {
		// SQLite's `LIMIT a, b` means OFFSET a, COUNT b.
		s := mustParse(t, "SELECT * FROM t LIMIT 20, 5")
		require.NotNil(t, s.Limit)
		c, _ := s.Limit.Count.Literal.AsInt()
		o, _ := s.Limit.Offset.Literal.AsInt()
		assert.Equal(t, int64(5), c)
		assert.Equal(t, int64(20), o)
	})

	t.Run("bind count", func(t *testing.T) {
		s := mustParse(t, "SELECT * FROM t LIMIT :n")
		require.NotNil(t, s.Limit)
		require.NotNil(t, s.Limit.Count)
		assert.Equal(t, ValueBind, s.Limit.Count.Kind)
		assert.Equal(t, ":n", s.Limit.Count.Bind)
	})
}

func TestOrderLimitMakeComplete(t *testing.T) {
	// ORDER BY and LIMIT are now modeled, so they no longer force incompleteness.
	for _, sql := range []string{
		"SELECT a FROM t ORDER BY a",
		"SELECT a FROM t LIMIT 5",
		"SELECT a FROM t WHERE a = 1 ORDER BY a DESC LIMIT 10 OFFSET 2",
	} {
		t.Run(sql, func(t *testing.T) {
			assert.True(t, mustParse(t, sql).Complete)
		})
	}
	// GROUP BY / CTE / compound still force incompleteness.
	for _, sql := range []string{
		"SELECT a FROM t GROUP BY a",
		"SELECT a FROM t UNION SELECT a FROM u",
	} {
		t.Run("incomplete/"+sql, func(t *testing.T) {
			assert.False(t, mustParse(t, sql).Complete)
		})
	}
}
