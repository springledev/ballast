package main

import "github.com/spf13/cobra"

// scheduleSetCmd is `ballast schedule set <service> "<cron>"` (spec
// §14 step 11: schedule management is a documented gap in spec §9's
// CLI surface — added here since it must be reachable somehow, and
// the REST API needs a CLI equivalent for every operation).
var scheduleSetCmd = &cobra.Command{
	Use:   "set <service> <cron>",
	Short: "Set a service's recurring backup schedule",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.SetSchedule(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			cmd.Printf("%s: scheduled %q\n", args[0], args[1])
			return nil
		})
	},
}

var scheduleEnableCmd = &cobra.Command{
	Use:   "enable <service>",
	Short: "Enable a service's schedule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setScheduleEnabled(cmd, args[0], true)
	},
}

var scheduleDisableCmd = &cobra.Command{
	Use:   "disable <service>",
	Short: "Disable a service's schedule without discarding it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setScheduleEnabled(cmd, args[0], false)
	},
}

func setScheduleEnabled(cmd *cobra.Command, service string, enabled bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
		if err := a.ops.SetScheduleEnabled(cmd.Context(), service, enabled); err != nil {
			return err
		}
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		cmd.Printf("%s: schedule %s\n", service, state)
		return nil
	})
}

var scheduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured schedules",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			list, err := a.ops.ListSchedules(cmd.Context())
			if err != nil {
				return err
			}
			if len(list) == 0 {
				cmd.Println("No schedules configured yet.")
				return nil
			}
			for _, s := range list {
				state := "disabled"
				if s.Enabled {
					state = "enabled"
				}
				cmd.Printf("%s\t%s\t%s\n", s.Service, s.Cron, state)
			}
			return nil
		})
	},
}

// registerScheduleCommands attaches `ballast schedule ...` to root.
func registerScheduleCommands(root *cobra.Command) {
	scheduleCmd := &cobra.Command{Use: "schedule", Short: "Manage backup schedules"}
	scheduleCmd.AddCommand(scheduleSetCmd, scheduleEnableCmd, scheduleDisableCmd, scheduleListCmd)
	root.AddCommand(scheduleCmd)
}
