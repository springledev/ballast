package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
)

var (
	verifyAll  bool
	verifyJSON bool
)

// verifyCmd is `ballast verify <service> | --all` (spec §9): a thin
// wrapper over api.Ops.VerifyService/VerifyAllServices, which
// re-verify a service's most recent snapshot the same way the real
// backup pipeline's VERIFYING phase does, without running a new
// backup.
var verifyCmd = &cobra.Command{
	Use:   "verify [service]",
	Short: "Re-verify the most recent backup of one service, or every configured service",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if verifyAll == (len(args) == 1) {
			return fmt.Errorf("verify: pass exactly one service name, or --all (not both)")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		client, err := useDaemon(cmd, cfg)
		if err != nil {
			return err
		}
		if client != nil {
			return verifyViaDaemon(cmd, client, args)
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			var reports []api.VerifyReport
			if verifyAll {
				reports, err = a.ops.VerifyAllServices(cmd.Context())
			} else {
				var rep api.VerifyReport
				rep, err = a.ops.VerifyService(cmd.Context(), args[0])
				reports = []api.VerifyReport{rep}
				// A single service that could not be verified at all
				// (no snapshot, misconfigured destination, ...) is a
				// hard command failure — --all instead reports it
				// per-service and only warns via the aggregate error.
				if err != nil {
					return err
				}
			}

			if verifyJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if encErr := enc.Encode(reports); encErr != nil {
					return encErr
				}
				return err
			}
			printVerifyReports(cmd, reports)
			return err
		})
	},
}

// verifyViaDaemon runs the verification inside the daemon and prints
// the same output the single-shot path does.
func verifyViaDaemon(cmd *cobra.Command, c *daemonClient, args []string) error {
	var reports []api.VerifyReport
	if verifyAll {
		if err := c.call(cmd.Context(), http.MethodPost, "/api/v1/verify", nil, &reports); err != nil {
			return fmt.Errorf("verifying all services: %w", err)
		}
	} else {
		var rep api.VerifyReport
		if err := c.call(cmd.Context(), http.MethodPost, "/api/v1/services/"+url.PathEscape(args[0])+"/verify", nil, &rep); err != nil {
			return fmt.Errorf("verifying %s: %w", args[0], err)
		}
		reports = []api.VerifyReport{rep}
	}
	if verifyJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			return err
		}
	} else {
		printVerifyReports(cmd, reports)
	}
	var bad []string
	for _, r := range reports {
		if r.Light == "red" {
			bad = append(bad, r.Service)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("verification failed for: %s", strings.Join(bad, ", "))
	}
	return nil
}

// printVerifyReports renders each report in plain English (spec §10
// beginner requirement).
func printVerifyReports(cmd *cobra.Command, reports []api.VerifyReport) {
	for _, r := range reports {
		cmd.Printf("%s: %s\n", r.Service, lightLabel(r.Light))
		if r.Error != "" {
			cmd.Printf("  %s\n", r.Error)
			continue
		}
		for _, c := range r.Checks {
			mark := "FAILED"
			if c.Passed {
				mark = "ok"
			}
			cmd.Printf("  - %s: %s — %s\n", c.Method, mark, c.Detail)
			if c.Warning != "" {
				cmd.Printf("    warning: %s\n", c.Warning)
			}
		}
	}
}

func init() {
	verifyCmd.Flags().BoolVar(&verifyAll, "all", false, "verify every configured service")
	verifyCmd.Flags().BoolVar(&verifyJSON, "json", false, "print machine-readable JSON")
}
