// Package newrelic is a dfetch Connector backed by New Relic's NerdGraph
// GraphQL API (https://docs.newrelic.com/docs/apis/nerdgraph/get-started/introduction-new-relic-nerdgraph/).
// It is a dynamic source: NRDB event types (Transaction, Span, Log, custom
// types) are discovered on demand and scanned by translating the pushed-down
// request into NRQL. It additionally serves curated tables (accounts,
// entities, alert_policies, alert_conditions, issues, incidents) from
// NerdGraph object queries. It is config-only (`type: newrelic`): construction
// requires an account_id param and a User API key (NOT the ingest license key)
// from the environment.
package newrelic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	defaultBaseURL = "https://api.newrelic.com/graphql"
	euBaseURL      = "https://api.eu.newrelic.com/graphql"

	// nrqlHardCap is NRQL's LIMIT MAX — no query returns more events.
	nrqlHardCap = 5000
	// defaultNRQLTimeout is the NerdGraph nrql(timeout:) argument in seconds.
	defaultNRQLTimeout = 70
	// defaultWindow is the SINCE window applied when a query has no timestamp
	// lower bound (NRQL itself would default to the last hour anyway; making it
	// explicit lets the scanner warn about it).
	defaultWindow = time.Hour
	// maxPages caps cursor pagination on the curated tables.
	maxPages = 10
	// batchSize is rows per emitted chunk.
	batchSize = 1000
	// keysetSince is the lookback for DescribeTable's keyset() probe.
	keysetSince = "1 day ago"
	// eventTypesSince is the lookback for ListTables' SHOW EVENT TYPES.
	eventTypesSince = "1 week ago"
)

// Connector talks to one New Relic account through NerdGraph.
type Connector struct {
	client      *http.Client
	baseURL     string
	apiKey      string
	account     int64
	maxRows     int // cap on an un-pushable-LIMIT NRDB scan; <= nrqlHardCap
	nrqlTimeout int // seconds

	// colCache memoizes keyset() lookups so the engine's DescribeTable at
	// planning time and Scan's own lookup cost one round-trip per event type.
	mu       sync.Mutex
	colCache map[string]tableInfo
}

// tableInfo is one event type's cached schema plus which columns are boolean
// (needed to translate `= 0/1` filters to NRQL's false/true).
type tableInfo struct {
	cols     []source.Column
	boolCols map[string]bool
}

// New builds a New Relic connector. Required: params["account_id"], and a User
// API key from $DFETCH_NEWRELIC_API_KEY or $NEW_RELIC_API_KEY. Optional params:
// "region" ("US" default, "EU"), "base_url" (overrides region; used by tests),
// "max_rows" (cap on un-pushable-LIMIT scans; default and max 5000, the NRQL
// hard cap), "nrql_timeout" (seconds, default 70, max 120). It is config-only —
// registered for `type: newrelic`, never auto-instantiated.
func New(params map[string]any) (source.Connector, error) {
	apiKey := firstEnv("DFETCH_NEWRELIC_API_KEY", "NEW_RELIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("newrelic: no API key; set $NEW_RELIC_API_KEY to a User API key (NerdGraph does not accept the ingest license key)")
	}
	account, ok := intParam(params["account_id"])
	if !ok || account <= 0 {
		return nil, fmt.Errorf("newrelic: params.account_id is required (your New Relic account id)")
	}

	c := &Connector{
		// otelhttp.NewTransport adds an OpenTelemetry client span per request
		// (a no-op until a tracer provider is installed).
		client: &http.Client{
			Timeout:   (defaultNRQLTimeout + 30) * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		baseURL:     defaultBaseURL,
		apiKey:      apiKey,
		account:     int64(account),
		maxRows:     nrqlHardCap,
		nrqlTimeout: defaultNRQLTimeout,
		colCache:    map[string]tableInfo{},
	}
	if r, ok := params["region"].(string); ok && strings.EqualFold(r, "EU") {
		c.baseURL = euBaseURL
	}
	if bu, ok := params["base_url"].(string); ok && bu != "" {
		c.baseURL = strings.TrimSuffix(bu, "/")
	}
	if n, ok := intParam(params["max_rows"]); ok && n > 0 {
		c.maxRows = min(n, nrqlHardCap)
	}
	if n, ok := intParam(params["nrql_timeout"]); ok && n > 0 {
		c.nrqlTimeout = min(n, 120)
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

// --- GraphQL client ---

// gqlError is one entry of NerdGraph's errors array.
type gqlError struct {
	Message string `json:"message"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// gqlPost POSTs {query, variables} to the NerdGraph endpoint and unmarshals the
// "data" object into out. Any non-empty GraphQL errors array is fatal even when
// data is partially populated: partial data is a subset of the requested rows,
// which the engine's local re-filter can never correct.
func (c *Connector) gqlPost(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables,omitempty"`
	}{query, variables})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("newrelic: POST %s: %w", c.baseURL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("API-Key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("newrelic: POST %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("newrelic: POST %s: %s: %s (401/403 usually means the key is not a User API key)", c.baseURL, resp.Status, truncate(data, 200))
	}

	var env gqlResponse
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("newrelic: decoding response: %w", err)
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, len(env.Errors))
		for i, e := range env.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("newrelic: graphql: %s", strings.Join(msgs, "; "))
	}
	if out != nil && env.Data != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("newrelic: decoding data: %w", err)
		}
	}
	return nil
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// nrqlGQL wraps one NRQL statement; the NRQL rides in a variable so no
// GraphQL-document escaping is ever needed.
const nrqlGQL = `query($account: Int!, $nrql: Nrql!, $timeout: Seconds) {
  actor { account(id: $account) { nrql(query: $nrql, timeout: $timeout) { results } } }
}`

// nrqlQuery runs one NRQL statement via actor.account(id).nrql and returns the
// raw results array (each element is an object keyed by attribute name).
func (c *Connector) nrqlQuery(ctx context.Context, nrql string) ([]map[string]any, error) {
	var out struct {
		Actor struct {
			Account struct {
				Nrql *struct {
					Results []map[string]any `json:"results"`
				} `json:"nrql"`
			} `json:"account"`
		} `json:"actor"`
	}
	vars := map[string]any{"account": c.account, "nrql": nrql, "timeout": c.nrqlTimeout}
	if err := c.gqlPost(ctx, nrqlGQL, vars, &out); err != nil {
		return nil, err
	}
	if out.Actor.Account.Nrql == nil {
		return nil, fmt.Errorf("newrelic: account %d returned no NRQL data (is account_id right?)", c.account)
	}
	return out.Actor.Account.Nrql.Results, nil
}

// --- table resolution ---

// Tables returns the curated table schemas. NRDB event types are dynamic and
// resolved on demand via DescribeTable/ListTables instead.
func (c *Connector) Tables() []source.TableSchema {
	names := make([]string, 0, len(curatedTables))
	for name := range curatedTables {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]source.TableSchema, 0, len(names))
	for _, name := range names {
		out = append(out, source.TableSchema{Name: name, Columns: curatedTables[name]})
	}
	return out
}

// ListTables lists NRDB event types seen in the last week plus the curated
// tables, filtered by a case-insensitive substring.
func (c *Connector) ListTables(ctx context.Context, opts source.ListOptions) ([]string, error) {
	results, err := c.nrqlQuery(ctx, "SHOW EVENT TYPES SINCE "+eventTypesSince)
	if err != nil {
		return nil, err
	}
	names := parseEventTypes(results)
	for name := range curatedTables {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []string
	for _, name := range names {
		if opts.Filter != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(opts.Filter)) {
			continue
		}
		out = append(out, name)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// DescribeTable resolves one table's columns: curated names first (fixed
// schemas, no API call — they always win over a same-named NRDB event type),
// then the keyset() of the event type. found is false for an unknown or idle
// event type.
func (c *Connector) DescribeTable(ctx context.Context, table string) (source.TableSchema, bool, error) {
	if cols, ok := curatedTables[table]; ok {
		return source.TableSchema{Name: table, Columns: cols}, true, nil
	}
	info, err := c.eventTableInfo(ctx, table)
	if err != nil {
		return source.TableSchema{}, false, err
	}
	if len(info.cols) == 0 {
		return source.TableSchema{}, false, nil
	}
	return source.TableSchema{Name: table, Columns: info.cols}, true, nil
}

// eventTableInfo returns an event type's columns via keyset(), memoized for the
// connector's lifetime (event-type schemas are stable within a run).
func (c *Connector) eventTableInfo(ctx context.Context, table string) (tableInfo, error) {
	// A name that can't be backtick-quoted safely is not a table (and must not
	// reach NRQL: it could otherwise smuggle clauses through the FROM position).
	if strings.ContainsAny(table, "`\r\n") {
		return tableInfo{}, nil
	}

	c.mu.Lock()
	cached, ok := c.colCache[table]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	results, err := c.nrqlQuery(ctx, "SELECT keyset() FROM "+quoteAttr(table)+" SINCE "+keysetSince)
	if err != nil {
		return tableInfo{}, fmt.Errorf("newrelic: describing %q: %w", table, err)
	}
	cols, boolCols := parseKeyset(results)
	info := tableInfo{cols: cols, boolCols: boolCols}
	c.mu.Lock()
	c.colCache[table] = info
	c.mu.Unlock()
	return info, nil
}

// --- scanning ---

// Scan dispatches curated tables to their NerdGraph fetchers and everything
// else to the dynamic NRDB path.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	switch req.Table {
	case "accounts":
		return c.scanAccounts(ctx, emit)
	case "entities":
		return c.scanEntities(ctx, req, emit)
	case "alert_policies":
		return c.scanPolicies(ctx, req, emit)
	case "alert_conditions":
		return c.scanConditions(ctx, req, emit)
	case "issues":
		return c.scanIssues(ctx, req, emit)
	case "incidents":
		return c.scanIncidents(ctx, req, emit)
	default:
		return c.scanNRDB(ctx, req, emit)
	}
}

// scanNRDB translates the request into one NRQL query and emits its results.
// There is no pagination: NRQL caps results at 5000 (nrqlHardCap), and the scan
// warns when the cap bounded an un-pushed-LIMIT query.
func (c *Connector) scanNRDB(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	info, err := c.eventTableInfo(ctx, req.Table)
	if err != nil {
		return err
	}
	if len(info.cols) == 0 {
		return fmt.Errorf("newrelic: no event type %q in account %d (see `dfetch tables <schema>`)", req.Table, c.account)
	}

	plan := buildNRQL(req, info.cols, info.boolCols, c.maxRows, defaultWindow, time.Now())
	results, err := c.nrqlQuery(ctx, plan.NRQL)
	if err != nil {
		return err
	}

	names := colNames(plan.Cols)
	for start := 0; start < len(results); start += batchSize {
		end := min(start+batchSize, len(results))
		page := make([][]any, 0, end-start)
		for _, obj := range results[start:end] {
			row := make([]any, len(plan.Cols))
			for i, col := range plan.Cols {
				if v, ok := obj[col.Name]; ok {
					row[i] = normalizeNRDB(v, col.Type)
				}
			}
			page = append(page, row)
		}
		if err := emit(&source.Rows{Columns: names, Rows: page}); err != nil {
			return err
		}
	}

	if plan.Capped && len(results) >= c.maxRows {
		if err := emit(source.Warn("newrelic.%s: capped at %d rows (NRQL LIMIT MAX is %d); narrow the timestamp window or add filters", req.Table, c.maxRows, nrqlHardCap)); err != nil {
			return err
		}
	}
	if plan.Windowed {
		if err := emit(source.Warn("newrelic.%s: no timestamp lower bound; searched only the last %s — add timestamp >= <epoch_ms> for more history", req.Table, defaultWindow)); err != nil {
			return err
		}
	}
	return nil
}

// --- curated-table pagination helpers ---

// cursorPages drives a cursor-paginated fetch: fetch runs once per page (empty
// cursor first) and returns the next cursor ("" when exhausted). When pushLimit
// is set the scan stops early at stopAt rows; otherwise it stops at the
// maxPages cap with a truncation warning.
func cursorPages(ctx context.Context, table string, cols []source.Column, stopAt int, pushLimit bool,
	fetch func(ctx context.Context, cursor string) (rows [][]any, next string, err error),
	emit func(*source.Rows) error,
) error {
	sent, cursor := 0, ""
	for pages := 0; ; pages++ {
		rows, next, err := fetch(ctx, cursor)
		if err != nil {
			return err
		}
		if pushLimit && sent+len(rows) > stopAt {
			rows = rows[:stopAt-sent]
		}
		if len(rows) > 0 {
			if err := emit(&source.Rows{Columns: colNames(cols), Rows: rows}); err != nil {
				return err
			}
			sent += len(rows)
		}
		if (pushLimit && sent >= stopAt) || next == "" {
			return nil
		}
		if pages == maxPages-1 {
			return emit(source.Warn("newrelic.%s: stopped at the %d-page cap; results may be incomplete — add filters or a LIMIT", table, maxPages))
		}
		cursor = next
	}
}

// pageLimit reports how many rows to fetch before stopping early, when a LIMIT
// was pushed and safe (with an OFFSET the connector must fetch limit+offset so
// SQLite can re-apply the OFFSET).
func pageLimit(req source.ScanRequest, safe bool) (stopAt int, pushLimit bool) {
	if req.Limit == nil || !safe {
		return 0, false
	}
	stopAt = *req.Limit
	if req.Offset != nil {
		stopAt += *req.Offset
	}
	return stopAt, true
}

// limitSafe reports whether a pushed LIMIT may stop pagination early: no ORDER
// BY, and every filter an equality on one of the columns whose push is exact
// (consumed). Curated searches that only narrow (entity search) pass no columns
// here, so any filter blocks the early stop.
func limitSafe(req source.ScanRequest, consumed ...string) bool {
	if len(req.OrderBy) != 0 {
		return false
	}
	set := make(map[string]bool, len(consumed))
	for _, c := range consumed {
		set[c] = true
	}
	for _, f := range req.Filters {
		if f.Op != sqlparse.OpEq || !set[f.Column] {
			return false
		}
	}
	return true
}

func colNames(cols []source.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}
