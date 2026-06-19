package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intPtr(n int) *int { return &n }

func newTestConnector(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL})
	require.NoError(t, err)
	return c.(*Connector)
}

func eqFilter(col, val string) source.Filter {
	return source.Filter{Column: col, Op: sqlparse.OpEq, Value: val}
}

// tenIssues is a one-page response of 10 plain issues numbered 1..10.
const tenIssues = `[
	{"number":1,"state":"open"},{"number":2,"state":"open"},{"number":3,"state":"open"},
	{"number":4,"state":"open"},{"number":5,"state":"open"},{"number":6,"state":"open"},
	{"number":7,"state":"open"},{"number":8,"state":"open"},{"number":9,"state":"open"},
	{"number":10,"state":"open"}]`

// Fix: LIMIT push-down must account for OFFSET. With LIMIT 2 OFFSET 3 the
// connector must fetch limit+offset=5 rows so SQLite can apply the OFFSET; if it
// stops at 2, SQLite's OFFSET 3 over 2 rows returns nothing.
func TestScanIssuesLimitWithOffsetFetchesEnough(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(tenIssues))
	})

	limit, offset := 2, 3
	rows, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
		OrderBy: []source.OrderTerm{{Column: "updated_at", Desc: true}},
		Limit:   &limit,
		Offset:  &offset,
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "per_page=5")
	assert.Len(t, rows.Rows, 5) // limit + offset, not just limit
}

// Fix: a multi-key ORDER BY cannot be honored by the API (one sort field), so
// LIMIT must not be pushed — otherwise the API's single-key top-N truncates rows
// the composite order would keep.
func TestScanIssuesMultiTermOrderDoesNotPushLimit(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(tenIssues))
	})

	limit := 2
	rows, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
		OrderBy: []source.OrderTerm{{Column: "updated_at", Desc: true}, {Column: "number"}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "per_page=100")
	assert.Len(t, rows.Rows, 10) // not truncated to the limit
}

// Fix: repos must store the user-supplied owner (not the API's canonical casing)
// so the engine's verbatim WHERE owner = '<as written>' still matches the row.
func TestScanReposPreservesOwnerCasing(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"go","full_name":"golang/go","owner":{"login":"golang"}}`))
	})

	rows, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "repos",
		Filters: []source.Filter{eqFilter("owner", "Golang"), eqFilter("name", "go")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, "Golang", rows.Rows[0][0]) // user's casing preserved
}

func TestTables(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	tables := c.Tables()
	names := make([]string, 0, len(tables))
	for _, tbl := range tables {
		names = append(names, tbl.Name)
		assert.NotEmpty(t, tbl.Columns)
	}
	assert.ElementsMatch(t, []string{"issues", "pulls", "repos"}, names)
}

func TestScanIssuesPushdownAndPRFilter(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{"number":1,"title":"a bug","state":"open","user":{"login":"alice"},"comments":2,
			 "labels":[{"name":"bug"},{"name":"p1"}],"created_at":"2024-01-01","updated_at":"2024-02-01",
			 "closed_at":null,"body":"x","html_url":"http://gh/1"},
			{"number":2,"title":"a PR","state":"open","pull_request":{"url":"http://gh/pr/2"}}
		]`))
	})

	req := source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "golang"), eqFilter("repo", "go"), eqFilter("state", "open")},
		OrderBy: []source.OrderTerm{{Column: "updated_at", Desc: true}},
		Limit:   intPtr(10),
	}
	rows, err := c.Scan(context.Background(), req)
	require.NoError(t, err)

	// Pushdown reflected in the outbound request.
	assert.Equal(t, "/repos/golang/go/issues", gotPath)
	assert.Contains(t, gotQuery, "state=open")
	assert.Contains(t, gotQuery, "sort=updated")
	assert.Contains(t, gotQuery, "direction=desc")
	assert.Contains(t, gotQuery, "per_page=10")

	// PR excluded; owner/repo injected; columns in schema order.
	assert.Equal(t, colNames(issuesCols), rows.Columns)
	require.Len(t, rows.Rows, 1)
	row := rows.Rows[0]
	assert.Equal(t, "golang", row[0]) // owner
	assert.Equal(t, "go", row[1])     // repo
	assert.Equal(t, int64(1), row[2]) // number
	assert.Equal(t, "alice", row[5])  // user_login
	assert.Equal(t, "bug,p1", row[7]) // labels
	assert.Nil(t, row[10])            // closed_at null
}

func TestScanIssuesRequiresOwnerRepo(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("should not call the API without owner/repo")
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "golang")}, // missing repo
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo")
}

func TestScanIssuesPagination(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"number":2,"title":"two","state":"open"}]`))
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"number":1,"title":"one","state":"open"}]`))
	})
	// No LIMIT → both pages collected.
	rows, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, int64(1), rows.Rows[0][2])
	assert.Equal(t, int64(2), rows.Rows[1][2])
}

func TestLimitNotPushedWhenOrderUnmapped(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[{"number":1,"title":"one","state":"open"}]`))
	})
	// ORDER BY on a non-sortable column → sort not mapped → per_page must NOT be the limit.
	_, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
		OrderBy: []source.OrderTerm{{Column: "title", Desc: true}},
		Limit:   intPtr(3),
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "per_page=100")
	assert.NotContains(t, gotQuery, "sort=")
}

func TestScanReposSingle(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/golang/go", r.URL.Path)
		_, _ = w.Write([]byte(`{"name":"go","full_name":"golang/go","language":"Go",
			"stargazers_count":120000,"forks_count":17000,"open_issues_count":9000,
			"private":false,"owner":{"login":"golang"}}`))
	})
	rows, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "repos",
		Filters: []source.Filter{eqFilter("owner", "golang"), eqFilter("name", "go")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, "golang", rows.Rows[0][0]) // owner
	assert.Equal(t, "go", rows.Rows[0][1])     // name
	assert.Equal(t, int64(120000), rows.Rows[0][5])
}

func TestScanPulls(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/o/r/pulls", r.URL.Path)
		_, _ = w.Write([]byte(`[{"number":7,"title":"pr","state":"open","draft":true,"merged_at":null}]`))
	})
	rows, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "pulls",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, int64(7), rows.Rows[0][2])
	assert.Equal(t, true, rows.Rows[0][6]) // draft
	assert.Nil(t, rows.Rows[0][9])         // merged_at null
}

func TestAPIError(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	_, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "missing")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Not Found")
}

func TestUnknownTable(t *testing.T) {
	c, _ := New(nil)
	_, err := c.Scan(context.Background(), source.ScanRequest{Table: "nope"})
	assert.Error(t, err)
}

func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`
	assert.Equal(t, "https://api.github.com/x?page=2", nextLink(h))
	assert.Empty(t, nextLink(`<https://api.github.com/x?page=9>; rel="last"`))
	assert.Empty(t, nextLink(""))
}
