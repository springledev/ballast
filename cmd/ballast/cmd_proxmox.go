package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/inventory"
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

			// Each verification layer is reported on its own (spec §17.4).
			if res.Verification == nil {
				cmd.Println("This dump is not verified as restorable.")
				return nil
			}
			for _, l := range res.Verification.Layers {
				mark := "PASS"
				if !l.Passed {
					mark = "FAIL"
				}
				cmd.Printf("  [%s] %s\n", mark, l.Detail)
			}
			if !res.Verification.Verified {
				return fmt.Errorf("guest %d was backed up but is NOT verified as restorable — see the failed checks above", res.VMID)
			}
			cmd.Println("Verified: the guest's settings and every attached service restore correctly.")

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

var (
	jobName, jobNode, jobStorage, jobMode, jobSchedule, jobCopyTo, jobCopyPath string
	jobVMID, jobKeepLast, jobReadBackMiB                                       int
)

var proxmoxJobAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Back up a Proxmox guest on a schedule",
	Long: `Add a scheduled Proxmox guest backup. It runs like any other Ballast service:
the guest is dumped with vzdump, the dump is checked (it is on the storage at
the size Proxmox reported and its settings read back), and the light turns
green only when that passes.

"--keep-last" removes only the dumps Ballast made itself; other backups on the
same storage are never touched. To also send each dump to a backup destination
(for example an offsite copy), give --copy-to and --copy-path, the folder on
this machine where the Proxmox storage appears (such as an NFS mount).

Example:

  ballast proxmox job add --name pve-vm100 --node pve --vmid 100 --storage backup-nas --keep-last 2 --schedule "0 3 * * 0"`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			s := inventory.Service{
				Name: jobName, Kind: inventory.KindProxmoxGuest,
				ProxmoxNode: jobNode, ProxmoxVMID: jobVMID,
				Destination: jobCopyTo,
				Guest:       &inventory.GuestJob{Storage: jobStorage, Mode: jobMode, KeepLast: jobKeepLast, CopyPath: jobCopyPath, ReadBackSampleMiB: jobReadBackMiB},
			}
			if err := a.ops.PutService(cmd.Context(), s); err != nil {
				return err
			}
			cmd.Printf("%s: will back up guest %d on %s to %s\n", jobName, jobVMID, jobNode, jobStorage)
			if jobSchedule != "" {
				if err := a.ops.SetSchedule(cmd.Context(), jobName, jobSchedule); err != nil {
					return err
				}
				cmd.Printf("%s: scheduled %q\n", jobName, jobSchedule)
				cmd.Println("A new schedule never starts a backup by surprise. It first runs at its next slot, unless this has never been backed up at all, which runs on the next check. To back up right now, run `ballast backup`.")
			}
			printJobPolicies(cmd, a, jobName)
			if chk, err := a.ops.CheckProxmoxStorage(cmd.Context(), jobName, jobNode, jobStorage, jobVMID); err == nil && chk.Warning != "" {
				cmd.Println("Note: " + chk.Warning)
			}
			return nil
		})
	},
}

var cfgName, cfgTo, cfgSchedule, cfgOut string

var proxmoxConfigAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Save the Proxmox settings on a schedule",
	Long: `Add a scheduled snapshot of the Proxmox host's own settings: firewall rules,
every guest's settings, storage definitions, pools, users and permissions. They
are read through the API, saved as JSON files in a backup destination, and read
back to prove every guest is included. Passwords, tokens and keys are left out.
Ballast only reads from Proxmox here; it never changes its settings.

Example:

  ballast proxmox config add --name pve-config --to local --schedule "30 2 * * *"`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			s := inventory.Service{Name: cfgName, Kind: inventory.KindProxmoxConfig, Destination: cfgTo}
			if err := a.ops.PutService(cmd.Context(), s); err != nil {
				return err
			}
			cmd.Printf("%s: will save the Proxmox settings to %s\n", cfgName, cfgTo)
			if cfgSchedule != "" {
				if err := a.ops.SetSchedule(cmd.Context(), cfgName, cfgSchedule); err != nil {
					return err
				}
				cmd.Printf("%s: scheduled %q\n", cfgName, cfgSchedule)
			}
			return nil
		})
	},
}

var proxmoxConfigShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Read the Proxmox settings now and print or save them",
	Long: `Read the Proxmox host's settings right now and print them as JSON, or save them
to a folder with --out (one file per part). Nothing is stored in a backup and
nothing is changed in Proxmox.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			v, err := a.ops.ViewProxmoxConfig(cmd.Context())
			if err != nil {
				return err
			}
			for _, s := range v.Skipped {
				cmd.PrintErrf("not readable with this token: %s\n", s)
			}
			if cfgOut == "" {
				for _, part := range proxmox.SnapshotParts() {
					cmd.Printf("// %s\n%s\n", part, v.Parts[part])
				}
				return nil
			}
			if err := os.MkdirAll(cfgOut, 0o700); err != nil {
				return fmt.Errorf("creating %s: %w", cfgOut, err)
			}
			for _, part := range proxmox.SnapshotParts() {
				p := filepath.Join(cfgOut, proxmox.SnapshotFileName(part))
				if err := os.WriteFile(p, v.Parts[part], 0o600); err != nil {
					return fmt.Errorf("writing %s: %w", p, err)
				}
				cmd.Println("saved " + p)
			}
			return nil
		})
	},
}

var proxmoxJobEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Change a Proxmox backup job's schedule, keep-last or copy destination",
	Long: `Change a Proxmox guest backup job in place. Only the settings you give are
changed; the job keeps its run history and verification record.

Examples:

  ballast proxmox job edit pve-vm100 --schedule "0 4 * * 0" --keep-last 3
  ballast proxmox job edit pve-vm100 --copy-to b2 --copy-path /mnt/pve-backup
  ballast proxmox job edit pve-vm100 --copy-to ""    # stop copying offsite`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fl := cmd.Flags()
		var e api.GuestJobEdit
		if fl.Changed("schedule") {
			e.Schedule = &jobSchedule
		}
		if fl.Changed("keep-last") {
			e.KeepLast = &jobKeepLast
		}
		if fl.Changed("copy-to") {
			e.CopyTo = &jobCopyTo
		}
		if fl.Changed("copy-path") {
			e.CopyPath = &jobCopyPath
		}
		if fl.Changed("read-back-sample-mib") {
			e.ReadBackSampleMiB = &jobReadBackMiB
		}
		if e == (api.GuestJobEdit{}) {
			return fmt.Errorf("nothing to change: give --schedule, --keep-last, --copy-to, --copy-path or --read-back-sample-mib")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if _, err := a.ops.EditGuestJob(cmd.Context(), args[0], e); err != nil {
				return err
			}
			cmd.Printf("%s: updated. Its run history and verification record are kept.\n", args[0])
			printJobPolicies(cmd, a, args[0])
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

	f := proxmoxJobAddCmd.Flags()
	f.StringVar(&jobName, "name", "", "name for this backup job (required)")
	f.StringVar(&jobNode, "node", "", "Proxmox node name (required)")
	f.IntVar(&jobVMID, "vmid", 0, "guest VMID (required)")
	f.StringVar(&jobStorage, "storage", "", "Proxmox storage the dump is written to (required)")
	f.StringVar(&jobMode, "mode", "snapshot", "backup mode: snapshot, suspend or stop")
	f.IntVar(&jobKeepLast, "keep-last", 0, "keep this many of the dumps Ballast made (0 keeps them all)")
	f.StringVar(&jobSchedule, "schedule", "", "cron schedule, e.g. \"0 3 * * 0\"")
	f.StringVar(&jobCopyTo, "copy-to", "", "also copy each dump to this backup destination")
	f.StringVar(&jobCopyPath, "copy-path", "", "folder on this machine where the Proxmox storage appears (needed with --copy-to)")
	f.IntVar(&jobReadBackMiB, "read-back-sample-mib", 0, "check a copy by downloading back only the first N MiB of the dump instead of all of it (0 downloads it all; faster for big dumps, but the rest is not read back)")
	for _, n := range []string{"name", "node", "vmid", "storage"} {
		_ = proxmoxJobAddCmd.MarkFlagRequired(n)
	}
	jobCmd := &cobra.Command{Use: "job", Short: "Scheduled Proxmox guest backups"}
	ef := proxmoxJobEditCmd.Flags()
	ef.StringVar(&jobSchedule, "schedule", "", "new cron schedule")
	ef.IntVar(&jobReadBackMiB, "read-back-sample-mib", 0, "check a copy by downloading back only the first N MiB of the dump instead of all of it (0 downloads it all; faster for big dumps, but the rest is not read back)")
	ef.IntVar(&jobKeepLast, "keep-last", 0, "keep this many of the dumps Ballast made (0 keeps them all)")
	ef.StringVar(&jobCopyTo, "copy-to", "", "copy each dump to this backup destination (empty stops copying)")
	ef.StringVar(&jobCopyPath, "copy-path", "", "folder on this machine where the Proxmox storage appears")
	jobCmd.AddCommand(proxmoxJobAddCmd, proxmoxJobEditCmd)

	cf := proxmoxConfigAddCmd.Flags()
	cf.StringVar(&cfgName, "name", "proxmox-config", "name for this job")
	cf.StringVar(&cfgTo, "to", "", "backup destination to store the settings in (required)")
	cf.StringVar(&cfgSchedule, "schedule", "", "cron schedule, e.g. \"30 2 * * *\"")
	_ = proxmoxConfigAddCmd.MarkFlagRequired("to")
	proxmoxConfigShowCmd.Flags().StringVar(&cfgOut, "out", "", "folder to save the JSON files in (default: print them)")
	configCmd := &cobra.Command{Use: "config", Short: "Proxmox settings snapshots"}
	configCmd.AddCommand(proxmoxConfigAddCmd, proxmoxConfigShowCmd)

	proxmoxCmd := &cobra.Command{Use: "proxmox", Short: "Manage Ballast's connection to a Proxmox host"}
	proxmoxCmd.AddCommand(proxmoxConnectCmd, proxmoxTestCmd, proxmoxGuestsCmd, proxmoxBackupCmd, proxmoxRemoveCmd, jobCmd, configCmd)
	root.AddCommand(proxmoxCmd)
}

// printJobPolicies says how many backups of a Proxmox job are kept on
// the Proxmox storage and in each copy destination, so a copy never
// quietly keeps more than the operator expects.
func printJobPolicies(cmd *cobra.Command, a *app, name string) {
	s, err := a.ops.GetService(cmd.Context(), name)
	if err != nil || s.Guest == nil {
		return
	}
	if s.Guest.KeepLast > 0 {
		cmd.Printf("%s: on %s Ballast keeps the newest %d dump(s) it made.\n", name, s.Guest.Storage, s.Guest.KeepLast)
	} else {
		cmd.Printf("%s: on %s Ballast keeps every dump it makes (give --keep-last to limit this).\n", name, s.Guest.Storage)
	}
	for _, t := range s.EffectiveTargets() {
		d, err := a.ops.GetDestination(cmd.Context(), t.Destination)
		if err != nil {
			continue
		}
		own := ""
		if t.Retention == nil {
			own = " (set with `ballast service retention` to change it)"
		}
		cmd.Printf("%s: in %s the copies keep %s%s.\n", name, d.Name, retentionWords(t.EffectiveRetention(d)), own)
	}
}
