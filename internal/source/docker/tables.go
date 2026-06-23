package docker

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/dmashuda/dfetch/internal/source"
)

func col(name, typ string) source.Column { return source.Column{Name: name, Type: typ} }

func colNames(cols []source.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

// toJSON renders a nested value as a JSON string for a TEXT column, so it can be
// queried later with SQLite's json_extract. A nil value — including a typed-nil
// map/slice, which marshals to the literal "null" — becomes SQL NULL.
func toJSON(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	return string(b)
}

// Column names mirror the Docker Engine API JSON fields where practical; nested
// objects/arrays (ports, labels, mounts, ipam, …) are stored as JSON strings.

var containersCols = []source.Column{
	col("id", "TEXT"), col("name", "TEXT"), col("image", "TEXT"),
	col("image_id", "TEXT"), col("command", "TEXT"), col("created", "INTEGER"),
	col("state", "TEXT"), col("status", "TEXT"),
	col("ports", "TEXT"), col("labels", "TEXT"), col("mounts", "TEXT"),
}

var imagesCols = []source.Column{
	col("id", "TEXT"), col("parent_id", "TEXT"), col("repo_tags", "TEXT"),
	col("repo_digests", "TEXT"), col("created", "INTEGER"), col("size", "INTEGER"),
	col("shared_size", "INTEGER"), col("containers", "INTEGER"), col("labels", "TEXT"),
}

var volumesCols = []source.Column{
	col("name", "TEXT"), col("driver", "TEXT"), col("mountpoint", "TEXT"),
	col("created_at", "TEXT"), col("scope", "TEXT"),
	col("labels", "TEXT"), col("options", "TEXT"),
}

var networksCols = []source.Column{
	col("id", "TEXT"), col("name", "TEXT"), col("driver", "TEXT"),
	col("scope", "TEXT"), col("created", "TEXT"),
	col("internal", "INTEGER"), col("attachable", "INTEGER"),
	col("ipam", "TEXT"), col("labels", "TEXT"), col("options", "TEXT"),
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// --- containers ---

type apiContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Ports   []any             `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
	Mounts  []any             `json:"Mounts"`
}

func (c *Connector) scanContainers(ctx context.Context, emit func(*source.Rows) error) error {
	var cs []apiContainer
	// all=true so stopped containers are included (a superset SQLite can trim).
	if err := c.getJSON(ctx, "/containers/json?all=true", &cs); err != nil {
		return err
	}
	rows := make([][]any, len(cs))
	for i, x := range cs {
		name := ""
		if len(x.Names) > 0 {
			name = strings.TrimPrefix(x.Names[0], "/")
		}
		rows[i] = []any{
			x.ID, name, x.Image, x.ImageID, x.Command, x.Created,
			x.State, x.Status, toJSON(x.Ports), toJSON(x.Labels), toJSON(x.Mounts),
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(containersCols), Rows: rows})
}

// --- images ---

type apiImage struct {
	ID          string            `json:"Id"`
	ParentID    string            `json:"ParentId"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	SharedSize  int64             `json:"SharedSize"`
	Containers  int64             `json:"Containers"`
	Labels      map[string]string `json:"Labels"`
}

func (c *Connector) scanImages(ctx context.Context, emit func(*source.Rows) error) error {
	var imgs []apiImage
	// shared-size=true populates SharedSize; without it Docker returns -1 for every image.
	if err := c.getJSON(ctx, "/images/json?shared-size=true", &imgs); err != nil {
		return err
	}
	rows := make([][]any, len(imgs))
	for i, x := range imgs {
		rows[i] = []any{
			x.ID, x.ParentID, toJSON(x.RepoTags), toJSON(x.RepoDigests),
			x.Created, x.Size, x.SharedSize, x.Containers, toJSON(x.Labels),
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(imagesCols), Rows: rows})
}

// --- volumes ---

type apiVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Scope      string            `json:"Scope"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
}

func (c *Connector) scanVolumes(ctx context.Context, emit func(*source.Rows) error) error {
	// The volumes endpoint wraps the list in {"Volumes": [...]}.
	var resp struct {
		Volumes []apiVolume `json:"Volumes"`
	}
	if err := c.getJSON(ctx, "/volumes", &resp); err != nil {
		return err
	}
	rows := make([][]any, len(resp.Volumes))
	for i, x := range resp.Volumes {
		rows[i] = []any{
			x.Name, x.Driver, x.Mountpoint, x.CreatedAt, x.Scope,
			toJSON(x.Labels), toJSON(x.Options),
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(volumesCols), Rows: rows})
}

// --- networks ---

type apiNetwork struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Scope      string            `json:"Scope"`
	Created    string            `json:"Created"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	IPAM       any               `json:"IPAM"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
}

func (c *Connector) scanNetworks(ctx context.Context, emit func(*source.Rows) error) error {
	var nets []apiNetwork
	if err := c.getJSON(ctx, "/networks", &nets); err != nil {
		return err
	}
	rows := make([][]any, len(nets))
	for i, x := range nets {
		rows[i] = []any{
			x.ID, x.Name, x.Driver, x.Scope, x.Created,
			boolInt(x.Internal), boolInt(x.Attachable),
			toJSON(x.IPAM), toJSON(x.Labels), toJSON(x.Options),
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(networksCols), Rows: rows})
}
