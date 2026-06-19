package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestRunEmitsSpans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	e := engineWith(map[string]source.Connector{"github": issuesConn()})
	_, err := e.Run(context.Background(),
		"SELECT number FROM github.issues WHERE owner='golang' AND repo='go' AND state='open'")
	require.NoError(t, err)

	names := map[string]bool{}
	dbSpans := 0
	for _, s := range sr.Ended() {
		names[s.Name()] = true
		if strings.HasPrefix(s.Name(), "sql.") {
			dbSpans++
		}
	}

	assert.True(t, names["engine.Run"], "engine.Run span")
	assert.True(t, names["engine.loadSource"], "engine.loadSource span")
	assert.True(t, names["connector.scan"], "connector.scan span")
	assert.Positive(t, dbSpans, "expected otelsql (sql.*) spans for the SQLite work")
}
