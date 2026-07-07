// Package github is a dfetch Connector backed by the GitHub REST API. It exposes
// the issues, pulls, repos, commits, releases, workflow_runs, and artifacts tables
// under the SQL schema "github" and pushes down equality filters, ORDER BY, and
// LIMIT to the API where it can.
package github

import (
	"context"
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

	"github.com/dmashuda/dfetch/internal/source"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const defaultBaseURL = "https://api.github.com"

// maxPages caps pagination when a LIMIT can't be pushed safely, so an unbounded
// query doesn't fetch the entire repository.
const maxPages = 10

// Connector talks to the GitHub REST API.
type Connector struct {
	client       *http.Client
	baseURL      string
	tokenCommand []string

	tokenOnce sync.Once
	token     string
	tokenErr  error
}

// New builds a GitHub connector. Supported params: "base_url" (override the API
// host, used in tests/Enterprise) and "token_command" (fallback argv used to
// fetch a token when $GITHUB_TOKEN / $GH_TOKEN are unset).
func New(params map[string]any) (source.Connector, error) {
	c := &Connector{
		// otelhttp.NewTransport adds an OpenTelemetry client span per request
		// (a no-op until a tracer provider is installed).
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		baseURL:      defaultBaseURL,
		tokenCommand: []string{},
		token:        firstEnv("GITHUB_TOKEN", "GH_TOKEN"),
	}
	if bu, ok := params["base_url"].(string); ok && bu != "" {
		c.baseURL = strings.TrimSuffix(bu, "/")
	}
	if raw, ok := params["token_command"]; ok {
		cmd, err := stringListParam("token_command", raw)
		if err != nil {
			return nil, err
		}
		c.tokenCommand = cmd
	}
	return c, nil
}

// getToken resolves the token once, on first use. Connectors are built eagerly
// for every query (engine.New), so token_command is deferred to Scan to avoid
// shelling out on queries that never touch github.*.
func (c *Connector) getToken(ctx context.Context) (string, error) {
	if c.token != "" || len(c.tokenCommand) == 0 {
		return c.token, nil
	}
	c.tokenOnce.Do(func() { c.token, c.tokenErr = runTokenCommand(ctx, c.tokenCommand) })
	return c.token, c.tokenErr
}

func runTokenCommand(ctx context.Context, cmd []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// #nosec G204 -- token_command is explicit user configuration and is run
	// directly without a shell.
	out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			stderr := strings.TrimRight(string(ee.Stderr), "\n")
			if stderr != "" {
				return "", fmt.Errorf("token_command %q: %w: %s", cmd, err, stderr)
			}
		}
		return "", fmt.Errorf("token_command %q: %w", cmd, err)
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
				return nil, fmt.Errorf("github: %s[%d] must be a string", name, i)
			}
			items[i] = s
		}
		return cleanStringList(name, items)
	default:
		return nil, fmt.Errorf("github: %s must be a list of strings", name)
	}
}

func cleanStringList(name string, items []string) ([]string, error) {
	out := make([]string, 0, len(items))
	for i, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("github: %s[%d] must not be empty", name, i)
		}
		out = append(out, item)
	}
	return out, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Tables returns the schemas of the github.* tables.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "issues", Columns: issuesCols},
		{Name: "pulls", Columns: pullsCols},
		{Name: "repos", Columns: reposCols},
		{Name: "commits", Columns: commitsCols},
		{Name: "releases", Columns: releasesCols},
		{Name: "workflow_runs", Columns: workflowRunsCols},
		{Name: "artifacts", Columns: artifactsCols},
	}
}

// Scan dispatches to the per-table fetchers, which emit one chunk per API page.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	if _, err := c.getToken(ctx); err != nil {
		return err
	}
	switch req.Table {
	case "issues":
		return c.scanIssues(ctx, req, emit)
	case "pulls":
		return c.scanPulls(ctx, req, emit)
	case "repos":
		return c.scanRepos(ctx, req, emit)
	case "commits":
		return c.scanCommits(ctx, req, emit)
	case "releases":
		return c.scanReleases(ctx, req, emit)
	case "workflow_runs":
		return c.scanWorkflowRuns(ctx, req, emit)
	case "artifacts":
		return c.scanArtifacts(ctx, req, emit)
	default:
		return fmt.Errorf("github: unknown table %q", req.Table)
	}
}

// --- HTTP ---

// getJSON fetches rawurl, decodes the body into v, and returns the URL of the
// next page (from the Link header) if any.
func (c *Connector) getJSON(ctx context.Context, rawurl string, v any) (next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", fmt.Errorf("github: GET %s: %w", rawurl, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: GET %s: %w", rawurl, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("github: GET %s: reading response: %w", rawurl, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("github: GET %s: %s: %s", rawurl, resp.Status, apiMessage(body))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return "", fmt.Errorf("github: decoding %s: %w", rawurl, err)
	}
	return nextLink(resp.Header.Get("Link")), nil
}

// apiMessage extracts the "message" field from a GitHub error body.
func apiMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	return strings.TrimSpace(string(body))
}

// nextLink returns the rel="next" URL from a Link header, or "".
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		isNext := false
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				isNext = true
			}
		}
		if isNext {
			u := strings.TrimSpace(segs[0])
			return strings.TrimSuffix(strings.TrimPrefix(u, "<"), ">")
		}
	}
	return ""
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

// intEq returns the int64 value of an equality filter on col, if present. Numeric
// literals arrive as int64 from the planner.
func intEq(req source.ScanRequest, col string) (int64, bool) {
	f, ok := req.Filter(col)
	if !ok || f.Op != source.OpEq {
		return 0, false
	}
	n, ok := f.Value.(int64)
	return n, ok
}

// requireStringEq is stringEq but returns a helpful error when the filter is
// absent (e.g. owner/repo are required path params for an endpoint).
func requireStringEq(req source.ScanRequest, col string) (string, error) {
	s, ok := stringEq(req, col)
	if !ok {
		return "", fmt.Errorf("github.%s requires %s = '...' in the WHERE clause", req.Table, col)
	}
	return s, nil
}

// orderParam maps the first ORDER BY term to a GitHub (sort, direction) pair
// using the allowed sort fields; ok is false when it can't be mapped.
func orderParam(terms []source.OrderTerm, allowed map[string]string) (sort, direction string, ok bool) {
	if len(terms) == 0 {
		return "", "", false
	}
	s, ok := allowed[terms[0].Column]
	if !ok {
		return "", "", false
	}
	direction = "asc"
	if terms[0].Desc {
		direction = "desc"
	}
	return s, direction, true
}

// pageLimit decides per_page, how many rows to fetch before stopping (stopAt),
// and whether LIMIT can be pushed. Pushing LIMIT is only safe when the API
// returns exactly the rows the query would keep — i.e. every filter was consumed
// by the endpoint AND the ordering was honored (or there is no ORDER BY); the
// caller passes that determination as safe. When an OFFSET is present the
// connector must still fetch limit+offset rows so SQLite can apply the OFFSET,
// so stopAt accounts for it. stopAt is 0 (no early stop) when LIMIT is not pushed.
func pageLimit(req source.ScanRequest, safe bool) (perPage, stopAt int, pushLimit bool) {
	pushLimit = req.Limit != nil && safe
	if !pushLimit {
		return 100, 0, false
	}
	stopAt = *req.Limit
	if req.Offset != nil {
		stopAt += *req.Offset
	}
	perPage = 100
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
// pushed. The API honors a single sort field, so the ordering is fully honored
// only when there is no ORDER BY or exactly one term that maps; a multi-key
// ORDER BY cannot be honored and must not push LIMIT.
func limitSafe(req source.ScanRequest, sortMapped bool, allowed ...string) bool {
	orderHonored := len(req.OrderBy) == 0 || (len(req.OrderBy) == 1 && sortMapped)
	return orderHonored && consumedAll(req, allowed...)
}

func escapePath(s string) string { return url.PathEscape(s) }
