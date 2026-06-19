package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dmashuda/dfetch/cmd"
	"github.com/dmashuda/dfetch/internal/telemetry"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run())
}

// run sets up telemetry (a no-op unless an OTLP endpoint is configured), runs
// the CLI, and flushes pending spans before returning the exit code. It exists
// so the deferred shutdown actually runs — os.Exit would skip it.
func run() int {
	shutdown, err := telemetry.Setup(context.Background(), version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dfetch: telemetry setup:", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	cmd.SetVersion(version)
	if err := cmd.Execute(); err != nil {
		return 1
	}
	return 0
}
