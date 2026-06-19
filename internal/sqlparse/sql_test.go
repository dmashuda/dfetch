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
			assertRoundTrips(t, sql)
		})
	}
}

// TestRoundTripComplex exercises bigger, messy queries that combine multiple
// joins, subqueries, aliases, and WHERE clauses mixing comparisons, identity,
// pattern, range, and set operators (with raw fallbacks like OR and column-to-
// column predicates interleaved).
func TestRoundTripComplex(t *testing.T) {
	cases := map[string]string{
		"orders with subquery join and mixed filters": `
			SELECT u.id, u.name AS customer, o.total, p.label
			FROM main.users AS u
			JOIN orders AS o ON o.user_id = u.id
			LEFT JOIN (SELECT id, label FROM products WHERE active = TRUE) AS p ON p.id = o.product_id
			WHERE u.age BETWEEN 21 AND 65
			  AND o.status IN ('paid', 'shipped')
			  AND u.email NOT LIKE '%@test.com'
			  AND o.deleted_at IS NULL
			  AND u.region IS NOT DISTINCT FROM :region`,

		"self join with binds and column-to-column predicate": `
			SELECT a.id, b.id
			FROM edges AS a
			JOIN edges AS b ON a.dst = b.src
			WHERE a.weight >= :min AND b.weight < 100 AND a.kind <> b.kind AND a.label GLOB 'n*'`,

		"three way join with using, natural, in-list and quoted identifiers": `
			SELECT DISTINCT t."first name", c.country
			FROM "user table" AS t
			JOIN contacts AS c USING (user_id)
			NATURAL JOIN regions
			WHERE c.country IN ('US', 'CA', 'MX')
			  AND t."first name" IS NOT NULL
			  AND c.score > 0.5`,

		"derived table with not-between, blob and is-distinct-from": `
			SELECT s.id, s.amount
			FROM (SELECT id, amount, acct FROM sales WHERE amount > 1000) AS s
			JOIN accounts AS a ON a.id = s.acct
			WHERE a.type = 'premium'
			  AND s.amount NOT BETWEEN 0 AND 100
			  AND a.token = X'deadbeef'
			  AND a.flags IS DISTINCT FROM 0`,

		"cross join, multi-key on, parenthesized or fallback": `
			SELECT p.sku, p.price, w.qty
			FROM products AS p
			CROSS JOIN warehouses AS w
			JOIN inventory AS i ON i.sku = p.sku AND i.wid = w.id
			WHERE (p.price < 10 OR p.price > 1000)
			  AND p.category IN ('a', 'b', 'c')
			  AND w.region LIKE 'EU%'
			  AND i.qty IS NOT NULL`,
	}

	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			assertRoundTrips(t, sql)
		})
	}
}

func assertRoundTrips(t *testing.T, sql string) {
	t.Helper()

	q1, err := Parse(sql)
	require.NoError(t, err)

	rendered := q1.SQL()

	q2, err := Parse(rendered)
	require.NoErrorf(t, err, "rendered SQL did not parse: %q", rendered)

	// Rendering is stable: a second pass produces identical text.
	assert.Equal(t, rendered, q2.SQL(), "render is not idempotent")

	// The structured AST is preserved across the round trip.
	assert.Equal(t, clearText(q1.Stmt), clearText(q2.Stmt), "structure changed across round trip\nrendered: %s", rendered)
}
