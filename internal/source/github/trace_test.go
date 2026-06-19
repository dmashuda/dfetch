package github

import (
	"context"
	"net/http"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestScanEmitsHTTPClientSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	c := newTestConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"number":1,"state":"open"}]`))
	})

	_, err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "issues",
		Filters: []source.Filter{eqFilter("owner", "o"), eqFilter("repo", "r")},
	})
	require.NoError(t, err)

	clientSpans := 0
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			clientSpans++
		}
	}
	assert.Positive(t, clientSpans, "expected an otelhttp client span for the GitHub request")
}
