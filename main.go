package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/dmashuda/dfetch/cmd"
	"github.com/dmashuda/dfetch/internal/telemetry"
)

// version is injected at build time via -ldflags "-X main.version=...". When it
// isn't (e.g. plain `go install`), resolveVersion falls back to the module
// version Go recorded in the binary's build info.
var version = "dev"

func main() {
	os.Exit(run())
}

// run sets up signal handling and telemetry (a no-op unless an OTLP endpoint is
// configured), runs the CLI, and flushes pending spans before returning the exit
// code. It exists so the deferred shutdown actually runs — os.Exit would skip it.
func run() int {
	// Cancel the root context on Ctrl-C / SIGTERM so in-flight connector scans
	// stop and the deferred cleanup (telemetry flush, per-request temp dirs)
	// still runs. Once cancelled, stop() restores the default handler, so a
	// second signal kills the process immediately.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()

	v := resolveVersion()
	shutdown, err := telemetry.Setup(ctx, v)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dfetch: telemetry setup:", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	cmd.SetVersion(v)
	if err := cmd.Execute(ctx); err != nil {
		return 1
	}
	return 0
}

// resolveVersion returns the release version: the -ldflags value when one was
// injected, else the main module's version from build info (set by
// `go install module@vX.Y.Z`), else "dev".
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}
