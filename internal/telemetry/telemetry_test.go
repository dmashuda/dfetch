package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_SDK_DISABLED",
	} {
		t.Setenv(k, "")
	}
}

func TestEnabled(t *testing.T) {
	t.Run("off when no endpoint", func(t *testing.T) {
		clearOTelEnv(t)
		assert.False(t, Enabled())
	})
	t.Run("on with otlp endpoint", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
		assert.True(t, Enabled())
	})
	t.Run("on with traces endpoint", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
		assert.True(t, Enabled())
	})
	t.Run("disabled overrides endpoint", func(t *testing.T) {
		clearOTelEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
		t.Setenv("OTEL_SDK_DISABLED", "true")
		assert.False(t, Enabled())
	})
}

func TestSetupDisabledIsNoop(t *testing.T) {
	clearOTelEnv(t)
	shutdown, err := Setup(context.Background(), "test")
	require.NoError(t, err)
	require.NotNil(t, shutdown)
	assert.NoError(t, shutdown(context.Background()))
}

func TestSetupEnabledInstallsProvider(t *testing.T) {
	// Restore the global provider afterwards so other packages' tests are not
	// affected by the one installed here.
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	shutdown, err := Setup(context.Background(), "v1.2.3")
	require.NoError(t, err) // exporter is created lazily; no dial here
	require.NotNil(t, shutdown)
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// A real (non-no-op) provider is now installed.
	assert.NotNil(t, Tracer())
}
