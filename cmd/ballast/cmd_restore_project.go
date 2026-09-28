package main

import (
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
)

var (
	restoreProjectTo   string
	restoreProjectAsOf string
)

// restoreProjectCmd is `ballast restore-project <project> --to <dir>
// [--as-of <time>]` (spec §14 step 23): the full-stack restore
// wizard's CLI surface, restoring every service tied to the same
// Docker Compose stack (inventory.Service.Project) in dependency
// order — compose config, then Docker volumes, then DB dumps.
var restoreProjectCmd = &cobra.Command{
	Use:   "restore-project <project>",
	Short: "Restore every service of one Docker Compose stack together (compose config, volumes, DB dumps)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var asOf *time.Time
		if restoreProjectAsOf != "" {
			t, err := time.Parse(time.RFC3339, restoreProjectAsOf)
			if err != nil {
				return fmt.Errorf("--as-of must be an RFC3339 timestamp (e.g. 2026-01-02T15:04:05Z): %w", err)
			}
			asOf = &t
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			rep, err := a.ops.RestoreProject(cmd.Context(), api.RestoreProjectRequest{
				Project: args[0], AsOf: asOf, To: restoreProjectTo,
			})
			printRestoreProjectReport(cmd, rep)
			return err
		})
	},
}

// printRestoreProjectReport renders each service's outcome in plain
// English (spec §10 beginner requirement).
func printRestoreProjectReport(cmd *cobra.Command, rep api.RestoreProjectReport) {
	for _, r := range rep.Results {
		if r.Error != "" {
			cmd.Printf("%s (%s): FAILED — %s\n", r.Service, r.Kind, r.Error)
			continue
		}
		cmd.Printf("%s (%s): restored snapshot %s to %s\n", r.Service, r.Kind, r.SnapshotID, r.RestoredTo)
		for _, v := range r.Volumes {
			cmd.Printf("  volume restored into new Docker volume %q\n", v)
		}
	}
}

func init() {
	restoreProjectCmd.Flags().StringVar(&restoreProjectTo, "to", "", "directory to restore config/dump files into, one subdirectory per service (required)")
	restoreProjectCmd.Flags().StringVar(&restoreProjectAsOf, "as-of", "", "restore each service's newest backup at or before this time (RFC3339; default: each service's latest)")
	_ = restoreProjectCmd.MarkFlagRequired("to")
}
