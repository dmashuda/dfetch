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

// maxTraces caps how many traces api_v3 returns for one scan (query.search_depth)
// so an unbounded query doesn't pull the entire store.
const maxTraces = 1000

// Connector talks to the Jaeger api_v3 query service.
type Connector struct {
	client  *http.Client
	baseURL string
	token   string
}

// New builds a Jaeger connector. Supported params: "base_url" (override the
// Jaeger query host; defaults to http://localhost:16686, also used by tests). An
// optional bearer token comes from $JAEGER_TOKEN for secured deployments; local
// Jaeger needs none.
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
	return c, nil
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
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
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
func timeBounds(req source.ScanRequest, now time.Time, window time.Duration) (min, max time.Time) {
	min, max = now.Add(-window), now
	var minSet, maxSet bool
	for _, f := range req.Filters {
		if f.Column != "start_time" {
			continue
		}
		switch f.Op {
		case sqlparse.OpGt, sqlparse.OpGte:
			if t, ok := parseTime(f.Value); ok {
				min, minSet = t, true
			}
		case sqlparse.OpLt, sqlparse.OpLte:
			if t, ok := parseTime(f.Value); ok {
				max, maxSet = t, true
			}
		case sqlparse.OpBetween:
			if len(f.Values) == 2 {
				if t, ok := parseTime(f.Values[0]); ok {
					min, minSet = t, true
				}
				if t, ok := parseTime(f.Values[1]); ok {
					max, maxSet = t, true
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

func parseTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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
