package cmd

import (
	"fmt"
	"io"

	"github.com/dmashuda/dfetch/config"
	"github.com/dmashuda/dfetch/engine"
	"github.com/spf13/cobra"
)

var queryFormat string

var queryCmd = &cobra.Command{
	Use:   "query <sql>",
	Short: "Run a SQL query across configured data sources",
	Long: "Parse and validate a SQLite-syntax query, fetch the referenced data sources, load them into a\n" +
		"per-request local SQLite database, and resolve the query against it.\n\n" +
		"Pass \"-\" as the query to read the SQL from stdin (e.g. dfetch query - < report.sql).",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sql := args[0]
		if sql == "-" {
			b, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("reading query from stdin: %w", err)
			}
			sql = string(b)
		}

		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		eng, err := engine.New(cfg)
		if err != nil {
			return fmt.Errorf("initializing engine: %w", err)
		}

		result, err := eng.Run(cmd.Context(), sql)
		if err != nil {
			return err
		}

		printWarnings(cmd, result.Warnings)
		return result.Write(cmd.OutOrStdout(), queryFormat)
	},
}

// printWarnings writes any non-fatal result warnings to stderr, so stdout stays
// clean for piping (e.g. --format json|csv).
func printWarnings(cmd *cobra.Command, warnings []string) {
	for _, w := range warnings {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w)
	}
}

func init() {
	queryCmd.Flags().StringVar(&queryFormat, "format", "table", "output format: table|json|csv")
	rootCmd.AddCommand(queryCmd)
}
