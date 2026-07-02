// Package jaeger is a dfetch Connector backed by Jaeger's api_v3 query service.
// It exposes the spans, services, and operations tables under the SQL schema
// "jaeger" and pushes down the service/operation/time-window/duration filters
// that api_v3 understands. The api returns the OpenTelemetry (OTLP) data model;
// the connector flattens it into one row per span.
package jaeger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const defaultBaseURL = "http://localhost:16686"

// defaultWindow is the start-time window applied when a query has no filter on
// start_time. api_v3 requires both start_time_min and start_time_max, so an
// unbounded query needs a default rather than fetching all of history.
const defaultWindow = time.Hour

// defaultWarnTraces is the trace count at which an uncapped scan warns that the
// result is large and may have been truncated server-side (so the user can narrow
// the window or set max_traces). It is only a warning threshold, never sent to the
// server.
const defaultWarnTraces = 1000

// Connector talks to the Jaeger api_v3 query service. maxTraces is 0 by default,
// meaning the api_v3 search_depth is left unset so the Jaeger query service
// applies its own limit. We deliberately do NOT send a default search_depth:
// api_v3 rejects any value that is not strictly below the server's configured
// max-traces limit ("search depth must be greater than 0 and less than max
// traces"), so a fixed default breaks deployments with a low limit. Setting the
// max_traces param opts into an explicit cap (which must be below that limit).
type Connector struct {
	client    *http.Client
	baseURL   string
	token     string
	maxTraces int // 0 = unset (omit search_depth; let the server bound the scan)
}

// New builds a Jaeger connector. Supported params: "base_url" (override the
// Jaeger query host; defaults to http://localhost:16686, also used by tests) and
// "max_traces" (an explicit api_v3 search_depth cap; omitted by default so the
// server's own limit applies — set it below the server's max-traces if you want a
// deterministic cap). An optional bearer token comes from $JAEGER_TOKEN for
// secured deployments; local Jaeger needs none.
func New(params map[string]any) (source.Connector, error) {
	c := &Connector{
		// otelhttp.NewTransport adds an OpenTelemetry client span per request
		// (a no-op until a tracer provider is installed).
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		baseURL: defaultBaseURL,
		token:   firstEnv("JAEGER_TOKEN"),
	}
	if bu, ok := params["base_url"].(string); ok && bu != "" {
		c.baseURL = strings.TrimSuffix(bu, "/")
	}
	if n, ok := intParam(params["max_traces"]); ok && n > 0 {
		c.maxTraces = n
	}
	return c, nil
}

// intParam coerces a YAML/JSON numeric param to int (YAML may decode as int,
// int64, or float64).
func intParam(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Tables returns the schemas of the jaeger.* tables.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "spans", Columns: spansCols},
		{Name: "services", Columns: servicesCols},
		{Name: "operations", Columns: operationsCols},
	}
}

// Scan dispatches to the per-table fetchers.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	switch req.Table {
	case "spans":
		return c.scanSpans(ctx, req, emit)
	case "services":
		return c.scanServices(ctx, req, emit)
	case "operations":
		return c.scanOperations(ctx, req, emit)
	default:
		return fmt.Errorf("jaeger: unknown table %q", req.Table)
	}
}

// --- HTTP ---

// get issues a GET and returns the response after checking the status. On a
// non-2xx it reads and closes the body and returns the api_v3 error message; on
// success the caller owns (and must close) resp.Body.
func (c *Connector) get(ctx context.Context, rawurl string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, fmt.Errorf("jaeger: GET %s: %w", rawurl, err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jaeger: GET %s: %w", rawurl, err)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("jaeger: GET %s: %s: %s", rawurl, resp.Status, apiMessage(body))
	}
	return resp, nil
}

// getJSON fetches rawurl and decodes the single JSON object body into v.
func (c *Connector) getJSON(ctx context.Context, rawurl string, v any) error {
	resp, err := c.get(ctx, rawurl)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("jaeger: decoding %s: %w", rawurl, err)
	}
	return nil
}

// apiMessage extracts the message from an api_v3 error body
// ({"error":{"message":...}}), falling back to the raw body.
func apiMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(body))
}

// --- filter / push-down helpers ---

// stringEq returns the string value of an equality filter on col, if present.
func stringEq(req source.ScanRequest, col string) (string, bool) {
	f, ok := req.Filter(col)
	if !ok || f.Op != sqlparse.OpEq {
		return "", false
	}
	s, ok := f.Value.(string)
	return s, ok
}

// requireStringEq is stringEq but returns a helpful error when the filter is
// absent (api_v3 requires service_name for traces and operations).
func requireStringEq(req source.ScanRequest, col string) (string, error) {
	s, ok := stringEq(req, col)
	if !ok {
		return "", fmt.Errorf("jaeger.%s requires %s = '...' in the WHERE clause", req.Table, col)
	}
	return s, nil
}

// timeBounds derives the [min, max] start-time window from range filters on the
// start_time column, defaulting to the last window and now when a bound is absent
// (api_v3 requires both). The engine offers >, >=, <, <=, BETWEEN as filters; an
// unparseable literal is ignored (SQLite re-applies the predicate anyway).
// hasStartTimeLowerBound reports whether the query set a lower start_time bound.
// When it didn't, timeBounds applies the default window to the lower bound (even
// if an upper bound was given), so the caller warns that the result is windowed.
func hasStartTimeLowerBound(req source.ScanRequest) bool {
	for _, f := range req.Filters {
		if f.Column != "start_time" {
			continue
		}
		switch f.Op {
		case sqlparse.OpGt, sqlparse.OpGte, sqlparse.OpBetween:
			return true
		}
	}
	return false
}

func timeBounds(req source.ScanRequest, now time.Time, window time.Duration) (min, max time.Time) {
	min, max = now.Add(-window), now
	var minSet, maxSet bool
	for _, f := range req.Filters {
		if f.Column != "start_time" {
			continue
		}
		switch f.Op {
		case sqlparse.OpGt, sqlparse.OpGte:
			if t, slack, ok := parseTime(f.Value); ok {
				min, minSet = t.Add(-slack), true
			}
		case sqlparse.OpLt, sqlparse.OpLte:
			if t, slack, ok := parseTime(f.Value); ok {
				max, maxSet = t.Add(slack), true
			}
		case sqlparse.OpBetween:
			if len(f.Values) == 2 {
				if t, slack, ok := parseTime(f.Values[0]); ok {
					min, minSet = t.Add(-slack), true
				}
				if t, slack, ok := parseTime(f.Values[1]); ok {
					max, maxSet = t.Add(slack), true
				}
			}
		}
	}
	// With only an upper bound, anchor the default window to max rather than to
	// now: otherwise a historical max leaves min (now-window) > max, an inverted
	// window that api_v3 returns nothing for, dropping rows the query wanted.
	if maxSet && !minSet {
		min = max.Add(-window)
	}
	return min, max
}

// parseTime parses a start_time literal and reports the outward slack the
// derived window bound needs so the fetched window stays a superset of the
// rows SQLite's verbatim text comparison keeps. start_time is stored as
// fixed-width UTC RFC3339 text (see startTimeText), so:
//
//   - a stored-compatible literal ("2006-01-02", or a Z-suffixed
//     "2006-01-02T15:04:05[.frac]Z") orders lexically the way it orders
//     chronologically down to the second; one second of slack absorbs the
//     sub-second boundary cases (e.g. stored "…00.9…Z" sorts before a bare
//     "…00Z" upper bound but is chronologically after it);
//   - a parseable but lexically-divergent literal (a space between date and
//     time, or a non-UTC offset) compares by its wall-clock digits, which can
//     disagree with chronology by up to a day ('T' sorts above ' ', so every
//     same-day stored row exceeds a space-form literal; an offset shifts the
//     wall-clock digits by up to ±14h) — those get a day of slack.
func parseTime(v any) (t time.Time, slack time.Duration, ok bool) {
	s, isStr := v.(string)
	if !isStr {
		return time.Time{}, 0, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			if strings.HasSuffix(s, "Z") {
				return t, time.Second, true
			}
			return t, 24 * time.Hour, true // non-UTC offset: wall-clock digits diverge
		}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, time.Second, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, 24 * time.Hour, true // space form: every same-day row sorts above it
	}
	return time.Time{}, 0, false
}

// durationBounds derives the api_v3 duration_min/duration_max (Go duration
// strings ending in "s") from range filters on the duration_ms column. Empty
// strings mean "not set".
func durationBounds(req source.ScanRequest) (min, max string) {
	for _, f := range req.Filters {
		if f.Column != "duration_ms" {
			continue
		}
		switch f.Op {
		case sqlparse.OpGt, sqlparse.OpGte:
			if s, ok := durSeconds(f.Value); ok {
				min = s
			}
		case sqlparse.OpLt, sqlparse.OpLte:
			if s, ok := durSeconds(f.Value); ok {
				max = s
			}
		case sqlparse.OpBetween:
			if len(f.Values) == 2 {
				if s, ok := durSeconds(f.Values[0]); ok {
					min = s
				}
				if s, ok := durSeconds(f.Values[1]); ok {
					max = s
				}
			}
		}
	}
	return min, max
}

// durSeconds formats a millisecond value as the seconds string api_v3 expects
// (e.g. 5 -> "0.005s").
func durSeconds(v any) (string, bool) {
	var ms float64
	switch n := v.(type) {
	case int64:
		ms = float64(n)
	case float64:
		ms = n
	default:
		return "", false
	}
	return strconv.FormatFloat(ms/1000, 'f', -1, 64) + "s", true
}
