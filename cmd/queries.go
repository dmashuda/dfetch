package cmd

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/dmashuda/dfetch/internal/config"
	"github.com/spf13/cobra"
)

var queriesCmd = &cobra.Command{
	Use:   "queries",
	Short: "List saved queries",
	Long:  "List the saved queries configured in dfetch.yaml, with their parameters and description.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		out := cmd.OutOrStdout()
		if len(cfg.Queries) == 0 {
			_, err := fmt.Fprintln(out, "no saved queries configured")
			return err
		}

		queries := make([]config.SavedQuery, len(cfg.Queries))
		copy(queries, cfg.Queries)
		sort.Slice(queries, func(i, j int) bool { return queries[i].Name < queries[j].Name })

		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tPARAMS\tDESCRIPTION")
		for _, q := range queries {
			params := "-"
			if len(q.Params) > 0 {
				params = strings.Join(q.Params, ", ")
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", q.Name, params, q.Description)
		}
		return w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(queriesCmd)
}
