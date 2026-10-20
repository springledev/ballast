package main

import (
	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
)

var (
	restoreSnapshot string
	restoreFiles    []string
	restoreTo       string
	restoreFrom     string
)

// restoreCmd is `ballast restore <service> [--snapshot id] [--file
// path]... --to <dir>` (spec §9). Ops.Restore enforces --to and the
// read-only-by-default target-directory rule; this command is thin.
var restoreCmd = &cobra.Command{
	Use:   "restore <service>",
	Short: "Restore a service's backup into a new directory (read-only-by-default)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			err := a.ops.Restore(cmd.Context(), api.RestoreRequest{
				Service:  args[0],
				From:     restoreFrom,
				Snapshot: restoreSnapshot,
				Paths:    restoreFiles,
				To:       restoreTo,
			})
			if err != nil {
				return err
			}
			cmd.Printf("%s: restored to %s\n", args[0], restoreTo)
			return nil
		})
	},
}

func init() {
	restoreCmd.Flags().StringVar(&restoreFrom, "from", "", "destination to restore from (default: the service's main destination)")
	restoreCmd.Flags().StringVar(&restoreSnapshot, "snapshot", "", "snapshot ID to restore (default: this service's latest)")
	restoreCmd.Flags().StringArrayVar(&restoreFiles, "file", nil, "restrict the restore to this path (repeatable)")
	restoreCmd.Flags().StringVar(&restoreTo, "to", "", "directory to restore into (required; must not already hold data)")
}
