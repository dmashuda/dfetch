package jira

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newTestConnector(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("JIRA_API_TOKEN", "test")
	t.Setenv("JIRA_EMAIL", "test@test.com")
	c, err := New(map[string]any{"base_url": srv.URL})
	require.NoError(t, err)
	return c.(*Connector)
}

func eqFilter(col string, val any) source.Filter {
	return source.Filter{Column: col, Op: source.OpEq, Value: val}
}

func inFilter(col string, vals ...any) source.Filter {
	return source.Filter{Column: col, Op: source.OpIn, Values: vals}
}

// collectScan runs Scan and accumulates every emitted chunk into one Rows, so
// tests can assert on the full result. Columns come from the first chunk (the
// connector repeats them per page).
func collectScan(c source.Connector, req source.ScanRequest) (*source.Rows, error) {
	rows := &source.Rows{}
	err := c.Scan(context.Background(), req, func(chunk *source.Rows) error {
		if rows.Columns == nil && len(chunk.Columns) > 0 {
			rows.Columns = chunk.Columns
		}
		rows.Rows = append(rows.Rows, chunk.Rows...)
		rows.Warnings = append(rows.Warnings, chunk.Warnings...)
		return nil
	})
	return rows, err
}

// collectScanDetailed runs Scan once and returns both the accumulated result
// and the per-chunk row counts, so a test that needs both the full rows and the
// streaming shape doesn't have to run a stateful handler twice.
func collectScanDetailed(c source.Connector, req source.ScanRequest) (*source.Rows, []int, error) {
	rows := &source.Rows{}
	var sizes []int
	err := c.Scan(context.Background(), req, func(chunk *source.Rows) error {
		if rows.Columns == nil && len(chunk.Columns) > 0 {
			rows.Columns = chunk.Columns
		}
		rows.Rows = append(rows.Rows, chunk.Rows...)
		rows.Warnings = append(rows.Warnings, chunk.Warnings...)
		if len(chunk.Rows) > 0 {
			sizes = append(sizes, len(chunk.Rows))
		}
		return nil
	})
	return rows, sizes, err
}

func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	var v map[string]any
	require.NoError(t, json.Unmarshal(b, &v))
	return v
}

// --- New / auth ---

func TestNewRequiresBaseURL(t *testing.T) {
	_, err := New(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base_url is required")

	_, err = New(map[string]any{"base_url": ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base_url is required")
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c, err := New(map[string]any{"base_url": "https://x.atlassian.net/"})
	require.NoError(t, err)
	assert.Equal(t, "https://x.atlassian.net", c.(*Connector).baseURL)
}

func TestAuthHeaderFromEnv(t *testing.T) {
	t.Setenv("JIRA_EMAIL", "me@example.com")
	t.Setenv("JIRA_API_TOKEN", "s3cr3t")
	c, err := New(map[string]any{"base_url": "https://x.atlassian.net"})
	require.NoError(t, err)

	h, err := c.(*Connector).getAuthHeader(context.Background())
	require.NoError(t, err)
	assert.True(t, len(h) > len("Basic "))
	assert.Contains(t, h, "Basic ")
}

func TestAuthHeaderFromCommand(t *testing.T) {
	c, err := New(map[string]any{
		"base_url":            "https://x.atlassian.net",
		"auth_header_command": []any{"echo", "Bearer from-command"},
	})
	require.NoError(t, err)

	h, err := c.(*Connector).getAuthHeader(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer from-command", h)
}

func TestAuthHeaderNoneConfigured(t *testing.T) {
	c, err := New(map[string]any{"base_url": "https://x.atlassian.net"})
	require.NoError(t, err)
	h, err := c.(*Connector).getAuthHeader(context.Background())
	require.NoError(t, err)
	assert.Empty(t, h)
}

// Fix: the engine Scans jira tables concurrently, so getAuthHeader must be
// race-free — every path goes through authOnce (run with -race).
func TestGetAuthHeaderConcurrent(t *testing.T) {
	c, err := New(map[string]any{
		"base_url":            "https://x.atlassian.net",
		"auth_header_command": []any{"echo", "Bearer from-command"},
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := c.(*Connector).getAuthHeader(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, "Bearer from-command", h)
		}()
	}
	wg.Wait()
}

func TestNewRejectsInvalidAuthHeaderCommand(t *testing.T) {
	_, err := New(map[string]any{"base_url": "https://x.atlassian.net", "auth_header_command": "cat foo"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_header_command must be a list of strings")
}

// --- Tables ---

func TestTables(t *testing.T) {
	c, err := New(map[string]any{"base_url": "https://x.atlassian.net"})
	require.NoError(t, err)
	tables := c.Tables()
	names := make([]string, 0, len(tables))
	for _, tbl := range tables {
		names = append(names, tbl.Name)
		assert.NotEmpty(t, tbl.Columns)
	}
	assert.ElementsMatch(t, []string{"issues", "projects", "comments"}, names)
}

// --- API error surfacing ---

func TestAPIErrorSurfacing(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorMessages":["The JQL is invalid"]}`))
	})
	_, err := collectScan(c, source.ScanRequest{Table: "issues"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "The JQL is invalid")
}

func TestAPIErrorSurfacing401(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errorMessages":["Unauthorized"]}`))
	})
	_, err := collectScan(c, source.ScanRequest{Table: "issues"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JIRA_EMAIL")
	assert.Contains(t, err.Error(), "JIRA_API_TOKEN")
}

// --- issues ---

const issuesPage1 = `{
	"issues": [
		{"id":"10001","key":"PROJ-1","fields":{
			"summary":"first bug","description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"desc one"}]}]},
			"status":{"name":"In Progress","statusCategory":{"name":"In Progress"}},
			"issuetype":{"name":"Bug"},"priority":{"name":"High"},"resolution":null,
			"assignee":{"accountId":"acc-1","displayName":"Alice"},
			"reporter":{"accountId":"acc-2","displayName":"Bob"},
			"labels":["backend","urgent"],
			"created":"2024-01-01T10:00:00.000+0000","updated":"2024-02-01T10:00:00.000+0000",
			"resolutiondate":null,"duedate":"2024-03-01",
			"project":{"key":"PROJ"},"parent":{"key":"PROJ-0"}
		}}
	],
	"nextPageToken": "page2token"
}`

const issuesPage2 = `{
	"issues": [
		{"id":"10002","key":"PROJ-2","fields":{
			"summary":"second bug","description":null,
			"status":{"name":"Done","statusCategory":{"name":"Done"}},
			"issuetype":{"name":"Task"},"priority":null,"resolution":{"name":"Fixed"},
			"assignee":null,"reporter":null,"labels":[],
			"created":"2024-01-02T10:00:00.000+0000","updated":"2024-02-02T10:00:00.000+0000",
			"resolutiondate":"2024-02-02T10:00:00.000+0000","duedate":null,
			"project":{"key":"PROJ"},"parent":null
		}}
	]
}`

func TestScanIssuesHappyPathTwoPages(t *testing.T) {
	var bodies []map[string]any
	calls := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rest/api/3/search/jql", r.URL.Path)
		bodies = append(bodies, readBody(t, r))
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(issuesPage1))
		} else {
			_, _ = w.Write([]byte(issuesPage2))
		}
	})

	req := source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("project_key", "PROJ")},
	}
	rows, sizes, err := collectScanDetailed(c, req)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 1}, sizes) // one chunk per page
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, colNames(issuesCols), rows.Columns)

	row := rows.Rows[0]
	assert.Equal(t, int64(10001), row[0])
	assert.Equal(t, "PROJ-1", row[1])
	assert.Equal(t, "PROJ", row[2])
	assert.Equal(t, "Bug", row[3])
	assert.Equal(t, "In Progress", row[4])
	assert.Equal(t, "In Progress", row[5])
	assert.Equal(t, "High", row[6])
	assert.Nil(t, row[7]) // resolution
	assert.Equal(t, "first bug", row[8])
	assert.Equal(t, "desc one", row[9])
	assert.Equal(t, "acc-1", row[10])
	assert.Equal(t, "Alice", row[11])
	assert.Equal(t, "acc-2", row[12])
	assert.Equal(t, "Bob", row[13])
	assert.JSONEq(t, `["backend","urgent"]`, row[14].(string))
	assert.Equal(t, "2024-01-01T10:00:00.000+0000", row[15])
	assert.Equal(t, "2024-02-01T10:00:00.000+0000", row[16])
	assert.Nil(t, row[17]) // resolved
	assert.Equal(t, "2024-03-01", row[18])
	assert.Equal(t, "PROJ-0", row[19])
	assert.Contains(t, row[20].(string), "/browse/PROJ-1")

	row2 := rows.Rows[1]
	assert.Nil(t, row2[6]) // priority
	assert.Equal(t, "Fixed", row2[7])
	assert.Nil(t, row2[9])  // description
	assert.Nil(t, row2[10]) // assignee_account_id
	assert.Nil(t, row2[12]) // reporter_account_id
	assert.Nil(t, row2[19]) // parent_key

	// Push-down landed in the request body: JQL translated the eq filter, all
	// fields requested explicitly, and the second page carried the token.
	require.Len(t, bodies, 2)
	assert.Equal(t, `project = "PROJ"`, bodies[0]["jql"])
	fields, ok := bodies[0]["fields"].([]any)
	require.True(t, ok)
	assert.Contains(t, fields, "summary")
	assert.Contains(t, fields, "description")
	assert.NotContains(t, bodies[0], "nextPageToken")
	assert.Equal(t, "page2token", bodies[1]["nextPageToken"])
}

func TestScanIssuesLimitPushedWhenConsumedAndOrdered(t *testing.T) {
	var gotBody map[string]any
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		_, _ = w.Write([]byte(issuesPage1))
	})

	limit := 5
	req := source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("status", "In Progress")},
		OrderBy: []source.OrderTerm{{Column: "updated", Desc: true}},
		Limit:   &limit,
	}
	_, err := collectScan(c, req)
	require.NoError(t, err)
	assert.Equal(t, `status = "In Progress" ORDER BY updated DESC`, gotBody["jql"])
	assert.EqualValues(t, 5, gotBody["maxResults"])
}

// Fix: an unmappable ORDER BY column means the API can't honor the ordering, so
// a pushed LIMIT would truncate the wrong rows — LIMIT must not be pushed, and
// pagination should instead be bounded by the page cap with a warning.
func TestScanIssuesLimitNotPushedWithUnmappableOrder(t *testing.T) {
	calls := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotBody := readBody(t, r)
		assert.NotContains(t, gotBody["jql"], "ORDER BY")
		w.Header().Set("Content-Type", "application/json")
		// Always advertise a next page so the scan runs to the cap.
		_, _ = w.Write([]byte(`{"issues":[{"id":"1","key":"P-1","fields":{
			"status":{"name":"x","statusCategory":{"name":"x"}},"issuetype":{"name":"x"},
			"project":{"key":"P"},"created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-01T00:00:00.000+0000"
		}}],"nextPageToken":"more"}`))
	})

	limit := 2
	req := source.ScanRequest{
		Table:   "issues",
		OrderBy: []source.OrderTerm{{Column: "summary"}}, // not a mappable JQL sort field
		Limit:   &limit,
	}
	res, err := collectScan(c, req)
	require.NoError(t, err)
	assert.Equal(t, maxPages, calls)
	// Two warnings: the filterless scan got the default -30d bound, and the
	// unpushed LIMIT ran into the page cap.
	require.Len(t, res.Warnings, 2)
	assert.Contains(t, res.Warnings[0], defaultBoundClause)
	assert.Contains(t, res.Warnings[1], "jira.issues")
	assert.Contains(t, res.Warnings[1], "cap")
}

// Fix: LIMIT push-down must account for OFFSET. With LIMIT 2 OFFSET 3 the
// connector must fetch limit+offset=5 rows so SQLite can apply the OFFSET.
func TestScanIssuesLimitWithOffsetFetchesEnough(t *testing.T) {
	var gotBody map[string]any
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		_, _ = w.Write([]byte(`{"issues":[
			{"id":"1","key":"P-1","fields":{"status":{"name":"x","statusCategory":{"name":"x"}},"issuetype":{"name":"x"},"project":{"key":"P"},"created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-01T00:00:00.000+0000"}},
			{"id":"2","key":"P-2","fields":{"status":{"name":"x","statusCategory":{"name":"x"}},"issuetype":{"name":"x"},"project":{"key":"P"},"created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-01T00:00:00.000+0000"}},
			{"id":"3","key":"P-3","fields":{"status":{"name":"x","statusCategory":{"name":"x"}},"issuetype":{"name":"x"},"project":{"key":"P"},"created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-01T00:00:00.000+0000"}},
			{"id":"4","key":"P-4","fields":{"status":{"name":"x","statusCategory":{"name":"x"}},"issuetype":{"name":"x"},"project":{"key":"P"},"created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-01T00:00:00.000+0000"}},
			{"id":"5","key":"P-5","fields":{"status":{"name":"x","statusCategory":{"name":"x"}},"issuetype":{"name":"x"},"project":{"key":"P"},"created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-01T00:00:00.000+0000"}}
		]}`))
	})

	limit, offset := 2, 3
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("project_key", "P")},
		Limit:   &limit,
		Offset:  &offset,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 5, gotBody["maxResults"]) // limit + offset, not just limit
	assert.Len(t, rows.Rows, 5)
}

// Fix: a range filter's pushed JQL bound is widened (tzSlack + minute
// rounding), so it is NOT an exact translation — the LIMIT must not be pushed,
// or it could be spent entirely on slack-window rows the local re-filter drops.
func TestScanIssuesRangeFilterBlocksLimitPush(t *testing.T) {
	var gotBody map[string]any
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		_, _ = w.Write([]byte(issuesPage2)) // single page, no next token
	})

	limit := 2
	req := source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{{Column: "created", Op: source.OpGte, Value: "2024-06-01T00:00:00Z"}},
		Limit:   &limit,
	}
	_, err := collectScan(c, req)
	require.NoError(t, err)
	assert.Equal(t, `created >= "2024-05-31 00:00"`, gotBody["jql"])
	assert.EqualValues(t, defaultPageSize, gotBody["maxResults"]) // full page, not the limit
}

// Fix: when no filter translates, the injected `created >= -30d` bound narrows
// the result to history the query never asked for — it must surface a warning
// and must not let a LIMIT ride the search (the top-N of the window is not the
// top-N overall).
func TestScanIssuesDefaultBoundWarnsAndBlocksLimit(t *testing.T) {
	var gotBody map[string]any
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		_, _ = w.Write([]byte(issuesPage2))
	})

	limit := 2
	res, err := collectScan(c, source.ScanRequest{
		Table:   "issues",
		OrderBy: []source.OrderTerm{{Column: "created"}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, defaultBoundClause+" ORDER BY created ASC", gotBody["jql"])
	assert.EqualValues(t, defaultPageSize, gotBody["maxResults"]) // LIMIT not pushed
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], defaultBoundClause)
}

// Auth is required: with no credentials configured a scan fails with a message
// naming both options, and no request goes out.
func TestScanWithoutCredentialsErrors(t *testing.T) {
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true }))
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL})
	require.NoError(t, err)

	_, err = collectScan(c, source.ScanRequest{Table: "projects"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JIRA_EMAIL")
	assert.Contains(t, err.Error(), "auth_header_command")
	assert.False(t, called, "no request should be sent without credentials")
}

// The generated JQL is recorded on a jira.jql span (db.query.text) — it rides
// in the POST body, invisible to the generic otelhttp span, so without this
// traces can't show what was pushed down (mirrors newrelic.nrql).
func TestScanIssuesRecordsJQLSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(issuesPage2))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("project_key", "PROJ")},
	})
	require.NoError(t, err)

	jqlSpans := make([]sdktrace.ReadOnlySpan, 0, 1)
	for _, s := range sr.Ended() {
		if s.Name() == "jira.jql" {
			jqlSpans = append(jqlSpans, s)
		}
	}
	require.Len(t, jqlSpans, 1) // one page -> one span
	var got string
	for _, kv := range jqlSpans[0].Attributes() {
		if kv.Key == "db.query.text" {
			got = kv.Value.AsString()
		}
	}
	assert.Equal(t, `project = "PROJ"`, got)
}

func TestScanIssuesInvalidIDErrors(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"id":"not-a-number","key":"P-1","fields":{"status":{"name":"x","statusCategory":{"name":"x"}},"issuetype":{"name":"x"},"project":{"key":"P"}}}]}`))
	})
	_, err := collectScan(c, source.ScanRequest{Table: "issues"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid id")
}

// --- projects ---

func TestScanProjectsPagingAndKeysParam(t *testing.T) {
	calls := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, []string{"PROJ", "OTHER"}, r.URL.Query()["keys"])
		if calls == 1 {
			assert.Equal(t, "0", r.URL.Query().Get("startAt"))
			_, _ = w.Write([]byte(`{"values":[{"id":"1","key":"PROJ","name":"Project One","projectTypeKey":"software","style":"next-gen","isPrivate":false,"self":"http://x/rest/api/3/project/1"}],"isLast":false}`))
			return
		}
		assert.Equal(t, "1", r.URL.Query().Get("startAt"))
		_, _ = w.Write([]byte(`{"values":[{"id":"2","key":"OTHER","name":"Other","projectTypeKey":"business","style":"classic","isPrivate":true,"self":"http://x/rest/api/3/project/2"}],"isLast":true}`))
	})

	req := source.ScanRequest{
		Table:   "projects",
		Filters: []source.Filter{inFilter("key", "PROJ", "OTHER")},
	}
	rows, sizes, err := collectScanDetailed(c, req)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 1}, sizes)
	assert.Equal(t, 2, calls)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, colNames(projectsCols), rows.Columns)
	assert.Equal(t, int64(1), rows.Rows[0][0])
	assert.Equal(t, "PROJ", rows.Rows[0][1])
	assert.Equal(t, "Project One", rows.Rows[0][2])
	assert.Equal(t, "software", rows.Rows[0][3])
	assert.Equal(t, "next-gen", rows.Rows[0][4])
	assert.Equal(t, false, rows.Rows[0][5])
	assert.Equal(t, true, rows.Rows[1][5])
}

func TestScanProjectsOrderByPush(t *testing.T) {
	var gotOrderBy string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotOrderBy = r.URL.Query().Get("orderBy")
		_, _ = w.Write([]byte(`{"values":[],"isLast":true}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "projects",
		OrderBy: []source.OrderTerm{{Column: "name", Desc: true}},
	})
	require.NoError(t, err)
	assert.Equal(t, "-name", gotOrderBy)
}

// Fix: only the FIRST key filter is pushed (stringEqOrIn), so a second filter
// on key makes a pushed LIMIT unsafe — the truncated fetch could hold only
// rows the unpushed filter then discards.
func TestScanProjectsTwoKeyFiltersDoNotPushLimit(t *testing.T) {
	var gotMaxResults string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotMaxResults = r.URL.Query().Get("maxResults")
		_, _ = w.Write([]byte(`{"values":[],"isLast":true}`))
	})
	limit := 1
	_, err := collectScan(c, source.ScanRequest{
		Table: "projects",
		Filters: []source.Filter{
			inFilter("key", "AAA", "BBB"),
			eqFilter("key", "BBB"),
		},
		Limit: &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, "100", gotMaxResults) // full page: LIMIT was not pushed
}

// --- comments ---

func TestScanCommentsMissingIssueKeyErrorsWithoutRequest(t *testing.T) {
	called := false
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	_, err := collectScan(c, source.ScanRequest{Table: "comments"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue_key")
	assert.False(t, called, "no HTTP request should be made when issue_key is missing")
}

func TestScanCommentsHappyPathAndCasing(t *testing.T) {
	calls := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/rest/api/3/issue/proj-1/comment", r.URL.Path)
		if calls == 1 {
			assert.Equal(t, "0", r.URL.Query().Get("startAt"))
			_, _ = w.Write([]byte(`{"comments":[
				{"id":"1","author":{"accountId":"a1","displayName":"Alice"},
				 "body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"hello"}]}]},
				 "created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-01T00:00:00.000+0000","jsdPublic":true}
			],"startAt":0,"maxResults":1,"total":2}`))
			return
		}
		assert.Equal(t, "1", r.URL.Query().Get("startAt"))
		_, _ = w.Write([]byte(`{"comments":[
			{"id":"2","author":null,"body":null,"created":"2024-01-02T00:00:00.000+0000","updated":"2024-01-02T00:00:00.000+0000","jsdPublic":null}
		],"startAt":1,"maxResults":1,"total":2}`))
	})

	req := source.ScanRequest{
		Table:   "comments",
		Filters: []source.Filter{eqFilter("issue_key", "proj-1")}, // caller's casing
	}
	rows, err := collectScan(c, req)
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, colNames(commentsCols), rows.Columns)

	row0 := rows.Rows[0]
	assert.Equal(t, "proj-1", row0[0]) // stored with the caller's casing
	assert.Equal(t, int64(1), row0[1])
	assert.Equal(t, "a1", row0[2])
	assert.Equal(t, "Alice", row0[3])
	assert.Equal(t, "hello", row0[4])
	assert.Equal(t, true, row0[7])

	row1 := rows.Rows[1]
	assert.Nil(t, row1[2]) // author_account_id
	assert.Nil(t, row1[4]) // body
	assert.Nil(t, row1[7]) // is_public
}

// Fix: a zero-row page with total still ahead (comments hidden by visibility
// restrictions, stale total) must terminate the scan — with a pushed LIMIT the
// page cap is disabled, so without the guard the identical request would
// repeat forever.
func TestScanCommentsEmptyPageWithStaleTotalTerminates(t *testing.T) {
	calls := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"comments":[
				{"id":"1","author":null,"body":null,"created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-01T00:00:00.000+0000"}
			],"startAt":0,"maxResults":100,"total":8}`))
			return
		}
		// total says more, but nothing visible comes back.
		_, _ = w.Write([]byte(`{"comments":[],"startAt":1,"maxResults":100,"total":8}`))
	})

	limit := 5 // pushed (single key, no other filters) -> page cap disabled
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "comments",
		Filters: []source.Filter{eqFilter("issue_key", "P-1")},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Len(t, rows.Rows, 1)
}

func TestScanCommentsMultipleIssueKeysDoNotPushLimit(t *testing.T) {
	seen := map[string]bool{}
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = true
		_, _ = w.Write([]byte(`{"comments":[],"startAt":0,"maxResults":0,"total":0}`))
	})
	limit := 1
	_, err := collectScan(c, source.ScanRequest{
		Table:   "comments",
		Filters: []source.Filter{inFilter("issue_key", "A-1", "A-2")},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.True(t, seen["/rest/api/3/issue/A-1/comment"])
	assert.True(t, seen["/rest/api/3/issue/A-2/comment"])
}
