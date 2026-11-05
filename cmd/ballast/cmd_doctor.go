package main

import (
	"encoding/json"

	"github.com/spf13/cobra"
	"github.com/springledev/ballast/internal/api"
)

var (
	doctorJSON     bool
	selfBackupJSON bool
)

// doctorCmd is `ballast doctor [--json]` (spec §9): a thin wrapper over
// api.Ops.Doctor, which checks restic, Docker hosts, Proxmox, destinations,
// and disk space to catch configuration problems early.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check restic, Docker hosts, destinations, and disk space for common configuration problems",
	Args:  noArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		forceCredentialNotice = true
		defer func() { forceCredentialNotice = false }()
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			report, err := a.ops.Doctor(cmd.Context())
			if doctorJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if encErr := enc.Encode(report); encErr != nil {
					return encErr
				}
				return err
			}
			printDoctorReport(cmd, report)
			return err
		})
	},
}

// printDoctorReport renders each check in plain English (spec §10
// beginner requirement).
func printDoctorReport(cmd *cobra.Command, report api.DoctorReport) {
	for _, c := range report.Checks {
		mark := "[FAIL]"
		switch c.Status {
		case api.DoctorOK:
			mark = "[ok]"
		case api.DoctorWarn:
			mark = "[WARN]"
		}
		cmd.Printf("%s %s %s — %s\n", mark, c.Area, c.Name, c.Detail)
	}
}

// selfBackupCmd is `ballast self-backup <destination> [--json]`: a thin
// wrapper over api.Ops.SelfBackup, which backs up Ballast's own state DB,
// config, and catalog to a destination and verifies it.
var selfBackupCmd = &cobra.Command{
	Use:   "self-backup <destination>",
	Short: "Back up Ballast's own state database and configuration to a destination",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			result, err := a.ops.SelfBackup(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if selfBackupJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			printSelfBackupResult(cmd, result)
			return nil
		})
	},
}

// printSelfBackupResult renders the self-backup result in plain English.
func printSelfBackupResult(cmd *cobra.Command, result api.SelfBackupResult) {
	cmd.Printf("Self-backup to %s: snapshot %s\n", result.Destination, result.SnapshotID)
	cmd.Printf("Files: %d\n", len(result.Files))
	for _, f := range result.Files {
		cmd.Printf("  - %s\n", f)
	}
	verified := "not verified"
	if result.Verified {
		verified = "verified"
	}
	cmd.Printf("Status: %s\n", verified)
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "print machine-readable JSON")
	selfBackupCmd.Flags().BoolVar(&selfBackupJSON, "json", false, "print machine-readable JSON")
}
