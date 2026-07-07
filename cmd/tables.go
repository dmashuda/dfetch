package cmd

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/dmashuda/dfetch/config"
	"github.com/dmashuda/dfetch/engine"
	"github.com/spf13/cobra"
)

var tablesCmd = &cobra.Command{
	Use:   "tables [schema[.table]] [filter]",
	Short: "List schemas, tables, and columns",
	Long: `List available data in increasing detail:

  dfetch tables                  schemas and how many tables each serves
  dfetch tables <schema>         the table names in a schema (optional name filter)
  dfetch tables <schema> <pat>   only tables whose name contains <pat>
  dfetch tables <schema>.<table> the columns of one table`,
	Args: cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		eng, err := engine.New(cfg)
		if err != nil {
			return fmt.Errorf("initializing engine: %w", err)
		}

		ctx := cmd.Context()
		out := cmd.OutOrStdout()

		// No args: one line per schema with its table count.
		if len(args) == 0 {
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, s := range eng.SchemaSummaries(ctx) {
				count := "? tables"
				if s.TableCount >= 0 {
					count = fmt.Sprintf("%d tables", s.TableCount)
				}
				_, _ = fmt.Fprintf(w, "%s\t(%s)\n", s.Schema, count)
			}
			return w.Flush()
		}

		// "schema.table": describe one table's columns.
		if schema, table, ok := strings.Cut(args[0], "."); ok {
			ts, err := eng.DescribeTable(ctx, schema, table)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "%s.%s\n", schema, ts.Name)
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, c := range ts.Columns {
				_, _ = fmt.Fprintf(w, "  %s\t%s\n", c.Name, c.Type)
			}
			return w.Flush()
		}

		// "schema" (+ optional filter): list table names, schema-qualified.
		filter := ""
		if len(args) == 2 {
			filter = args[1]
		}
		names, err := eng.ListTables(ctx, args[0], filter)
		if err != nil {
			return err
		}
		for _, n := range names {
			_, _ = fmt.Fprintf(out, "%s.%s\n", args[0], n)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(tablesCmd)
}
