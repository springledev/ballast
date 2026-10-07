package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	destLocation           string
	destType               string
	destPasswordCredential string
	destInit               bool
	destKeepDaily          int
	destKeepWeekly         int
	destKeepMonthly        int
	destKeepLast           int

	// Backend settings flags (spec §17.1): which of these apply
	// depends on --type, and backend.Location rejects any that don't.
	destPath      string
	destEndpoint  string
	destBucket    string
	destRegion    string
	destAccountID string
	destHost      string
	destPort      int
	destUser      string
	destURL       string
	destMaxMB     int

	// Backend credential flags. The secret access key itself is never
	// accepted as a flag value (it would sit in shell history and be
	// visible to anyone listing processes) — only its key ID is, and
	// the secret is then read from stdin, exactly like `ballast secret
	// set` reads a secret value.
	destAccessKeyID string
	destSSHKeyFile  string
)

var destinationAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Configure a new restic backup destination",
	Long: `Configure a new restic backup destination.

--type chooses the storage backend; each type takes its own settings
flags. Examples:

  # A folder on this machine or a mounted NAS share
  ballast destination add local --type local --path /srv/backups/ballast --init

  # Amazon S3 or an S3-compatible service (MinIO, Wasabi, ...)
  ballast destination add aws --type s3 --endpoint s3.amazonaws.com --bucket my-bucket --region us-east-1 \
      --access-key-id AKIA... --init
  (the secret access key is then read from stdin, one line — never pass it as a flag)

  # Backblaze B2, via its S3-compatible API
  ballast destination add b2 --type b2 --endpoint s3.us-west-004.backblazeb2.com --bucket my-bucket \
      --access-key-id <keyID> --init

  # Cloudflare R2
  ballast destination add r2 --type r2 --account-id <accountID> --bucket my-bucket \
      --access-key-id <keyID> --init

  # Any server reachable over SSH
  ballast destination add nas --type sftp --host backup.example.com --user ballast --path /srv/backups/ballast \
      --ssh-key-file ./ballast_key --init

  # A second copy of config and database backups in Proton Drive (report-only),
  # then sign in with: ballast destination sign-in proton
  ballast destination add proton --type proton-drive --path /my-files/ballast --keep-last 7

  # A raw restic repository string for advanced setups (rest-server, rclone:, ...)
  ballast destination add rest --type custom --url rest:https://backup.example.com/ballast --init

For backward compatibility, --location alone (with no --type) still works
exactly as before: an absolute path is treated as a local destination, and
anything else as a custom restic repository string.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		creds, err := readDestinationCredentials(cmd)
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			d := inventory.Destination{
				Name: args[0], PasswordCredential: destPasswordCredential,
				KeepLast: destKeepLast, KeepDaily: destKeepDaily, KeepWeekly: destKeepWeekly, KeepMonthly: destKeepMonthly,
			}
			if destType != "" {
				d.Type = backend.Type(destType)
				d.Settings = backend.Settings{
					Path: destPath, Endpoint: destEndpoint, Bucket: destBucket, Region: destRegion,
					AccountID: destAccountID, Host: destHost, Port: destPort, User: destUser, URL: destURL, MaxArtifactMB: destMaxMB,
				}
			} else {
				d.Location = destLocation
			}
			req := api.DestinationSetup{Destination: d, Credentials: creds, Init: destInit}
			if err := a.ops.SetupDestination(cmd.Context(), req); err != nil {
				return err
			}
			saved, err := a.ops.GetDestination(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			cmd.Printf("%s: configured (%s) at %s (repository password file: %s)\n", saved.Name, saved.Type, saved.Location, saved.PasswordCredential)
			if destInit {
				cmd.Println("  repository initialized")
			}
			return nil
		})
	},
}

// readDestinationCredentials builds a backend.Credentials from the
// command's credential flags. Only the access key *ID* and an SSH key
// *file path* are ever accepted as flag values; the secret itself is
// always read from stdin, one line, exactly like `ballast secret set`
// reads a value from stdin.
func readDestinationCredentials(cmd *cobra.Command) (backend.Credentials, error) {
	if destAccessKeyID != "" && destSSHKeyFile != "" {
		return backend.Credentials{}, fmt.Errorf("give either --access-key-id or --ssh-key-file, not both")
	}
	if destAccessKeyID != "" {
		if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			// Typed at a terminal: never echo the secret on screen.
			cmd.PrintErr("Secret access key: ")
			raw, err := term.ReadPassword(int(f.Fd()))
			cmd.PrintErrln()
			if err != nil {
				return backend.Credentials{}, fmt.Errorf("reading the secret access key: %w", err)
			}
			return backend.Credentials{AccessKeyID: destAccessKeyID, SecretAccessKey: strings.TrimSpace(string(raw))}, nil
		}
		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && line == "" {
			return backend.Credentials{}, fmt.Errorf("no secret access key given: pipe it on stdin")
		}
		return backend.Credentials{AccessKeyID: destAccessKeyID, SecretAccessKey: strings.TrimRight(line, "\r\n")}, nil
	}
	if destSSHKeyFile != "" {
		data, err := os.ReadFile(destSSHKeyFile)
		if err != nil {
			return backend.Credentials{}, fmt.Errorf("reading SSH key file %s: %w", destSSHKeyFile, err)
		}
		return backend.Credentials{SSHPrivateKey: string(data)}, nil
	}
	return backend.Credentials{}, nil
}

var destinationListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured destinations",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			list, err := a.ops.ListDestinations(cmd.Context())
			if err != nil {
				return err
			}
			if len(list) == 0 {
				cmd.Println("No destinations configured yet. Run `ballast destination add` to add one.")
				return nil
			}
			for _, d := range list {
				cmd.Printf("%s\t%s\t%s\tkeep=%dd/%dw/%dm\n", d.Name, d.Type, d.Location, d.KeepDaily, d.KeepWeekly, d.KeepMonthly)
			}
			return nil
		})
	},
}

// destinationSignInCmd is `ballast destination sign-in <name>`: the
// terminal twin of the web UI's "Sign in to Proton Drive".
var destinationSignInCmd = &cobra.Command{
	Use:   "sign-in <name>",
	Short: "Sign in to Proton Drive for a Proton Drive destination",
	Long: `Starts Proton's own browser sign-in and prints the link to open (on any
device). Your Proton password and 2FA code are typed on Proton's page and
never pass through Ballast.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			info, err := a.ops.ProtonSignIn(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			shown := false
			for !info.Done {
				if info.Link != "" && !shown {
					shown = true
					cmd.Printf("Open this link and sign in to Proton:\n  %s\n", info.Link)
					if info.Code != "" {
						cmd.Printf("If asked for a code, enter: %s\n", info.Code)
					}
					cmd.Println("Waiting for you to finish...")
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(time.Second):
				}
				if info, err = a.ops.ProtonSignInStatus(cmd.Context(), args[0]); err != nil {
					return err
				}
			}
			if info.Error != "" {
				return fmt.Errorf("destination %s: %s", args[0], info.Error)
			}
			cmd.Printf("%s: signed in to Proton Drive\n", args[0])
			return nil
		})
	},
}

// destinationTestCmd is `ballast destination test <name>` (spec §9).
var destinationTestCmd = &cobra.Command{
	Use:   "test <name>",
	Short: "Check that a destination is reachable",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.TestDestination(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("%s: reachable\n", args[0])
			return nil
		})
	},
}

// destinationInitCmd is `ballast destination init <name>` (spec §14
// step 19): set up (or re-check) a destination's restic repository
// without resending its settings, for a destination that was added
// without --init, or whose storage was emptied out from under it.
var destinationInitCmd = &cobra.Command{
	Use:   "init <name>",
	Short: "Set up a destination's backup repository",
	Long: `Set up a destination's backup repository.

This is only needed if the destination was added without ticking "set
up the storage now" (or, on the command line, without --init), or if
the storage it points at was emptied since. It's safe to run again on
a destination that's already set up — nothing happens.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.InitDestination(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("repository for %s is ready\n", args[0])
			return nil
		})
	},
}

// healthKeep is the --read-data default meaning "leave it as it is".
const healthKeep = "keep"

var (
	healthEvery  int
	healthSubset string
	healthOff    bool
)

// destinationCheckCmd is `ballast destination check <name>` (spec
// §19.7): run restic check on a destination now.
var destinationCheckCmd = &cobra.Command{
	Use:   "check <name>",
	Short: "Check that a destination's backup repository is healthy",
	Long: `Check that a destination's backup repository is healthy.

This runs "restic check" and reads back a share of the stored data to
make sure it is not damaged. Ballast also does this on a schedule (by
default weekly). Use --every, --read-data or --off to change how the
schedule works instead of checking now.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			name := args[0]
			every, subset, off := healthEvery, healthSubset, healthOff
			// Reset so a second run in the same process starts clean.
			healthEvery, healthSubset, healthOff = 0, healthKeep, false
			if every != 0 || subset != healthKeep || off {
				cur, err := a.ops.GetRepoHealth(cmd.Context(), name)
				if err != nil {
					return err
				}
				s := api.RepoHealthSettings{Enabled: !off, IntervalHours: cur.IntervalHours, Subset: cur.Subset}
				if every != 0 {
					s.IntervalHours = every
				}
				if subset != healthKeep {
					s.Subset = subset
				}
				if _, err := a.ops.ConfigureRepoHealth(cmd.Context(), name, s); err != nil {
					return err
				}
				if !s.Enabled {
					cmd.Printf("%s: scheduled checks are off\n", name)
				} else {
					cmd.Printf("%s: checked every %d hours, reading back %q of the data\n", name, s.IntervalHours, s.Subset)
				}
				return nil
			}
			st, err := a.ops.CheckRepoHealth(cmd.Context(), name)
			if err != nil {
				return err
			}
			cmd.Printf("%s: %s - %s\n", name, st.Light, st.Detail)
			if st.State != "ok" {
				return fmt.Errorf("destination %s: %s", name, st.Detail)
			}
			return nil
		})
	},
}

var destinationRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a configured destination",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.DeleteDestination(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("%s: removed\n", args[0])
			return nil
		})
	},
}

// registerDestinationCommands attaches `ballast destination ...`
// (spec §9) to root.
func registerDestinationCommands(root *cobra.Command) {
	destinationAddCmd.Flags().StringVar(&destType, "type", "", "destination type: local, s3, b2, r2, sftp, or custom (default: infer from --location)")
	destinationAddCmd.Flags().StringVar(&destLocation, "location", "", "restic repository location, e.g. /srv/backups or s3:https://... (used when --type is empty)")
	destinationAddCmd.Flags().StringVar(&destPath, "path", "", "folder path (local, sftp) or folder inside the bucket (s3, b2, r2)")
	destinationAddCmd.Flags().StringVar(&destEndpoint, "endpoint", "", "S3 API endpoint (s3, b2), e.g. s3.amazonaws.com")
	destinationAddCmd.Flags().StringVar(&destBucket, "bucket", "", "bucket name (s3, b2, r2)")
	destinationAddCmd.Flags().StringVar(&destRegion, "region", "", "region (s3 only; b2 and r2 derive theirs automatically)")
	destinationAddCmd.Flags().StringVar(&destAccountID, "account-id", "", "Cloudflare account ID (r2 only)")
	destinationAddCmd.Flags().StringVar(&destHost, "host", "", "server host name or IP address (sftp only)")
	destinationAddCmd.Flags().IntVar(&destPort, "port", 0, "server port (sftp only, default 22)")
	destinationAddCmd.Flags().StringVar(&destUser, "user", "", "user to log in as (sftp only)")
	destinationAddCmd.Flags().StringVar(&destURL, "url", "", "raw restic repository string (custom only)")
	destinationAddCmd.Flags().IntVar(&destMaxMB, "max-artifact-mb", 0, "largest single file in MB a Proton Drive copy accepts (default 1024)")
	destinationAddCmd.Flags().StringVar(&destAccessKeyID, "access-key-id", "", "access key ID (s3, b2, r2); the matching secret access key is then read from stdin, one line")
	destinationAddCmd.Flags().StringVar(&destSSHKeyFile, "ssh-key-file", "", "path to an unencrypted SSH private key file (sftp only)")
	destinationAddCmd.Flags().StringVar(&destPasswordCredential, "password-credential", "", "credential file name holding the repository password (default: ballast-restic-password.<name>)")
	destinationAddCmd.Flags().BoolVar(&destInit, "init", false, "initialize the restic repository now")
	destinationAddCmd.Flags().IntVar(&destKeepLast, "keep-last", 0, "newest snapshots to keep whatever their age (0 = off; set the daily/weekly/monthly flags to 0 to keep only these)")
	destinationAddCmd.Flags().IntVar(&destKeepDaily, "keep-daily", 7, "daily snapshots to keep")
	destinationAddCmd.Flags().IntVar(&destKeepWeekly, "keep-weekly", 4, "weekly snapshots to keep")
	destinationAddCmd.Flags().IntVar(&destKeepMonthly, "keep-monthly", 6, "monthly snapshots to keep")

	destinationCheckCmd.Flags().IntVar(&healthEvery, "every", 0, "change the schedule: hours between checks (default 168 = weekly)")
	destinationCheckCmd.Flags().StringVar(&healthSubset, "read-data", healthKeep, "change the schedule: share of data to read back (default 5%; 1/10 or 500M also work; empty = structure only)")
	destinationCheckCmd.Flags().BoolVar(&healthOff, "off", false, "turn scheduled checks off for this destination")

	destinationCmd := &cobra.Command{Use: "destination", Short: "Manage backup destinations"}
	destinationCmd.AddCommand(destinationAddCmd, destinationListCmd, destinationTestCmd, destinationInitCmd, destinationSignInCmd, destinationCheckCmd, destinationRemoveCmd)
	root.AddCommand(destinationCmd)
}
