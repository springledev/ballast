package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

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
		if preview, _ := cmd.Flags().GetBool("preview"); preview {
			return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
				pv, err := a.ops.Preview(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				printPreview(cmd, pv)
				return nil
			})
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
	cmd.Printf("%s: backup started by the Ballast daemon. The daemon runs it in the background and verifies it when it finishes; follow it with `ballast status` (shows the current step and how long it has run).\n", service)
	return nil
}

// printPreview writes a service preview in plain English. Nothing in it
// was run.
func printPreview(cmd *cobra.Command, pv *api.ServicePreview) {
	cmd.Printf("Preview for %s (nothing was run)\n\n", pv.Service)
	cmd.Println("What is backed up:")
	for _, p := range pv.Parts {
		cmd.Printf("  %-18s %s\n", p.Name, p.What)
		if len(p.Goes) > 0 {
			cmd.Printf("  %-18s goes to: %s\n", "", strings.Join(p.Goes, ", "))
		}
	}
	cmd.Println("\nWhere it goes:")
	for _, t := range pv.Targets {
		kind := ""
		if t.Secondary {
			kind = " (second copy)"
		}
		cmd.Printf("  %s%s: %s; keeps %s\n", t.Destination, kind, strings.Join(t.Parts, ", "), t.Keeps)
		if t.Note != "" {
			cmd.Printf("    %s\n", t.Note)
		}
	}
	if pv.EstimatedBytes != nil {
		cmd.Printf("\nEstimated size of folders on this machine: %s\n", humanBytes(*pv.EstimatedBytes))
	}
	if len(pv.StopContainers) > 0 {
		cmd.Printf("\nContainers stopped during the backup: %s\n", strings.Join(pv.StopContainers, ", "))
		if pv.StoppedSeconds != nil {
			cmd.Printf("They were down for about %d s during the last backup.\n", *pv.StoppedSeconds)
		} else {
			cmd.Println("There has been no backup yet, so how long they stay down is not known.")
		}
	} else {
		cmd.Println("\nNo containers are stopped.")
	}
	if len(pv.Excludes) > 0 {
		cmd.Printf("\nSkipped: %s\n", strings.Join(pv.Excludes, ", "))
	}
	for _, n := range pv.Notes {
		cmd.Printf("Note: %s\n", n)
	}
}

func init() {
	backupCmd.Flags().Bool("preview", false, "show what this backup would do (parts, destinations, containers stopped) without running it")
	// --now is accepted for spec §9 CLI-surface compatibility
	// ("ballast backup <service> [--now]"); running immediately is the
	// only mode `ballast backup` has, so the flag has no other effect.
	backupCmd.Flags().Bool("now", true, "run the backup immediately (the only supported mode)")
}
