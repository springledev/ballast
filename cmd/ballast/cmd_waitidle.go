package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// waitIdleDefault is how long `ballast wait-idle` waits when --timeout
// is not given.
const waitIdleDefault = 10 * time.Minute

var (
	waitIdleTimeout  = waitIdleDefault
	waitIdleInterval = 5 * time.Second
)

// waitIdleCmd is `ballast wait-idle [--timeout 10m]` (spec §21.5): it
// returns once no backup is running, so an upgrade script can wait for
// a quiet moment before swapping the binary.
var waitIdleCmd = &cobra.Command{
	Use:   "wait-idle",
	Args:  noArgs,
	Short: "Wait until no backup is running, then exit 0 (exit 1 if the timeout passes first)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			deadline := time.Now().Add(waitIdleTimeout)
			for {
				names, err := a.ops.RunningServices(cmd.Context())
				if err != nil {
					return err
				}
				if len(names) == 0 {
					cmd.Println("Nothing is running.")
					return nil
				}
				if !time.Now().Before(deadline) {
					return fmt.Errorf("still running after %s: %s; try again later, or raise --timeout", waitIdleTimeout, strings.Join(names, ", "))
				}
				select {
				case <-cmd.Context().Done():
					return fmt.Errorf("stopped waiting while still running: %s", strings.Join(names, ", "))
				case <-time.After(waitIdleInterval):
				}
			}
		})
	},
}

func init() {
	waitIdleCmd.Flags().DurationVar(&waitIdleTimeout, "timeout", waitIdleDefault, "how long to wait before giving up")
}
