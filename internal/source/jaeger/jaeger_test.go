package jaeger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestConnectorWith builds a connector against a test server with extra params.
func newTestConnectorWith(t *testing.T, params map[string]any, h http.HandlerFunc) *Connector {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	params["base_url"] = srv.URL
	c, err := New(params)
	require.NoError(t, err)
	return c.(*Connector)
}

func newTestConnector(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL})
	require.NoError(t, err)
	return c.(*Connector)
}

func eqFilter(col, val string) source.Filter {
	return source.Filter{Column: col, Op: source.OpEq, Value: val}
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

// scanChunkSizes records the row count of each data chunk (warning-only chunks
// carry no rows and are ignored here).
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

// oneTrace is a minimal OTLP TracesData wrapped in api_v3's {"result": ...},
// with two spans: a root server span (status error) and an internal child.
const oneTrace = `{"result":{"resourceSpans":[{
	"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"dfetch"}}]},
	"scopeSpans":[{"scope":{"name":"github.com/dmashuda/dfetch"},"spans":[
		{"traceId":"abc","spanId":"s1","parentSpanId":"","name":"engine.Run","kind":2,
		 "startTimeUnixNano":"1000000000","endTimeUnixNano":"1500000000",
		 "attributes":[{"key":"db.query.text","value":{"stringValue":"SELECT 1"}},
		               {"key":"rows","value":{"intValue":"42"}}],
		 "status":{"code":2,"message":"boom"}},
		{"traceId":"abc","spanId":"s2","parentSpanId":"s1","name":"connector.scan","kind":1,
		 "startTimeUnixNano":"1100000000","endTimeUnixNano":"1200000000",
		 "attributes":[],"status":{}}
	]}]
}]}}`

func TestTables(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	tables := c.Tables()
	names := make([]string, 0, len(tables))
	for _, tbl := range tables {
		names = append(names, tbl.Name)
		assert.NotEmpty(t, tbl.Columns)
	}
	assert.ElementsMatch(t, []string{"spans", "services", "operations"}, names)
}

func TestScanSpansFlattensAndMaps(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v3/traces", r.URL.Path)
		_, _ = w.Write([]byte(oneTrace))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "spans",
		Filters: []source.Filter{eqFilter("service_name", "dfetch")},
	})
	require.NoError(t, err)
	assert.Equal(t, colNames(spansCols), rows.Columns)
	require.Len(t, rows.Rows, 2)

	// Root span: service hoisted, kind/status mapped, duration computed, attrs JSON.
	root := rows.Rows[0]
	assert.Equal(t, "abc", root[0])        // trace_id
	assert.Equal(t, "s1", root[1])         // span_id
	assert.Nil(t, root[2])                 // parent_span_id (empty -> nil)
	assert.Equal(t, "engine.Run", root[3]) // operation_name
	assert.Equal(t, "dfetch", root[4])     // service_name
	assert.Equal(t, "server", root[6])     // kind
	// start_time is fixed-width UTC text so lexical order == chronological order.
	assert.Equal(t, "1970-01-01T00:00:01.000000000Z", root[7])
	assert.Equal(t, int64(1000000000), root[8]) // start_time_unix_nano
	assert.Equal(t, 500.0, root[9])             // duration_ms = (1.5e9-1e9)/1e6
	assert.Equal(t, "error", root[10])          // status_code
	assert.Equal(t, "boom", root[11])           // status_message
	assert.Contains(t, root[12], `"db.query.text":"SELECT 1"`)
	assert.Contains(t, root[12], `"rows":42`) // intValue parsed to number

	// Child span: parent set, internal kind, unset status -> nil message.
	child := rows.Rows[1]
	assert.Equal(t, "s1", child[2])       // parent_span_id
	assert.Equal(t, "internal", child[6]) // kind
	assert.Equal(t, "unset", child[10])   // status_code
	assert.Nil(t, child[11])              // status_message
	assert.Equal(t, "{}", child[12])      // empty attributes
}

func TestScanSpansPushdown(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"result":{"resourceSpans":[]}}`))
	})

	limit := 5
	_, err := collectScan(c, source.ScanRequest{
		Table: "spans",
		Filters: []source.Filter{
			eqFilter("service_name", "dfetch"),
			eqFilter("operation_name", "engine.Run"),
			{Column: "start_time", Op: source.OpGte, Value: "2026-06-01T00:00:00Z"},
			{Column: "start_time", Op: source.OpLt, Value: "2026-06-02T00:00:00Z"},
			{Column: "duration_ms", Op: source.OpGt, Value: int64(5)},
		},
		Limit: &limit, // must NOT be pushed
	})
	require.NoError(t, err)

	assert.Equal(t, "dfetch", gotQuery.Get("query.service_name"))
	assert.Equal(t, "engine.Run", gotQuery.Get("query.operation_name"))
	// Bounds carry one second of outward slack (see parseTime).
	assert.Equal(t, "2026-05-31T23:59:59Z", gotQuery.Get("query.start_time_min"))
	assert.Equal(t, "2026-06-02T00:00:01Z", gotQuery.Get("query.start_time_max"))
	assert.Equal(t, "0.005s", gotQuery.Get("query.duration_min"))
	assert.Empty(t, gotQuery.Get("query.search_depth")) // omitted by default; the SQL LIMIT is not pushed
}

func TestScanSpansDefaultsTimeWindow(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"result":{"resourceSpans":[]}}`))
	})

	_, err := collectScan(c, source.ScanRequest{
		Table:   "spans",
		Filters: []source.Filter{eqFilter("service_name", "dfetch")},
	})
	require.NoError(t, err)
	// No start_time filter -> both bounds defaulted (last hour .. now).
	assert.NotEmpty(t, gotQuery.Get("query.start_time_min"))
	assert.NotEmpty(t, gotQuery.Get("query.start_time_max"))
}

// With only an upper bound on start_time, the default lower bound must anchor to
// max (not now), or a historical query yields min > max — an inverted window
// api_v3 returns nothing for.
func TestScanSpansUpperBoundOnlyTimeWindow(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"result":{"resourceSpans":[]}}`))
	})

	_, err := collectScan(c, source.ScanRequest{
		Table: "spans",
		Filters: []source.Filter{
			eqFilter("service_name", "dfetch"),
			{Column: "start_time", Op: source.OpLt, Value: "2020-01-01T00:00:00Z"},
		},
	})
	require.NoError(t, err)

	max := gotQuery.Get("query.start_time_max")
	min := gotQuery.Get("query.start_time_min")
	assert.Equal(t, "2020-01-01T00:00:01Z", max) // parsed bound + 1s slack
	tmin, err1 := time.Parse(time.RFC3339, min)
	tmax, err2 := time.Parse(time.RFC3339, max)
	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.True(t, tmin.Before(tmax), "min (%s) must be before max (%s), not an inverted window", min, max)
}

// timeBounds widens parsed bounds outward so the fetched window stays a
// superset of the rows SQLite's verbatim text comparison keeps (see parseTime):
// one second for stored-compatible literals (Z-suffixed RFC3339, date-only), a
// day for space-form or non-UTC-offset literals, whose lexical order against
// the stored text diverges from chronology.
func TestTimeBoundsSlack(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		literal string
		wantMin string // window min for a start_time >= literal filter
		wantMax string // window max for a start_time <= literal filter
	}{
		"rfc3339 zulu": {
			"2026-06-01T10:00:00Z",
			"2026-06-01T09:59:59Z", "2026-06-01T10:00:01Z",
		},
		"rfc3339 fractional zulu": {
			"2026-06-01T10:00:00.5Z",
			"2026-06-01T09:59:59.5Z", "2026-06-01T10:00:01.5Z",
		},
		"date only": {
			"2026-06-01",
			"2026-05-31T23:59:59Z", "2026-06-01T00:00:01Z",
		},
		"space form": {
			"2026-06-01 10:00:00",
			"2026-05-31T10:00:00Z", "2026-06-02T10:00:00Z",
		},
		"non-utc offset": {
			"2026-06-01T10:00:00+02:00", // 08:00Z
			"2026-05-31T08:00:00Z", "2026-06-02T08:00:00Z",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			min, _ := timeBounds(source.ScanRequest{Filters: []source.Filter{
				{Column: "start_time", Op: source.OpGte, Value: tc.literal},
			}}, now, defaultWindow)
			assert.Equal(t, tc.wantMin, min.UTC().Format(time.RFC3339Nano), "min")

			_, max := timeBounds(source.ScanRequest{Filters: []source.Filter{
				{Column: "start_time", Op: source.OpLte, Value: tc.literal},
			}}, now, defaultWindow)
			assert.Equal(t, tc.wantMax, max.UTC().Format(time.RFC3339Nano), "max")
		})
	}
}

// A trace_id equality uses the by-ID endpoint and needs neither service_name nor
// a time window.
func TestScanSpansByTraceID(t *testing.T) {
	var gotPath string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		assert.Empty(t, r.URL.RawQuery) // no service_name / time window
		_, _ = w.Write([]byte(oneTrace))
	})

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "spans",
		Filters: []source.Filter{eqFilter("trace_id", "abc")},
	})
	require.NoError(t, err)
	assert.Equal(t, "/api/v3/traces/abc", gotPath)
	require.Len(t, rows.Rows, 2)
	assert.Empty(t, rows.Warnings) // direct trace lookup applies no window/cap
}

// A service_name scan with no start_time filter falls back to the default recent
// window and warns that results may be incomplete.
func TestScanSpansDefaultWindowWarning(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oneTrace))
	})
	res, err := collectScan(c, source.ScanRequest{
		Table:   "spans",
		Filters: []source.Filter{eqFilter("service_name", "dfetch")},
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "jaeger.spans")
	assert.Contains(t, res.Warnings[0], "start_time")
}

// An upper-bound-only start_time filter still leaves the lower bound defaulted to
// a window, so the warning must fire (the connector silently searched only 1h).
func TestScanSpansUpperBoundOnlyWarns(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oneTrace))
	})
	res, err := collectScan(c, source.ScanRequest{
		Table: "spans",
		Filters: []source.Filter{
			eqFilter("service_name", "dfetch"),
			{Column: "start_time", Op: source.OpLt, Value: "2026-01-01T00:00:00Z"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "start_time")
}

// With an explicit lower start_time bound, no default-window warning is emitted.
func TestScanSpansWithStartTimeNoWindowWarning(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oneTrace))
	})
	res, err := collectScan(c, source.ScanRequest{
		Table: "spans",
		Filters: []source.Filter{
			eqFilter("service_name", "dfetch"),
			{Column: "start_time", Op: source.OpGte, Value: "2026-01-01T00:00:00Z"},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
}

// By default no search_depth is sent (the server bounds the scan); setting
// max_traces opts into an explicit api_v3 search_depth cap.
func TestScanSpansSearchDepthConfigurable(t *testing.T) {
	req := source.ScanRequest{Table: "spans", Filters: []source.Filter{eqFilter("service_name", "dfetch")}}

	gotDefault := "sentinel"
	def := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotDefault = r.URL.Query().Get("query.search_depth")
		_, _ = w.Write([]byte(oneTrace))
	})
	_, err := collectScan(def, req)
	require.NoError(t, err)
	assert.Empty(t, gotDefault, "default must not send search_depth") // omitted

	var gotConfigured string
	cfg := newTestConnectorWith(t, map[string]any{"max_traces": 250}, func(w http.ResponseWriter, r *http.Request) {
		gotConfigured = r.URL.Query().Get("query.search_depth")
		_, _ = w.Write([]byte(oneTrace))
	})
	_, err = collectScan(cfg, req)
	require.NoError(t, err)
	assert.Equal(t, "250", gotConfigured) // honors max_traces
}

// Reproduces the customer's server (a low max-traces limit) and proves the
// out-of-the-box default works against it, while an over-limit max_traces fails.
func TestScanSpansServerMaxTracesLimit(t *testing.T) {
	const serverMax = 500
	// Mirrors api_v3: an explicit search_depth must be in (0, serverMax); an absent
	// one is fine (the server applies its own limit).
	handler := func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("query.search_depth"); v != "" {
			if d, _ := strconv.Atoi(v); d <= 0 || d >= serverMax {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"httpCode":400,"message":"search depth must be greater than 0 and less than max traces"}}`))
				return
			}
		}
		_, _ = w.Write([]byte(oneTrace))
	}
	req := source.ScanRequest{Table: "spans", Filters: []source.Filter{eqFilter("service_name", "dfetch")}}

	// Out of the box (no search_depth sent) → works against the low-limit server.
	res, err := collectScan(newTestConnector(t, handler), req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Rows)

	// An explicit max_traces at/above the server limit → the error is surfaced.
	_, err = collectScan(newTestConnectorWith(t, map[string]any{"max_traces": 1000}, handler), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max traces")

	// An explicit max_traces below the server limit → success.
	res, err = collectScan(newTestConnectorWith(t, map[string]any{"max_traces": 100}, handler), req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Rows)
}

func TestScanSpansRequiresService(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("should not call the API without service_name")
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := collectScan(c, source.ScanRequest{Table: "spans"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service_name")
}

// The grpc-gateway may return multiple concatenated {"result":...} objects;
// each becomes its own emitted chunk.
func TestScanSpansStreamsMultipleResults(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oneTrace + oneTrace))
	})
	sizes, err := scanChunkSizes(c, source.ScanRequest{
		Table:   "spans",
		Filters: []source.Filter{eqFilter("service_name", "dfetch")},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 2}, sizes) // two chunks, two spans each
}

func TestScanServices(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v3/services", r.URL.Path)
		_, _ = w.Write([]byte(`{"services":["dfetch","jaeger-all-in-one"]}`))
	})
	rows, err := collectScan(c, source.ScanRequest{Table: "services"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, "dfetch", rows.Rows[0][0])
}

func TestScanOperations(t *testing.T) {
	var gotQuery url.Values
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v3/operations", r.URL.Path)
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"operations":[{"name":"engine.Run","spanKind":"internal"}]}`))
	})
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "operations",
		Filters: []source.Filter{eqFilter("service_name", "dfetch"), eqFilter("span_kind", "internal")},
	})
	require.NoError(t, err)
	assert.Equal(t, "dfetch", gotQuery.Get("service"))
	assert.Equal(t, "internal", gotQuery.Get("spanKind"))
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, []any{"dfetch", "engine.Run", "internal"}, rows.Rows[0])
}

func TestScanOperationsRequiresService(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("should not call the API without service_name")
	})
	_, err := collectScan(c, source.ScanRequest{Table: "operations"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service_name")
}

func TestAPIError(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"httpCode":400,"message":"plain bad request"}}`))
	})
	_, err := collectScan(c, source.ScanRequest{
		Table:   "spans",
		Filters: []source.Filter{eqFilter("service_name", "dfetch")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plain bad request")
}

func TestUnknownTable(t *testing.T) {
	c, _ := New(nil)
	_, err := collectScan(c, source.ScanRequest{Table: "nope"})
	assert.Error(t, err)
}
