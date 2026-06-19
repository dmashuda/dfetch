package cmd

import (
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/dmashuda/dfetch/internal/config"
	"github.com/dmashuda/dfetch/internal/engine"
	"github.com/spf13/cobra"
)

var tablesCmd = &cobra.Command{
	Use:   "tables [schema]",
	Short: "List available tables and their columns",
	Long:  "List the connector schemas, their tables, and each table's columns. Pass a schema name to show only that schema.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		eng, err := engine.New(cfg)
		if err != nil {
			return fmt.Errorf("initializing engine: %w", err)
		}

		schemas := eng.Schemas()
		names := make([]string, 0, len(schemas))
		for name := range schemas {
			if len(args) == 1 && name != args[0] {
				continue
			}
			names = append(names, name)
		}
		if len(args) == 1 && len(names) == 0 {
			return fmt.Errorf("no connector for schema %q", args[0])
		}
		sort.Strings(names)

		out := cmd.OutOrStdout()
		for _, schema := range names {
			tables := schemas[schema]
			sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
			for _, t := range tables {
				_, _ = fmt.Fprintf(out, "%s.%s\n", schema, t.Name)
				w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				for _, c := range t.Columns {
					_, _ = fmt.Fprintf(w, "  %s\t%s\n", c.Name, c.Type)
				}
				_ = w.Flush()
				_, _ = fmt.Fprintln(out)
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(tablesCmd)
}
