package main

import (
	"fmt"
	"os"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/spf13/cobra"
)

var (
	proxmoxHost           string
	proxmoxTokenID        string
	proxmoxInsecure       bool
	proxmoxSkipTest       bool
	proxmoxTokenSecret    string
	proxmoxSecretFile     string
	proxmoxBackupNode     string
	proxmoxBackupVMID     int
	proxmoxBackupStorage  string
	proxmoxBackupMode     string
	proxmoxBackupKeepLast int
)

// readProxmoxCredentials builds a proxmox.Credentials from the
// command's token-secret flags. The secret can come from a file
// (--token-secret-file, e.g. read from a password manager's export)
// or be typed/piped directly (--token-secret); leaving both empty
// keeps whatever secret the connection already has.
func readProxmoxCredentials() (proxmox.Credentials, error) {
	if proxmoxTokenSecret != "" && proxmoxSecretFile != "" {
		return proxmox.Credentials{}, fmt.Errorf("give either --token-secret or --token-secret-file, not both")
	}
	if proxmoxSecretFile != "" {
		data, err := os.ReadFile(proxmoxSecretFile)
		if err != nil {
			return proxmox.Credentials{}, fmt.Errorf("reading token secret file %s: %w", proxmoxSecretFile, err)
		}
		return proxmox.Credentials{TokenSecret: string(data)}, nil
	}
	return proxmox.Credentials{TokenSecret: proxmoxTokenSecret}, nil
}

var proxmoxConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Configure Ballast's connection to a Proxmox host",
	Long: `Configure Ballast's connection to a Proxmox host (spec §17.3).

Ballast reaches Proxmox through its API, never SSH, using an API token scoped
to the VM.Backup permission on /vms. Create the token in the Proxmox web UI
under Datacenter -> Permissions -> API Tokens, then:

  ballast proxmox connect --host 192.168.1.10 --token-id ballast@pve!backup --token-secret-file ./token.txt

By default Ballast connects with the new settings before saving, so a typo
never leaves a connection that doesn't work; pass --no-test to save one that
is offline right now.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		creds, err := readProxmoxCredentials()
		if err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			conn := proxmox.Connection{Host: proxmoxHost, TokenID: proxmoxTokenID, Insecure: proxmoxInsecure}
			req := api.ProxmoxConnectionSetup{Connection: conn, Credentials: creds, SkipTest: proxmoxSkipTest}
			res, err := a.ops.SetupProxmoxConnection(cmd.Context(), req)
			if err != nil {
				return err
			}
			cmd.Printf("Proxmox: configured (%s)\n", res.Connection.Host)
			if res.Test != nil {
				cmd.Printf("  connected — Proxmox VE %s\n", res.Test.Version)
			} else {
				cmd.Println("  not tested (--no-test): the connection was saved without connecting to it")
			}
			return nil
		})
	},
}

var proxmoxTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Check that the configured Proxmox connection works",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			tr, err := a.ops.TestProxmoxConnection(cmd.Context())
			if err != nil {
				return err
			}
			cmd.Printf("connected — Proxmox VE %s\n", tr.Version)
			return nil
		})
	},
}

var proxmoxGuestsCmd = &cobra.Command{
	Use:   "guests",
	Short: "List the VMs and LXC containers on the Proxmox host",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			guests, err := a.ops.ListProxmoxGuests(cmd.Context())
			if err != nil {
				return err
			}
			if len(guests) == 0 {
				cmd.Println("No guests found.")
				return nil
			}
			for _, g := range guests {
				cmd.Printf("%s\t%d\t%s\t%s\t%s\n", g.Node, g.VMID, g.Type, g.Name, g.Status)
			}
			return nil
		})
	},
}

var proxmoxBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Back up a Proxmox guest with vzdump",
	Long: `Back up a Proxmox guest with vzdump to a specified storage.

Example:

  ballast proxmox backup --node pve --vmid 100 --storage local --mode snapshot

This is a dump only and is not verified as restorable until the verification
drill runs (spec §14 step 26). The result will include the backup volume ID,
size, and any pruned dumps.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			opts := proxmox.GuestBackupOptions{
				Node:     proxmoxBackupNode,
				VMID:     proxmoxBackupVMID,
				Storage:  proxmoxBackupStorage,
				Mode:     proxmoxBackupMode,
				KeepLast: proxmoxBackupKeepLast,
			}
			res, err := a.ops.BackupProxmoxGuest(cmd.Context(), opts)
			if err != nil {
				return err
			}

			// Print basic backup info
			cmd.Printf("backed up guest %d to %s\n", res.VMID, res.Volume)
			if res.Size > 0 {
				cmd.Printf("  size: %d bytes\n", res.Size)
			}

			// Print any pruned dumps
			if len(res.Pruned) > 0 {
				cmd.Printf("  pruned: %d dump(s)\n", len(res.Pruned))
				for _, v := range res.Pruned {
					cmd.Printf("    %s\n", v)
				}
			}

			// Print prune error if it occurred
			if res.PruneError != "" {
				cmd.Printf("  WARNING: pruning failed: %s\n", res.PruneError)
			}

			// Reminder that this is not yet verified
			cmd.Println("This dump is not verified as restorable until the verification drill runs.")

			return nil
		})
	},
}

var proxmoxRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove the configured Proxmox connection",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.DeleteProxmoxConnection(cmd.Context()); err != nil {
				return err
			}
			cmd.Println("Proxmox connection removed.")
			return nil
		})
	},
}

// registerProxmoxCommands attaches `ballast proxmox ...` (spec §14
// step 24, §17.3) to root.
func registerProxmoxCommands(root *cobra.Command) {
	proxmoxConnectCmd.Flags().StringVar(&proxmoxHost, "host", "", "Proxmox API address, e.g. 192.168.1.10 or https://192.168.1.10:8006 (required)")
	proxmoxConnectCmd.Flags().StringVar(&proxmoxTokenID, "token-id", "", "API token ID, e.g. ballast@pve!backup (required)")
	proxmoxConnectCmd.Flags().StringVar(&proxmoxTokenSecret, "token-secret", "", "the API token's secret (prefer --token-secret-file; a value here may be visible in your shell history)")
	proxmoxConnectCmd.Flags().StringVar(&proxmoxSecretFile, "token-secret-file", "", "path to a file containing the API token's secret")
	proxmoxConnectCmd.Flags().BoolVar(&proxmoxInsecure, "insecure", false, "skip TLS certificate verification (Proxmox's default certificate is self-signed)")
	proxmoxConnectCmd.Flags().BoolVar(&proxmoxSkipTest, "no-test", false, "save the connection without connecting to it first")
	_ = proxmoxConnectCmd.MarkFlagRequired("host")
	_ = proxmoxConnectCmd.MarkFlagRequired("token-id")

	proxmoxBackupCmd.Flags().StringVar(&proxmoxBackupNode, "node", "", "Proxmox node name (required)")
	proxmoxBackupCmd.Flags().IntVar(&proxmoxBackupVMID, "vmid", 0, "guest VMID (required)")
	proxmoxBackupCmd.Flags().StringVar(&proxmoxBackupStorage, "storage", "", "storage name for the backup, e.g. local (required)")
	proxmoxBackupCmd.Flags().StringVar(&proxmoxBackupMode, "mode", "snapshot", "backup mode: snapshot, suspend, or stop (default: snapshot)")
	proxmoxBackupCmd.Flags().IntVar(&proxmoxBackupKeepLast, "keep-last", 0, "keep this many recent backups, delete older ones (default: 0, prune nothing)")
	_ = proxmoxBackupCmd.MarkFlagRequired("node")
	_ = proxmoxBackupCmd.MarkFlagRequired("vmid")
	_ = proxmoxBackupCmd.MarkFlagRequired("storage")

	proxmoxCmd := &cobra.Command{Use: "proxmox", Short: "Manage Ballast's connection to a Proxmox host"}
	proxmoxCmd.AddCommand(proxmoxConnectCmd, proxmoxTestCmd, proxmoxGuestsCmd, proxmoxBackupCmd, proxmoxRemoveCmd)
	root.AddCommand(proxmoxCmd)
}
