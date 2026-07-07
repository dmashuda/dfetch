package cmd

import (
	"fmt"
	"strings"

	"github.com/dmashuda/dfetch/config"
	"github.com/dmashuda/dfetch/engine"
	"github.com/spf13/cobra"
)

var (
	runFormat     string
	runAllColumns bool
)

var runCmd = &cobra.Command{
	Use:   "run <name> [args...]",
	Short: "Run a saved query by name",
	Long: `Run a query saved in the config by name, binding positional arguments to its
declared parameters in order.

  dfetch run fetch-trace abc123   bind :trace_id = abc123

By default only the query's configured columns are shown; pass --all-columns to
return every column the query produces. Use 'dfetch queries' to list saved queries.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		name := args[0]
		sq, ok := cfg.Query(name)
		if !ok {
			return fmt.Errorf("no saved query named %q (run 'dfetch queries' to list)", name)
		}

		values := args[1:]
		if len(values) != len(sq.Params) {
			return fmt.Errorf("query %q expects %d argument(s) (%s), got %d",
				name, len(sq.Params), usageParams(sq.Params), len(values))
		}
		params := make(map[string]any, len(sq.Params))
		for i, p := range sq.Params {
			params[p] = values[i]
		}

		eng, err := engine.New(cfg)
		if err != nil {
			return fmt.Errorf("initializing engine: %w", err)
		}

		result, err := eng.RunWithParams(cmd.Context(), sq.SQL, params)
		if err != nil {
			return err
		}

		printWarnings(cmd, result.Warnings)

		if !runAllColumns {
			result, err = result.Project(sq.Columns)
			if err != nil {
				return err
			}
		}

		return result.Write(cmd.OutOrStdout(), runFormat)
	},
}

// usageParams renders a query's parameter names as a placeholder usage hint,
// e.g. "<trace_id> <span_id>", or "<none>" when the query takes no parameters.
func usageParams(params []string) string {
	if len(params) == 0 {
		return "<none>"
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = "<" + p + ">"
	}
	return strings.Join(parts, " ")
}

func init() {
	runCmd.Flags().StringVar(&runFormat, "format", "table", "output format: table|json|csv")
	runCmd.Flags().BoolVar(&runAllColumns, "all-columns", false, "return every column the query produces, ignoring the saved column list")
	rootCmd.AddCommand(runCmd)
}
