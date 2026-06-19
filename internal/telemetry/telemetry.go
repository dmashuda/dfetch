// Package telemetry configures OpenTelemetry tracing for dfetch. Tracing is
// off by default: instrumentation creates spans against the global no-op
// TracerProvider and costs almost nothing until Setup installs a real provider,
// which only happens when an OTLP endpoint is configured via the environment.
package telemetry

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// scopeName identifies dfetch's instrumentation scope on emitted spans.
const scopeName = "github.com/dmashuda/dfetch"

// Enabled reports whether OTLP tracing is configured via the environment.
// Tracing turns on only when an OTLP endpoint is set (and not explicitly
// disabled), so running dfetch without those vars emits no traces.
func Enabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Setup installs a global TracerProvider exporting over OTLP/HTTP when tracing
// is Enabled, and returns a shutdown func that flushes pending spans. When
// tracing is not enabled it installs nothing and returns a no-op shutdown.
func Setup(ctx context.Context, serviceVersion string) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, nil
	}

	exp, err := otlptracehttp.New(ctx) // reads OTEL_EXPORTER_OTLP_* from the env
	if err != nil {
		return noop, err
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("dfetch"),
		semconv.ServiceVersion(serviceVersion),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// Tracer returns dfetch's tracer. It is a no-op tracer unless Setup installed a
// provider, so callers can start spans unconditionally.
func Tracer() trace.Tracer {
	return otel.Tracer(scopeName)
}
