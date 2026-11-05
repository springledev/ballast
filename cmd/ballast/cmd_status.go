package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/springledev/ballast/internal/api"
)

var (
	statusService string
	statusJSON    bool
	statusVerify  bool
	statusRunning bool
	statusCheck   bool
)

// statusCmd is `ballast status [--service X] [--json] [--verify]`
// (spec §9), plus `--check` for scripts (spec §21.2).
var statusCmd = &cobra.Command{
	Use:   "status [service]",
	Args:  cobra.MaximumNArgs(1),
	Short: "Show backup status for one or all configured services",
	RunE: func(cmd *cobra.Command, args []string) error {
		if statusCheck {
			if statusRunning {
				return fmt.Errorf("`ballast status --check` cannot be combined with --running: --check gives one exit code for every service and --running gives another. Use one of them")
			}
			if statusJSON {
				return fmt.Errorf("`ballast status --check` cannot be combined with --json, because its summary line is plain text. Use one of them")
			}
		}
		service := statusService
		if len(args) == 1 {
			if service != "" && service != args[0] {
				return fmt.Errorf("`ballast status` got two different services (%q and --service %q); give just one", args[0], service)
			}
			service = args[0]
		}
		if statusCheck {
			return runStatusCheck(cmd, service)
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if statusRunning {
				return reportRunning(cmd, a)
			}
			st, err := a.ops.Status(cmd.Context(), service, statusVerify)
			if err != nil {
				return err
			}
			if statusJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			printStatusPlain(cmd, st)
			printStatusDestinations(cmd, a, st, service != "")
			return nil
		})
	},
}

// runStatusCheck answers `status --check` (spec §21.2). A status that
// cannot be read at all is exit 3, with the reason on stderr; otherwise
// the exit code and one summary line come from checkStatus.
func runStatusCheck(cmd *cobra.Command, service string) error {
	var st []api.ServiceStatus
	cfg, err := loadConfig()
	if err == nil {
		err = withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			var serr error
			st, serr = a.ops.Status(cmd.Context(), service, false)
			return serr
		})
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Status cannot be read: %v\n", err)
		return &exitError{code: 3}
	}
	code, summary := checkStatus(st)
	cmd.Println(summary)
	if code == 0 {
		return nil
	}
	return &exitError{code: code}
}

// checkStatus maps the services' lights to the exit code of
// `status --check` and a one-line summary (spec §21.2). The worst light
// wins: 2 when any service is red; otherwise 1 when any is amber, stale,
// running or has no backups yet, since none of those is confirmed green;
// otherwise 0. No services at all is 1 for the same reason.
func checkStatus(services []api.ServiceStatus) (int, string) {
	// The self-backup waiting for its first scheduled run has no light
	// yet, which is expected, not a problem: leave it out until it runs.
	confirmable := make([]api.ServiceStatus, 0, len(services))
	var waiting []api.ServiceStatus
	for _, s := range services {
		if s.FirstRunPending {
			waiting = append(waiting, s)
		} else {
			confirmable = append(confirmable, s)
		}
	}
	services = confirmable
	if len(services) == 0 && len(waiting) > 0 {
		when := ""
		if waiting[0].FirstRunAt != nil {
			when = " (due " + waiting[0].FirstRunAt.Local().Format("Mon 2 Jan 15:04") + ")"
		}
		return 1, fmt.Sprintf("%s is waiting for its first scheduled run%s, so there is no backup to confirm yet.", waiting[0].Service, when)
	}
	if len(services) == 0 {
		return 1, "No services are configured, so there is no backup to confirm."
	}
	var red, amber, running, none, green []string
	for _, s := range services {
		light, stale := "", false
		if len(s.Tiers) > 0 {
			light, stale = s.Tiers[0].Light, s.Tiers[0].Stale
		}
		name := s.Service
		if stale {
			name += " [stale]"
		}
		switch {
		case light == "red":
			red = append(red, name)
		case light == "amber" || (light == "green" && stale):
			amber = append(amber, name)
		case light == "green":
			green = append(green, name)
		case light == "running":
			running = append(running, name)
		default:
			none = append(none, name)
		}
	}
	code := 0
	switch {
	case len(red) > 0:
		code = 2
	case len(amber) > 0 || len(running) > 0 || len(none) > 0:
		code = 1
	}
	var parts []string
	if len(red) > 0 {
		parts = append(parts, fmt.Sprintf("%d red (%s)", len(red), strings.Join(red, ", ")))
	}
	if len(amber) > 0 {
		parts = append(parts, fmt.Sprintf("%d amber (%s)", len(amber), strings.Join(amber, ", ")))
	}
	if len(running) > 0 {
		parts = append(parts, fmt.Sprintf("%d running (%s)", len(running), strings.Join(running, ", ")))
	}
	if len(none) > 0 {
		parts = append(parts, fmt.Sprintf("%d no backups yet (%s)", len(none), strings.Join(none, ", ")))
	}
	if len(green) > 0 {
		parts = append(parts, fmt.Sprintf("%d green", len(green)))
	}
	return code, "Status: " + strings.Join(parts, ", ") + "."
}

// printStatusPlain renders status in plain English (spec §10
// beginner requirement: never assume the reader knows the jargon).
func printStatusPlain(cmd *cobra.Command, services []api.ServiceStatus) {
	if len(services) == 0 {
		cmd.Println("No services configured yet. Run `ballast service add` to add one.")
		return
	}
	for _, s := range services {
		tier := s.Tiers[0]
		if s.FirstRunPending {
			if s.FirstRunAt != nil {
				cmd.Printf("%s: no backups yet, first run at %s\n", s.Service, s.FirstRunAt.Format("Mon 2 Jan 15:04"))
			} else {
				cmd.Printf("%s: no backups yet, and no schedule will start one\n", s.Service)
			}
			continue
		}
		cmd.Printf("%s: %s\n", s.Service, lightLabel(tier.Light))
		if tier.Light == "running" && tier.Phase != "" {
			cmd.Printf("  now: %s (running for %s)\n", tier.Phase, (time.Duration(tier.ElapsedSeconds) * time.Second).String())
		}
		for _, t := range s.Targets {
			if t.Upload != nil && t.Upload.TotalBytes > 0 {
				cmd.Printf("  uploading to %s: %.0f%% (%s of %s)\n", t.Destination, t.Upload.Percent, humanSize(t.Upload.BytesDone), humanSize(t.Upload.TotalBytes))
			}
		}
		if tier.Waiting != "" {
			cmd.Printf("  %s\n", tier.Waiting)
		}
		if tier.LastError != "" {
			cmd.Printf("  last error: %s\n", tier.LastError)
		}
		if statusVerify {
			for _, v := range s.Verification {
				mark := "FAILED"
				if v.Passed {
					mark = "ok"
				}
				cmd.Printf("  - %s: %s — %s\n", v.Method, mark, v.Detail)
			}
		}
	}
}

// printStatusDestinations lists the repository health of the
// destinations behind the shown services (all of them when no single
// service was asked for). Health that cannot be read is skipped: it
// must never hide the backup status above it.
func printStatusDestinations(cmd *cobra.Command, a *app, shown []api.ServiceStatus, filtered bool) {
	list, err := a.ops.ListRepoHealth(cmd.Context())
	if err != nil || len(list) == 0 {
		return
	}
	want := map[string]bool{}
	for _, s := range shown {
		want[s.Destination] = true
		for _, t := range s.Targets {
			want[t.Destination] = true
		}
	}
	var lines []string
	for _, st := range list {
		if filtered && !want[st.Destination] {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: repository %s", st.Destination, healthSummary(st)))
	}
	if len(lines) == 0 {
		return
	}
	cmd.Println("Destinations:")
	for _, l := range lines {
		cmd.Println(l)
	}
}

// lightLabel renders a traffic-light color in plain English.
func lightLabel(light string) string {
	switch light {
	case "green":
		return "green (last backup succeeded and was verified)"
	case "amber":
		return "amber (backup uploaded but could not be fully verified)"
	case "red":
		return "red (last backup failed)"
	case "running":
		return "a backup is running right now"
	default:
		return "no backups yet"
	}
}

func init() {
	statusCmd.Flags().BoolVar(&statusRunning, "running", false, "only say whether a backup is running; exits 1 if one is, 0 if not")
	statusCmd.Flags().BoolVar(&statusCheck, "check", false, "for scripts: exit 0 when every service is green, 1 for amber, stale, running or no backups yet, 2 for red, 3 when the status cannot be read")
	statusCmd.Flags().StringVar(&statusService, "service", "", "show only this service")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "print machine-readable JSON")
	statusCmd.Flags().BoolVar(&statusVerify, "verify", false, "include per-check verification detail")
}

// reportRunning prints whether any backup is running and returns an
// exitError(1) when one is, so a script can branch on the exit code.
func reportRunning(cmd *cobra.Command, a *app) error {
	names, err := a.ops.RunningServices(cmd.Context())
	if err != nil {
		return err
	}
	if len(names) == 0 {
		cmd.Println("Nothing is running.")
		return nil
	}
	cmd.Printf("A backup is running right now: %s\n", strings.Join(names, ", "))
	return &exitError{code: 1}
}
