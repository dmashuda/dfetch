package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intPtr(n int) *int { return &n }

// newTestConnector points a Slack connector at an httptest server with no auth
// header configured (auth_header_command empty, $SLACK_TOKEN unset in tests).
func newTestConnector(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	t.Setenv("SLACK_TOKEN", "")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL, "auth_header_command": []any{}})
	require.NoError(t, err)
	return c.(*Connector)
}

func eqFilter(col, val string) source.Filter {
	return source.Filter{Column: col, Op: sqlparse.OpEq, Value: val}
}

// collectScan runs Scan and accumulates every emitted chunk into one Rows.
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

// scanChunkSizes records the row count of each non-empty data chunk.
func scanChunkSizes(c source.Connector, req source.ScanRequest) ([]int, error) {
	var sizes []int
	err := c.Scan(context.Background(), req, func(chunk *source.Rows) error {
		if len(chunk.Rows) > 0 {
			sizes = append(sizes, len(chunk.Rows))
		}
		return nil
	})
	return sizes, err
}

func TestTables(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	tables := c.Tables()
	names := make([]string, 0, len(tables))
	for _, ts := range tables {
		names = append(names, ts.Name)
		assert.NotEmpty(t, ts.Columns, "table %q has no columns", ts.Name)
	}
	assert.ElementsMatch(t, []string{"channels", "users", "messages", "search"}, names)
}

func TestScanChannels(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/conversations.list", r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true,"channels":[
			{"id":"C1","name":"general","is_private":false,"is_archived":false,"is_general":true,
			 "num_members":42,"creator":"U1","created":1600000000,
			 "topic":{"value":"company-wide"},"purpose":{"value":"announcements"}},
			{"id":"C2","name":"random","is_private":true,"is_archived":true,"num_members":7,
			 "creator":"U2","created":1600000001,"topic":{"value":""},"purpose":{"value":""}}
		]}`))
	})

	rows, err := collectScan(c, source.ScanRequest{Table: "channels"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, colNames(channelsCols), rows.Columns)

	// First row: types and column order.
	first := rows.Rows[0]
	assert.Equal(t, "C1", first[0])
	assert.Equal(t, "general", first[1])
	assert.Equal(t, false, first[2]) // is_private
	assert.Equal(t, true, first[4])  // is_general
	assert.Equal(t, int64(42), first[5])
	assert.Equal(t, "company-wide", first[6])
	assert.Equal(t, "U1", first[8])
	assert.Equal(t, int64(1600000000), first[9])
}

func TestScanChannelsExcludeArchivedPushDown(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"ok":true,"channels":[]}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "channels",
		Filters: []source.Filter{{Column: "is_archived", Op: sqlparse.OpEq, Value: int64(0)}},
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "exclude_archived=true")
}

func TestScanUsers(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/users.list", r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true,"members":[
			{"id":"U1","name":"alice","deleted":false,"is_bot":false,"is_admin":true,"tz":"America/New_York",
			 "profile":{"real_name":"Alice A","display_name":"alice","email":"alice@example.com","title":"Eng"}},
			{"id":"U2","name":"bot","is_bot":true,"profile":{}}
		]}`))
	})

	rows, err := collectScan(c, source.ScanRequest{Table: "users"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, colNames(usersCols), rows.Columns)

	alice := rows.Rows[0]
	assert.Equal(t, "U1", alice[0])
	assert.Equal(t, "alice", alice[1])
	assert.Equal(t, "Alice A", alice[2]) // real_name from profile
	assert.Equal(t, "alice@example.com", alice[4])
	assert.Equal(t, false, alice[5]) // is_bot
	assert.Equal(t, true, alice[6])  // is_admin
	assert.Equal(t, "Eng", alice[9])

	// Empty optional fields become NULL.
	bot := rows.Rows[1]
	assert.Equal(t, true, bot[5]) // is_bot
	assert.Nil(t, bot[2])         // real_name
	assert.Nil(t, bot[4])         // email
}

func TestScanMessagesRequiresChannel(t *testing.T) {
	called := false
	c := newTestConnector(t, func(http.ResponseWriter, *http.Request) { called = true })
	_, err := collectScan(c, source.ScanRequest{Table: "messages"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "channel")
	assert.False(t, called, "must not call the API without the required filter")
}

func TestScanMessages(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/conversations.history", r.URL.Path)
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"ok":true,"messages":[
			{"type":"message","ts":"1600000002.000200","user":"U1","text":"hi","thread_ts":"1600000002.000200",
			 "reply_count":3,"reactions":[{"name":"+1","count":2}],"edited":{"ts":"1600000003.000000"}},
			{"type":"message","subtype":"channel_join","ts":"1600000001.000100","text":"joined"}
		]}`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "messages",
		Filters: []source.Filter{eqFilter("channel", "C1")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Contains(t, gotQuery, "channel=C1")
	assert.Equal(t, colNames(messagesCols), rows.Columns)

	m := rows.Rows[0]
	assert.Equal(t, "C1", m[0]) // synthetic channel column
	assert.Equal(t, "1600000002.000200", m[1])
	assert.Equal(t, "U1", m[3])
	assert.Equal(t, int64(3), m[7])
	assert.Equal(t, `[{"name":"+1","count":2}]`, m[8]) // reactions JSON passthrough
	assert.Equal(t, "1600000003.000000", m[9])         // edited_ts

	// No edit, no user -> NULLs.
	j := rows.Rows[1]
	assert.Equal(t, "channel_join", j[5])
	assert.Nil(t, j[3]) // user
	assert.Nil(t, j[9]) // edited_ts
	assert.Nil(t, j[8]) // reactions
}

func TestScanMessagesTsRangePushDown(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "messages",
		Filters: []source.Filter{
			eqFilter("channel", "C1"),
			{Column: "ts", Op: sqlparse.OpBetween, Values: []any{"1600000000.0", "1600000100.0"}},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "oldest=1600000000.0")
	assert.Contains(t, gotQuery, "latest=1600000100.0")
}

func TestScanSearchRequiresQuery(t *testing.T) {
	called := false
	c := newTestConnector(t, func(http.ResponseWriter, *http.Request) { called = true })
	_, err := collectScan(c, source.ScanRequest{Table: "search"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query")
	assert.False(t, called)
}

func TestScanSearch(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/search.messages", r.URL.Path)
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"ok":true,"messages":{"matches":[
			{"type":"message","ts":"1600000002.000200","user":"U1","username":"alice","text":"deploy done",
			 "channel":{"id":"C1","name":"general"},"permalink":"https://x/p","score":0.87}
		],"paging":{"count":100,"total":1,"page":1,"pages":1}}}`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "search",
		Filters: []source.Filter{eqFilter("query", "deploy")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Contains(t, gotQuery, "query=deploy")
	assert.Equal(t, colNames(searchCols), rows.Columns)

	m := rows.Rows[0]
	assert.Equal(t, "deploy", m[0]) // echoed query column
	assert.Equal(t, "1600000002.000200", m[1])
	assert.Equal(t, "C1", m[2])
	assert.Equal(t, "general", m[3])
	assert.Equal(t, "U1", m[4])
	assert.Equal(t, "deploy done", m[6])
	assert.Equal(t, 0.87, m[8])
}

// Cursor pagination yields one chunk per page; the last page has an empty cursor.
func TestCursorPaginationChunks(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"ok":true,"members":[{"id":"U1"},{"id":"U2"}],
				"response_metadata":{"next_cursor":"page2"}}`))
		case "page2":
			_, _ = w.Write([]byte(`{"ok":true,"members":[{"id":"U3"}],"response_metadata":{"next_cursor":""}}`))
		default:
			t.Fatalf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	})

	sizes, err := scanChunkSizes(c, source.ScanRequest{Table: "users"})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 1}, sizes)
}

// Search page-based pagination walks page=1..pages.
func TestSearchPagination(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			_, _ = w.Write([]byte(`{"ok":true,"messages":{"matches":[{"ts":"1"},{"ts":"2"}],
				"paging":{"count":2,"total":3,"page":1,"pages":2}}}`))
		case "2":
			_, _ = w.Write([]byte(`{"ok":true,"messages":{"matches":[{"ts":"3"}],
				"paging":{"count":2,"total":3,"page":2,"pages":2}}}`))
		default:
			t.Fatalf("unexpected page %q", page)
		}
	})

	sizes, err := scanChunkSizes(c, source.ScanRequest{
		Table:   "search",
		Filters: []source.Filter{eqFilter("query", "x")},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 1}, sizes)
}

// ok:false surfaces the Slack error.
func TestScanErrorEnvelope(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	})
	_, err := collectScan(c, source.ScanRequest{Table: "channels"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_auth")
}

// search.messages with a bot token returns not_allowed_token_type; that error
// must be surfaced verbatim.
func TestSearchTokenTypeError(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_allowed_token_type"}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "search",
		Filters: []source.Filter{eqFilter("query", "x")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not_allowed_token_type")
}

// LIMIT is pushed for an unfiltered, unordered cursor scan: limit lands on the
// request and the scan stops once enough rows arrive.
func TestLimitPushDown(t *testing.T) {
	var gotLimit string
	pages := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		pages++
		// Always advertise another page; a pushed LIMIT must stop us anyway.
		_, _ = w.Write([]byte(`{"ok":true,"members":[{"id":"U1"},{"id":"U2"},{"id":"U3"}],
			"response_metadata":{"next_cursor":"more"}}`))
	})

	rows, err := collectScan(c, source.ScanRequest{Table: "users", Limit: intPtr(2)})
	require.NoError(t, err)
	assert.Equal(t, "2", gotLimit)
	assert.Len(t, rows.Rows, 2)
	assert.Equal(t, 1, pages, "a pushed LIMIT should fetch a single page")
}

// LIMIT must NOT be pushed when an ORDER BY is present (the cursor endpoints have
// no server-side sort), so the connector keeps paging and SQLite does the top-N.
func TestLimitNotPushedWithOrder(t *testing.T) {
	var gotLimit string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`{"ok":true,"members":[{"id":"U1"}],"response_metadata":{"next_cursor":""}}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "users",
		OrderBy: []source.OrderTerm{{Column: "name"}},
		Limit:   intPtr(2),
	})
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(defaultPageSize), gotLimit)
}

// An unbounded scan stops at the page cap and emits a truncation warning.
func TestPageCapWarning(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"members":[{"id":"U1"}],"response_metadata":{"next_cursor":"more"}}`))
	})
	res, err := collectScan(c, source.ScanRequest{Table: "users"})
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "slack.users")
	assert.Contains(t, res.Warnings[0], "cap")
	assert.Len(t, res.Rows, maxPages)
}

// auth_header_command output is sent verbatim as the Authorization header.
func TestAuthHeaderCommand(t *testing.T) {
	t.Setenv("SLACK_TOKEN", "")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true,"channels":[]}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(map[string]any{
		"base_url":            srv.URL,
		"auth_header_command": []any{"printf", "Bearer xoxb-from-cmd"},
	})
	require.NoError(t, err)
	_, err = collectScan(c, source.ScanRequest{Table: "channels"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer xoxb-from-cmd", gotAuth)
}

// $SLACK_TOKEN is sent as a Bearer header.
func TestSlackTokenEnv(t *testing.T) {
	t.Setenv("SLACK_TOKEN", "xoxb-env")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true,"channels":[]}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(map[string]any{"base_url": srv.URL})
	require.NoError(t, err)
	_, err = collectScan(c, source.ScanRequest{Table: "channels"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer xoxb-env", gotAuth)
}

// A failing auth_header_command surfaces its error and aborts the scan.
func TestAuthHeaderCommandError(t *testing.T) {
	t.Setenv("SLACK_TOKEN", "")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)

	c, err := New(map[string]any{
		"base_url":            srv.URL,
		"auth_header_command": []any{"false"},
	})
	require.NoError(t, err)
	_, err = collectScan(c, source.ScanRequest{Table: "channels"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_header_command")
	assert.False(t, called, "scan must not hit the API when auth fails")
}

// messages LIMIT is pushed when the only filters are channel equality + an
// inclusive ts bound and the order is ts DESC (matching the API's newest-first
// order).
func TestMessagesLimitPushedWithTsDesc(t *testing.T) {
	var gotLimit string
	pages := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		pages++
		_, _ = w.Write([]byte(`{"ok":true,"messages":[
			{"ts":"3"},{"ts":"2"},{"ts":"1"}],"response_metadata":{"next_cursor":"more"}}`))
	})
	rows, err := collectScan(c, source.ScanRequest{
		Table: "messages",
		Filters: []source.Filter{
			eqFilter("channel", "C1"),
			{Column: "ts", Op: sqlparse.OpGte, Value: int64(0)},
		},
		OrderBy: []source.OrderTerm{{Column: "ts", Desc: true}},
		Limit:   intPtr(2),
	})
	require.NoError(t, err)
	assert.Equal(t, "2", gotLimit)
	assert.Len(t, rows.Rows, 2)
	assert.Equal(t, 1, pages)
	// An int ts bound is rendered as a plain integer string.
	assert.Equal(t, "0", asString(int64(0)))
	assert.Equal(t, "1.5", asString(float64(1.5)))
	assert.Equal(t, "", asString(true))
}

// Both ends of a ts window (ts > A AND ts < B) must be pushed as oldest+latest;
// req.Filter would return only the first, silently dropping one bound.
func TestMessagesTsBothBoundsPushed(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "messages",
		Filters: []source.Filter{
			eqFilter("channel", "C1"),
			{Column: "ts", Op: sqlparse.OpGt, Value: "100.0"},
			{Column: "ts", Op: sqlparse.OpLt, Value: "200.0"},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "oldest=100.0")
	assert.Contains(t, gotQuery, "latest=200.0")
	assert.Contains(t, gotQuery, "inclusive=true")
}

// messages LIMIT is NOT pushed when an unsupported filter is present, when the
// order is ts ASC (opposite the API's order), or when a ts bound is exclusive
// (>/<), since the inclusive API window could return a boundary row SQLite drops.
func TestMessagesLimitNotPushed(t *testing.T) {
	cases := []source.ScanRequest{
		{ // ts ASC can't ride the API's newest-first order
			Table:   "messages",
			Filters: []source.Filter{eqFilter("channel", "C1")},
			OrderBy: []source.OrderTerm{{Column: "ts", Desc: false}},
			Limit:   intPtr(2),
		},
		{ // a non-pushable filter (user) means the API result isn't the kept set
			Table:   "messages",
			Filters: []source.Filter{eqFilter("channel", "C1"), eqFilter("user", "U1")},
			Limit:   intPtr(2),
		},
		{ // an exclusive ts bound can undercount a pushed LIMIT
			Table: "messages",
			Filters: []source.Filter{
				eqFilter("channel", "C1"),
				{Column: "ts", Op: sqlparse.OpGt, Value: int64(0)},
			},
			Limit: intPtr(2),
		},
	}
	for _, req := range cases {
		var gotLimit string
		c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
			gotLimit = r.URL.Query().Get("limit")
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"1"}],"response_metadata":{"next_cursor":""}}`))
		})
		_, err := collectScan(c, req)
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(defaultPageSize), gotLimit)
	}
}

func TestUnknownTable(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	err = c.Scan(context.Background(), source.ScanRequest{Table: "nope"}, func(*source.Rows) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown table")
}

func TestNewRejectsBadAuthCommand(t *testing.T) {
	_, err := New(map[string]any{"auth_header_command": "not-a-list"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_header_command")
	assert.True(t, strings.Contains(err.Error(), "list"))
}
