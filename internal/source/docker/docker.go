// Package docker is a dfetch Connector backed by the Docker Engine API. It
// exposes the containers, images, volumes, and networks tables under the SQL
// schema "docker", talking to the local daemon over its unix socket.
//
// The connector does no push-down: each scan fetches the full resource list and
// emits it as one chunk, and SQLite re-applies the query. Returning a superset
// is always correct, and the Docker list endpoints are small and unpaginated.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dmashuda/dfetch/internal/source"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// defaultSocket is the local Docker daemon's unix socket.
const defaultSocket = "/var/run/docker.sock"

// Connector talks to the Docker Engine API. baseURL is a dummy host
// ("http://docker") when dialing the unix socket, or a real http(s) URL when a
// base_url override points at a TCP daemon or a test server.
type Connector struct {
	client  *http.Client
	baseURL string
}

// New builds a Docker connector. Supported params: "socket" (override the unix
// socket path) and "base_url" (an http(s) URL to a TCP daemon or test server,
// which takes precedence over the socket). New(nil) defaults to the local
// socket and never dials, so dfetch works even when Docker isn't running — only
// docker.* queries fail.
func New(params map[string]any) (source.Connector, error) {
	if bu, ok := params["base_url"].(string); ok && bu != "" {
		return &Connector{
			client: &http.Client{
				Timeout:   30 * time.Second,
				Transport: otelhttp.NewTransport(http.DefaultTransport),
			},
			baseURL: strings.TrimSuffix(bu, "/"),
		}, nil
	}

	socket := defaultSocket
	if s, ok := params["socket"].(string); ok && s != "" {
		socket = strings.TrimPrefix(s, "unix://")
	}
	// The dialer ignores the request's host:port and always dials the socket, so
	// any baseURL host works; "http://docker" keeps request URLs readable in traces.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &Connector{
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(transport),
		},
		baseURL: "http://docker",
	}, nil
}

// Tables returns the schemas of the docker.* tables.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "containers", Columns: containersCols},
		{Name: "images", Columns: imagesCols},
		{Name: "volumes", Columns: volumesCols},
		{Name: "networks", Columns: networksCols},
	}
}

// Scan dispatches to the per-table fetchers. Each emits the full list as one chunk.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	switch req.Table {
	case "containers":
		return c.scanContainers(ctx, emit)
	case "images":
		return c.scanImages(ctx, emit)
	case "volumes":
		return c.scanVolumes(ctx, emit)
	case "networks":
		return c.scanNetworks(ctx, emit)
	default:
		return fmt.Errorf("docker: unknown table %q", req.Table)
	}
}

// getJSON fetches path (relative to baseURL) and decodes the body into v.
func (c *Connector) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("docker: GET %s: %s: %s", path, resp.Status, apiMessage(body))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("docker: decoding %s: %w", path, err)
	}
	return nil
}

// apiMessage extracts the "message" field from a Docker error body, falling back
// to the raw body.
func apiMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	return strings.TrimSpace(string(body))
}
