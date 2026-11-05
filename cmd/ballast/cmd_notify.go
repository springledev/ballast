package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/notify"
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

	// channel command flags
	channelURL         string
	channelUsername    string
	channelTokenFile   string
	channelAllServices bool
	channelServices    []string
	channelClearToken  bool
	channelJSON        bool
	channelType        string
	channelEvents      []string
	channelTiers       []string
	channelSMTPHost    string
	channelSMTPPort    int
	channelSMTPSec     string
	channelFrom        string
	channelTo          []string
	channelDigest      bool
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

var notifyChannelListCmd = &cobra.Command{
	Use:   "list",
	Args:  noArgs,
	Short: "List notification channels",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			cs, err := a.ops.ListChannels(cmd.Context())
			if err != nil {
				return err
			}
			if channelJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(cs)
			}
			if len(cs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No notification channels yet. Add one with: ballast notify channel add ...\n")
				return nil
			}
			for i, c := range cs {
				if i > 0 {
					fmt.Fprintln(cmd.OutOrStdout())
				}
				printChannelView(cmd.OutOrStdout(), c)
			}
			return nil
		})
	},
}

var notifyChannelAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a notification channel",
	Long: `Add a notification channel.

The address and token are saved as root-only credential files, never in the
database. The token comes only from --token-file (a file whose contents are
used) or the environment variable BALLAST_NTFY_TOKEN, so it does not appear
in your shell history.

When no service flags are given, --all-services is the default.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if channelType == "" {
			channelType = notify.ChannelTypeNtfy
		}
		if channelURL == "" && channelType != notify.ChannelTypeEmail {
			return fmt.Errorf("--url is required to add a channel")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			token, err := readChannelToken()
			if err != nil {
				return err
			}
			// Default to apply-to-all if no service flags given
			applyToAll := channelAllServices || (len(channelServices) == 0 && !channelAllServices)
			in := api.ChannelInput{
				Name:       args[0],
				Type:       channelType,
				URL:        channelURL,
				Username:   channelUsername,
				Token:      token,
				ApplyToAll: applyToAll && len(channelServices) == 0,
				Services:   channelServices,
				Events:     channelEvents,
				Tiers:      channelTiers,
				Email:      channelEmailSettings(nil),
				Digest:     &channelDigest,
			}
			v, err := a.ops.PutChannel(cmd.Context(), in)
			if err != nil {
				return err
			}
			printChannelView(cmd.OutOrStdout(), v)
			return nil
		})
	},
}

var notifyChannelEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a notification channel",
	Long: `Edit a notification channel.

Any flag you omit keeps its current value. To clear the token without
replacing it, use --clear-token.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			token, err := readChannelToken()
			if err != nil {
				return err
			}
			var base *notify.EmailSettings
			if cs, err := a.ops.ListChannels(cmd.Context()); err == nil {
				for _, c := range cs {
					if c.Name == args[0] {
						base = c.Email
					}
				}
			}
			in := api.ChannelInput{
				Name:       args[0],
				URL:        channelURL,
				Username:   channelUsername,
				Token:      token,
				ClearToken: channelClearToken,
				ApplyToAll: channelAllServices,
				Services:   channelServices,
				Events:     channelEvents,
				Tiers:      channelTiers,
				Email:      channelEmailSettings(base),
			}
			if cmd.Flags().Changed("digest") {
				in.Digest = &channelDigest
			}
			v, err := a.ops.PutChannel(cmd.Context(), in)
			if err != nil {
				return err
			}
			printChannelView(cmd.OutOrStdout(), v)
			return nil
		})
	},
}

var notifyChannelRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a notification channel",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.DeleteChannel(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed channel %q.\n", args[0])
			return nil
		})
	},
}

var notifyChannelTestCmd = &cobra.Command{
	Use:   "test <name>",
	Short: "Send a test notification through a channel",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			detail, err := a.ops.TestChannelResult(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Sent. %s\n", detail)
			return nil
		})
	},
}

// readChannelToken reads the token from --token-file or BALLAST_NTFY_TOKEN
// environment variable, trimming whitespace. Returns empty string if neither
// is set.
func readChannelToken() (string, error) {
	if channelTokenFile != "" {
		data, err := os.ReadFile(channelTokenFile)
		if err != nil {
			return "", fmt.Errorf("reading token file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(os.Getenv("BALLAST_NTFY_TOKEN")), nil
}

// channelEmailSettings builds mail settings from the --smtp-* flags on
// top of base (the stored settings, when editing). It returns nil when
// no mail flag was given, which keeps the stored settings.
func channelEmailSettings(base *notify.EmailSettings) *notify.EmailSettings {
	if channelSMTPHost == "" && channelSMTPPort == 0 && channelSMTPSec == "" && channelFrom == "" && len(channelTo) == 0 {
		return nil
	}
	e := notify.EmailSettings{Security: notify.EmailSecurityStartTLS, Port: 587}
	if base != nil {
		e = *base
	}
	if channelSMTPHost != "" {
		e.Host = channelSMTPHost
	}
	if channelSMTPPort != 0 {
		e.Port = channelSMTPPort
	}
	if channelSMTPSec != "" {
		e.Security = channelSMTPSec
	}
	if channelFrom != "" {
		e.From = channelFrom
	}
	if len(channelTo) > 0 {
		e.To = channelTo
	}
	return &e
}

func addChannelRoutingFlags(c *cobra.Command) {
	f := c.Flags()
	f.StringSliceVar(&channelEvents, "event", nil, "only send these events: job_failure, verification_failure, freshness_breached, success_heartbeat, recovery (default all)")
	f.StringSliceVar(&channelTiers, "tier", nil, "only send for these backup copies, e.g. restic (default all)")
	f.BoolVar(&channelDigest, "digest", false, "also send the weekly digest (a one-message summary of every service) here")
	f.StringVar(&channelSMTPHost, "smtp-host", "", "email: mail server")
	f.IntVar(&channelSMTPPort, "smtp-port", 0, "email: mail server port (587 by default)")
	f.StringVar(&channelSMTPSec, "smtp-security", "", "email: starttls, tls or none")
	f.StringVar(&channelFrom, "from", "", "email: sender address")
	f.StringSliceVar(&channelTo, "to", nil, "email: recipient address (may be given multiple times)")
}

// printChannelView prints a ChannelView in plain English.
func printChannelView(w io.Writer, c api.ChannelView) {
	fmt.Fprintf(w, "Channel: %s\n", c.Name)
	fmt.Fprintf(w, "  Type:      %s\n", c.Type)
	fmt.Fprintf(w, "  Address:   %s\n", c.URLDisplay)
	if c.Email != nil {
		fmt.Fprintf(w, "  Mail:      %s -> %s via %s:%d (%s)\n", c.Email.From, strings.Join(c.Email.To, ", "), c.Email.Host, c.Email.Port, c.Email.Security)
	}
	if len(c.Events) > 0 {
		fmt.Fprintf(w, "  Sends:     %s\n", strings.Join(c.Events, ", "))
	}
	if len(c.Tiers) > 0 {
		fmt.Fprintf(w, "  Copies:    %s\n", strings.Join(c.Tiers, ", "))
	}
	if c.Username != "" {
		fmt.Fprintf(w, "  Username:  %s\n", c.Username)
	}
	fmt.Fprintf(w, "  Token:     %s\n", onOff(c.HasToken))
	fmt.Fprintf(w, "  Weekly digest: %s\n", onOff(c.Digest))
	if c.ApplyToAll {
		fmt.Fprintf(w, "  Applies to: all services\n")
	} else if len(c.Services) > 0 {
		fmt.Fprintf(w, "  Applies to: %s\n", strings.Join(c.Services, ", "))
	} else {
		fmt.Fprintf(w, "  Applies to: (none)\n")
	}
}

// registerNotifyCommands attaches `ballast notify ...` to root.
func registerNotifyCommands(root *cobra.Command) {
	notifySetCmd.Flags().BoolVar(&notifyEnabled, "enabled", true, "alert when no good backup has happened for too long")
	notifySetCmd.Flags().IntVar(&notifyMaxAgeHours, "max-age-hours", 0, "hours without a good backup before alerting (0 = based on the schedule)")
	notifySetCmd.Flags().BoolVar(&notifyHeartbeat, "heartbeat", false, "send a message after every verified backup")

	notifyChannelListCmd.Flags().BoolVar(&channelJSON, "json", false, "output as JSON")

	notifyChannelAddCmd.Flags().StringVar(&channelURL, "url", "", "channel address (required)")
	notifyChannelAddCmd.Flags().StringVar(&channelUsername, "username", "", "optional username for authentication")
	notifyChannelAddCmd.Flags().StringVar(&channelTokenFile, "token-file", "", "file containing the access token or password")
	notifyChannelAddCmd.Flags().BoolVar(&channelAllServices, "all-services", false, "apply to all services (default if no --service flags given)")
	notifyChannelAddCmd.Flags().StringSliceVar(&channelServices, "service", nil, "apply to this service (may be given multiple times)")

	notifyChannelAddCmd.Flags().StringVar(&channelType, "type", notify.ChannelTypeNtfy, "ntfy, webhook or email")
	addChannelRoutingFlags(notifyChannelAddCmd)
	addChannelRoutingFlags(notifyChannelEditCmd)
	notifyChannelEditCmd.Flags().StringVar(&channelURL, "url", "", "new channel address (leave empty to keep current)")
	notifyChannelEditCmd.Flags().StringVar(&channelUsername, "username", "", "new username (leave empty to keep current)")
	notifyChannelEditCmd.Flags().StringVar(&channelTokenFile, "token-file", "", "file containing new access token or password")
	notifyChannelEditCmd.Flags().BoolVar(&channelClearToken, "clear-token", false, "remove the access token")
	notifyChannelEditCmd.Flags().BoolVar(&channelAllServices, "all-services", false, "apply to all services")
	notifyChannelEditCmd.Flags().StringSliceVar(&channelServices, "service", nil, "apply to this service (may be given multiple times)")

	notifyChannelCmd := &cobra.Command{Use: "channel", Short: "Manage notification channels"}
	notifyChannelCmd.AddCommand(notifyChannelListCmd, notifyChannelAddCmd, notifyChannelEditCmd, notifyChannelRemoveCmd, notifyChannelTestCmd)

	notifyCmd := &cobra.Command{Use: "notify", Short: "Choose when Ballast alerts you about each service"}
	notifyCmd.AddCommand(notifyShowCmd, notifySetCmd, notifyChannelCmd, notifyDigestCmd)
	root.AddCommand(notifyCmd)
}

// notifyDigestCmd is `ballast notify digest`: it sends the weekly digest
// now to every channel that has it turned on.
var notifyDigestCmd = &cobra.Command{
	Use:   "digest",
	Short: "Send the weekly digest now",
	Long: `Sends the weekly digest right away to every notification channel that
has it turned on (ballast notify channel edit <name> --digest).

The digest is one message: how many services are green, amber and red,
when each one last had a verified restore, and free disk space.
` + "`ballast serve`" + ` (or ` + "`ballast run-due`" + `) sends it automatically once a week.`,
	Args: noArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			res, err := a.ops.SendDigest(cmd.Context())
			for _, n := range res.Sent {
				cmd.Printf("Sent the weekly digest to %s.\n", n)
			}
			return err
		})
	},
}
