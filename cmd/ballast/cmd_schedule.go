package main

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/springledev/ballast/internal/api"
)

// nextRunText is how the CLI says when a schedule runs next, in loc:
// "paused", "Next run: Tue 2026-11-03 03:00", or, when a missed slot
// makes the next check back up at once, "Runs at the next check, then
// Wed 2026-11-04 01:00".
func nextRunText(info api.ScheduleInfo, loc *time.Location) string {
	if !info.Enabled {
		return "paused"
	}
	if info.NextRun == nil {
		return "never runs"
	}
	at := info.NextRun.In(loc).Format("Mon 2006-01-02 15:04")
	if info.DueNow {
		return "Runs at the next check, then " + at
	}
	return "Next run: " + at
}

// surpriseNotice tells the user, accurately, whether the next check
// will start a backup.
func surpriseNotice(info api.ScheduleInfo) string {
	if info.Enabled && info.DueNow {
		return "A backup is due, so the next check (run-due, or the running service) will back this up straight away, then follow the schedule. To back up right now, run `ballast backup`."
	}
	return "Saving a schedule never starts a backup by surprise: it first runs at its next slot. To back up right now, run `ballast backup`."
}

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
			info, err := a.ops.SetSchedule(cmd.Context(), args[0], args[1], nil)
			if err != nil {
				return err
			}
			if info.Enabled {
				cmd.Printf("%s: scheduled %q. %s\n", args[0], args[1], nextRunText(info, time.Local))
			} else {
				cmd.Printf("%s: saved %q. The schedule is paused, so it will not run. Turn it on with `ballast schedule enable %s`.\n", args[0], args[1], args[0])
			}
			cmd.Println(surpriseNotice(info))
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
	Args:  noArgs,
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
				cmd.Printf("%s\t%s\t%s\t%s\n", s.Service, s.Cron, state, nextRunText(s, time.Local))
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
