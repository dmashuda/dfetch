// Package discord is a dfetch Connector backed by the Discord REST API. It
// exposes the channels, members, messages, and threads tables under the SQL
// schema "discord" and pushes down what it safely can (required path filters, a
// message id range, and LIMIT). Auth is a single Authorization header: either
// $DISCORD_TOKEN (a bare bot token, sent as "Bot <token>") or
// params.auth_header_command, whose output is used verbatim.
package discord

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
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const defaultBaseURL = "https://discord.com/api/v10"

// userAgent identifies the client to Discord, which requires a User-Agent on API
// requests.
const userAgent = "DiscordBot (https://github.com/dmashuda/dfetch, 1.0)"

// maxPages caps pagination when a LIMIT can't be pushed safely, so an unbounded
// query doesn't page through an entire guild's history.
const maxPages = 10

// Connector talks to the Discord REST API.
type Connector struct {
	client            *http.Client
	baseURL           string
	authHeaderCommand []string

	authOnce   sync.Once
	authHeader string
	authErr    error
}

// New builds a Discord connector. Supported params: "base_url" (override the API
// host, used in tests) and "auth_header_command" (argv whose stdout is used
// verbatim as the Authorization header when $DISCORD_TOKEN is unset).
func New(params map[string]any) (source.Connector, error) {
	c := &Connector{
		// otelhttp.NewTransport adds an OpenTelemetry client span per request
		// (a no-op until a tracer provider is installed).
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		baseURL:           defaultBaseURL,
		authHeaderCommand: []string{},
	}
	// $DISCORD_TOKEN holds a bare bot token; Discord expects it as "Bot <token>".
	if tok := firstEnv("DISCORD_TOKEN"); tok != "" {
		c.authHeader = "Bot " + tok
	}
	if bu, ok := params["base_url"].(string); ok && bu != "" {
		c.baseURL = strings.TrimSuffix(bu, "/")
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

// getAuthHeader resolves the Authorization header once, on first use. Connectors
// are built eagerly for every query (engine.New), so auth_header_command is
// deferred to Scan to avoid shelling out on queries that never touch discord.*.
func (c *Connector) getAuthHeader(ctx context.Context) (string, error) {
	if c.authHeader != "" || len(c.authHeaderCommand) == 0 {
		return c.authHeader, nil
	}
	c.authOnce.Do(func() { c.authHeader, c.authErr = runAuthHeaderCommand(ctx, c.authHeaderCommand) })
	return c.authHeader, c.authErr
}

// runAuthHeaderCommand runs cmd and returns its stdout (trailing newline trimmed)
// as the full Authorization header value, used verbatim by the connector.
func runAuthHeaderCommand(ctx context.Context, cmd []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// #nosec G204 -- auth_header_command is explicit user configuration and is run
	// directly without a shell.
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
				return nil, fmt.Errorf("discord: %s[%d] must be a string", name, i)
			}
			items[i] = s
		}
		return cleanStringList(name, items)
	default:
		return nil, fmt.Errorf("discord: %s must be a list of strings", name)
	}
}

func cleanStringList(name string, items []string) ([]string, error) {
	out := make([]string, 0, len(items))
	for i, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("discord: %s[%d] must not be empty", name, i)
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

// Tables returns the schemas of the discord.* tables.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "channels", Columns: channelsCols},
		{Name: "members", Columns: membersCols},
		{Name: "messages", Columns: messagesCols},
		{Name: "threads", Columns: threadsCols},
	}
}

// Scan dispatches to the per-table fetchers, which emit one chunk per API page.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	if _, err := c.getAuthHeader(ctx); err != nil {
		return err
	}
	switch req.Table {
	case "channels":
		return c.scanChannels(ctx, req, emit)
	case "members":
		return c.scanMembers(ctx, req, emit)
	case "messages":
		return c.scanMessages(ctx, req, emit)
	case "threads":
		return c.scanThreads(ctx, req, emit)
	default:
		return fmt.Errorf("discord: unknown table %q", req.Table)
	}
}

// --- HTTP ---

// get issues a GET to path with query q and decodes the body into v. Discord has
// no success envelope, so a non-2xx status is the error signal; the JSON error
// body's "message" is surfaced.
func (c *Connector) get(ctx context.Context, path string, q url.Values, v any) error {
	rawurl := c.baseURL + path
	if enc := q.Encode(); enc != "" {
		rawurl += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return fmt.Errorf("discord: GET %s: %w", path, err)
	}
	req.Header.Set("User-Agent", userAgent)
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("discord: GET %s: reading response: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord: GET %s: %s: %s", path, resp.Status, apiMessage(body))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("discord: decoding %s: %w", path, err)
	}
	return nil
}

// apiMessage extracts the "message" field from a Discord error body.
func apiMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	return strings.TrimSpace(string(body))
}

// cursorPages runs an id-cursor-paginated endpoint: it calls fetch once per page
// (passing the cursor id for the next page, "" for the first), emits one chunk
// per page, honors a pushed LIMIT (stopAt), and caps an unbounded scan at
// maxPages with a truncation warning. fetch returns "" as the next cursor when
// the page is the last one.
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
			return emit(source.Warn("discord.%s: stopped at the %d-page cap; results may be incomplete — add a LIMIT or narrower filters", table, maxPages))
		}
	}
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
// absent (e.g. guild_id / channel are required API path params).
func requireStringEq(req source.ScanRequest, col string) (string, error) {
	s, ok := stringEq(req, col)
	if !ok {
		return "", fmt.Errorf("discord.%s requires %s = '...' in the WHERE clause", req.Table, col)
	}
	return s, nil
}

// pageLimit decides the per-page size, how many rows to fetch before stopping
// (stopAt), and whether LIMIT can be pushed. Pushing LIMIT is only safe when the
// API returns exactly the rows the query would keep; the caller passes that
// determination as safe. An OFFSET still requires fetching limit+offset rows so
// SQLite can apply it. stopAt is 0 (no early stop) when LIMIT is not pushed.
// maxSize is the endpoint's maximum page size.
func pageLimit(req source.ScanRequest, safe bool, maxSize int) (perPage, stopAt int, pushLimit bool) {
	pushLimit = req.Limit != nil && safe
	if !pushLimit {
		return maxSize, 0, false
	}
	stopAt = *req.Limit
	if req.Offset != nil {
		stopAt += *req.Offset
	}
	perPage = maxSize
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
		if f.Op != sqlparse.OpEq || !set[f.Column] {
			return false
		}
	}
	return true
}

// limitSafe reports whether LIMIT may be pushed for the endpoints with a fixed
// server-side order: only when there is no ORDER BY and every filter is a
// consumed equality on an allowed column.
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
