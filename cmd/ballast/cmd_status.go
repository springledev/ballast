package main

import (
	"encoding/json"

	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
)

var (
	statusService string
	statusJSON    bool
	statusVerify  bool
)

// statusCmd is `ballast status [--service X] [--json] [--verify]`
// (spec §9).
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show backup status for one or all configured services",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			st, err := a.ops.Status(cmd.Context(), statusService, statusVerify)
			if err != nil {
				return err
			}
			if statusJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			printStatusPlain(cmd, st)
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
	statusCmd.Flags().StringVar(&statusService, "service", "", "show only this service")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "print machine-readable JSON")
	statusCmd.Flags().BoolVar(&statusVerify, "verify", false, "include per-check verification detail")
}
