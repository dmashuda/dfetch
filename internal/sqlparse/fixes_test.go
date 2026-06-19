package sqlparse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Fix #1: non-commutative pattern operators must not be flipped ---

func TestPatternOpValueOnLeftNotFlipped(t *testing.T) {
	// LIKE/GLOB/REGEXP/MATCH are not commutative. With the value on the left,
	// rewriting to column-on-left would invert the predicate, so it must stay
	// unstructured (Raw) rather than be mis-structured.
	for _, sql := range []string{
		"SELECT * FROM t WHERE 'a%' LIKE name",
		"SELECT * FROM t WHERE 'x' GLOB col",
		"SELECT * FROM t WHERE '^a' REGEXP col",
		"SELECT * FROM t WHERE 'm' MATCH col",
	} {
		t.Run(sql, func(t *testing.T) {
			s := mustParse(t, sql)
			require.Len(t, s.Where, 1)
			assert.Equalf(t, OpNone, s.Where[0].Op, "non-commutative op must not be flipped (got structured predicate)")
		})
	}
}

func TestPatternOpColumnOnLeftStillStructured(t *testing.T) {
	// Regression: the normal column-on-left form stays structured.
	s := mustParse(t, "SELECT * FROM t WHERE name LIKE 'a%'")
	require.Len(t, s.Where, 1)
	assert.Equal(t, OpLike, s.Where[0].Op)
	assert.Equal(t, "name", s.Where[0].Column)
}

func TestRelationalFlipStillWorks(t *testing.T) {
	// Regression: commutative/relational flips must keep working.
	s := mustParse(t, "SELECT * FROM t WHERE 100 > a")
	require.Len(t, s.Where, 1)
	assert.Equal(t, OpLt, s.Where[0].Op)
	assert.Equal(t, "a", s.Where[0].Column)

	s = mustParse(t, "SELECT * FROM t WHERE 5 = a")
	assert.Equal(t, OpEq, s.Where[0].Op)
	assert.Equal(t, "a", s.Where[0].Column)
}

// --- Fix #2: Select.Complete reflects whether unmodeled clauses were dropped ---

func TestSelectComplete(t *testing.T) {
	complete := []string{
		"SELECT a FROM t",
		"SELECT a FROM t WHERE a = 1",
		"SELECT a, b FROM t JOIN u ON t.id = u.id",
		"SELECT DISTINCT a FROM t",
	}
	for _, sql := range complete {
		t.Run("complete/"+sql, func(t *testing.T) {
			assert.True(t, mustParse(t, sql).Complete, "fully-modeled query should be Complete")
		})
	}

	lossy := []string{
		"SELECT a FROM t ORDER BY a",
		"SELECT a FROM t LIMIT 10",
		"SELECT dept, count(*) FROM t GROUP BY dept",
		"SELECT dept FROM t GROUP BY dept HAVING count(*) > 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT a FROM t UNION SELECT a FROM u",
		"SELECT a FROM t ORDER BY a LIMIT 10",
	}
	for _, sql := range lossy {
		t.Run("lossy/"+sql, func(t *testing.T) {
			assert.False(t, mustParse(t, sql).Complete, "query with unmodeled clauses must not be Complete")
		})
	}
}

// --- Fix #3: keyword identifiers must be quoted so the render reparses ---

func TestQuoteIdent(t *testing.T) {
	assert.Equal(t, "users", quoteIdent("users"))
	assert.Equal(t, `"order"`, quoteIdent("order")) // keyword
	// keyword detected case-insensitively, identifier casing preserved
	assert.Equal(t, `"SELECT"`, quoteIdent("SELECT"))
	assert.Equal(t, `"My Table"`, quoteIdent("My Table")) // not a bareword
	assert.Equal(t, "id", quoteIdent("id"))               // ordinary identifier
	assert.Equal(t, `"left"`, quoteIdent("left"))         // keyword
}

func TestRenderQuotesKeywordIdentifiers(t *testing.T) {
	for _, sql := range []string{
		`SELECT "order" FROM t`,
		`SELECT t."select" FROM t`,
		`SELECT * FROM "table" AS x WHERE x."where" = 1`,
	} {
		t.Run(sql, func(t *testing.T) {
			q, err := Parse(sql)
			require.NoError(t, err)
			rendered := q.SQL()
			_, err = Parse(rendered)
			assert.NoErrorf(t, err, "rendered SQL must reparse: %s", rendered)
		})
	}
}
