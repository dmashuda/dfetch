package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestConnector spins up an httptest server and points the connector at it
// via base_url (bypassing the unix socket).
func newTestConnector(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL})
	require.NoError(t, err)
	return c.(*Connector)
}

// collectScan runs Scan and accumulates every emitted chunk into one Rows.
func collectScan(c source.Connector, req source.ScanRequest) (*source.Rows, error) {
	rows := &source.Rows{}
	err := c.Scan(context.Background(), req, func(chunk *source.Rows) error {
		if rows.Columns == nil {
			rows.Columns = chunk.Columns
		}
		rows.Rows = append(rows.Rows, chunk.Rows...)
		return nil
	})
	return rows, err
}

func TestTables(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	tables := c.Tables()

	got := make(map[string][]string, len(tables))
	for _, ts := range tables {
		got[ts.Name] = ts.ColumnNames()
	}
	assert.ElementsMatch(t, []string{"containers", "images", "volumes", "networks"}, keys(got))
	assert.Equal(t, []string{
		"id", "name", "image", "image_id", "command", "created",
		"state", "status", "ports", "labels", "mounts",
	}, got["containers"])
	assert.Equal(t, []string{"name", "driver", "mountpoint", "created_at", "scope", "labels", "options"}, got["volumes"])
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestScanContainers(t *testing.T) {
	var gotPath string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		_, _ = w.Write([]byte(`[
			{"Id":"abc","Names":["/web"],"Image":"nginx","ImageID":"sha256:1","Command":"nginx","Created":1700000000,"State":"running","Status":"Up 2 hours","Ports":[{"PrivatePort":80}],"Labels":{"com.docker.compose.project":"demo"},"Mounts":[{"Type":"volume"}]},
			{"Id":"def","Names":["/db"],"Image":"postgres","ImageID":"sha256:2","Command":"postgres","Created":1700000100,"State":"exited","Status":"Exited (0)","Labels":{}}
		]`))
	})

	rows, err := collectScan(c, source.ScanRequest{Table: "containers"})
	require.NoError(t, err)
	assert.Equal(t, "/containers/json?all=true", gotPath) // stopped containers included

	require.Len(t, rows.Rows, 2)
	first := rows.Rows[0]
	assert.Equal(t, "abc", first[0])
	assert.Equal(t, "web", first[1]) // leading slash trimmed
	assert.Equal(t, "nginx", first[2])
	assert.Equal(t, int64(1700000000), first[5])
	assert.Equal(t, "running", first[6])
	assert.Contains(t, first[9], `"com.docker.compose.project":"demo"`) // labels JSON column
}

func TestScanImages(t *testing.T) {
	var gotPath string
	c := newTestConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		_, _ = w.Write([]byte(`[
			{"Id":"sha256:img1","ParentId":"","RepoTags":["nginx:latest"],"RepoDigests":["nginx@sha256:dd"],"Created":1699999999,"Size":12345,"SharedSize":-1,"Containers":2,"Labels":null}
		]`))
	})

	rows, err := collectScan(c, source.ScanRequest{Table: "images"})
	require.NoError(t, err)
	assert.Contains(t, gotPath, "shared-size=true") // so SharedSize isn't always -1
	require.Len(t, rows.Rows, 1)
	r := rows.Rows[0]
	assert.Equal(t, "sha256:img1", r[0])
	assert.Contains(t, r[2], "nginx:latest") // repo_tags JSON
	assert.Equal(t, int64(12345), r[5])      // size
	assert.Equal(t, int64(2), r[7])          // containers
	assert.Nil(t, r[8])                      // null labels -> NULL
}

func TestScanVolumesUnwrapsEnvelope(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Volumes":[
			{"Name":"data","Driver":"local","Mountpoint":"/var/lib/docker/volumes/data/_data","CreatedAt":"2026-01-01T00:00:00Z","Scope":"local","Labels":{"k":"v"},"Options":null}
		],"Warnings":null}`))
	})

	rows, err := collectScan(c, source.ScanRequest{Table: "volumes"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	r := rows.Rows[0]
	assert.Equal(t, "data", r[0])
	assert.Equal(t, "local", r[1])
	assert.Contains(t, r[5], `"k":"v"`) // labels JSON
}

func TestScanNetworks(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"Id":"net1","Name":"bridge","Driver":"bridge","Scope":"local","Created":"2026-01-01T00:00:00Z","Internal":true,"Attachable":false,"IPAM":{"Driver":"default"},"Labels":{},"Options":{}}
		]`))
	})

	rows, err := collectScan(c, source.ScanRequest{Table: "networks"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	r := rows.Rows[0]
	assert.Equal(t, "bridge", r[1])
	assert.Equal(t, int64(1), r[5])                // internal -> 1
	assert.Equal(t, int64(0), r[6])                // attachable -> 0
	assert.Contains(t, r[7], `"Driver":"default"`) // ipam JSON
}

func TestScanAPIError(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})

	_, err := collectScan(c, source.ScanRequest{Table: "containers"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

// An empty resource list must not emit a zero-row chunk (README connector contract).
func TestScanEmptyListEmitsNothing(t *testing.T) {
	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})

	emitted := 0
	err := c.Scan(context.Background(), source.ScanRequest{Table: "containers"}, func(*source.Rows) error {
		emitted++
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, emitted, "no chunk should be emitted for an empty list")
}

func TestScanUnknownTable(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	_, err = collectScan(c, source.ScanRequest{Table: "nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}
