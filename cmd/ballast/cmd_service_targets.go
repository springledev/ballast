package main

import (
	"fmt"
	"strings"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/spf13/cobra"
)

var (
	targetAdd     []string
	targetRemove  []string
	serviceAlso   []string
	targetPrimary string
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
				s.Targets = targets
				if err := a.ops.PutService(cmd.Context(), s); err != nil {
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

func init() {
	serviceTargetsCmd.Flags().StringArrayVar(&targetAdd, "add", nil, "send to one more destination: NAME or NAME=part,part (repeatable)")
	serviceTargetsCmd.Flags().StringVar(&targetPrimary, "primary", "", "make this destination the main one (it must already be one of the service's destinations)")
	serviceTargetsCmd.Flags().StringArrayVar(&targetRemove, "remove", nil, "stop sending to this destination (repeatable)")
}
