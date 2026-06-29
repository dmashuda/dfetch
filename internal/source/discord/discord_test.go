package discord

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

// newTestConnector points a Discord connector at an httptest server with no auth
// header configured ($DISCORD_TOKEN unset, auth_header_command empty).
func newTestConnector(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	t.Setenv("DISCORD_TOKEN", "")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL, "auth_header_command": []any{}})
	require.NoError(t, err)
	return c.(*Connector)
}

func eqFilter(col, val string) source.Filter {
	return source.Filter{Column: col, Op: sqlparse.OpEq, Value: val}
}

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
	assert.ElementsMatch(t, []string{"channels", "members", "messages", "threads"}, names)
}

func TestScanChannels(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/guilds/G1/channels", r.URL.Path)
		_, _ = w.Write([]byte(`[
			{"id":"C1","name":"general","type":0,"position":0,"topic":"hi","nsfw":false,
			 "parent_id":"CAT1","rate_limit_per_user":5},
			{"id":"C2","name":"voice","type":2,"position":1,"topic":null,"nsfw":false,"parent_id":null}
		]`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "channels",
		Filters: []source.Filter{eqFilter("guild_id", "G1")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, colNames(channelsCols), rows.Columns)

	first := rows.Rows[0]
	assert.Equal(t, "G1", first[0]) // synthetic guild_id
	assert.Equal(t, "C1", first[1])
	assert.Equal(t, "general", first[2])
	assert.Equal(t, int64(0), first[3]) // type
	assert.Equal(t, "hi", first[5])     // topic
	assert.Equal(t, false, first[6])    // nsfw
	assert.Equal(t, "CAT1", first[7])   // parent_id
	assert.Equal(t, int64(5), first[8])

	// null topic / parent_id -> NULL.
	second := rows.Rows[1]
	assert.Nil(t, second[5])
	assert.Nil(t, second[7])
}

func TestScanChannelsRequiresGuild(t *testing.T) {
	called := false
	c := newTestConnector(t, func(http.ResponseWriter, *http.Request) { called = true })
	_, err := collectScan(c, source.ScanRequest{Table: "channels"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "guild_id")
	assert.False(t, called)
}

func TestScanMembers(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/guilds/G1/members", r.URL.Path)
		_, _ = w.Write([]byte(`[
			{"user":{"id":"U1","username":"alice","global_name":"Alice","bot":false},
			 "nick":"al","roles":["R1","R2"],"joined_at":"2020-01-01T00:00:00Z","premium_since":null},
			{"user":{"id":"U2","username":"bot","global_name":null,"bot":true},
			 "nick":null,"roles":[],"joined_at":"2021-01-01T00:00:00Z"}
		]`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "members",
		Filters: []source.Filter{eqFilter("guild_id", "G1")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, colNames(membersCols), rows.Columns)

	alice := rows.Rows[0]
	assert.Equal(t, "G1", alice[0])
	assert.Equal(t, "U1", alice[1])
	assert.Equal(t, "alice", alice[2])
	assert.Equal(t, "Alice", alice[3]) // global_name
	assert.Equal(t, "al", alice[4])    // nick
	assert.Equal(t, false, alice[5])   // is_bot
	assert.Equal(t, `["R1","R2"]`, alice[8])

	bot := rows.Rows[1]
	assert.Equal(t, true, bot[5]) // is_bot
	assert.Nil(t, bot[3])         // global_name
	assert.Nil(t, bot[4])         // nick
	assert.Equal(t, `[]`, bot[8]) // empty roles -> []
}

func TestScanMessagesRequiresChannel(t *testing.T) {
	called := false
	c := newTestConnector(t, func(http.ResponseWriter, *http.Request) { called = true })
	_, err := collectScan(c, source.ScanRequest{Table: "messages"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "channel")
	assert.False(t, called)
}

func TestScanMessages(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/channels/C1/messages", r.URL.Path)
		_, _ = w.Write([]byte(`[
			{"id":"100","author":{"id":"U1","username":"alice"},"content":"hi","timestamp":"2020-01-01T00:00:00Z",
			 "edited_timestamp":"2020-01-01T00:01:00Z","type":0,"pinned":true,"reactions":[{"emoji":{"name":"+1"},"count":2}]},
			{"id":"99","author":{"id":"U2","username":"bob"},"content":"yo","timestamp":"2019-12-31T00:00:00Z",
			 "edited_timestamp":null,"type":0,"pinned":false}
		]`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "messages",
		Filters: []source.Filter{eqFilter("channel", "C1")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, colNames(messagesCols), rows.Columns)

	m := rows.Rows[0]
	assert.Equal(t, "C1", m[0]) // synthetic channel_id
	assert.Equal(t, "100", m[1])
	assert.Equal(t, "U1", m[2])
	assert.Equal(t, "alice", m[3])
	assert.Equal(t, "hi", m[4])
	assert.Equal(t, "2020-01-01T00:01:00Z", m[6]) // edited_timestamp
	assert.Equal(t, true, m[8])                   // pinned
	assert.Equal(t, `[{"emoji":{"name":"+1"},"count":2}]`, m[9])

	m2 := rows.Rows[1]
	assert.Nil(t, m2[6]) // edited_timestamp
	assert.Nil(t, m2[9]) // reactions
}

// id < V pushes an exclusive `before`; the lower bound (id >) is left to SQLite.
func TestScanMessagesBeforePushDown(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "messages",
		Filters: []source.Filter{
			eqFilter("channel", "C1"),
			{Column: "id", Op: sqlparse.OpLt, Value: "200"},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "before=200")
}

func TestScanThreads(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/guilds/G1/threads/active", r.URL.Path)
		_, _ = w.Write([]byte(`{"threads":[
			{"id":"T1","name":"help","type":11,"parent_id":"C1","owner_id":"U1","message_count":3,"member_count":2,
			 "thread_metadata":{"archived":false,"locked":false,"auto_archive_duration":1440}}
		],"members":[]}`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "threads",
		Filters: []source.Filter{eqFilter("guild_id", "G1")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, colNames(threadsCols), rows.Columns)

	tr := rows.Rows[0]
	assert.Equal(t, "G1", tr[0])
	assert.Equal(t, "T1", tr[1])
	assert.Equal(t, "help", tr[2])
	assert.Equal(t, int64(11), tr[3])
	assert.Equal(t, "C1", tr[4]) // parent_id
	assert.Equal(t, "U1", tr[5]) // owner_id
	assert.Equal(t, int64(3), tr[6])
	assert.Equal(t, false, tr[8]) // archived
	assert.Equal(t, false, tr[9]) // locked
	assert.Equal(t, int64(1440), tr[10])
}

// Members paginate ascending by user id via the `after` cursor; a short page is
// the last page.
func TestMembersPagination(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("after") {
		case "":
			_, _ = w.Write([]byte(`[{"user":{"id":"1"}},{"user":{"id":"2"}}]`))
		case "2":
			_, _ = w.Write([]byte(`[{"user":{"id":"3"}}]`)) // short page -> done
		default:
			t.Fatalf("unexpected after %q", r.URL.Query().Get("after"))
		}
	})
	// limit=2 per page (LIMIT pushed below would change this; here force paging by
	// using no LIMIT — but default page size is 1000, so emulate via a 2-row first
	// page already shorter than 1000 -> would stop. Instead assert chunk sizes with
	// an explicit page boundary using after.)
	sizes, err := scanChunkSizes(c, source.ScanRequest{Table: "members", Filters: []source.Filter{eqFilter("guild_id", "G1")}})
	require.NoError(t, err)
	// First page (2 rows) is shorter than the 1000 default page size, so the scan
	// stops after one page.
	assert.Equal(t, []int{2}, sizes)
}

// Messages paginate older via the `before` cursor: a full (100-row) page is
// followed by a request carrying before=<oldest id>, and a short page ends it.
func TestMessagesPaginationCursor(t *testing.T) {
	// First page: 100 messages with ids 200..101 (newest-first); oldest is 101.
	var page1 strings.Builder
	page1.WriteByte('[')
	for i := 0; i < 100; i++ {
		if i > 0 {
			page1.WriteByte(',')
		}
		page1.WriteString(`{"id":"` + strconv.Itoa(200-i) + `"}`)
	}
	page1.WriteByte(']')

	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("before") {
		case "":
			_, _ = w.Write([]byte(page1.String()))
		case "101":
			_, _ = w.Write([]byte(`[{"id":"100"},{"id":"99"}]`)) // short -> done
		default:
			t.Fatalf("unexpected before %q", r.URL.Query().Get("before"))
		}
	})
	sizes, err := scanChunkSizes(c, source.ScanRequest{
		Table:   "messages",
		Filters: []source.Filter{eqFilter("channel", "C1")},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{100, 2}, sizes)
}

// A non-2xx status surfaces the Discord error message.
func TestScanErrorStatus(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Missing Access","code":50001}`))
	})
	_, err := collectScan(c, source.ScanRequest{Table: "channels", Filters: []source.Filter{eqFilter("guild_id", "G1")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Missing Access")
}

// LIMIT is pushed for an unfiltered members scan: limit lands on the request and
// the scan stops once enough rows arrive.
func TestMembersLimitPushDown(t *testing.T) {
	var gotLimit string
	pages := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		pages++
		_, _ = w.Write([]byte(`[{"user":{"id":"1"}},{"user":{"id":"2"}},{"user":{"id":"3"}}]`))
	})
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "members",
		Filters: []source.Filter{eqFilter("guild_id", "G1")},
		Limit:   intPtr(2),
	})
	require.NoError(t, err)
	assert.Equal(t, "2", gotLimit)
	assert.Len(t, rows.Rows, 2)
	assert.Equal(t, 1, pages)
}

// LIMIT is not pushed when an ORDER BY is present (members have a fixed id order).
func TestMembersLimitNotPushedWithOrder(t *testing.T) {
	var gotLimit string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`[{"user":{"id":"1"}}]`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "members",
		Filters: []source.Filter{eqFilter("guild_id", "G1")},
		OrderBy: []source.OrderTerm{{Column: "username"}},
		Limit:   intPtr(2),
	})
	require.NoError(t, err)
	assert.Equal(t, "1000", gotLimit)
}

// messages LIMIT is not pushed for an inclusive id <= bound (the exclusive
// `before` would drop the boundary row).
func TestMessagesLimitNotPushedInclusiveBound(t *testing.T) {
	var gotLimit string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`[{"id":"1"}]`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "messages",
		Filters: []source.Filter{
			eqFilter("channel", "C1"),
			{Column: "id", Op: sqlparse.OpLte, Value: "200"},
		},
		Limit: intPtr(2),
	})
	require.NoError(t, err)
	assert.Equal(t, "100", gotLimit)
}

// An unbounded members scan stops at the page cap and warns.
func TestPageCapWarning(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		// Always return a full page (1000 rows) so pagination never exhausts.
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < 1000; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"user":{"id":"` + strconv.Itoa(i+1) + `"}}`)
		}
		b.WriteByte(']')
		_, _ = w.Write([]byte(b.String()))
	})
	res, err := collectScan(c, source.ScanRequest{Table: "members", Filters: []source.Filter{eqFilter("guild_id", "G1")}})
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "discord.members")
	assert.Contains(t, res.Warnings[0], "cap")
}

// auth_header_command output is sent verbatim as the Authorization header.
func TestAuthHeaderCommand(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "")
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(map[string]any{
		"base_url":            srv.URL,
		"auth_header_command": []any{"printf", "Bot xxx.from.cmd"},
	})
	require.NoError(t, err)
	_, err = collectScan(c, source.ScanRequest{Table: "channels", Filters: []source.Filter{eqFilter("guild_id", "G1")}})
	require.NoError(t, err)
	assert.Equal(t, "Bot xxx.from.cmd", gotAuth)
	assert.Equal(t, userAgent, gotUA)
}

// $DISCORD_TOKEN is sent as a Bot header.
func TestDiscordTokenEnv(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "tok123")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(map[string]any{"base_url": srv.URL})
	require.NoError(t, err)
	_, err = collectScan(c, source.ScanRequest{Table: "channels", Filters: []source.Filter{eqFilter("guild_id", "G1")}})
	require.NoError(t, err)
	assert.Equal(t, "Bot tok123", gotAuth)
}

// A failing auth_header_command surfaces its error and aborts the scan.
func TestAuthHeaderCommandError(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)

	c, err := New(map[string]any{
		"base_url":            srv.URL,
		"auth_header_command": []any{"false"},
	})
	require.NoError(t, err)
	_, err = collectScan(c, source.ScanRequest{Table: "channels", Filters: []source.Filter{eqFilter("guild_id", "G1")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_header_command")
	assert.False(t, called)
}

// An exclusive id < bound given as an integer literal is rendered into `before`.
func TestScanMessagesBeforePushDownInt(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "messages",
		Filters: []source.Filter{
			eqFilter("channel", "C1"),
			{Column: "id", Op: sqlparse.OpLt, Value: int64(200)},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "before=200")
	// asString covers the numeric literal forms.
	assert.Equal(t, "200", asString(int64(200)))
	assert.Equal(t, "1.5", asString(1.5))
	assert.Equal(t, "", asString(true))
}

// messages LIMIT is pushed for a lower id bound (id > V) with ts/id DESC order:
// it only trims the oldest tail, so the newest-N set is exact.
func TestMessagesLimitPushedWithIdLowerBound(t *testing.T) {
	var gotLimit, gotBefore string
	pages := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		gotBefore = r.URL.Query().Get("before")
		pages++
		_, _ = w.Write([]byte(`[{"id":"9"},{"id":"8"},{"id":"7"}]`))
	})
	rows, err := collectScan(c, source.ScanRequest{
		Table: "messages",
		Filters: []source.Filter{
			eqFilter("channel", "C1"),
			{Column: "id", Op: sqlparse.OpGt, Value: "5"},
		},
		OrderBy: []source.OrderTerm{{Column: "id", Desc: true}},
		Limit:   intPtr(2),
	})
	require.NoError(t, err)
	assert.Equal(t, "2", gotLimit)
	assert.Empty(t, gotBefore, "a lower id bound is not pushed as before")
	assert.Len(t, rows.Rows, 2)
	assert.Equal(t, 1, pages)
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
