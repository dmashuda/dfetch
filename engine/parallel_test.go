package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/require"
)

// barrierConn signals arrival into Scan, then blocks until release is closed —
// which only happens once every barrierConn has arrived. If the engine scanned
// sources serially, the first Scan would block forever (the second never
// arrives) and time out, so this deterministically proves parallel fetch.
type barrierConn struct {
	table   string
	arrive  *sync.WaitGroup
	release <-chan struct{}
}

func (b barrierConn) Tables() []source.TableSchema {
	return []source.TableSchema{{Name: b.table, Columns: []source.Column{{Name: "x", Type: "INTEGER"}}}}
}

func (b barrierConn) Scan(_ context.Context, _ source.ScanRequest, emit func(*source.Rows) error) error {
	b.arrive.Done()
	select {
	case <-b.release:
	case <-time.After(2 * time.Second):
		return errors.New("scan did not run in parallel (barrier timeout)")
	}
	return emit(&source.Rows{Columns: []string{"x"}, Rows: [][]any{{int64(1)}}})
}

func TestRunFetchesSourcesConcurrently(t *testing.T) {
	var arrive sync.WaitGroup
	arrive.Add(2)
	release := make(chan struct{})
	go func() { arrive.Wait(); close(release) }()

	e := engineWith(map[string]source.Connector{
		"a": barrierConn{table: "t", arrive: &arrive, release: release},
		"b": barrierConn{table: "t", arrive: &arrive, release: release},
	})

	res, err := e.Run(context.Background(), "SELECT * FROM a.t, b.t")
	require.NoError(t, err) // fails (timeout) if the two scans ran serially
	require.Len(t, res.Rows, 1)
}
