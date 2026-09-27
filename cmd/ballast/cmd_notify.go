package main

import (
	"fmt"
	"io"

	"github.com/springledev/ballast/internal/notify"
	"github.com/spf13/cobra"
)

// `ballast notify` is a documented addition to spec §9's CLI surface,
// like `ballast schedule` (see cmd_schedule.go): the web UI's Settings
// page reads and writes these settings, and every GUI action must also
// be doable from the CLI (spec §10).

var notifyShowCmd = &cobra.Command{
	Use:   "show <service>",
	Short: "Show when Ballast will alert you about a service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			s, err := a.ops.GetNotificationSettings(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printNotifySettings(cmd.OutOrStdout(), s)
			return nil
		})
	},
}

// printNotifySettings describes s in plain English, one setting per
// line.
func printNotifySettings(w io.Writer, s notify.Settings) {
	maxAge := "based on its schedule (2 days for nightly, 9 days for weekly)"
	if s.MaxAgeHours != nil {
		maxAge = fmt.Sprintf("%d hours", *s.MaxAgeHours)
	}
	fmt.Fprintf(w, "Notifications for %s:\n", s.Service)
	fmt.Fprintf(w, "  Alert when a backup is missing:   %s\n", onOff(s.Enabled))
	fmt.Fprintf(w, "  Counts as missing after:          %s\n", maxAge)
	fmt.Fprintf(w, "  Message after every good backup:  %s\n", onOff(s.HeartbeatEnabled))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

var (
	notifyEnabled     bool
	notifyMaxAgeHours int
	notifyHeartbeat   bool
)

var notifySetCmd = &cobra.Command{
	Use:   "set <service>",
	Short: "Choose when Ballast alerts you about a service",
	Long: `Choose when Ballast alerts you about a service.

Failed backups are always reported. These settings control the rest:

  --enabled         also alert when no good backup has happened for too long
                    (for example because the machine was off, or the schedule
                    was turned off by mistake)
  --max-age-hours   how long "too long" is; leave it out to use the service's
                    schedule (2 days for nightly backups, 9 days for weekly)
  --heartbeat       send a short "all good" message after every verified backup,
                    so silence itself becomes a warning sign

Each run replaces all three settings, so pass every flag you want to keep,
not only the one you are changing.

Where messages go is set up separately, with a credential file holding your
ntfy topic URL or webhook URL (see the notifications guide).`,
	Example: `  ballast notify set immich --heartbeat
  ballast notify set immich --max-age-hours 72`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			service := args[0]
			s := notify.Settings{Service: service, Enabled: notifyEnabled, HeartbeatEnabled: notifyHeartbeat}
			if notifyMaxAgeHours > 0 {
				v := notifyMaxAgeHours
				s.MaxAgeHours = &v
			}
			if err := a.ops.SetNotificationSettings(cmd.Context(), service, s); err != nil {
				return err
			}
			printNotifySettings(cmd.OutOrStdout(), s)
			return nil
		})
	},
}

// registerNotifyCommands attaches `ballast notify ...` to root.
func registerNotifyCommands(root *cobra.Command) {
	notifySetCmd.Flags().BoolVar(&notifyEnabled, "enabled", true, "alert when no good backup has happened for too long")
	notifySetCmd.Flags().IntVar(&notifyMaxAgeHours, "max-age-hours", 0, "hours without a good backup before alerting (0 = based on the schedule)")
	notifySetCmd.Flags().BoolVar(&notifyHeartbeat, "heartbeat", false, "send a message after every verified backup")

	notifyCmd := &cobra.Command{Use: "notify", Short: "Choose when Ballast alerts you about each service"}
	notifyCmd.AddCommand(notifyShowCmd, notifySetCmd)
	root.AddCommand(notifyCmd)
}
