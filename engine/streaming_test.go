package engine

import (
	"context"
	"testing"

	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunkConn emits its rows across several chunks, exercising the engine's
// per-chunk (streaming) load path.
type chunkConn struct {
	table  string
	cols   []source.Column
	chunks [][][]any
}

func (c chunkConn) Tables() []source.TableSchema {
	return []source.TableSchema{{Name: c.table, Columns: c.cols}}
}

func (c chunkConn) Scan(_ context.Context, _ source.ScanRequest, emit func(*source.Rows) error) error {
	names := make([]string, len(c.cols))
	for i, col := range c.cols {
		names[i] = col.Name
	}
	for _, rows := range c.chunks {
		if err := emit(&source.Rows{Columns: names, Rows: rows}); err != nil {
			return err
		}
	}
	return nil
}

// A connector that emits multiple chunks has every chunk loaded; the final query
// sees the union of all of them.
func TestRunLoadsAllStreamedChunks(t *testing.T) {
	conn := chunkConn{
		table: "t",
		cols:  []source.Column{{Name: "n", Type: "INTEGER"}},
		chunks: [][][]any{
			{{int64(1)}, {int64(2)}},
			{{int64(3)}},
			{{int64(4)}, {int64(5)}},
		},
	}
	e := engineWith(map[string]source.Connector{"s": conn})

	res, err := e.Run(context.Background(), "SELECT n FROM s.t ORDER BY n")
	require.NoError(t, err)
	require.Len(t, res.Rows, 5)
	assert.Equal(t, int64(1), res.Rows[0][0])
	assert.Equal(t, int64(5), res.Rows[4][0])
}
