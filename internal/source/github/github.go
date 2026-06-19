// Package github is a dfetch Connector backed by the GitHub REST API. It exposes
// the issues, pulls, and repos tables under the SQL schema "github" and pushes
// down equality filters, ORDER BY, and LIMIT to the API where it can.
package github

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

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
)

const defaultBaseURL = "https://api.github.com"

// maxPages caps pagination when a LIMIT can't be pushed safely, so an unbounded
// query doesn't fetch the entire repository.
const maxPages = 10

// Connector talks to the GitHub REST API.
type Connector struct {
	client  *http.Client
	baseURL string
	token   string
}

// New builds a GitHub connector. Supported params: "base_url" (override the API
// host, used in tests/Enterprise). The token comes from $GITHUB_TOKEN or
// $GH_TOKEN; unauthenticated requests work but are heavily rate-limited.
func New(params map[string]any) (source.Connector, error) {
	c := &Connector{
		client:  &http.Client{Timeout: 30 * time.Second},
		baseURL: defaultBaseURL,
		token:   firstEnv("GITHUB_TOKEN", "GH_TOKEN"),
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

// Tables returns the schemas of the github.* tables.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "issues", Columns: issuesCols},
		{Name: "pulls", Columns: pullsCols},
		{Name: "repos", Columns: reposCols},
	}
}

// Scan dispatches to the per-table fetchers.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest) (*source.Rows, error) {
	switch req.Table {
	case "issues":
		return c.scanIssues(ctx, req)
	case "pulls":
		return c.scanPulls(ctx, req)
	case "repos":
		return c.scanRepos(ctx, req)
	default:
		return nil, fmt.Errorf("github: unknown table %q", req.Table)
	}
}

// --- HTTP ---

// getJSON fetches rawurl, decodes the body into v, and returns the URL of the
// next page (from the Link header) if any.
func (c *Connector) getJSON(ctx context.Context, rawurl string, v any) (next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
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
	if !ok || f.Op != sqlparse.OpEq {
		return "", false
	}
	s, ok := f.Value.(string)
	return s, ok
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

// pageLimit decides per_page and whether LIMIT can be pushed. Pushing LIMIT is
// only safe when the API returns exactly the rows the query would keep — i.e.
// every filter was consumed by the endpoint AND the ordering was honored (or
// there is no ORDER BY). The caller passes that determination as safe.
func pageLimit(req source.ScanRequest, safe bool) (perPage int, pushLimit bool) {
	pushLimit = req.Limit != nil && safe
	perPage = 100
	if pushLimit && *req.Limit < perPage {
		perPage = *req.Limit
	}
	if perPage < 1 {
		perPage = 1
	}
	return perPage, pushLimit
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
		if f.Op != sqlparse.OpEq || !set[f.Column] {
			return false
		}
	}
	return true
}

// limitSafe combines the ordering and filter conditions under which LIMIT may be
// pushed.
func limitSafe(req source.ScanRequest, sortMapped bool, allowed ...string) bool {
	return (len(req.OrderBy) == 0 || sortMapped) && consumedAll(req, allowed...)
}

func escapePath(s string) string { return url.PathEscape(s) }
