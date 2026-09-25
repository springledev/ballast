package main

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

var snapshotsJSON bool

// snapshotsCmd is `ballast snapshots <service> [--json]` (spec §9).
var snapshotsCmd = &cobra.Command{
	Use:   "snapshots <service>",
	Short: "List restic snapshots for one service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			snaps, err := a.ops.Snapshots(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if snapshotsJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(snaps)
			}
			if len(snaps) == 0 {
				cmd.Printf("%s: no snapshots yet\n", args[0])
				return nil
			}
			for _, s := range snaps {
				cmd.Printf("%s  %s  %v\n", s.ShortID, s.Time.Local().Format("2006-01-02 15:04:05"), s.Paths)
			}
			return nil
		})
	},
}

func init() {
	snapshotsCmd.Flags().BoolVar(&snapshotsJSON, "json", false, "print machine-readable JSON")
}
