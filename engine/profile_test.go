package engine

import (
	"context"
	"os"
	"testing"

	"github.com/dmashuda/dfetch/config"
)

// BenchmarkProfileQuery runs the query in $DFETCH_QUERY end-to-end so its CPU and
// memory profiles can be captured with `go test -cpuprofile/-memprofile` (see the
// `profile` Make target). It is a profiling harness, not a correctness test: it
// hits whatever data sources the query references, so it is skipped unless
// DFETCH_QUERY is set.
func BenchmarkProfileQuery(b *testing.B) {
	query := os.Getenv("DFETCH_QUERY")
	if query == "" {
		b.Skip("set DFETCH_QUERY to the SQL to profile (see `make profile`)")
	}

	eng, err := New(&config.Config{})
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
