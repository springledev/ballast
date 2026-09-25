package main

import (
	"fmt"

	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
)

// backupCmd is `ballast backup <service>` (spec §9). It runs
// synchronously through Ops.Backup, which itself only ever reaches
// the pipeline via scheduler.Coordinator.RunNow — the same path every
// other run-starting command uses.
var backupCmd = &cobra.Command{
	Use:   "backup <service>",
	Short: "Run a verified backup for one configured service now",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			runErr := a.ops.Backup(cmd.Context(), args[0])
			if runErr != nil {
				if !api.InterruptedOnly(runErr) {
					return runErr
				}
				// Only an EARLIER run was found interrupted and marked
				// failed while claiming the lock (spec §14 step 11 hard
				// rule 6); this run itself succeeded and is verified —
				// report the old failure as a warning, not as this
				// command failing.
				fmt.Fprintf(cmd.ErrOrStderr(), "ballast: warning: %s\n", runErr)
			}
			cmd.Printf("%s: backup succeeded and is verified\n", args[0])
			return nil
		})
	},
}

func init() {
	// --now is accepted for spec §9 CLI-surface compatibility
	// ("ballast backup <service> [--now]"); running immediately is the
	// only mode `ballast backup` has, so the flag has no other effect.
	backupCmd.Flags().Bool("now", true, "run the backup immediately (the only supported mode)")
}
