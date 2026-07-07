// Package jira is a dfetch Connector backed by the Jira Cloud REST API
// (https://developer.atlassian.com/cloud/jira/platform/rest/v3/). It exposes
// the issues, projects, and comments tables under a configured SQL schema and
// pushes down what it safely can: issues translates the WHERE/ORDER BY into JQL
// for the enhanced search endpoint; projects and comments push equality/IN
// filters, a single-term ORDER BY, and LIMIT. It is config-only (`type: jira`):
// there is no default host, every site is its own
// https://<yoursite>.atlassian.net.
//
// Auth is required, as a single Authorization header: either $JIRA_EMAIL +
// $JIRA_API_TOKEN (sent as HTTP Basic) or params.auth_header_command, whose
// output is used verbatim. With neither configured, requests fail with a
// message naming both options; a 401 response is reported with the same hint.
package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dmashuda/dfetch/source"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// maxPages caps pagination when a LIMIT can't be pushed safely, so an unbounded
// query doesn't page through an entire site's history.
const maxPages = 10

// defaultPageSize is the per-request page size when no LIMIT is pushed (or the
// pushed LIMIT exceeds one page).
const defaultPageSize = 100

// Connector talks to one Jira Cloud site's REST API.
type Connector struct {
	client            *http.Client
	baseURL           string
	authHeaderCommand []string

	// authHeader is set eagerly in New when $JIRA_EMAIL/$JIRA_API_TOKEN are
	// present (cheap: no process spawn); auth_header_command is deferred to
	// first use via authOnce so a query that never touches jira.* doesn't shell
	// out.
	authOnce   sync.Once
	authHeader string
	authErr    error
}

// New builds a Jira connector. Required: params["base_url"] (e.g.
// https://yoursite.atlassian.net). Optional: params["auth_header_command"] (argv
// whose stdout is used verbatim as the Authorization header when
// $JIRA_EMAIL/$JIRA_API_TOKEN are unset). It is config-only — registered for
// `type: jira`, never auto-instantiated, because there is no default host.
func New(params map[string]any) (source.Connector, error) {
	baseURL, _ := params["base_url"].(string)
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("jira: params.base_url is required (e.g. https://yoursite.atlassian.net)")
	}

	c := &Connector{
		// otelhttp.NewTransport adds an OpenTelemetry client span per request
		// (a no-op until a tracer provider is installed).
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		baseURL:           strings.TrimSuffix(baseURL, "/"),
		authHeaderCommand: []string{},
	}
	if email, token := os.Getenv("JIRA_EMAIL"), os.Getenv("JIRA_API_TOKEN"); email != "" && token != "" {
		c.authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))
	}
	if raw, ok := params["auth_header_command"]; ok {
		cmd, err := stringListParam("auth_header_command", raw)
		if err != nil {
			return nil, err
		}
		c.authHeaderCommand = cmd
	}
	return c, nil
}

// getAuthHeader resolves the Authorization header once, on first use.
// Connectors are built eagerly for every query (engine.New), so
// auth_header_command is deferred to Scan to avoid shelling out on queries that
// never touch jira.*. All paths go through authOnce — the engine Scans jira
// tables concurrently, and Once.Do is what makes the closure's write of
// c.authHeader visible to every other goroutine's read (a bare fast-path read
// would race with the writer).
func (c *Connector) getAuthHeader(ctx context.Context) (string, error) {
	c.authOnce.Do(func() {
		if c.authHeader != "" || len(c.authHeaderCommand) == 0 {
			return // env-resolved in New, or no auth configured
		}
		c.authHeader, c.authErr = runHeaderCommand(ctx, c.authHeaderCommand)
	})
	return c.authHeader, c.authErr
}

// runHeaderCommand runs cmd and returns its stdout (trailing newline trimmed),
// used verbatim as the Authorization header value.
func runHeaderCommand(ctx context.Context, cmd []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// #nosec G204 -- auth_header_command is explicit user configuration and is
	// run directly without a shell.
	out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			stderr := strings.TrimRight(string(ee.Stderr), "\n")
			if stderr != "" {
				return "", fmt.Errorf("auth_header_command %q: %w: %s", cmd, err, stderr)
			}
		}
		return "", fmt.Errorf("auth_header_command %q: %w", cmd, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func stringListParam(name string, raw any) ([]string, error) {
	switch v := raw.(type) {
	case []string:
		return cleanStringList(name, v)
	case []any:
		items := make([]string, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("jira: %s[%d] must be a string", name, i)
			}
			items[i] = s
		}
		return cleanStringList(name, items)
	default:
		return nil, fmt.Errorf("jira: %s must be a list of strings", name)
	}
}

func cleanStringList(name string, items []string) ([]string, error) {
	out := make([]string, 0, len(items))
	for i, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("jira: %s[%d] must not be empty", name, i)
		}
		out = append(out, item)
	}
	return out, nil
}

// Tables returns the schemas of the jira.* tables.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "issues", Columns: issuesCols},
		{Name: "projects", Columns: projectsCols},
		{Name: "comments", Columns: commentsCols},
	}
}

// Scan dispatches to the per-table fetchers, which emit one chunk per API page.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	if _, err := c.getAuthHeader(ctx); err != nil {
		return err
	}
	switch req.Table {
	case "issues":
		return c.scanIssues(ctx, req, emit)
	case "projects":
		return c.scanProjects(ctx, req, emit)
	case "comments":
		return c.scanComments(ctx, req, emit)
	default:
		return fmt.Errorf("jira: unknown table %q", req.Table)
	}
}

// --- HTTP ---

// jiraErrorBody is Jira's error response shape:
// {"errorMessages": ["..."], "errors": {"field": "..."}}.
type jiraErrorBody struct {
	ErrorMessages []string          `json:"errorMessages"`
	Errors        map[string]string `json:"errors"`
}

// apiMessage extracts a readable message from a non-2xx Jira response body, with
// a hint toward the auth env vars on a 401.
func apiMessage(status int, body []byte) string {
	msg := strings.TrimSpace(string(body))
	var e jiraErrorBody
	if json.Unmarshal(body, &e) == nil {
		parts := make([]string, 0, len(e.ErrorMessages)+len(e.Errors))
		parts = append(parts, e.ErrorMessages...)
		for field, m := range e.Errors {
			parts = append(parts, field+": "+m)
		}
		if len(parts) > 0 {
			msg = strings.Join(parts, "; ")
		}
	}
	if status == http.StatusUnauthorized {
		msg += " (check $JIRA_EMAIL / $JIRA_API_TOKEN, or auth_header_command)"
	}
	return msg
}

// doJSON issues method to rawurl (with an optional JSON-encoded body) and
// decodes the response into v.
func (c *Connector) doJSON(ctx context.Context, method, rawurl string, body []byte, v any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawurl, rdr)
	if err != nil {
		return fmt.Errorf("jira: %s %s: %w", method, rawurl, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	authHeader, err := c.getAuthHeader(ctx)
	if err != nil {
		return err
	}
	if authHeader == "" {
		return errors.New("jira: no credentials configured; set $JIRA_EMAIL + $JIRA_API_TOKEN or params.auth_header_command")
	}
	req.Header.Set("Authorization", authHeader)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("jira: %s %s: %w", method, rawurl, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("jira: %s %s: reading response: %w", method, rawurl, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("jira: %s %s: %s: %s", method, rawurl, resp.Status, apiMessage(resp.StatusCode, respBody))
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, v); err != nil {
		return fmt.Errorf("jira: decoding %s %s: %w", method, rawurl, err)
	}
	return nil
}

// getJSON issues a GET to rawurl and decodes the response into v.
func (c *Connector) getJSON(ctx context.Context, rawurl string, v any) error {
	return c.doJSON(ctx, http.MethodGet, rawurl, nil, v)
}

// postJSON issues a POST to rawurl with reqBody marshaled as JSON and decodes
// the response into v.
func (c *Connector) postJSON(ctx context.Context, rawurl string, reqBody, v any) error {
	b, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("jira: encoding request to %s: %w", rawurl, err)
	}
	return c.doJSON(ctx, http.MethodPost, rawurl, b, v)
}

// --- filter / order / paging helpers ---

// stringEqOrIn returns the string values of an equality or IN filter on col.
func stringEqOrIn(req source.ScanRequest, col string) []string {
	f, ok := req.Filter(col)
	if !ok {
		return nil
	}
	switch f.Op {
	case source.OpEq:
		if s, ok := f.Value.(string); ok && s != "" {
			return []string{s}
		}
	case source.OpIn:
		out := make([]string, 0, len(f.Values))
		for _, v := range f.Values {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// orderParam maps the first ORDER BY term to the ready-to-send orderBy query
// value ("field", or "-field" for descending); ok is false when it can't be
// mapped or there's more than one term (the projects/comments endpoints only
// sort by a single field).
func orderParam(terms []source.OrderTerm, allowed map[string]string) (orderBy string, ok bool) {
	if len(terms) != 1 {
		return "", false
	}
	field, ok := allowed[terms[0].Column]
	if !ok {
		return "", false
	}
	if terms[0].Desc {
		return "-" + field, true
	}
	return field, true
}

// pageLimit reports how many rows to fetch before stopping early (stopAt), and
// whether a pushed LIMIT is safe to rely on for that (pushLimit). safe is the
// caller's determination that every filter was consumed by the upstream request
// and the ordering was fully honored. With an OFFSET the connector must still
// fetch limit+offset rows so SQLite can apply the OFFSET locally.
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

// pageSize picks maxResults for one request: a full page normally, or shrunk to
// stopAt when a pushed LIMIT is smaller than a page (never below 1).
func pageSize(stopAt int, pushLimit bool) int {
	if !pushLimit || stopAt >= defaultPageSize {
		return defaultPageSize
	}
	if stopAt < 1 {
		return 1
	}
	return stopAt
}

// pageCapped emits a truncation warning when an unbounded scan (no pushed
// LIMIT) hits the maxPages cap with more pages still available.
func pageCapped(pushLimit bool, pages int, hasNext bool, table string, emit func(*source.Rows) error) error {
	if pushLimit || pages != maxPages-1 || !hasNext {
		return nil
	}
	return emit(source.Warn("jira.%s: stopped at the %d-page cap; results may be incomplete — add a LIMIT or narrower filters", table, maxPages))
}

func colNames(cols []source.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

func escapePath(s string) string { return url.PathEscape(s) }
