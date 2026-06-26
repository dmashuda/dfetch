// Package cmd implements the dfetch command-line interface (cobra): the root
// command and the query, run, queries, tables, and version subcommands. main
// calls Execute; each subcommand loads the config, builds the engine, and renders
// the result.
package cmd

import (
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

// SetVersion sets the version string reported by `dfetch version`.
func SetVersion(v string) {
	version = v
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ./dfetch.yaml, then $XDG_CONFIG_HOME/dfetch/dfetch.yaml, then ~/dfetch.yaml)")
}
