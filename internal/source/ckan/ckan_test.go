package ckan

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// collectScan runs Scan and accumulates every emitted chunk into one Rows,
// including any Warnings.
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

// scanChunkSizes runs Scan and records the row count of each data chunk
// (warning-only chunks carry no rows and are ignored here).
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

// searchResp renders a package_search envelope with the given count and packages.
func searchResp(count int, packagesJSON string) string {
	return `{"success":true,"result":{"count":` + itoa(count) + `,"results":` + packagesJSON + `}}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func TestTables(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	tables := c.Tables()
	names := make([]string, len(tables))
	for i, tbl := range tables {
		names[i] = tbl.Name
	}
	assert.Equal(t, []string{"datasets", "resources", "organizations", "groups"}, names)
	assert.Equal(t, colNames(datasetsCols), tables[0].ColumnNames())
	assert.Equal(t, colNames(resourcesCols), tables[1].ColumnNames())
	assert.Equal(t, colNames(orgGroupCols), tables[2].ColumnNames())
	assert.Equal(t, colNames(orgGroupCols), tables[3].ColumnNames())
}

func TestNewDefaults(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	assert.Equal(t, defaultBaseURL, c.(*Connector).baseURL)
}

func TestScanDatasetsMapsRows(t *testing.T) {
	const pkg = `[{
		"id":"abc","name":"air-quality","title":"Air Quality","notes":"hourly readings",
		"author":"EPA","maintainer":"ops","license_id":"cc-by","license_title":"CC-BY",
		"state":"active","private":false,"num_resources":3,"num_tags":2,
		"metadata_created":"2020-01-01T00:00:00","metadata_modified":"2021-02-03T00:00:00",
		"url":"http://example/air","organization":{"name":"epa","title":"EPA"},
		"tags":[{"name":"air"},{"name":"quality"}]
	}]`
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/3/action/package_search", r.URL.Path)
		_, _ = w.Write([]byte(searchResp(1, pkg)))
	})

	rows, err := collectScan(c, source.ScanRequest{Table: "datasets"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, colNames(datasetsCols), rows.Columns)

	row := rows.Rows[0]
	assert.Equal(t, "abc", row[0])          // id
	assert.Equal(t, "air-quality", row[1])  // name
	assert.Equal(t, false, row[9])          // private (bool)
	assert.Equal(t, "epa", row[10])         // organization
	assert.Equal(t, "EPA", row[11])         // organization_title
	assert.Equal(t, int64(3), row[12])      // num_resources
	assert.Equal(t, int64(2), row[13])      // num_tags
	assert.Equal(t, "air,quality", row[17]) // tags
	assert.Nil(t, row[18])                  // q (no search term)
}

func TestScanDatasetsNullOrganization(t *testing.T) {
	const pkg = `[{"id":"x","name":"n","organization":null,"tags":[]}]`
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(searchResp(1, pkg)))
	})
	rows, err := collectScan(c, source.ScanRequest{Table: "datasets"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Nil(t, rows.Rows[0][10])       // organization
	assert.Nil(t, rows.Rows[0][11])       // organization_title
	assert.Equal(t, "", rows.Rows[0][17]) // tags (empty join)
}

func TestScanDatasetsPushesFilters(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})

	_, err := collectScan(c, source.ScanRequest{
		Table: "datasets",
		Filters: []source.Filter{
			eqFilter("q", "climate"),
			eqFilter("organization", "epa"),
			{Column: "res_format", Op: sqlparse.OpIn, Values: []any{}}, // ignored: not pushable column
		},
	})
	// Only the pushable filters land; the unknown column is dropped here but
	// still re-applied by SQLite (the engine wouldn't have offered it).
	require.NoError(t, err)
	assert.Equal(t, "climate", gotQuery.Get("q"))
	assert.Equal(t, `organization:"epa"`, gotQuery.Get("fq"))
}

func TestScanDatasetsEchoesSearchTermIntoQColumn(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(searchResp(1, `[{"id":"x","name":"n"}]`)))
	})
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "datasets",
		Filters: []source.Filter{eqFilter("q", "climate")},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, "climate", rows.Rows[0][18]) // q column echoes the search term
}

func TestScanDatasetsRejectsNonEqualityQ(t *testing.T) {
	var called bool
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	// A non-equality operator on the virtual q column can't be honored (the
	// column has no stored value), so it must error rather than silently
	// returning zero rows.
	_, err := collectScan(c, source.ScanRequest{
		Table:   "datasets",
		Filters: []source.Filter{{Column: "q", Op: sqlparse.OpLike, Value: "%climate%"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "q column supports only equality")
	assert.False(t, called, "must not hit the API when the q filter is invalid")
}

func TestScanResourcesRejectsNonEqualityQ(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "resources",
		Filters: []source.Filter{{Column: "q", Op: sqlparse.OpIn, Values: []any{"a", "b"}}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "q column supports only equality")
}

func TestScanDatasetsPushesDateRange(t *testing.T) {
	var gotFq string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotFq = r.URL.Query().Get("fq")
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "datasets",
		Filters: []source.Filter{
			{Column: "metadata_modified", Op: sqlparse.OpGte, Value: "2020-01-01"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "metadata_modified:[2020-01-01T00:00:00Z TO *]", gotFq)
}

// A date range is pushed widened (inclusive whole-second bounds), so the API
// returns a superset: LIMIT must NOT ride along, or the truncation happens
// before SQLite drops the widened boundary rows.
func TestScanDatasetsDateRangeDoesNotPushLimit(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	limit := 5
	_, err := collectScan(c, source.ScanRequest{
		Table: "datasets",
		Filters: []source.Filter{
			{Column: "metadata_modified", Op: sqlparse.OpGte, Value: "2020-01-01"},
		},
		OrderBy: []source.OrderTerm{{Column: "metadata_modified", Desc: true}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, "metadata_modified:[2020-01-01T00:00:00Z TO *]", gotQuery.Get("fq"))
	assert.Equal(t, itoa(defaultRows), gotQuery.Get("rows")) // fq narrows, but LIMIT is NOT pushed
}

// tags:"x" matches datasets *containing* the tag while SQLite compares the
// comma-joined list verbatim — a superset, so the fq is pushed but LIMIT is not.
func TestScanDatasetsTagsFilterDoesNotPushLimit(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	limit := 5
	_, err := collectScan(c, source.ScanRequest{
		Table:   "datasets",
		Filters: []source.Filter{eqFilter("tags", "climate")},
		OrderBy: []source.OrderTerm{{Column: "metadata_modified", Desc: true}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, `tags:"climate"`, gotQuery.Get("fq"))
	assert.Equal(t, itoa(defaultRows), gotQuery.Get("rows"))
}

// Sub-second bounds must round outward: truncating an upper bound would exclude
// rows in the dropped fraction from the fetch entirely (wrong rows, not a
// superset). Lower bounds truncate down, upper bounds round up.
func TestScanDatasetsSubSecondBoundsWiden(t *testing.T) {
	var gotFq string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotFq = r.URL.Query().Get("fq")
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table: "datasets",
		Filters: []source.Filter{
			{Column: "metadata_modified", Op: sqlparse.OpBetween,
				Values: []any{"2020-01-01T10:00:00.4Z", "2020-06-01T10:00:05.7Z"}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "metadata_modified:[2020-01-01T10:00:00Z TO 2020-06-01T10:00:06Z]", gotFq)
}

func TestScanDatasetsPushesSortAndLimit(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(searchResp(100, `[{"id":"x","name":"n"}]`)))
	})
	limit := 5
	_, err := collectScan(c, source.ScanRequest{
		Table:   "datasets",
		OrderBy: []source.OrderTerm{{Column: "metadata_modified", Desc: true}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, "metadata_modified desc", gotQuery.Get("sort"))
	assert.Equal(t, "5", gotQuery.Get("rows")) // LIMIT pushed as rows
}

func TestScanDatasetsTitleSortRemapped(t *testing.T) {
	var gotSort string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotSort = r.URL.Query().Get("sort")
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "datasets",
		OrderBy: []source.OrderTerm{{Column: "title"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "title_string asc", gotSort) // title sorts on title_string
}

func TestScanDatasetsLimitWithOffsetFetchesEnough(t *testing.T) {
	var gotRows string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotRows = r.URL.Query().Get("rows")
		// 5 rows so the limit+offset window is satisfied in one page.
		_, _ = w.Write([]byte(searchResp(5, `[{"id":"1"},{"id":"2"},{"id":"3"},{"id":"4"},{"id":"5"}]`)))
	})
	limit, offset := 2, 3
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "datasets",
		OrderBy: []source.OrderTerm{{Column: "metadata_modified", Desc: true}},
		Limit:   &limit,
		Offset:  &offset,
	})
	require.NoError(t, err)
	assert.Equal(t, "5", gotRows) // limit+offset, so SQLite can apply OFFSET
	assert.Len(t, rows.Rows, 5)
}

func TestScanDatasetsUnsortableOrderDoesNotPushLimit(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	limit := 5
	_, err := collectScan(c, source.ScanRequest{
		Table:   "datasets",
		OrderBy: []source.OrderTerm{{Column: "title"}, {Column: "num_resources"}}, // num_resources not sortable
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Empty(t, gotQuery.Get("sort"))                    // ordering can't be honored
	assert.Equal(t, itoa(defaultRows), gotQuery.Get("rows")) // so LIMIT is NOT pushed
}

func TestScanDatasetsUnconsumedFilterDoesNotPushLimit(t *testing.T) {
	var gotRows string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotRows = r.URL.Query().Get("rows")
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	limit := 5
	// A LIKE on title is not pushable; SQLite must re-filter, so the API result
	// is a superset and LIMIT must not be pushed.
	_, err := collectScan(c, source.ScanRequest{
		Table:   "datasets",
		Filters: []source.Filter{{Column: "title", Op: sqlparse.OpLike, Value: "%air%"}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, itoa(defaultRows), gotRows)
}

func TestScanDatasetsPaginates(t *testing.T) {
	var starts []string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		starts = append(starts, r.URL.Query().Get("start"))
		start := r.URL.Query().Get("start")
		// count=150: page one (start=0) full, page two (start=100) has 50.
		if start == "0" {
			_, _ = w.Write([]byte(searchResp(150, page(100))))
		} else {
			_, _ = w.Write([]byte(searchResp(150, page(50))))
		}
	})
	sizes, err := scanChunkSizes(c, source.ScanRequest{Table: "datasets"})
	require.NoError(t, err)
	assert.Equal(t, []int{100, 50}, sizes) // one chunk per page
	assert.Equal(t, []string{"0", "100"}, starts)
}

func TestScanDatasetsMaxPagesCap(t *testing.T) {
	var calls int
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(searchResp(1_000_000, page(100)))) // always more available
	})
	res, err := collectScan(c, source.ScanRequest{Table: "datasets"})
	require.NoError(t, err)
	assert.Equal(t, maxPages, calls)      // unbounded scan capped
	assert.Len(t, res.Rows, maxPages*100) // maxPages full data pages
	require.NotEmpty(t, res.Warnings)     // truncation surfaced
	assert.Contains(t, res.Warnings[0], "cap")
	assert.Contains(t, res.Warnings[0], "datagov.datasets")
}

func TestScanDatasetsAPIError(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"error":{"__type":"Validation Error","message":"bad query"}}`))
	})
	_, err := collectScan(c, source.ScanRequest{Table: "datasets"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad query")
}

// The api.gsa.gov gateway serves CKAN under "/v3/action" and requires an api_key
// query parameter; both are reachable through config alone.
func TestGatewayActionPathAndAPIKey(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.URL.Query().Get("api_key")
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	}))
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL, "action_path": "/v3/action", "api_key": "DEMO_KEY"})
	require.NoError(t, err)

	_, err = collectScan(c, source.ScanRequest{Table: "datasets"})
	require.NoError(t, err)
	assert.Equal(t, "/v3/action/package_search", gotPath)
	assert.Equal(t, "DEMO_KEY", gotKey)
}

func TestScanResourcesFlattens(t *testing.T) {
	const pkgs = `[
		{"id":"p1","name":"air","resources":[
			{"id":"r1","package_id":"p1","name":"data.csv","format":"CSV","size":1024,"datastore_active":true,"last_modified":"2021-01-01T00:00:00"},
			{"id":"r2","package_id":"p1","name":"data.zip","format":"ZIP","size":null,"datastore_active":null,"last_modified":""}
		]},
		{"id":"p2","name":"water","resources":[
			{"id":"r3","package_id":"p2","name":"w.json","format":"JSON","size":"2048"}
		]}
	]`
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(searchResp(2, pkgs))) // count = 2 datasets (yielding 3 resources)
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "resources",
		Filters: []source.Filter{eqFilter("q", "test")},
	})
	require.NoError(t, err)
	assert.Equal(t, "test", gotQuery.Get("q")) // q pushed to package_search
	require.Len(t, rows.Rows, 3)               // flattened across both datasets

	r1 := rows.Rows[0]
	assert.Equal(t, "r1", r1[0])        // id
	assert.Equal(t, "p1", r1[1])        // package_id
	assert.Equal(t, "air", r1[2])       // package_name (from the dataset)
	assert.Equal(t, int64(1024), r1[8]) // size coerced to int64
	assert.Equal(t, true, r1[12])       // datastore_active
	assert.Equal(t, "test", r1[13])     // q echoed

	assert.Nil(t, rows.Rows[1][8])                // size null -> nil
	assert.Nil(t, rows.Rows[1][11])               // empty last_modified -> nil
	assert.Nil(t, rows.Rows[1][12])               // datastore_active null -> nil
	assert.Equal(t, int64(2048), rows.Rows[2][8]) // numeric string size -> int64
}

func TestScanResourcesNeverPushesLimit(t *testing.T) {
	var gotRows string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotRows = r.URL.Query().Get("rows")
		_, _ = w.Write([]byte(searchResp(0, `[]`)))
	})
	limit := 3
	_, err := collectScan(c, source.ScanRequest{Table: "resources", Limit: &limit})
	require.NoError(t, err)
	// A dataset-page LIMIT is not a resource LIMIT, so it is never pushed.
	assert.Equal(t, itoa(defaultRows), gotRows)
}

func TestScanOrganizations(t *testing.T) {
	const orgs = `{"success":true,"result":[
		{"id":"o1","name":"epa","title":"EPA","package_count":42,"state":"active","type":"organization","created":"2020-01-01T00:00:00"},
		{"id":"o2","name":"noaa","title":"NOAA","package_count":7}
	]}`
	var gotPath, gotAllFields string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAllFields = r.URL.Query().Get("all_fields")
		_, _ = w.Write([]byte(orgs))
	})
	rows, err := collectScan(c, source.ScanRequest{Table: "organizations"})
	require.NoError(t, err)
	assert.Equal(t, "/api/3/action/organization_list", gotPath)
	assert.Equal(t, "true", gotAllFields)
	assert.Equal(t, colNames(orgGroupCols), rows.Columns)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, "epa", rows.Rows[0][1])
	assert.Equal(t, int64(42), rows.Rows[0][4]) // package_count
	assert.Nil(t, rows.Rows[1][8])              // empty created -> nil
}

func TestScanGroupsHitsGroupList(t *testing.T) {
	var gotPath string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"g1","name":"climate","title":"Climate"}]}`))
	})
	rows, err := collectScan(c, source.ScanRequest{Table: "groups"})
	require.NoError(t, err)
	assert.Equal(t, "/api/3/action/group_list", gotPath)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, "climate", rows.Rows[0][1])
}

func TestScanUnknownTable(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	err = c.Scan(context.Background(), source.ScanRequest{Table: "nope"}, func(*source.Rows) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown table")
}

// page renders n minimal package dicts.
func page(n int) string {
	out := "["
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += `{"id":"` + itoa(i) + `","name":"n` + itoa(i) + `"}`
	}
	return out + "]"
}
