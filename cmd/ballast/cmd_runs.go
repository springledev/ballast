package main

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

var (
	runsJSON  bool
	runsLimit int
)

// runsCmd is `ballast runs <service> [--json] [--limit N]`: a
// service's past runs, newest first, from the same Ops function the
// history API uses.
var runsCmd = &cobra.Command{
	Use:   "runs <service>",
	Short: "List past backup runs for one service, newest first",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			page, err := a.ops.ListRuns(args[0], runsLimit, 0)
			if err != nil {
				return err
			}
			if runsJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(page)
			}
			if len(page.Runs) == 0 {
				cmd.Printf("%s: no runs yet\n", args[0])
				return nil
			}
			for _, r := range page.Runs {
				cmd.Printf("#%d  %s  %s  %s\n", r.ID, r.StartedAt.Local().Format("2006-01-02 15:04:05"), r.State, r.Light)
			}
			return nil
		})
	},
}

func init() {
	runsCmd.Flags().BoolVar(&runsJSON, "json", false, "print machine-readable JSON")
	runsCmd.Flags().IntVar(&runsLimit, "limit", 0, "how many runs to show (default 20, at most 100)")
}
