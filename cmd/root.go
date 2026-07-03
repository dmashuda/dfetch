// Package cmd implements the dfetch command-line interface (cobra): the root
// command and the query, run, queries, tables, and version subcommands. main
// calls Execute; each subcommand loads the config, builds the engine, and renders
// the result.
package cmd

import (
	"context"
	"os"

	"github.com/dmashuda/dfetch/internal/telemetry"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var (
	cfgFile string
	version = "dev"
)

var rootCmd = &cobra.Command{
	Use:           "dfetch",
	Short:         "Query and join data across any data source with SQL on demand",
	Long:          "dfetch connects to arbitrary data sources, exposes each as a SQLite table, loads them into a\nper-request local SQLite database, and resolves your SQL (SQLite syntax) against it.",
	SilenceUsage:  true,
	SilenceErrors: false,
}

// SetVersion sets the version string reported by `dfetch version` and the
// cobra-provided `dfetch --version` flag.
func SetVersion(v string) {
	version = v
	rootCmd.Version = v
}

// Execute runs the root command inside a root `cli.<command>` span, so every
// subcommand — not just query, whose engine.Run starts its own child span —
// produces one trace per invocation; engine, connector, and SQLite spans nest
// under it via cmd.Context(). The span is against the no-op tracer unless an
// OTLP endpoint is configured (see internal/telemetry), so untraced runs pay
// nothing. Cancelling ctx (e.g. on Ctrl-C) propagates to every subcommand's
// cmd.Context(), aborting in-flight connector scans.
func Execute(ctx context.Context) error {
	args := os.Args[1:]
	ctx, span := telemetry.Tracer().Start(ctx, commandSpanName(args),
		trace.WithAttributes(attribute.StringSlice("cli.args", args)))
	defer span.End()

	err := rootCmd.ExecuteContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// commandSpanName names the root span after the subcommand cobra will invoke
// ("cli.tables", "cli.query", …). It resolves the raw args the same way
// ExecuteContext is about to (cobra's Find skips flags), falling back to
// "cli.dfetch" for the bare root command, --help/--version, or an unknown
// command.
func commandSpanName(args []string) string {
	// cobra only registers the help subcommand inside Execute; init it here so
	// `dfetch help <cmd>` resolves instead of falling back.
	rootCmd.InitDefaultHelpCmd()
	c, _, err := rootCmd.Find(args)
	if err != nil || c == nil || c == rootCmd {
		return "cli.dfetch"
	}
	return "cli." + c.Name()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ./dfetch.yaml, then $XDG_CONFIG_HOME/dfetch/dfetch.yaml, then ~/dfetch.yaml)")
}
