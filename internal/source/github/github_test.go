package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intPtr(n int) *int { return &n }

func newTestConnector(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL, "token_command": []any{}})
	require.NoError(t, err)
	return c.(*Connector)
}

func eqFilter(col, val string) source.Filter {
	return source.Filter{Column: col, Op: source.OpEq, Value: val}
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

// scanChunkSizes runs Scan and records the row count of each data chunk, so
// streaming tests can assert one chunk per page (warning-only chunks are ignored).
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

// tenIssues is a one-page response of 10 plain issues numbered 1..10.
const tenIssues = `[
	{"number":1,"state":"open"},{"number":2,"state":"open"},{"number":3,"state":"open"},
	{"number":4,"state":"open"},{"number":5,"state":"open"},{"number":6,"state":"open"},
	{"number":7,"state":"open"},{"number":8,"state":"open"},{"number":9,"state":"open"},
	{"number":10,"state":"open"}]`

// When an uncapped scan hits the maxPages cap with more pages available, the
// connector emits a truncation warning; a single-page scan does not.
func TestScanIssuesPageCapWarning(t *testing.T) {
	// Handler always advertises a next page, so the scan never exhausts.
	capped := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<http://`+r.Host+r.URL.Path+`?page=next>; rel="next"`)
		_, _ = w.Write([]byte(tenIssues))
	})
	req := source.ScanRequest{Table: "issues", Filters: []source.Filter{eqFilter("owner", "golang"), eqFilter("repo", "go")}}
	res, err := collectScan(capped, req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "github.issues")
	assert.Contains(t, res.Warnings[0], "cap")

	// A single page (no next link) exhausts cleanly: no warning.
	single := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tenIssues))
	})
	res, err = collectScan(single, req)
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
}

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
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
		OrderBy: []source.OrderTerm{{Column: "updated_at", Desc: true}},
		Limit:   &limit,
		Offset:  &offset,
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "per_page=100") // full page even with a pushed LIMIT
	assert.Len(t, rows.Rows, 5)                  // limit + offset, not just limit
}

// A pushed LIMIT on a PR-heavy repo may never be satisfied: the issues endpoint
// interleaves PRs, which are dropped client-side and don't count toward the
// LIMIT. The scan must stop after maxPages consecutive issue-free pages with a
// truncation warning — not walk the repo's whole history in limit-sized pages.
func TestScanIssuesPROnlyRepoBoundsPaging(t *testing.T) {
	calls := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 { // follow-up requests use the Link URL, which this test builds bare
			assert.Equal(t, "100", r.URL.Query().Get("per_page")) // never limit-sized
		}
		// Every item is a PR; always advertise a next page.
		w.Header().Set("Link", `<http://`+r.Host+r.URL.Path+`?page=next>; rel="next"`)
		_, _ = w.Write([]byte(`[{"number":1,"state":"open","pull_request":{"url":"http://gh/pr/1"}}]`))
	})

	limit := 2
	res, err := collectScan(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Empty(t, res.Rows)
	assert.Equal(t, maxPages, calls) // bounded, not the repo's entire history
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "github.issues")
	assert.Contains(t, res.Warnings[0], "pull requests")
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
	rows, err := collectScan(c, source.ScanRequest{
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

	rows, err := collectScan(c, source.ScanRequest{
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
	assert.ElementsMatch(t, []string{"issues", "pulls", "repos", "commits", "releases", "workflow_runs", "artifacts"}, names)
}

func TestNewRejectsInvalidTokenCommand(t *testing.T) {
	_, err := New(map[string]any{"token_command": strings.Join([]string{"gh", "auth"}, " ")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_command must be a list of strings")

	_, err = New(map[string]any{"token_command": []any{"gh", 1}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_command[1] must be a string")
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
	rows, err := collectScan(c, req)
	require.NoError(t, err)

	// Pushdown reflected in the outbound request.
	assert.Equal(t, "/repos/golang/go/issues", gotPath)
	assert.Contains(t, gotQuery, "state=open")
	assert.Contains(t, gotQuery, "sort=updated")
	assert.Contains(t, gotQuery, "direction=desc")
	// issues always fetches full pages: a limit-sized page could be all PRs.
	assert.Contains(t, gotQuery, "per_page=100")

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

func TestScanDefaultsToAllStates(t *testing.T) {
	// The GitHub API defaults issues/pulls to state=open; an unfiltered scan
	// must request state=all so the connector returns a superset.
	for _, table := range []string{"issues", "pulls"} {
		t.Run(table, func(t *testing.T) {
			var gotQuery url.Values
			c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				_, _ = w.Write([]byte(`[]`))
			})

			_, err := collectScan(c, source.ScanRequest{
				Table:   table,
				Filters: []source.Filter{eqFilter("owner", "golang"), eqFilter("repo", "go")},
			})
			require.NoError(t, err)
			assert.Equal(t, "all", gotQuery.Get("state"))
		})
	}
}

func TestScanIssuesRequiresOwnerRepo(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("should not call the API without owner/repo")
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := collectScan(c, source.ScanRequest{
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
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, int64(1), rows.Rows[0][2])
	assert.Equal(t, int64(2), rows.Rows[1][2])
}

// Each API page is emitted as its own chunk (streamed), not buffered into one.
func TestScanIssuesStreamsOneChunkPerPage(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"number":3,"state":"open"},{"number":4,"state":"open"}]`))
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"number":1,"state":"open"},{"number":2,"state":"open"}]`))
	})

	sizes, err := scanChunkSizes(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 2}, sizes) // one chunk per page
}

// A pushed LIMIT stops paging at the cumulative count across pages — even when
// the API returns fewer rows per page than per_page — trimming the final chunk.
func TestScanIssuesLimitStopsAcrossPages(t *testing.T) {
	calls := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		n, _ := strconv.Atoi(page)
		// Always advertise a next page, so only the LIMIT can stop us.
		w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=`+strconv.Itoa(n+1)+`>; rel="next"`)
		_, _ = w.Write([]byte(`[{"number":1,"state":"open"},{"number":2,"state":"open"}]`))
	})

	limit := 3
	sizes, err := scanChunkSizes(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
		OrderBy: []source.OrderTerm{{Column: "updated_at", Desc: true}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 1}, sizes) // page 2 trimmed to hit LIMIT 3 total
	assert.Equal(t, 2, calls)           // stopped — did not fetch page 3
}

// A pushed LIMIT larger than maxPages*perPage must keep paging past the
// maxPages safety cap (which only guards un-pushed scans) and deliver the full
// LIMIT, not silently stop at 1000 rows.
func TestScanIssuesLimitAboveMaxPages(t *testing.T) {
	var page strings.Builder
	page.WriteString("[")
	for i := 0; i < 100; i++ {
		if i > 0 {
			page.WriteString(",")
		}
		page.WriteString(`{"number":1,"state":"open"}`)
	}
	page.WriteString("]")
	body := page.String()

	calls := 0
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		p := r.URL.Query().Get("page")
		if p == "" {
			p = "1"
		}
		n, _ := strconv.Atoi(p)
		w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=`+strconv.Itoa(n+1)+`>; rel="next"`)
		_, _ = w.Write([]byte(body))
	})

	limit := 1500 // > maxPages(10) * perPage(100)
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
		OrderBy: []source.OrderTerm{{Column: "updated_at", Desc: true}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Len(t, rows.Rows, 1500) // full LIMIT delivered, not capped at 1000
	assert.Equal(t, 15, calls)     // paged past the maxPages=10 cap
}

func TestLimitNotPushedWhenOrderUnmapped(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[{"number":1,"title":"one","state":"open"}]`))
	})
	// ORDER BY on a non-sortable column → sort not mapped → per_page must NOT be the limit.
	_, err := collectScan(c, source.ScanRequest{
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
	rows, err := collectScan(c, source.ScanRequest{
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
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "pulls",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, int64(7), rows.Rows[0][2])
	assert.Equal(t, true, rows.Rows[0][6]) // draft
	assert.Nil(t, rows.Rows[0][9])         // merged_at null
}

func TestScanCommits(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{"sha":"abc","html_url":"http://gh/c/abc",
			 "commit":{"message":"fix bug","author":{"name":"Alice","email":"a@x.io","date":"2024-01-01T00:00:00Z"},
			           "committer":{"name":"Bob","email":"b@x.io","date":"2024-01-02T00:00:00Z"}},
			 "author":{"login":"alice"},"committer":{"login":"bob"}}
		]`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table: "commits",
		Filters: []source.Filter{
			eqFilter("owner", "golang"), eqFilter("repo", "go"),
			eqFilter("path", "src/x.go"), eqFilter("author_login", "alice"),
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "/repos/golang/go/commits", gotPath)
	assert.Contains(t, gotQuery, "path=src%2Fx.go")
	assert.Contains(t, gotQuery, "author=alice")

	assert.Equal(t, colNames(commitsCols), rows.Columns)
	require.Len(t, rows.Rows, 1)
	row := rows.Rows[0]
	assert.Equal(t, "golang", row[0])   // owner
	assert.Equal(t, "go", row[1])       // repo
	assert.Equal(t, "src/x.go", row[2]) // path (echoed from the filter)
	assert.Equal(t, "abc", row[3])      // sha
	assert.Equal(t, "fix bug", row[4])  // message
	assert.Equal(t, "alice", row[5])    // author_login
	assert.Equal(t, "Alice", row[6])    // author_name
	assert.Equal(t, "bob", row[9])      // committer_login
}

// path is filter-only on the API; when no path filter is given the synthetic
// column is NULL rather than erroring.
func TestScanCommitsPathNullWhenUnfiltered(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"sha":"abc","commit":{"message":"x"}}]`))
	})
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Nil(t, rows.Rows[0][2]) // path null when unfiltered
}

// sha is a start-ref, not an exact match, so its presence must NOT push LIMIT
// (the API returns the ref's ancestors too).
func TestScanCommitsShaDoesNotPushLimit(t *testing.T) {
	var gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[{"sha":"abc","commit":{"message":"x"}}]`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "commits",
		Filters: []source.Filter{
			eqFilter("owner", "o"), eqFilter("repo", "r"), eqFilter("sha", "main"),
		},
		Limit: intPtr(5),
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "sha=main")
	assert.Contains(t, gotQuery, "per_page=100") // LIMIT not pushed
}

func TestScanCommitsRequiresOwnerRepo(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("should not call the API without owner/repo")
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{eqFilter("owner", "o")}, // missing repo
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo")
}

func TestScanReleases(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/o/r/releases", r.URL.Path)
		_, _ = w.Write([]byte(`[
			{"tag_name":"v1.0.0","name":"First","draft":false,"prerelease":true,
			 "created_at":"2024-01-01T00:00:00Z","published_at":null,
			 "author":{"login":"alice"},"html_url":"http://gh/r/1","body":"notes"}
		]`))
	})
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "releases",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)
	assert.Equal(t, colNames(releasesCols), rows.Columns)
	require.Len(t, rows.Rows, 1)
	row := rows.Rows[0]
	assert.Equal(t, "v1.0.0", row[2]) // tag_name
	assert.Equal(t, true, row[5])     // prerelease
	assert.Nil(t, row[7])             // published_at null
	assert.Equal(t, "alice", row[8])  // author_login
}

func TestScanWorkflowRuns(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[
			{"id":42,"name":"CI","head_branch":"main","head_sha":"deadbeef","run_number":7,
			 "event":"push","status":"completed","conclusion":"success","workflow_id":99,
			 "actor":{"login":"alice"},"created_at":"2024-01-01T00:00:00Z",
			 "updated_at":"2024-01-01T00:05:00Z","run_started_at":"2024-01-01T00:01:00Z",
			 "html_url":"http://gh/run/42"}
		]}`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table: "workflow_runs",
		Filters: []source.Filter{
			eqFilter("owner", "o"), eqFilter("repo", "r"),
			eqFilter("head_branch", "main"), eqFilter("status", "completed"),
			eqFilter("event", "push"), eqFilter("actor_login", "alice"),
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "/repos/o/r/actions/runs", gotPath)
	assert.Contains(t, gotQuery, "branch=main")
	assert.Contains(t, gotQuery, "status=completed")
	assert.Contains(t, gotQuery, "event=push")
	assert.Contains(t, gotQuery, "actor=alice")

	assert.Equal(t, colNames(workflowRunsCols), rows.Columns)
	require.Len(t, rows.Rows, 1)
	row := rows.Rows[0]
	assert.Equal(t, int64(42), row[2])   // id
	assert.Equal(t, "main", row[4])      // head_branch
	assert.Equal(t, "completed", row[8]) // status
	assert.Equal(t, "success", row[9])   // conclusion
	assert.Equal(t, int64(99), row[10])  // workflow_id
	assert.Equal(t, "alice", row[11])    // actor_login
}

func TestScanWorkflowRunsNullConclusion(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"workflow_runs":[
			{"id":1,"status":"in_progress","conclusion":null}
		]}`))
	})
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "workflow_runs",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Nil(t, rows.Rows[0][9]) // conclusion null
}

func TestScanArtifacts(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"total_count":1,"artifacts":[
			{"id":11,"name":"dfetch_linux_amd64","size_in_bytes":556,"expired":false,
			 "created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:01:00Z",
			 "expires_at":null,"digest":"sha256:abc",
			 "archive_download_url":"http://gh/dl/11",
			 "workflow_run":{"id":42,"head_branch":"main","head_sha":"deadbeef"}}
		]}`))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table: "artifacts",
		Filters: []source.Filter{
			eqFilter("owner", "o"), eqFilter("repo", "r"), eqFilter("name", "dfetch_linux_amd64"),
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "/repos/o/r/actions/artifacts", gotPath)
	assert.Contains(t, gotQuery, "name=dfetch_linux_amd64")

	assert.Equal(t, colNames(artifactsCols), rows.Columns)
	require.Len(t, rows.Rows, 1)
	row := rows.Rows[0]
	assert.Equal(t, int64(11), row[2])   // id
	assert.Equal(t, int64(556), row[4])  // size_in_bytes
	assert.Equal(t, false, row[5])       // expired
	assert.Nil(t, row[8])                // expires_at null
	assert.Equal(t, int64(42), row[10])  // workflow_run_id hoisted
	assert.Equal(t, "main", row[11])     // head_branch hoisted
	assert.Equal(t, "deadbeef", row[12]) // head_sha hoisted
}

// A workflow_run_id filter selects one run's artifacts via the run-scoped endpoint.
func TestScanArtifactsByRun(t *testing.T) {
	var gotPath string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"artifacts":[{"id":1,"name":"a"}]}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "artifacts",
		Filters: []source.Filter{
			eqFilter("owner", "o"), eqFilter("repo", "r"),
			{Column: "workflow_run_id", Op: source.OpEq, Value: int64(42)},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "/repos/o/r/actions/runs/42/artifacts", gotPath)
}

func TestScanArtifactsRequiresOwnerRepo(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("should not call the API without owner/repo")
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "artifacts",
		Filters: []source.Filter{eqFilter("owner", "o")}, // missing repo
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo")
}

func TestAPIError(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "missing")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Not Found")
}

func TestUnknownTable(t *testing.T) {
	c, _ := New(map[string]any{"token_command": []any{}})
	_, err := collectScan(c, source.ScanRequest{Table: "nope"})
	assert.Error(t, err)
}

func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`
	assert.Equal(t, "https://api.github.com/x?page=2", nextLink(h))
	assert.Empty(t, nextLink(`<https://api.github.com/x?page=9>; rel="last"`))
	assert.Empty(t, nextLink(""))
}
