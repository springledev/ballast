package main

import (
	"fmt"
	"strings"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/spf13/cobra"
)

var (
	serviceCatalogEntry string
	serviceDestination  string
	serviceDockerHost   string
	serviceProject      string
	serviceProxmoxNode  string
	serviceProxmoxVMID  int
	serviceParams       []string
	serviceContainers   []string
	serviceSecrets      []string
	serviceKeepDaily    int
	serviceKeepWeekly   int
	serviceKeepMonthly  int
	serviceKeepLast     int
	serviceExcludes     []string

	retentionKeepLast    int
	retentionKeepDaily   int
	retentionKeepWeekly  int
	retentionKeepMonthly int
	retentionInherit     bool
)

// parseKV parses a list of "key=value" flag values into a map.
func parseKV(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid key=value pair %q (want e.g. path=/data)", p)
		}
		out[k] = v
	}
	return out, nil
}

// retentionFlagsChanged reports whether any --keep-* flag was given.
func retentionFlagsChanged(cmd *cobra.Command) bool {
	f := cmd.Flags()
	return f.Changed("keep-last") || f.Changed("keep-daily") || f.Changed("keep-weekly") || f.Changed("keep-monthly")
}

// retentionOverrideFromFlags builds a service retention override from
// whichever --keep-* flags were actually given on cmd, starting from
// base (normally the destination's own policy) so an unspecified flag
// keeps that value rather than defaulting to 0 ("keep nothing"). It
// returns nil if no --keep-* flag was given at all.
func retentionOverrideFromFlags(cmd *cobra.Command, base inventory.Retention, last, daily, weekly, monthly int) *inventory.Retention {
	if !retentionFlagsChanged(cmd) {
		return nil
	}
	ret := base
	if cmd.Flags().Changed("keep-last") {
		ret.KeepLast = last
	}
	if cmd.Flags().Changed("keep-daily") {
		ret.KeepDaily = daily
	}
	if cmd.Flags().Changed("keep-weekly") {
		ret.KeepWeekly = weekly
	}
	if cmd.Flags().Changed("keep-monthly") {
		ret.KeepMonthly = monthly
	}
	return &ret
}

var serviceAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Configure a new backup service from the catalog",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		params, err := parseKV(serviceParams)
		if err != nil {
			return err
		}
		containers, err := parseKV(serviceContainers)
		if err != nil {
			return err
		}
		secrets, err := parseKV(serviceSecrets)
		if err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			s := inventory.Service{
				Name: args[0], CatalogEntry: serviceCatalogEntry, Destination: serviceDestination, DockerHost: serviceDockerHost,
				Params: params, Containers: containers, Secrets: secrets, Project: serviceProject,
				ProxmoxNode: serviceProxmoxNode, ProxmoxVMID: serviceProxmoxVMID,
				Excludes: serviceExcludes,
			}
			// --also-to adds more destinations; the main one keeps
			// the --keep-* override handled below.
			if len(serviceAlso) > 0 {
				s.Targets = []inventory.Target{{Destination: serviceDestination}}
				for _, v := range serviceAlso {
					t, err := parseTargetFlag(v)
					if err != nil {
						return err
					}
					s.Targets = append(s.Targets, t)
				}
			}
			// A per-service retention override is only set when at
			// least one --keep-* flag was actually given; unspecified
			// ones follow the destination's own value, not 0 (which
			// would mean "keep nothing").
			if retentionFlagsChanged(cmd) {
				dest, err := a.ops.GetDestination(cmd.Context(), serviceDestination)
				if err != nil {
					return err
				}
				base := inventory.Retention{KeepLast: dest.KeepLast, KeepDaily: dest.KeepDaily, KeepWeekly: dest.KeepWeekly, KeepMonthly: dest.KeepMonthly}
				s.Retention = retentionOverrideFromFlags(cmd, base, serviceKeepLast, serviceKeepDaily, serviceKeepWeekly, serviceKeepMonthly)
			}
			if len(s.Targets) > 0 {
				s.Targets[0].Retention = s.Retention
			}
			if err := a.ops.PutService(cmd.Context(), s); err != nil {
				return err
			}
			cmd.Printf("%s: configured (catalog entry %s, destination %s)\n", args[0], serviceCatalogEntry, serviceDestination)
			for _, t := range s.Targets[min(1, len(s.Targets)):] {
				cmd.Printf("  also sends to %s\n", targetLine(t))
			}
			if serviceDockerHost != "" {
				cmd.Printf("  Docker host: %s\n", serviceDockerHost)
			}
			if serviceProject != "" {
				cmd.Printf("  Project: %s (see `ballast restore-project`)\n", serviceProject)
			}
			if serviceProxmoxVMID != 0 {
				cmd.Printf("  Proxmox guest: node %s, VMID %d\n", serviceProxmoxNode, serviceProxmoxVMID)
			}
			if s.Retention != nil {
				cmd.Printf("  keeps %s (overrides %s)\n", retentionWords(*s.Retention), serviceDestination)
			}
			return nil
		})
	},
}

// retentionLine formats s's effective retention policy against its
// destination, and whether it's inherited or its own override —
// shared by `service list` and `service retention`.
func retentionLine(s inventory.Service, dest inventory.Destination) string {
	eff := s.EffectiveRetention(dest)
	if s.Retention == nil {
		return fmt.Sprintf("keeps %s (same as %s)", retentionWords(eff), dest.Name)
	}
	return fmt.Sprintf("keeps %s (its own setting, not %s's)", retentionWords(eff), dest.Name)
}

var serviceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured backup services",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			list, err := a.ops.ListServices(cmd.Context())
			if err != nil {
				return err
			}
			if len(list) == 0 {
				cmd.Println("No services configured yet. Run `ballast service add` to add one.")
				return nil
			}
			dests, err := a.ops.ListDestinations(cmd.Context())
			if err != nil {
				return err
			}
			byName := make(map[string]inventory.Destination, len(dests))
			for _, d := range dests {
				byName[d.Name] = d
			}
			for _, s := range list {
				cmd.Printf("%s\tcatalog=%s\tdestination=%s\thost=%s\n", s.Name, s.CatalogEntry, s.Destination, s.DockerHost)
				if s.Project != "" {
					cmd.Printf("  project=%s\n", s.Project)
				}
				if dest, ok := byName[s.Destination]; ok {
					cmd.Printf("  %s\n", retentionLine(s, dest))
				}
				targets := s.EffectiveTargets()
				if len(targets) == 0 {
					cmd.Println("  (runs on Proxmox, no restic destination)")
					continue
				}
				for _, t := range targets[1:] {
					cmd.Printf("  also sends to %s\n", targetLine(t))
				}
			}
			return nil
		})
	},
}

var serviceRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a configured backup service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.DeleteService(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("%s: removed\n", args[0])
			return nil
		})
	},
}

// serviceRetentionCmd is `ballast service retention <name> [--keep-*
// ...] [--inherit]` (spec §14 step 19): change or clear a service's
// own retention override without touching anything else about it.
// With no flags at all, it just prints the service's current
// effective policy.
var serviceRetentionCmd = &cobra.Command{
	Use:   "retention <name>",
	Short: "Show or change a service's own backup-retention override",
	Long: `Show or change a service's own backup-retention override.

With no flags, this just prints the service's current effective
policy (its own override, or the destination's, whichever applies).
Give one or more --keep-* flags to set an override; give --inherit to
remove it, so the service goes back to following its destination's
policy.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if retentionInherit && retentionFlagsChanged(cmd) {
			return fmt.Errorf("give either --inherit or --keep-* flags, not both")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			s, err := a.ops.GetService(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			dest, err := a.ops.GetDestination(cmd.Context(), s.Destination)
			if err != nil {
				return err
			}
			switch {
			case retentionInherit:
				s.Retention = nil
			case retentionFlagsChanged(cmd):
				base := s.EffectiveRetention(dest)
				s.Retention = retentionOverrideFromFlags(cmd, base, retentionKeepLast, retentionKeepDaily, retentionKeepWeekly, retentionKeepMonthly)
			}
			if retentionInherit || retentionFlagsChanged(cmd) {
				// The main target carries the override.
				if len(s.Targets) > 0 {
					s.Targets[0].Retention = s.Retention
				}
				if err := a.ops.PutService(cmd.Context(), s); err != nil {
					return err
				}
			}
			cmd.Printf("%s: %s\n", args[0], retentionLine(s, dest))
			return nil
		})
	},
}

// registerServiceCommands attaches `ballast service ...` (spec §9:
// "ballast destination add | test <name>" and the equivalent service
// surface) to root.
func registerServiceCommands(root *cobra.Command) {
	serviceAddCmd.Flags().StringVar(&serviceCatalogEntry, "catalog", "", "catalog entry to back this service with (required)")
	serviceAddCmd.Flags().StringVar(&serviceDestination, "destination", "", "destination to back up to (required)")
	serviceAddCmd.Flags().StringVar(&serviceDockerHost, "host", "", "Docker host this service's containers run on (default: local)")
	serviceAddCmd.Flags().StringVar(&serviceProject, "project", "", "group this service with others of the same Docker Compose stack, for `ballast restore-project`")
	serviceAddCmd.Flags().StringVar(&serviceProxmoxNode, "proxmox-node", "", "Proxmox node this service's guest runs on (see `ballast proxmox guests`); required with --proxmox-vmid")
	serviceAddCmd.Flags().IntVar(&serviceProxmoxVMID, "proxmox-vmid", 0, "VMID of the Proxmox guest this service runs in; required with --proxmox-node")
	serviceAddCmd.Flags().StringArrayVar(&serviceParams, "param", nil, "catalog parameter as key=value (repeatable)")
	serviceAddCmd.Flags().StringArrayVar(&serviceContainers, "container", nil, "detected container as key=value (repeatable)")
	serviceAddCmd.Flags().StringArrayVar(&serviceSecrets, "secret", nil,
		"catalog {{ .Secrets.NAME }} reference as name=credential-file-name (repeatable); leave the value empty (name=) to use the default file name, then set it with `ballast secret set`")
	serviceAddCmd.Flags().IntVar(&serviceKeepLast, "keep-last", 0, "newest backups to keep whatever their age, for this service alone (0 = off)")
	serviceAddCmd.Flags().IntVar(&serviceKeepDaily, "keep-daily", 0, "daily snapshots to keep for this service alone (default: same as the destination)")
	serviceAddCmd.Flags().IntVar(&serviceKeepWeekly, "keep-weekly", 0, "weekly snapshots to keep for this service alone (default: same as the destination)")
	serviceAddCmd.Flags().IntVar(&serviceKeepMonthly, "keep-monthly", 0, "monthly snapshots to keep for this service alone (default: same as the destination)")
	_ = serviceAddCmd.MarkFlagRequired("catalog")
	_ = serviceAddCmd.MarkFlagRequired("destination")

	serviceRetentionCmd.Flags().IntVar(&retentionKeepLast, "keep-last", 0, "newest backups to keep whatever their age, for this service alone (0 = off)")
	serviceRetentionCmd.Flags().IntVar(&retentionKeepDaily, "keep-daily", 0, "daily snapshots to keep for this service alone")
	serviceRetentionCmd.Flags().IntVar(&retentionKeepWeekly, "keep-weekly", 0, "weekly snapshots to keep for this service alone")
	serviceRetentionCmd.Flags().IntVar(&retentionKeepMonthly, "keep-monthly", 0, "monthly snapshots to keep for this service alone")
	serviceRetentionCmd.Flags().BoolVar(&retentionInherit, "inherit", false, "remove the override; follow the destination's policy again")

	serviceCmd := &cobra.Command{Use: "service", Short: "Manage configured backup services"}
	registerExcludeFlags()
	serviceAddCmd.Flags().StringArrayVar(&serviceExcludes, "skip", nil, "file or folder pattern this service's backups skip, such as \"*.log\" (repeatable)")
	serviceAddCmd.Flags().StringArrayVar(&serviceAlso, "also-to", nil, "also send backups to another destination: NAME or NAME=part,part (repeatable)")
	serviceCmd.AddCommand(serviceAddCmd, serviceListCmd, serviceRemoveCmd, serviceRetentionCmd, serviceExcludesCmd, serviceTargetsCmd)
	root.AddCommand(serviceCmd)
}

// retentionWords describes r in plain English, e.g. "the newest 2, 7
// daily, 4 weekly and 6 monthly backups".
func retentionWords(r inventory.Retention) string {
	if r.KeepLast > 0 {
		return fmt.Sprintf("the newest %d, %d daily, %d weekly and %d monthly backups", r.KeepLast, r.KeepDaily, r.KeepWeekly, r.KeepMonthly)
	}
	return fmt.Sprintf("%d daily, %d weekly and %d monthly backups", r.KeepDaily, r.KeepWeekly, r.KeepMonthly)
}
