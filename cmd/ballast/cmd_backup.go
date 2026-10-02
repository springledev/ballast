package main

import (
	"fmt"
	"net/http"
	"net/url"

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
		client, err := useDaemon(cmd, cfg)
		if err != nil {
			return err
		}
		if client != nil {
			return backupViaDaemon(cmd, client, args[0])
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

// backupViaDaemon asks the running daemon to start the backup. The
// daemon runs it in the background (a backup can take hours), so this
// returns once it has started; the result shows up in `ballast status`.
func backupViaDaemon(cmd *cobra.Command, c *daemonClient, service string) error {
	var res struct {
		Interrupted bool `json:"previous_run_interrupted"`
	}
	if err := c.call(cmd.Context(), http.MethodPost, "/api/v1/services/"+url.PathEscape(service)+"/backup", nil, &res); err != nil {
		return fmt.Errorf("starting backup of %s: %w", service, err)
	}
	if res.Interrupted {
		fmt.Fprintf(cmd.ErrOrStderr(), "ballast: warning: an earlier backup of %s was found unfinished and has been marked failed\n", service)
	}
	cmd.Printf("%s: backup started by the Ballast daemon. It is verified when it finishes; check progress with `ballast status`.\n", service)
	return nil
}

func init() {
	// --now is accepted for spec §9 CLI-surface compatibility
	// ("ballast backup <service> [--now]"); running immediately is the
	// only mode `ballast backup` has, so the flag has no other effect.
	backupCmd.Flags().Bool("now", true, "run the backup immediately (the only supported mode)")
}
