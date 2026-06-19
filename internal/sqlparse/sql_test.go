package sqlparse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearText zeroes the free-text fields that legitimately differ after a
// render → reparse cycle (a structured predicate reconstructs its own text, and
// a flipped comparison reorders operands), leaving only the structured fields to
// compare.
func clearText(s *Select) *Select {
	if s == nil {
		return nil
	}
	for i := range s.From {
		s.From[i].Raw = ""
		clearText(s.From[i].Subquery)
	}
	for i := range s.Joins {
		for j := range s.Joins[i].On {
			s.Joins[i].On[j].Raw = ""
		}
	}
	for i := range s.Where {
		s.Where[i].Raw = ""
	}
	return s
}

// TestRoundTrip parses SQL to the AST, renders it back to SQL, and reparses it,
// asserting that (a) the rendered SQL parses, (b) rendering is stable (a second
// render is identical), and (c) the structured AST is preserved across the trip.
func TestRoundTrip(t *testing.T) {
	cases := []string{
		// projections
		"SELECT * FROM users",
		"SELECT id, name FROM users",
		"SELECT u.id, u.name AS n FROM users AS u",
		"SELECT DISTINCT city FROM users",
		"SELECT t.* FROM t",
		"SELECT COUNT(*), max(score) FROM t",

		// sources
		"SELECT * FROM main.events AS e",
		`SELECT * FROM "My Table"`,
		"SELECT * FROM a JOIN (SELECT id FROM b) AS sub ON sub.id = a.id",

		// join shapes
		"SELECT * FROM a JOIN b ON a.id = b.id",
		"SELECT * FROM a LEFT JOIN b ON a.id = b.id",
		"SELECT * FROM a RIGHT JOIN b ON a.id = b.id",
		"SELECT * FROM a FULL JOIN b ON a.id = b.id",
		"SELECT * FROM a CROSS JOIN b",
		"SELECT * FROM a, b",
		"SELECT * FROM a NATURAL JOIN b",
		"SELECT * FROM a JOIN b USING (id, org_id)",
		"SELECT * FROM a CROSS JOIN b JOIN c ON b.id = c.bid",

		// comparison operators
		"SELECT * FROM t WHERE a = 1",
		"SELECT * FROM t WHERE a <> 1",
		"SELECT * FROM t WHERE a < 1",
		"SELECT * FROM t WHERE a <= 1",
		"SELECT * FROM t WHERE a > 1",
		"SELECT * FROM t WHERE a >= 1",
		"SELECT * FROM t WHERE 100 > a", // flips to a < 100

		// pattern operators
		"SELECT * FROM t WHERE name LIKE 'a%'",
		"SELECT * FROM t WHERE name NOT LIKE 'a%'",
		"SELECT * FROM t WHERE name GLOB 'a*'",
		"SELECT * FROM t WHERE name NOT GLOB 'a*'",
		"SELECT * FROM t WHERE name REGEXP '^a'",
		"SELECT * FROM t WHERE name MATCH 'a'",

		// identity operators
		"SELECT * FROM t WHERE a IS 5",
		"SELECT * FROM t WHERE a IS NOT 5",
		"SELECT * FROM t WHERE a IS DISTINCT FROM 5",
		"SELECT * FROM t WHERE a IS NOT DISTINCT FROM 5",
		"SELECT * FROM t WHERE a IS NULL",
		"SELECT * FROM t WHERE a IS NOT NULL",

		// range / set
		"SELECT * FROM t WHERE age BETWEEN 18 AND 65",
		"SELECT * FROM t WHERE age NOT BETWEEN 18 AND 65",
		"SELECT * FROM t WHERE id IN (1, 2, 3)",
		"SELECT * FROM t WHERE id NOT IN (1, 2)",

		// values
		"SELECT * FROM t WHERE a = :id",
		"SELECT * FROM t WHERE a = ?",
		"SELECT * FROM t WHERE a = 'paid'",
		"SELECT * FROM t WHERE a = 3.14",
		"SELECT * FROM t WHERE a = TRUE",
		"SELECT * FROM t WHERE a = X'4142'",

		// composition + unstructured fallbacks
		"SELECT * FROM t WHERE a = 1 AND b < 2 AND c IS NULL",
		"SELECT * FROM t WHERE p = 1 OR q = 2",
		"SELECT * FROM t WHERE id IN (SELECT id FROM other)",
		`SELECT "first name" FROM t WHERE "first name" = 'x'`,
	}

	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			q1, err := Parse(sql)
			require.NoError(t, err)

			rendered := q1.SQL()

			q2, err := Parse(rendered)
			require.NoErrorf(t, err, "rendered SQL did not parse: %q", rendered)

			// Rendering is stable: a second pass produces identical text.
			assert.Equal(t, rendered, q2.SQL(), "render is not idempotent")

			// The structured AST is preserved across the round trip.
			assert.Equal(t, clearText(q1.Stmt), clearText(q2.Stmt), "structure changed across round trip\nrendered: %s", rendered)
		})
	}
}
