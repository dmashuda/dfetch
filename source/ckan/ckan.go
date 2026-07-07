// Package ckan is a dfetch Connector backed by a CKAN portal's Action API
// (https://docs.ckan.org/en/latest/api/). CKAN powers catalog.data.gov and many
// other open-data portals, all sharing one read-only JSON API, so a single
// connector pointed at a configurable base_url covers any of them; the default
// host is data.gov's catalog. It is registered as the builtin schema "datagov"
// and exposes the datasets, resources, organizations, and groups tables, pushing
// down the Solr-backed filters, ordering, and LIMIT/OFFSET that package_search
// understands.
package ckan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dmashuda/dfetch/source"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// defaultBaseURL points at data.gov's catalog. data.gov has moved its primary
// CKAN endpoint behind the GSA api.gsa.gov gateway, but catalog-old.data.gov
// still serves the standard CKAN Action API directly with no API key — so it is
// the zero-config default. To use the gateway instead, point base_url at
// https://api.gsa.gov/technology/datagov with action_path "/v3/action" and an
// api_key (DEMO_KEY works).
const defaultBaseURL = "https://catalog-old.data.gov"

// defaultActionPath is the standard CKAN Action API prefix. The api.gsa.gov
// gateway serves the same actions under "/v3/action" instead.
const defaultActionPath = "/api/3/action"

// defaultRows is the page size used when a LIMIT can't be pushed.
const defaultRows = 100

// maxRows is CKAN's per-request cap on package_search (the "rows" parameter).
const maxRows = 1000

// maxPages caps pagination when a LIMIT can't be pushed safely, so an unbounded
// query doesn't page through an entire (potentially huge) catalog.
const maxPages = 10

// Connector talks to a CKAN Action API.
type Connector struct {
	client     *http.Client
	baseURL    string
	actionPath string
	apiKey     string
}

// New builds a CKAN connector. Supported params: "base_url" (override the portal
// host; defaults to https://catalog-old.data.gov, also used by tests),
// "action_path" (the Action API prefix; defaults to "/api/3/action", set to
// "/v3/action" for the api.gsa.gov gateway), and "api_key" (sent as the api_key
// query parameter the gateway requires; falls back to $CKAN_API_KEY). Public
// CKAN reads need no key.
func New(params map[string]any) (source.Connector, error) {
	c := &Connector{
		// otelhttp.NewTransport adds an OpenTelemetry client span per request
		// (a no-op until a tracer provider is installed).
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		baseURL:    defaultBaseURL,
		actionPath: defaultActionPath,
		apiKey:     firstEnv("CKAN_API_KEY"),
	}
	if bu, ok := params["base_url"].(string); ok && bu != "" {
		c.baseURL = strings.TrimSuffix(bu, "/")
	}
	if ap, ok := params["action_path"].(string); ok && ap != "" {
		c.actionPath = "/" + strings.Trim(ap, "/")
	}
	if k, ok := params["api_key"].(string); ok && k != "" {
		c.apiKey = k
	}
	return c, nil
}

// actionURL builds the URL for a CKAN action, folding in the api_key query
// parameter when one is configured (the api.gsa.gov gateway requires it).
func (c *Connector) actionURL(action string, q url.Values) string {
	if c.apiKey != "" {
		q.Set("api_key", c.apiKey)
	}
	return c.baseURL + c.actionPath + "/" + action + "?" + q.Encode()
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Tables returns the schemas of the datagov.* tables.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "datasets", Columns: datasetsCols},
		{Name: "resources", Columns: resourcesCols},
		{Name: "organizations", Columns: orgGroupCols},
		{Name: "groups", Columns: orgGroupCols},
	}
}

// Scan dispatches to the per-table fetchers, which emit one chunk per API page.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	switch req.Table {
	case "datasets":
		return c.scanDatasets(ctx, req, emit)
	case "resources":
		return c.scanResources(ctx, req, emit)
	case "organizations":
		return c.scanList(ctx, "organization_list", orgGroupCols, emit)
	case "groups":
		return c.scanList(ctx, "group_list", orgGroupCols, emit)
	default:
		return fmt.Errorf("ckan: unknown table %q", req.Table)
	}
}

// --- HTTP ---

// getJSON fetches rawurl and decodes the Action API envelope into v.
func (c *Connector) getJSON(ctx context.Context, rawurl string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	// Some CKAN hosts reject requests without a descriptive User-Agent.
	req.Header.Set("User-Agent", "dfetch (+https://github.com/dmashuda/dfetch)")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ckan: GET %s: %s: %s", rawurl, resp.Status, apiMessage(body))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("ckan: decoding %s: %w", rawurl, err)
	}
	return nil
}

// apiMessage extracts a human-readable message from a CKAN error envelope
// ({"success":false,"error":{"message":...,"__type":...}}), falling back to the
// raw body.
func apiMessage(body []byte) string {
	var e struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != nil {
		if msg, ok := e.Error["message"]; ok {
			var s string
			if json.Unmarshal(msg, &s) == nil && s != "" {
				return s
			}
		}
		if t, ok := e.Error["__type"]; ok {
			var s string
			if json.Unmarshal(t, &s) == nil && s != "" {
				return s
			}
		}
	}
	return strings.TrimSpace(string(body))
}

// --- filter / order / push-down helpers ---

// searchTerm extracts the full-text search value from a `q = '...'` filter. The
// q column is a virtual search input — it has no stored value (it is only echoed
// back for an equality filter), so any other operator on q would make SQLite's
// verbatim re-filter compare against NULL and silently drop every row. Reject
// such a filter with a clear error instead. Returns a nil value (and no error)
// when there is no usable q filter. The returned value is `any` so it can be
// written straight into the row's q column (the searched string, or nil).
func searchTerm(req source.ScanRequest) (any, error) {
	f, ok := req.Filter("q")
	if !ok {
		return nil, nil
	}
	if f.Op != source.OpEq {
		return nil, fmt.Errorf("ckan.%s: the q column supports only equality (q = '...')", req.Table)
	}
	s, ok := f.Value.(string)
	if !ok {
		return nil, nil
	}
	return s, nil
}

// eqString returns the string value of an equality filter, if it is one.
func eqString(f source.Filter) (string, bool) {
	if f.Op != source.OpEq {
		return "", false
	}
	s, ok := f.Value.(string)
	return s, ok
}

// inStrings returns the solr-quoted members of an IN filter over string values.
func inStrings(f source.Filter) ([]string, bool) {
	if f.Op != source.OpIn || len(f.Values) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(f.Values))
	for _, v := range f.Values {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		out = append(out, solrQuote(s))
	}
	return out, true
}

// solrQuote wraps a value in double quotes for a Solr fq clause, escaping
// backslashes and quotes so spaces and special characters are matched literally.
func solrQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// solrRange turns a range filter on a date column into a Solr range expression
// ("[lo TO hi]", with "*" for an open end). It only succeeds when the bound(s)
// parse as a time; otherwise the filter is left for SQLite to re-apply. The
// expression always *widens* the SQL predicate: gt/gte (and lt/lte) collapse to
// an inclusive bound, and sub-second precision rounds outward (down for a lower
// bound, up for an upper bound, since Solr's whole-second format would otherwise
// tighten an upper bound and drop rows). The API therefore returns a superset,
// which the engine re-filters exactly — and which is why a range filter never
// counts as consumed for LIMIT push (see buildDatasetParams).
func solrRange(f source.Filter) (string, bool) {
	switch f.Op {
	case source.OpGt, source.OpGte:
		if t, ok := parseTime(f.Value); ok {
			return "[" + solrTimeFloor(t) + " TO *]", true
		}
	case source.OpLt, source.OpLte:
		if t, ok := parseTime(f.Value); ok {
			return "[* TO " + solrTimeCeil(t) + "]", true
		}
	case source.OpBetween:
		if len(f.Values) == 2 {
			lo, ok1 := parseTime(f.Values[0])
			hi, ok2 := parseTime(f.Values[1])
			if ok1 && ok2 {
				return "[" + solrTimeFloor(lo) + " TO " + solrTimeCeil(hi) + "]", true
			}
		}
	}
	return "", false
}

// solrTimeFloor renders a lower bound truncated to the second (widening down).
func solrTimeFloor(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

// solrTimeCeil renders an upper bound rounded up to the next whole second when
// it carries sub-second precision (widening up).
func solrTimeCeil(t time.Time) string {
	u := t.UTC()
	if u.Nanosecond() > 0 {
		u = u.Truncate(time.Second).Add(time.Second)
	}
	return u.Format("2006-01-02T15:04:05Z")
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

// sortableFields maps a dataset column to the Solr field package_search sorts on.
// Sorting on the title needs Solr's "title_string", not the indexed "title".
var sortableFields = map[string]string{
	"metadata_modified": "metadata_modified",
	"metadata_created":  "metadata_created",
	"title":             "title_string",
}

// sortParam builds the package_search "sort" value from the ORDER BY terms. ok is
// true only when every term maps to a sortable field (or there are none); a term
// the API can't reproduce makes the whole ordering unsafe to honor, so LIMIT must
// not be pushed.
func sortParam(terms []source.OrderTerm) (sort string, ok bool) {
	if len(terms) == 0 {
		return "", true
	}
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		field, ok := sortableFields[t.Column]
		if !ok {
			return "", false
		}
		dir := "asc"
		if t.Desc {
			dir = "desc"
		}
		parts = append(parts, field+" "+dir)
	}
	return strings.Join(parts, ", "), true
}

// pageLimit decides the page size ("rows"), how many rows to fetch before
// stopping (stopAt), and whether LIMIT can be pushed. Pushing is only safe when
// the API returns exactly the rows the query keeps — every filter consumed and
// the ordering honored — which the caller passes as safe. With an OFFSET we must
// still fetch limit+offset rows from the start so SQLite can re-apply the OFFSET
// over the verbatim query. stopAt is 0 (no early stop) when LIMIT is not pushed.
func pageLimit(req source.ScanRequest, safe bool) (rows, stopAt int, pushLimit bool) {
	pushLimit = req.Limit != nil && safe
	if !pushLimit {
		return defaultRows, 0, false
	}
	stopAt = *req.Limit
	if req.Offset != nil {
		stopAt += *req.Offset
	}
	rows = stopAt
	if rows > maxRows {
		rows = maxRows
	}
	if rows < 1 {
		rows = 1
	}
	return rows, stopAt, true
}
