package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
)

var (
	statusService string
	statusJSON    bool
	statusVerify  bool
	statusRunning bool
)

// statusCmd is `ballast status [--service X] [--json] [--verify]`
// (spec §9).
var statusCmd = &cobra.Command{
	Use:   "status [service]",
	Args:  cobra.MaximumNArgs(1),
	Short: "Show backup status for one or all configured services",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		service := statusService
		if len(args) == 1 {
			if service != "" && service != args[0] {
				return fmt.Errorf("`ballast status` got two different services (%q and --service %q); give just one", args[0], service)
			}
			service = args[0]
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

// printStatusPlain renders status in plain English (spec §10
// beginner requirement: never assume the reader knows the jargon).
func printStatusPlain(cmd *cobra.Command, services []api.ServiceStatus) {
	if len(services) == 0 {
		cmd.Println("No services configured yet. Run `ballast service add` to add one.")
		return
	}
	for _, s := range services {
		tier := s.Tiers[0]
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
