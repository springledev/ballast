// Command ballast is the single static binary for the Ballast backup
// tool: `ballast serve` hosts the API and web GUI, while `ballast
// backup`, `run-due` and friends drive backups from cron/systemd.
//
// This is the scaffolding step (spec §14 step 1): a working Cobra CLI
// with a minimal `serve` that exposes the embedded stub page and a
// `version` command. Later §14 steps fill in the engine behind these
// commands.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/springledev/ballast/web"
	"github.com/spf13/cobra"
)

// version is the build version, overridable at build time with
// -ldflags "-X main.version=...". Defaults to a plain dev marker.
var version = "dev"

var rootCmd = &cobra.Command{
	Use:   "ballast",
	Short: "Service-aware, verified-restorable backups for Docker homelabs",
	Long: `Ballast backs up Docker-hosted services using per-service
strategies from a YAML catalog, ships them via restic, and verifies
that every backup actually restores before marking it done.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the ballast version",
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.Println(version)
		return nil
	},
}

var serveAddr string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the Web UI and REST API",
	RunE: func(cmd *cobra.Command, args []string) error {
		h, err := web.Handler()
		if err != nil {
			return fmt.Errorf("loading embedded web UI: %w", err)
		}
		log.Printf("ballast %s listening on http://%s", version, serveAddr)
		return http.ListenAndServe(serveAddr, h)
	},
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:8000", "listen address for the web server")
	rootCmd.AddCommand(serveCmd, versionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
