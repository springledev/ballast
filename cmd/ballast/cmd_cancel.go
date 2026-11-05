package main

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"github.com/springledev/ballast/internal/api"
)

// cancelCmd is `ballast cancel <service>`: stop the service's running
// backup. It asks the daemon when one answers, otherwise it records
// the request in the database itself, which the process running the
// backup (`ballast backup`, `ballast run-due`) notices within a second.
// Either way the run ends CANCELLED, anything it stopped is started
// again, and the last good backup is kept.
var cancelCmd = &cobra.Command{
	Use:   "cancel <service>",
	Short: "Stop a running backup; the last good backup is kept",
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
		var res api.CancelResult
		if client != nil {
			if err := client.call(cmd.Context(), http.MethodPost, "/api/v1/services/"+url.PathEscape(args[0])+"/cancel", nil, &res); err != nil {
				return fmt.Errorf("stopping the backup of %s: %w", args[0], err)
			}
		} else {
			err := withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
				var err error
				res, err = a.ops.CancelService(cmd.Context(), args[0])
				return err
			})
			if err != nil {
				return err
			}
		}
		cmd.Println(res.Message)
		return nil
	},
}
