package cmd

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestCommandSpanName(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"subcommand":            {[]string{"tables"}, "cli.tables"},
		"subcommand with args":  {[]string{"query", "SELECT 1"}, "cli.query"},
		"flag before command":   {[]string{"--config", "x.yaml", "tables"}, "cli.tables"},
		"bare root":             {nil, "cli.dfetch"},
		"root flag only":        {[]string{"--version"}, "cli.dfetch"},
		"unknown command":       {[]string{"no-such-command"}, "cli.dfetch"},
		"help for a subcommand": {[]string{"help", "tables"}, "cli.help"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, commandSpanName(tc.args))
		})
	}
}

// recordSpans installs an in-memory TracerProvider for the test and returns the
// recorder; the previous global provider is restored on cleanup.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// setCLIArgs points both os.Args (which Execute names the span from) and the
// root command (which cobra parses) at the same argv for the test.
func setCLIArgs(t *testing.T, args ...string) {
	t.Helper()
	prev := os.Args
	os.Args = append([]string{"dfetch"}, args...)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		os.Args = prev
		rootCmd.SetArgs(nil)
	})
}

// Every command runs inside a root cli.<command> span — this is what makes
// `dfetch tables` (whose engine calls start no span of their own) show up as a
// trace at all.
func TestExecuteStartsCommandSpan(t *testing.T) {
	sr := recordSpans(t)
	setCLIArgs(t, "version")
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	require.NoError(t, Execute(context.Background()))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "cli.version", spans[0].Name())
	assert.Equal(t, codes.Unset, spans[0].Status().Code)

	attrs := make([]string, 0, len(spans[0].Attributes()))
	for _, kv := range spans[0].Attributes() {
		attrs = append(attrs, string(kv.Key))
	}
	assert.Contains(t, attrs, "cli.args")
}

func TestExecuteRecordsErrorOnSpan(t *testing.T) {
	sr := recordSpans(t)
	setCLIArgs(t, "no-such-command")
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	require.Error(t, Execute(context.Background()))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "cli.dfetch", spans[0].Name())
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	require.NotEmpty(t, spans[0].Events(), "the error should be recorded as a span event")
}
