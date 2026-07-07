package engine_test

import (
	"context"
	"os"
	"testing"

	"github.com/dmashuda/dfetch/connectors"
	"github.com/dmashuda/dfetch/engine"
)

// BenchmarkProfileQuery runs the query in $DFETCH_QUERY end-to-end so its CPU and
// memory profiles can be captured with `go test -cpuprofile/-memprofile` (see the
// `profile` Make target). It is a profiling harness, not a correctness test: it
// hits whatever data sources the query references, so it is skipped unless
// DFETCH_QUERY is set. It lives in the external engine_test package so it can use
// the default connector set without an engine -> connectors import cycle.
func BenchmarkProfileQuery(b *testing.B) {
	query := os.Getenv("DFETCH_QUERY")
	if query == "" {
		b.Skip("set DFETCH_QUERY to the SQL to profile (see `make profile`)")
	}

	opts, err := connectors.DefaultOptions()
	if err != nil {
		b.Fatal(err)
	}
	eng, err := engine.New(opts...)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := eng.Run(ctx, query); err != nil {
			b.Fatal(err)
		}
	}
}
