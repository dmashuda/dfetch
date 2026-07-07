// Package slack is a dfetch Connector backed by the Slack Web API. It exposes the
// channels, users, messages, and search tables under the SQL schema "slack" and
// pushes down equality/range filters and LIMIT to the API where it can. Auth is a
// single Authorization header: either $SLACK_TOKEN (a bare token, sent as
// "Bearer <token>") or params.auth_header_command, whose output is used verbatim.
//
// Browser session tokens (xoxc-...) additionally require Slack's "d" cookie. Set
// it with either $SLACK_COOKIE (the bare cookie value, sent as "Cookie: d=<value>")
// or params.cookie_command, whose output is used verbatim as the Cookie header.
package slack

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

const defaultBaseURL = "https://slack.com/api"

// maxPages caps pagination when a LIMIT can't be pushed safely, so an unbounded
// query doesn't page through an entire workspace.
const maxPages = 10

// defaultPageSize is the per-request page size when no LIMIT is pushed. Slack's
// cursor endpoints accept up to ~1000 but recommend <= 200.
const defaultPageSize = 200

// Connector talks to the Slack Web API.
type Connector struct {
	client     *http.Client
	baseURL    string
	authHeader *source.Credential
	cookie     *source.Credential
}

// New builds a Slack connector. Supported params: "base_url" (override the API
// host, used in tests), "auth_header_func"/"auth_header_command" (Go func /
// argv supplying the Authorization header verbatim when $SLACK_TOKEN is
// unset), and "cookie_func"/"cookie_command" (likewise for the Cookie header
// when $SLACK_COOKIE is unset). Both resolve lazily on first use (see
// source.Credential).
func New(params map[string]any) (source.Connector, error) {
	// $SLACK_TOKEN holds a bare token; Slack expects it as a Bearer credential.
	authHeader, err := source.NewCredential("slack", "auth_header", params, "", func() string {
		if tok := os.Getenv("SLACK_TOKEN"); tok != "" {
			return "Bearer " + tok
		}
		return ""
	})
	if err != nil {
		return nil, err
	}
	// $SLACK_COOKIE holds the bare "d" cookie value (xoxd-...); Slack expects it
	// as the "d" cookie.
	cookie, err := source.NewCredential("slack", "cookie", params, "", func() string {
		if ck := os.Getenv("SLACK_COOKIE"); ck != "" {
			return "d=" + ck
		}
		return ""
	})
	if err != nil {
		return nil, err
	}

	c := &Connector{
		// otelhttp.NewTransport adds an OpenTelemetry client span per request
		// (a no-op until a tracer provider is installed).
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		baseURL:    defaultBaseURL,
		authHeader: authHeader,
		cookie:     cookie,
	}
	if bu, ok := params["base_url"].(string); ok && bu != "" {
		c.baseURL = strings.TrimSuffix(bu, "/")
	}
	return c, nil
}

// Tables returns the schemas of the slack.* tables.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "channels", Columns: channelsCols},
		{Name: "users", Columns: usersCols},
		{Name: "messages", Columns: messagesCols},
		{Name: "search", Columns: searchCols},
	}
}

// Scan dispatches to the per-table fetchers, which emit one chunk per API page.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	if _, err := c.authHeader.Get(ctx); err != nil {
		return err
	}
	if _, err := c.cookie.Get(ctx); err != nil {
		return err
	}
	switch req.Table {
	case "channels":
		return c.scanChannels(ctx, req, emit)
	case "users":
		return c.scanUsers(ctx, req, emit)
	case "messages":
		return c.scanMessages(ctx, req, emit)
	case "search":
		return c.scanSearch(ctx, req, emit)
	default:
		return fmt.Errorf("slack: unknown table %q", req.Table)
	}
}

// --- HTTP ---

// envelope is the common header of every Slack Web API response.
type envelope struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Metadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// get issues a GET to method with query q, decodes the body into v, and returns
// the next cursor (response_metadata.next_cursor, "" when there are no more
// pages). It surfaces the Slack-level error (ok:false) as a Go error.
func (c *Connector) get(ctx context.Context, method string, q url.Values, v any) (next string, err error) {
	rawurl := c.baseURL + "/" + method
	if enc := q.Encode(); enc != "" {
		rawurl += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", fmt.Errorf("slack: GET %s: %w", method, err)
	}
	// Both cached after the resolution in Scan; errors surfaced there.
	if h, _ := c.authHeader.Get(ctx); h != "" {
		req.Header.Set("Authorization", h)
	}
	if ck, _ := c.cookie.Get(ctx); ck != "" {
		req.Header.Set("Cookie", ck)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("slack: GET %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("slack: GET %s: reading response: %w", method, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("slack: GET %s: %s", method, resp.Status)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("slack: decoding %s: %w", method, err)
	}
	if !env.OK {
		msg := env.Error
		if msg == "" {
			msg = "request failed"
		}
		return "", fmt.Errorf("slack: %s: %s", method, msg)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return "", fmt.Errorf("slack: decoding %s: %w", method, err)
	}
	return env.Metadata.NextCursor, nil
}

// cursorPages runs a cursor-paginated endpoint: it calls fetch once per page
// (passing the cursor for the next page, "" for the first), emits one chunk per
// page, honors a pushed LIMIT (stopAt), and caps an unbounded scan at maxPages
// with a truncation warning.
func cursorPages(ctx context.Context, table string, cols []source.Column, stopAt int, pushLimit bool,
	fetch func(ctx context.Context, cursor string) (rows [][]any, next string, err error),
	emit func(*source.Rows) error,
) error {
	sent := 0
	for cursor, pages := "", 0; ; pages++ {
		page, next, err := fetch(ctx, cursor)
		if err != nil {
			return err
		}
		if pushLimit && sent+len(page) > stopAt {
			page = page[:stopAt-sent]
		}
		sent += len(page)
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: colNames(cols), Rows: page}); err != nil {
				return err
			}
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
		cursor = next
		if cursor == "" {
			return nil
		}
		if !pushLimit && pages+1 >= maxPages {
			return emit(source.Warn("slack.%s: stopped at the %d-page cap; results may be incomplete — add a LIMIT or narrower filters", table, maxPages))
		}
	}
}

// --- filter / order helpers ---

// stringEq returns the string value of an equality filter on col, if present.
func stringEq(req source.ScanRequest, col string) (string, bool) {
	f, ok := req.Filter(col)
	if !ok || f.Op != source.OpEq {
		return "", false
	}
	s, ok := f.Value.(string)
	return s, ok
}

// requireStringEq is stringEq but returns a helpful error when the filter is
// absent (e.g. channel is a required API param for conversations.history).
func requireStringEq(req source.ScanRequest, col string) (string, error) {
	s, ok := stringEq(req, col)
	if !ok {
		return "", fmt.Errorf("slack.%s requires %s = '...' in the WHERE clause", req.Table, col)
	}
	return s, nil
}

// intEq returns the int64 value of an equality filter on col, if present.
func intEq(req source.ScanRequest, col string) (int64, bool) {
	f, ok := req.Filter(col)
	if !ok || f.Op != source.OpEq {
		return 0, false
	}
	n, ok := f.Value.(int64)
	return n, ok
}

// pageLimit decides the per-page size, how many rows to fetch before stopping
// (stopAt), and whether LIMIT can be pushed. Pushing LIMIT is only safe when the
// API returns exactly the rows the query would keep; the caller passes that
// determination as safe. An OFFSET still requires fetching limit+offset rows so
// SQLite can apply it. stopAt is 0 (no early stop) when LIMIT is not pushed.
func pageLimit(req source.ScanRequest, safe bool) (perPage, stopAt int, pushLimit bool) {
	pushLimit = req.Limit != nil && safe
	if !pushLimit {
		return defaultPageSize, 0, false
	}
	stopAt = *req.Limit
	if req.Offset != nil {
		stopAt += *req.Offset
	}
	perPage = defaultPageSize
	if stopAt < perPage {
		perPage = stopAt
	}
	if perPage < 1 {
		perPage = 1
	}
	return perPage, stopAt, true
}

// consumedAll reports whether every filter in the request is an equality on one
// of the allowed columns (i.e. the endpoint can honor all of them). If not, the
// API result is not the full filtered set and LIMIT must not be pushed.
func consumedAll(req source.ScanRequest, allowed ...string) bool {
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[a] = true
	}
	for _, f := range req.Filters {
		if f.Op != source.OpEq || !set[f.Column] {
			return false
		}
	}
	return true
}

// limitSafe combines the ordering and filter conditions under which LIMIT may be
// pushed for the cursor endpoints that have no server-side sort: the ordering is
// honored only when there is no ORDER BY, and every filter must be a consumed
// equality.
func limitSafe(req source.ScanRequest, allowed ...string) bool {
	return len(req.OrderBy) == 0 && consumedAll(req, allowed...)
}

func colNames(cols []source.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}
