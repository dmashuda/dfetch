package cmd

import (
	"fmt"

	"github.com/dmashuda/dfetch/internal/config"
	"github.com/dmashuda/dfetch/internal/engine"
	"github.com/spf13/cobra"
)

var queryFormat string

var queryCmd = &cobra.Command{
	Use:   "query <sql>",
	Short: "Run a SQL query across configured data sources",
	Long:  "Parse and validate a SQLite-syntax query, fetch the referenced data sources, load them into a\nper-request local SQLite database, and resolve the query against it.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		eng, err := engine.New(cfg)
		if err != nil {
			return fmt.Errorf("initializing engine: %w", err)
		}

		result, err := eng.Run(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		return result.Write(cmd.OutOrStdout(), queryFormat)
	},
}

func init() {
	queryCmd.Flags().StringVar(&queryFormat, "format", "table", "output format: table|json|csv")
	rootCmd.AddCommand(queryCmd)
}
