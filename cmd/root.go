// Package cmd implements the dfetch command-line interface (cobra): the root
// command and the query, run, queries, tables, and version subcommands. main
// calls Execute; each subcommand loads the config, builds the engine, and renders
// the result.
package cmd

import (
	"context"

	"github.com/spf13/cobra"
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

// Execute runs the root command. Cancelling ctx (e.g. on Ctrl-C) propagates to
// every subcommand's cmd.Context(), aborting in-flight connector scans.
func Execute(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ./dfetch.yaml, then $XDG_CONFIG_HOME/dfetch/dfetch.yaml, then ~/dfetch.yaml)")
}
