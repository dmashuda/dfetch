package newrelic

import (
	"testing"
	"time"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixed clock: default window lower bound = now - 1h = 1750000000000.
var testNow = time.UnixMilli(1750003600000)

// txCols is a representative Transaction schema (timestamp first, per keyset).
var txCols = []source.Column{
	{Name: "timestamp", Type: "INTEGER"},
	{Name: "appName", Type: "TEXT"},
	{Name: "duration", Type: "REAL"},
	{Name: "error", Type: "INTEGER"}, // boolean
}

var txBools = map[string]bool{"error": true}

func eqFilter(col string, v any) source.Filter {
	return source.Filter{Column: col, Op: sqlparse.OpEq, Value: v}
}

func buildFor(req source.ScanRequest) nrqlPlan {
	req.Table = "Transaction"
	return buildNRQL(req, txCols, txBools, nrqlHardCap, defaultWindow, testNow)
}

func TestBuildNRQLDefaults(t *testing.T) {
	plan := buildFor(source.ScanRequest{})
	assert.Equal(t, "SELECT * FROM `Transaction` LIMIT 5000 SINCE 1750000000000", plan.NRQL)
	assert.True(t, plan.Capped)
	assert.True(t, plan.Windowed)
	assert.False(t, plan.PushedLimit)
	assert.Equal(t, txCols, plan.Cols) // SELECT * emits every keyset column
}

func TestBuildNRQLProjection(t *testing.T) {
	plan := buildFor(source.ScanRequest{Columns: []string{"appName", "timestamp"}})
	assert.Contains(t, plan.NRQL, "SELECT `appName`, `timestamp` FROM")
	require.Len(t, plan.Cols, 2)
	assert.Equal(t, "appName", plan.Cols[0].Name) // projection order preserved
	assert.Equal(t, "timestamp", plan.Cols[1].Name)
}

// One case per operator: the rendered clause must be exactly equivalent to the
// SQLite predicate, or the filter must not be pushed at all.
func TestTranslateNRQLFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter source.Filter
		want   string // "" means not pushed
	}{
		{"string eq", eqFilter("appName", "billing"), "`appName` = 'billing'"},
		{"string escape", eqFilter("appName", `bi'll\ing`), `` + "`appName`" + ` = 'bi\'ll\\ing'`},
		{"not eq", source.Filter{Column: "appName", Op: sqlparse.OpNotEq, Value: "x"}, "`appName` != 'x'"},
		{"numeric gt", source.Filter{Column: "duration", Op: sqlparse.OpGt, Value: float64(1.5)}, "`duration` > 1.5"},
		{"numeric lte int", source.Filter{Column: "duration", Op: sqlparse.OpLte, Value: int64(3)}, "`duration` <= 3"},
		{"string relational not pushed", source.Filter{Column: "appName", Op: sqlparse.OpGt, Value: "m"}, ""},
		{"in list", source.Filter{Column: "appName", Op: sqlparse.OpIn, Values: []any{"a", "b"}}, "`appName` IN ('a', 'b')"},
		{"not in", source.Filter{Column: "appName", Op: sqlparse.OpNotIn, Values: []any{"a"}}, "`appName` NOT IN ('a')"},
		{"empty in not pushed", source.Filter{Column: "appName", Op: sqlparse.OpIn}, ""},
		{"between numeric", source.Filter{Column: "duration", Op: sqlparse.OpBetween, Values: []any{int64(1), int64(2)}}, "(`duration` >= 1 AND `duration` <= 2)"},
		{"between string not pushed", source.Filter{Column: "appName", Op: sqlparse.OpBetween, Values: []any{"a", "b"}}, ""},
		{"not between not pushed", source.Filter{Column: "duration", Op: sqlparse.OpNotBetween, Values: []any{int64(1), int64(2)}}, ""},
		{"like not pushed (case-insensitive in NRQL)", source.Filter{Column: "appName", Op: sqlparse.OpLike, Value: "bill%"}, ""},
		{"glob not pushed", source.Filter{Column: "appName", Op: sqlparse.OpGlob, Value: "b*"}, ""},
		{"bool eq 1", eqFilter("error", int64(1)), "`error` = true"},
		{"bool eq 0", eqFilter("error", int64(0)), "`error` = false"},
		{"bool eq other not pushed", eqFilter("error", int64(2)), ""},
		{"bool eq true literal", eqFilter("error", true), "`error` = true"},
		{"nil not pushed", eqFilter("appName", nil), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clause, ok := translateNRQLFilter(tc.filter, txBools)
			if tc.want == "" {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.want, clause)
		})
	}
}

// timestamp range filters are translated twice: an exact numeric WHERE clause
// (which makes them consumed) plus a 1ms-outward-widened SINCE/UNTIL window.
func TestBuildNRQLTimestamp(t *testing.T) {
	t.Run("lower bound", func(t *testing.T) {
		plan := buildFor(source.ScanRequest{Filters: []source.Filter{
			{Column: "timestamp", Op: sqlparse.OpGte, Value: int64(1750000000000)},
		}})
		assert.Contains(t, plan.NRQL, "WHERE `timestamp` >= 1750000000000")
		assert.Contains(t, plan.NRQL, "SINCE 1749999999999")
		assert.NotContains(t, plan.NRQL, "UNTIL")
		assert.False(t, plan.Windowed)
	})

	t.Run("between widens both sides", func(t *testing.T) {
		plan := buildFor(source.ScanRequest{Filters: []source.Filter{
			{Column: "timestamp", Op: sqlparse.OpBetween, Values: []any{int64(100), int64(200)}},
		}})
		assert.Contains(t, plan.NRQL, "WHERE (`timestamp` >= 100 AND `timestamp` <= 200)")
		assert.Contains(t, plan.NRQL, "SINCE 99 UNTIL 201")
	})

	t.Run("upper bound only anchors the window to it", func(t *testing.T) {
		upper := int64(1000000000000) // long before testNow: a now-anchored SINCE would invert
		plan := buildFor(source.ScanRequest{Filters: []source.Filter{
			{Column: "timestamp", Op: sqlparse.OpLt, Value: upper},
		}})
		assert.Contains(t, plan.NRQL, "SINCE 999996400001 UNTIL 1000000000001") // until - 1h
		assert.True(t, plan.Windowed)                                           // still warn: history is windowed
	})

	t.Run("equality bounds both sides", func(t *testing.T) {
		plan := buildFor(source.ScanRequest{Filters: []source.Filter{eqFilter("timestamp", int64(500))}})
		assert.Contains(t, plan.NRQL, "WHERE `timestamp` = 500")
		assert.Contains(t, plan.NRQL, "SINCE 499 UNTIL 501")
		assert.False(t, plan.Windowed)
	})

	t.Run("IN bounds the window by its min and max", func(t *testing.T) {
		plan := buildFor(source.ScanRequest{Filters: []source.Filter{
			{Column: "timestamp", Op: sqlparse.OpIn, Values: []any{int64(300), int64(100), int64(200)}},
		}})
		assert.Contains(t, plan.NRQL, "WHERE `timestamp` IN (300, 100, 200)")
		assert.Contains(t, plan.NRQL, "SINCE 99 UNTIL 301") // not the inverted 299..101
		assert.False(t, plan.Windowed)
	})

	t.Run("IN with an unparseable value bounds nothing", func(t *testing.T) {
		plan := buildFor(source.ScanRequest{Filters: []source.Filter{
			{Column: "timestamp", Op: sqlparse.OpIn, Values: []any{int64(100), "oops"}},
		}})
		assert.True(t, plan.Windowed) // default window, no partial bounds
	})

	t.Run("fractional bound is floored, not dropped", func(t *testing.T) {
		plan := buildFor(source.ScanRequest{Filters: []source.Filter{
			{Column: "timestamp", Op: sqlparse.OpGt, Value: float64(1750000000000.5)},
		}})
		assert.Contains(t, plan.NRQL, "WHERE `timestamp` > 1750000000000.5")
		// floor-1: still below the bound, so the window stays a superset of the
		// consumed WHERE clause instead of falling back to the default window.
		assert.Contains(t, plan.NRQL, "SINCE 1749999999999")
		assert.False(t, plan.Windowed)
	})

	t.Run("conjunction takes the tightest bounds", func(t *testing.T) {
		plan := buildFor(source.ScanRequest{Filters: []source.Filter{
			{Column: "timestamp", Op: sqlparse.OpGte, Value: int64(100)},
			{Column: "timestamp", Op: sqlparse.OpGte, Value: int64(300)},
			{Column: "timestamp", Op: sqlparse.OpLte, Value: int64(900)},
		}})
		assert.Contains(t, plan.NRQL, "SINCE 299 UNTIL 901")
	})
}

func TestBuildNRQLOrderLimit(t *testing.T) {
	limit := 50
	base := source.ScanRequest{
		Filters: []source.Filter{eqFilter("appName", "billing")},
		Limit:   &limit,
	}

	t.Run("consumed filters + timestamp order push the limit", func(t *testing.T) {
		req := base
		req.OrderBy = []source.OrderTerm{{Column: "timestamp", Desc: true}}
		plan := buildFor(req)
		assert.Contains(t, plan.NRQL, "ORDER BY `timestamp` DESC")
		assert.Contains(t, plan.NRQL, "LIMIT 50")
		assert.True(t, plan.PushedLimit)
	})

	t.Run("no order also pushes", func(t *testing.T) {
		plan := buildFor(base)
		assert.Contains(t, plan.NRQL, "LIMIT 50")
		assert.True(t, plan.PushedLimit)
	})

	t.Run("non-timestamp order blocks the push", func(t *testing.T) {
		req := base
		req.OrderBy = []source.OrderTerm{{Column: "appName"}}
		plan := buildFor(req)
		assert.NotContains(t, plan.NRQL, "ORDER BY")
		assert.Contains(t, plan.NRQL, "LIMIT 5000")
		assert.True(t, plan.Capped)
	})

	t.Run("unconsumed filter blocks the push", func(t *testing.T) {
		req := base
		req.Filters = append(req.Filters, source.Filter{Column: "appName", Op: sqlparse.OpLike, Value: "b%"})
		plan := buildFor(req)
		assert.Contains(t, plan.NRQL, "LIMIT 5000")
		assert.True(t, plan.Capped)
	})

	t.Run("offset rides the pushed limit", func(t *testing.T) {
		req := base
		offset := 20
		req.Offset = &offset
		plan := buildFor(req)
		assert.Contains(t, plan.NRQL, "LIMIT 70") // limit + offset
		assert.True(t, plan.PushedLimit)
	})

	t.Run("limit above the hard cap falls back to the cap", func(t *testing.T) {
		req := base
		big := 6000
		req.Limit = &big
		plan := buildFor(req)
		assert.Contains(t, plan.NRQL, "LIMIT 5000")
		assert.True(t, plan.Capped)
		assert.False(t, plan.PushedLimit)
	})
}

func TestParseKeyset(t *testing.T) {
	t.Run("array shape", func(t *testing.T) {
		cols, bools := parseKeyset([]map[string]any{{
			"stringKeys":  []any{"appName", "host"},
			"numericKeys": []any{"duration", "timestamp"},
			"booleanKeys": []any{"error"},
		}})
		require.NotEmpty(t, cols)
		assert.Equal(t, source.Column{Name: "timestamp", Type: "INTEGER"}, cols[0]) // forced INTEGER, first
		byName := map[string]string{}
		for _, c := range cols {
			byName[c.Name] = c.Type
		}
		assert.Equal(t, "TEXT", byName["appName"])
		assert.Equal(t, "REAL", byName["duration"])
		assert.Equal(t, "INTEGER", byName["error"])
		assert.True(t, bools["error"])
		assert.False(t, bools["duration"])
	})

	t.Run("row shape", func(t *testing.T) {
		cols, bools := parseKeyset([]map[string]any{
			{"key": "appName", "type": "string"},
			{"key": "error", "type": "boolean"},
		})
		assert.Equal(t, "timestamp", cols[0].Name) // injected even when keyset omits it
		assert.True(t, bools["error"])
	})

	t.Run("empty means unknown table", func(t *testing.T) {
		cols, _ := parseKeyset(nil)
		assert.Nil(t, cols)
	})

	t.Run("backtick keys dropped", func(t *testing.T) {
		cols, _ := parseKeyset([]map[string]any{{"stringKeys": []any{"ok", "bad`name"}}})
		names := colNames(cols)
		assert.Contains(t, names, "ok")
		assert.NotContains(t, names, "bad`name")
	})
}

func TestParseEventTypes(t *testing.T) {
	types := parseEventTypes([]map[string]any{
		{"eventType": "Transaction"},
		{"event type": "Custom"}, // defensive: sole string value
		{"eventType": "Span"},
	})
	assert.Equal(t, []string{"Transaction", "Custom", "Span"}, types)
}

func TestNormalizeNRDB(t *testing.T) {
	assert.Nil(t, normalizeNRDB(nil, "TEXT"))
	assert.Equal(t, "x", normalizeNRDB("x", "TEXT"))
	assert.Equal(t, true, normalizeNRDB(true, "INTEGER"))
	assert.Equal(t, int64(1750000000000), normalizeNRDB(float64(1750000000000), "INTEGER")) // timestamp
	assert.Equal(t, 1.5, normalizeNRDB(1.5, "REAL"))
	assert.Equal(t, 1.5, normalizeNRDB(1.5, "INTEGER")) // fractional stays float even under INTEGER affinity
	assert.JSONEq(t, `{"a":1}`, normalizeNRDB(map[string]any{"a": float64(1)}, "TEXT").(string))
	assert.JSONEq(t, `[1,2]`, normalizeNRDB([]any{float64(1), float64(2)}, "TEXT").(string))
}

func TestBuildEntitySearchQuery(t *testing.T) {
	q := buildEntitySearchQuery(source.ScanRequest{Filters: []source.Filter{
		eqFilter("name", "my'app"),
		eqFilter("domain", "APM"),
	}}, 42)
	assert.Equal(t, `accountId = 42 AND domain = 'APM' AND name = 'my\'app'`, q)

	// No filters: still scoped to the account (never an empty search query).
	assert.Equal(t, "accountId = 42", buildEntitySearchQuery(source.ScanRequest{}, 42))

	// guid maps to the search's id key.
	q = buildEntitySearchQuery(source.ScanRequest{Filters: []source.Filter{eqFilter("guid", "G1")}}, 42)
	assert.Equal(t, "accountId = 42 AND id = 'G1'", q)
}
