package engine

import (
	"context"
	"testing"

	"github.com/dmashuda/dfetch/config"
	"github.com/dmashuda/dfetch/localdb"
	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A caller-built connector registered via WithConnector serves queries.
func TestNewWithConnector(t *testing.T) {
	e, err := New(WithConnector("gh", issuesConn()))
	require.NoError(t, err)

	res, err := e.Run(context.Background(), "SELECT number FROM gh.issues WHERE owner='golang' AND repo='go'")
	require.NoError(t, err)
	assert.Len(t, res.Rows, 3)
}

// WithSources builds typed sources through the WithRegistry registry.
func TestNewWithSourcesAndRegistry(t *testing.T) {
	var gotParams map[string]any
	reg := source.NewRegistry()
	reg.Register("fake", func(params map[string]any) (source.Connector, error) {
		gotParams = params
		return issuesConn(), nil
	})

	e, err := New(
		WithRegistry(reg),
		WithSources(config.SourceConfig{Name: "mine", Type: "fake", Params: map[string]any{"k": "v"}}),
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"k": "v"}, gotParams)

	res, err := e.Run(context.Background(), "SELECT number FROM mine.issues WHERE owner='golang' AND repo='go'")
	require.NoError(t, err)
	assert.Len(t, res.Rows, 3)
}

// Without a registry, a typed source fails at New with the unknown-type error.
func TestNewTypedSourceWithoutRegistryErrors(t *testing.T) {
	_, err := New(WithSources(config.SourceConfig{Name: "db", Type: "postgres"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `connector "db"`)
	assert.Contains(t, err.Error(), "unknown connector type")
}

// Options apply in order: the last registration of a schema name wins, so a
// config source overrides an earlier WithConnector of the same name.
func TestNewLaterRegistrationWins(t *testing.T) {
	override := issuesConn()
	reg := source.NewRegistry()
	reg.Register("fake", func(map[string]any) (source.Connector, error) { return override, nil })

	e, err := New(
		WithRegistry(reg),
		WithConnector("gh", &fakeConn{}),
		WithConfig(&config.Config{Sources: []config.SourceConfig{{Name: "gh", Type: "fake"}}}),
	)
	require.NoError(t, err)
	assert.Same(t, override, e.connectors["gh"])
}

// A nil config is a no-op, and an engine without connectors is valid: queries
// fail per schema, not at construction.
func TestNewWithNilConfig(t *testing.T) {
	e, err := New(WithConfig(nil))
	require.NoError(t, err)

	_, err = e.Run(context.Background(), "SELECT * FROM nope.things WHERE owner='x'")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no connector for schema")
}

// recordingDB wraps the real localdb.DB and records the engine's calls, so the
// test asserts both the WithDB plumbing and the call sequence a custom DB
// implementation must serve.
type recordingDB struct {
	inner *localdb.DB
	calls []string
}

func (r *recordingDB) Attach(ctx context.Context, schema string) error {
	r.calls = append(r.calls, "attach:"+schema)
	return r.inner.Attach(ctx, schema)
}

func (r *recordingDB) CreateTable(ctx context.Context, schema string, ts source.TableSchema) error {
	r.calls = append(r.calls, "create:"+schema+"."+ts.Name)
	return r.inner.CreateTable(ctx, schema, ts)
}

func (r *recordingDB) Insert(ctx context.Context, schema, table string, cols []string, rows [][]any) error {
	r.calls = append(r.calls, "insert:"+schema+"."+table)
	return r.inner.Insert(ctx, schema, table, cols, rows)
}

func (r *recordingDB) Query(ctx context.Context, query string, args ...any) (*localdb.Result, error) {
	r.calls = append(r.calls, "query")
	return r.inner.Query(ctx, query, args...)
}

func (r *recordingDB) Close() error {
	r.calls = append(r.calls, "close")
	return r.inner.Close()
}

// WithDB supplies the per-request database: the engine opens it once, drives
// attach -> create -> insert -> query against it, and closes it.
func TestRunUsesWithDB(t *testing.T) {
	rec := &recordingDB{}
	e, err := New(
		WithConnector("gh", issuesConn()),
		WithDB(func(ctx context.Context) (DB, error) {
			inner, err := localdb.Open(ctx)
			if err != nil {
				return nil, err
			}
			rec.inner = inner
			return rec, nil
		}),
	)
	require.NoError(t, err)

	res, err := e.Run(context.Background(), "SELECT number FROM gh.issues WHERE owner='golang' AND repo='go'")
	require.NoError(t, err)
	assert.Len(t, res.Rows, 3)

	assert.Equal(t, []string{
		"attach:gh",
		"create:gh.issues",
		"insert:gh.issues",
		"query",
		"close",
	}, rec.calls)
}
