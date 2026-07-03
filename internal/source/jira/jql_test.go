package jira

import (
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildJQLEquality(t *testing.T) {
	cases := map[string]struct {
		filter source.Filter
		want   string
	}{
		"key":                 {eqFilter("key", "PROJ-1"), `key = "PROJ-1"`},
		"project_key":         {eqFilter("project_key", "PROJ"), `project = "PROJ"`},
		"issue_type":          {eqFilter("issue_type", "Bug"), `issuetype = "Bug"`},
		"status":              {eqFilter("status", "Done"), `status = "Done"`},
		"priority":            {eqFilter("priority", "High"), `priority = "High"`},
		"resolution":          {eqFilter("resolution", "Fixed"), `resolution = "Fixed"`},
		"assignee_account_id": {eqFilter("assignee_account_id", "acc-1"), `assignee = "acc-1"`},
		"reporter_account_id": {eqFilter("reporter_account_id", "acc-2"), `reporter = "acc-2"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plan := buildJQL(source.ScanRequest{Filters: []source.Filter{tc.filter}})
			assert.Equal(t, tc.want, plan.JQL)
			assert.True(t, plan.ConsumedAll)
			assert.False(t, plan.Defaulted)
		})
	}
}

func TestBuildJQLIn(t *testing.T) {
	plan := buildJQL(source.ScanRequest{Filters: []source.Filter{inFilter("key", "A-1", "A-2")}})
	assert.Equal(t, `key in ("A-1", "A-2")`, plan.JQL)
	assert.True(t, plan.ConsumedAll)

	plan = buildJQL(source.ScanRequest{Filters: []source.Filter{inFilter("project_key", "A", "B")}})
	assert.Equal(t, `project in ("A", "B")`, plan.JQL)
}

func TestBuildJQLUnsupportedFilterNotConsumed(t *testing.T) {
	plan := buildJQL(source.ScanRequest{Filters: []source.Filter{
		{Column: "summary", Op: sqlparse.OpLike, Value: "%foo%"},
	}})
	assert.False(t, plan.ConsumedAll)
	assert.Equal(t, defaultBoundClause, plan.JQL) // no restriction translated -> boundedness default
	assert.True(t, plan.Defaulted)
}

func TestBuildJQLBoundednessDefault(t *testing.T) {
	plan := buildJQL(source.ScanRequest{})
	assert.Equal(t, defaultBoundClause, plan.JQL)
	assert.True(t, plan.Defaulted)
	assert.True(t, plan.ConsumedAll) // no filters at all: vacuously consumed
}

// Pushed date bounds are widened by a day in each direction (JQL datetime
// literals are interpreted in the Jira user's timezone, unknown to the
// connector; see tzSlack), then rounded outward to the minute (JQL's
// granularity). Lower bounds land a day earlier, upper bounds a day later.
// Because the pushed clause is a widened superset, a range filter is never
// "consumed" — ConsumedAll stays false so a LIMIT can't ride the search.
func TestBuildJQLDateRangeRounding(t *testing.T) {
	cases := map[string]struct {
		op     sqlparse.Operator
		value  any
		values []any
		want   string
	}{
		"gte exact minute":  {op: sqlparse.OpGte, value: "2024-01-01T10:00:00Z", want: `created >= "2023-12-31 10:00"`},
		"gt truncates down": {op: sqlparse.OpGt, value: "2024-01-01T10:00:30Z", want: `created >= "2023-12-31 10:00"`},
		"lte exact minute":  {op: sqlparse.OpLte, value: "2024-01-01T10:00:00Z", want: `created <= "2024-01-02 10:00"`},
		"lt rounds up":      {op: sqlparse.OpLt, value: "2024-01-01T10:00:30Z", want: `created <= "2024-01-02 10:01"`},
		"date-only lower":   {op: sqlparse.OpGte, value: "2024-01-01", want: `created >= "2023-12-31 00:00"`},
		"space form lower":  {op: sqlparse.OpGte, value: "2024-01-01 10:00", want: `created >= "2023-12-31 10:00"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plan := buildJQL(source.ScanRequest{Filters: []source.Filter{
				{Column: "created", Op: tc.op, Value: tc.value},
			}})
			assert.Equal(t, tc.want, plan.JQL)
			assert.False(t, plan.ConsumedAll) // widened bound -> not exact
		})
	}

	// BETWEEN: low widened a day earlier and truncated down, high widened a day
	// later and rounded up, both bounds present.
	plan := buildJQL(source.ScanRequest{Filters: []source.Filter{
		{Column: "updated", Op: sqlparse.OpBetween, Values: []any{"2024-01-01T10:00:30Z", "2024-01-02T10:00:30Z"}},
	}})
	assert.Equal(t, `updated >= "2023-12-31 10:00" AND updated <= "2024-01-03 10:01"`, plan.JQL)
	assert.False(t, plan.ConsumedAll)
}

func TestBuildJQLBothBoundsFromSeparateFilters(t *testing.T) {
	// req.Filters (not req.Filter, which only returns the first) must be
	// iterated so both a lower and upper bound on the same column are collected.
	plan := buildJQL(source.ScanRequest{Filters: []source.Filter{
		{Column: "created", Op: sqlparse.OpGte, Value: "2024-01-01T00:00:00Z"},
		{Column: "created", Op: sqlparse.OpLte, Value: "2024-01-31T00:00:00Z"},
	}})
	assert.Equal(t, `created >= "2023-12-31 00:00" AND created <= "2024-02-01 00:00"`, plan.JQL)
	assert.False(t, plan.ConsumedAll)
}

func TestBuildJQLEmptyStringNotPushed(t *testing.T) {
	// Jira rejects `field = ""` as invalid JQL, so an empty-string equality is
	// left to the local re-filter instead of failing the whole query.
	plan := buildJQL(source.ScanRequest{Filters: []source.Filter{eqFilter("status", "")}})
	assert.Equal(t, defaultBoundClause, plan.JQL)
	assert.False(t, plan.ConsumedAll)
	assert.True(t, plan.Defaulted)

	// Same for an IN list containing an empty element: the whole clause drops.
	plan = buildJQL(source.ScanRequest{Filters: []source.Filter{inFilter("key", "A-1", "")}})
	assert.Equal(t, defaultBoundClause, plan.JQL)
	assert.False(t, plan.ConsumedAll)
}

func TestBuildJQLUnparseableDateNotConsumed(t *testing.T) {
	plan := buildJQL(source.ScanRequest{Filters: []source.Filter{
		{Column: "created", Op: sqlparse.OpGte, Value: "not-a-date"},
	}})
	assert.False(t, plan.ConsumedAll)
	assert.Equal(t, defaultBoundClause, plan.JQL)
}

func TestQuoteJQLEscaping(t *testing.T) {
	assert.Equal(t, `"plain"`, quoteJQL("plain"))
	assert.Equal(t, `"has \"quotes\""`, quoteJQL(`has "quotes"`))
	assert.Equal(t, `"back\\slash"`, quoteJQL(`back\slash`))
}

func TestJQLOrderBy(t *testing.T) {
	clause, ok := jqlOrderBy([]source.OrderTerm{{Column: "created"}, {Column: "updated", Desc: true}})
	require.True(t, ok)
	assert.Equal(t, "created ASC, updated DESC", clause)

	clause, ok = jqlOrderBy([]source.OrderTerm{{Column: "due_date", Desc: true}})
	require.True(t, ok)
	assert.Equal(t, "duedate DESC", clause)

	// A single unmappable term (or one term in a multi-term list) means no
	// ordering can be pushed at all.
	_, ok = jqlOrderBy([]source.OrderTerm{{Column: "created"}, {Column: "summary"}})
	assert.False(t, ok)

	// key, priority, and status are deliberately unmappable: JQL sorts them
	// semantically (keys numerically, priority/status by rank), which differs
	// from SQLite's TEXT collation — pushing them would let a pushed LIMIT
	// truncate the wrong prefix.
	for _, col := range []string{"key", "priority", "status"} {
		_, ok = jqlOrderBy([]source.OrderTerm{{Column: col}})
		assert.False(t, ok, col)
	}

	// No ORDER BY at all is trivially "pushed" (nothing to honor).
	clause, ok = jqlOrderBy(nil)
	assert.True(t, ok)
	assert.Empty(t, clause)
}

func TestBuildJQLOrderByAppendedToJQL(t *testing.T) {
	plan := buildJQL(source.ScanRequest{
		Filters: []source.Filter{eqFilter("project_key", "PROJ")},
		OrderBy: []source.OrderTerm{{Column: "updated", Desc: true}},
	})
	assert.Equal(t, `project = "PROJ" ORDER BY updated DESC`, plan.JQL)
	assert.True(t, plan.OrderOK)
}

// --- ADF -> plain text ---

func TestADFTextNested(t *testing.T) {
	doc := map[string]any{
		"type": "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "Hello "},
					map[string]any{"type": "text", "text": "world."},
				},
			},
			map[string]any{
				"type": "bulletList",
				"content": []any{
					map[string]any{
						"type": "listItem",
						"content": []any{
							map[string]any{
								"type":    "paragraph",
								"content": []any{map[string]any{"type": "text", "text": "item one"}},
							},
						},
					},
					map[string]any{
						"type": "listItem",
						"content": []any{
							map[string]any{
								"type":    "paragraph",
								"content": []any{map[string]any{"type": "text", "text": "item two"}},
							},
						},
					},
				},
			},
		},
	}
	got := adfText(doc)
	assert.Equal(t, "Hello world.\nitem one\nitem two", got)
}

func TestADFTextHardBreak(t *testing.T) {
	doc := map[string]any{
		"type": "paragraph",
		"content": []any{
			map[string]any{"type": "text", "text": "line one"},
			map[string]any{"type": "hardBreak"},
			map[string]any{"type": "text", "text": "line two"},
		},
	}
	assert.Equal(t, "line one\nline two", adfText(doc))
}

func TestADFTextMention(t *testing.T) {
	doc := map[string]any{
		"type": "paragraph",
		"content": []any{
			map[string]any{"type": "text", "text": "hi "},
			map[string]any{"type": "mention", "attrs": map[string]any{"id": "123", "text": "@alice"}},
		},
	}
	assert.Equal(t, "hi @alice", adfText(doc))
}

func TestADFTextEmptyOrNil(t *testing.T) {
	assert.Equal(t, "", adfText(nil))
	assert.Equal(t, "", adfText(map[string]any{"type": "doc", "content": []any{}}))
}

func TestADFFieldTextAbsentOrNull(t *testing.T) {
	assert.Nil(t, adfFieldText(nil))
	assert.Nil(t, adfFieldText([]byte("null")))
	assert.Nil(t, adfFieldText([]byte("")))
	got := adfFieldText([]byte(`{"type":"paragraph","content":[{"type":"text","text":"hi"}]}`))
	assert.Equal(t, "hi", got)
}
