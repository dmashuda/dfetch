package postgres

import (
	"testing"
	"time"

	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/assert"
)

func intPtr(n int) *int { return &n }

// orders is a representative table's column→pgType map.
var orders = map[string]string{
	"id":         "integer",
	"total":      "numeric",
	"status":     "text",
	"created_at": "timestamp with time zone",
}

func TestBuildSelectCappedSignal(t *testing.T) {
	// Unbounded scan → the maxRows safety cap is applied.
	_, _, capped := buildSelect("public", source.ScanRequest{Table: "orders"}, orders, 100000)
	assert.True(t, capped, "unbounded scan should report capped")

	// A pushed user LIMIT (safe order, all filters consumed) is not the cap, even
	// when the limit equals maxRows — so the caller must not warn.
	limit := 100000
	_, _, capped = buildSelect("public", source.ScanRequest{
		Table:   "orders",
		OrderBy: []source.OrderTerm{{Column: "id"}},
		Limit:   &limit,
	}, orders, 100000)
	assert.False(t, capped, "pushed user LIMIT must not be reported as the cap")
}

func TestBuildSelectProjection(t *testing.T) {
	sql, args, _ := buildSelect("public",
		source.ScanRequest{Table: "orders", Columns: []string{"id", "total"}}, orders, 100000)
	assert.Equal(t, `SELECT "id", "total" FROM "public"."orders" LIMIT 100000`, sql)
	assert.Empty(t, args)
}

func TestBuildSelectStarWhenNoColumns(t *testing.T) {
	sql, _, _ := buildSelect("public", source.ScanRequest{Table: "orders"}, orders, 100000)
	assert.Equal(t, `SELECT * FROM "public"."orders" LIMIT 100000`, sql)
}

func TestBuildSelectWhere(t *testing.T) {
	req := source.ScanRequest{Table: "orders", Filters: []source.Filter{
		{Column: "status", Op: source.OpEq, Value: "paid"},
		{Column: "total", Op: source.OpGte, Value: int64(5)},
	}}
	sql, args, _ := buildSelect("public", req, orders, 100000)
	assert.Equal(t, `SELECT * FROM "public"."orders" WHERE "status" = $1 AND "total" >= $2 LIMIT 100000`, sql)
	assert.Equal(t, []any{"paid", int64(5)}, args)
}

func TestBuildSelectInAndBetween(t *testing.T) {
	req := source.ScanRequest{Table: "orders", Filters: []source.Filter{
		{Column: "status", Op: source.OpIn, Values: []any{"paid", "shipped"}},
		{Column: "id", Op: source.OpBetween, Values: []any{int64(1), int64(9)}},
	}}
	sql, args, _ := buildSelect("public", req, orders, 100000)
	assert.Equal(t,
		`SELECT * FROM "public"."orders" WHERE "status" IN ($1, $2) AND "id" BETWEEN $3 AND $4 LIMIT 100000`, sql)
	assert.Equal(t, []any{"paid", "shipped", int64(1), int64(9)}, args)
}

// LIKE differs in case-sensitivity between SQLite and Postgres, so it is not
// pushed; SQLite re-applies it. Because a filter was skipped, LIMIT is not pushed.
func TestBuildSelectSkipsLikeAndDoesNotPushLimit(t *testing.T) {
	req := source.ScanRequest{
		Table:   "orders",
		Filters: []source.Filter{{Column: "status", Op: source.OpLike, Value: "paid%"}},
		OrderBy: []source.OrderTerm{{Column: "id", Desc: true}},
		Limit:   intPtr(5),
	}
	sql, args, _ := buildSelect("public", req, orders, 100000)
	assert.Equal(t, `SELECT * FROM "public"."orders" LIMIT 100000`, sql) // no WHERE, no ORDER BY, cap
	assert.Empty(t, args)
}

// Timestamp ordering rides the pushed LIMIT (fetched as limit+offset), with NULLS
// aligned to SQLite and all filters consumed.
func TestBuildSelectPushesOrderAndLimit(t *testing.T) {
	req := source.ScanRequest{
		Table:   "orders",
		Filters: []source.Filter{{Column: "status", Op: source.OpEq, Value: "paid"}},
		OrderBy: []source.OrderTerm{{Column: "created_at", Desc: true}},
		Limit:   intPtr(5),
		Offset:  intPtr(3),
	}
	sql, args, _ := buildSelect("public", req, orders, 100000)
	assert.Equal(t,
		`SELECT * FROM "public"."orders" WHERE "status" = $1 ORDER BY "created_at" DESC NULLS LAST LIMIT 8`, sql)
	assert.Equal(t, []any{"paid"}, args)
}

// A text order key can collate differently in Postgres, so LIMIT is not pushed.
func TestBuildSelectTextOrderDoesNotPushLimit(t *testing.T) {
	req := source.ScanRequest{
		Table:   "orders",
		OrderBy: []source.OrderTerm{{Column: "status"}}, // text
		Limit:   intPtr(5),
	}
	sql, _, _ := buildSelect("public", req, orders, 100000)
	assert.Equal(t, `SELECT * FROM "public"."orders" LIMIT 100000`, sql)
}

// numeric ordering is excluded too (REAL can lose a tie-breaking digit).
func TestOrderPushSafe(t *testing.T) {
	for _, ty := range []string{"integer", "bigint", "real", "double precision", "boolean", "timestamp with time zone", "date"} {
		assert.True(t, orderPushSafe(ty), ty)
	}
	for _, ty := range []string{"text", "character varying", "uuid", "numeric", "decimal", "json", ""} {
		assert.False(t, orderPushSafe(ty), ty)
	}
}

func TestPgTypeToAffinity(t *testing.T) {
	cases := map[string]string{
		"integer": "INTEGER", "bigint": "INTEGER", "smallint": "INTEGER", "boolean": "INTEGER",
		"real": "REAL", "double precision": "REAL", "numeric": "REAL", "decimal": "REAL",
		"text": "TEXT", "character varying": "TEXT", "uuid": "TEXT",
		"timestamp with time zone": "TEXT", "jsonb": "TEXT",
	}
	for pg, want := range cases {
		assert.Equal(t, want, pgTypeToAffinity(pg), pg)
	}
}

func TestNormalize(t *testing.T) {
	assert.Nil(t, normalize(nil))
	assert.Equal(t, int64(7), normalize(7))
	assert.Equal(t, int64(7), normalize(int32(7)))
	assert.Equal(t, int64(7), normalize(int64(7)))
	assert.Equal(t, float64(1.5), normalize(float32(1.5)))
	assert.Equal(t, "hi", normalize([]byte("hi")))
	assert.Equal(t, true, normalize(true))
	ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.FixedZone("EST", -5*3600))
	assert.Equal(t, "2026-06-21T17:00:00.000000Z", normalize(ts)) // coerced to UTC
}

// Timestamp text must be fixed-width so lexical order matches chronological
// order — orderPushSafe's justification for letting ORDER BY+LIMIT ride a
// temporal column. A trimmed-fraction format (RFC3339Nano) breaks this:
// "…05Z" sorts lexically after "…05.5Z" because 'Z' > '.'.
func TestNormalizeTimestampOrderIsLexical(t *testing.T) {
	early := time.Date(2026, 6, 21, 12, 0, 5, 0, time.UTC)            // whole second
	late := time.Date(2026, 6, 21, 12, 0, 5, 500_000_000, time.UTC)   // +0.5s
	latest := time.Date(2026, 6, 21, 12, 0, 5, 500_001_000, time.UTC) // +0.500001s
	a, b, c := normalize(early).(string), normalize(late).(string), normalize(latest).(string)
	assert.Less(t, a, b)
	assert.Less(t, b, c)
}

func TestNewRequiresDSN(t *testing.T) {
	t.Setenv("DFETCH_POSTGRES_DSN", "")
	t.Setenv("DATABASE_URL", "")
	_, err := New(map[string]any{})
	assert.Error(t, err)
}

func TestNewUsesParamsAndEnv(t *testing.T) {
	t.Setenv("DFETCH_POSTGRES_DSN", "postgres://env/db")
	c, err := New(map[string]any{"schema": "analytics", "max_rows": 25})
	assert.NoError(t, err)
	pc := c.(*Connector)
	assert.Equal(t, "analytics", pc.schema)
	assert.Equal(t, 25, pc.maxRows)
	assert.Empty(t, pc.Tables()) // dynamic: no static tables
}
