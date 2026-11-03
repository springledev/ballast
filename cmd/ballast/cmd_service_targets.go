package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/spf13/cobra"
)

var (
	targetAdd     []string
	targetRemove  []string
	serviceAlso   []string
	targetPrimary string
	// targetCleanup deletes old backups of parts a destination dropped.
	targetCleanup bool
)

// parseTargetFlag reads "destination" (every part) or
// "destination=part,part" into a target.
func parseTargetFlag(v string) (inventory.Target, error) {
	name, parts, hasParts := strings.Cut(v, "=")
	name = strings.TrimSpace(name)
	if name == "" {
		return inventory.Target{}, fmt.Errorf("invalid destination %q (want a name, or name=part,part)", v)
	}
	t := inventory.Target{Destination: name}
	if hasParts {
		for _, p := range strings.Split(parts, ",") {
			if p = strings.TrimSpace(p); p != "" {
				t.Parts = append(t.Parts, p)
			}
		}
		if len(t.Parts) == 0 {
			return inventory.Target{}, fmt.Errorf("destination %q: list at least one part after the =", name)
		}
	}
	return t, nil
}

// targetLine describes one target in plain English.
func targetLine(t inventory.Target) string {
	what := "everything"
	if len(t.Parts) > 0 {
		what = "only " + strings.Join(t.Parts, ", ")
	}
	line := fmt.Sprintf("%s (%s)", t.Destination, what)
	if t.Retention != nil {
		line += ", keeps " + retentionWords(*t.Retention)
	}
	return line
}

// serviceTargetsCmd is `ballast service targets <name>`: show or change
// the destinations a service's backups are sent to (spec §19.4).
var serviceTargetsCmd = &cobra.Command{
	Use:   "targets <name>",
	Short: "Show or change the destinations a service backs up to",
	Long: `Show or change the destinations a service backs up to.

One backup run makes the backup once and sends it to every destination,
for example a local disk and an offsite bucket. Each destination is
uploaded, checked and reported on separately, so a problem with one never
hides how the other did.

With no flags this lists the destinations. --add NAME sends to one more
destination (everything), or --add NAME=part,part sends it only those
parts, for example --add b2=library. --remove NAME stops sending to one.
When --add takes parts away from a destination, the old backups there still
hold them: Ballast shows which backups and how much space removing them
frees, and --remove-old-snapshots removes exactly those backups.
The first destination is the main one; --primary NAME makes a different one
the main one, keeping the service's run history and verification record.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			s, err := a.ops.GetService(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			targets := s.EffectiveTargets()
			if targetPrimary != "" {
				if len(targetAdd) > 0 || len(targetRemove) > 0 {
					return fmt.Errorf("use --primary on its own; run --add or --remove as a separate command")
				}
				s, err = a.ops.SetPrimaryTarget(cmd.Context(), s.Name, targetPrimary)
				if err != nil {
					return err
				}
				targets = s.EffectiveTargets()
			}
			if len(targetAdd) > 0 || len(targetRemove) > 0 {
				targets = append([]inventory.Target(nil), targets...)
				for _, name := range targetRemove {
					kept := targets[:0:0]
					found := false
					for _, t := range targets {
						if t.Destination == name {
							found = true
							continue
						}
						kept = append(kept, t)
					}
					if !found {
						return fmt.Errorf("service %s: it does not back up to %q", s.Name, name)
					}
					targets = kept
				}
				for _, v := range targetAdd {
					t, err := parseTargetFlag(v)
					if err != nil {
						return err
					}
					replaced := false
					for i := range targets {
						if targets[i].Destination == t.Destination {
							t.Retention = targets[i].Retention
							targets[i], replaced = t, true
						}
					}
					if !replaced {
						targets = append(targets, t)
					}
				}
				if len(targets) == 0 {
					return fmt.Errorf("service %s: needs at least one destination", s.Name)
				}
				before := s.EffectiveTargets()
				s.Targets = targets
				if err := putServiceWarn(cmd.Context(), a, cmd.ErrOrStderr(), s); err != nil {
					return err
				}
				if err := offerDropCleanup(cmd, a, s.Name, before, targets); err != nil {
					return err
				}
			}
			cmd.Printf("%s sends its backups to:\n", s.Name)
			for _, t := range targets {
				cmd.Printf("  %s\n", targetLine(t))
			}
			return nil
		})
	},
}

// offerDropCleanup shows, for every destination that lost parts in this
// change, the old backups still holding them and the space removing them
// frees; with --remove-old-snapshots it removes exactly those backups.
func offerDropCleanup(cmd *cobra.Command, a *app, name string, before, after []inventory.Target) error {
	pv, err := a.ops.Preview(cmd.Context(), name)
	if err != nil {
		return nil // a service without a preview has no parts to drop
	}
	var all []string
	for _, p := range pv.Parts {
		all = append(all, p.Name)
	}
	for _, nt := range after {
		for _, ot := range before {
			if ot.Destination != nt.Destination {
				continue
			}
			if dropped := api.DroppedParts(ot, nt, all); len(dropped) > 0 {
				if err := dropCleanup(cmd.Context(), cmd, a, name, nt, dropped); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// dropCleanup prints the plan for one destination and, when asked,
// carries it out.
func dropCleanup(ctx context.Context, cmd *cobra.Command, a *app, name string, nt inventory.Target, parts []string) error {
	dest := nt.Destination
	plan, err := a.ops.PreviewDropParts(ctx, name, dest, parts)
	if err != nil {
		cmd.Printf("%s no longer sends %s to %s; could not check the old backups there: %v\n", name, strings.Join(parts, ", "), dest, err)
		return nil
	}
	cmd.Printf("%s no longer sends %s to %s.\n", name, strings.Join(parts, ", "), dest)
	if plan.Blocked != "" {
		cmd.Printf("  %s\n", plan.Blocked)
		return nil
	}
	cmd.Printf("  %d older backup(s) there still hold it; removing them frees about %s.\n", len(plan.Snapshots), humanSize(plan.FreedBytes))
	cmd.Printf("  %s\n", plan.Warning)
	if !targetCleanup {
		cmd.Printf("  To remove them, run: ballast service targets %s --add %s=%s --remove-old-snapshots\n", name, dest, strings.Join(nt.Parts, ","))
		return nil
	}
	ids := make([]string, 0, len(plan.Snapshots))
	for _, s := range plan.Snapshots {
		ids = append(ids, s.ID)
	}
	if _, err := a.ops.DropParts(ctx, name, dest, parts, ids); err != nil {
		return err
	}
	cmd.Printf("  Removed %d old backup(s) from %s and reclaimed the space.\n", len(ids), dest)
	return nil
}

// humanSize renders a byte count in plain units.
func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func init() {
	serviceTargetsCmd.Flags().StringArrayVar(&targetAdd, "add", nil, "send to one more destination: NAME or NAME=part,part (repeatable)")
	serviceTargetsCmd.Flags().StringVar(&targetPrimary, "primary", "", "make this destination the main one (it must already be one of the service's destinations)")
	serviceTargetsCmd.Flags().BoolVar(&targetCleanup, "remove-old-snapshots", false, "when --add takes parts away from a destination, also remove the old backups there that hold them")
	serviceTargetsCmd.Flags().StringArrayVar(&targetRemove, "remove", nil, "stop sending to this destination (repeatable)")
}
