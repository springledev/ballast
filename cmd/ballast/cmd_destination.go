package main

import (
	"github.com/springledev/ballast/internal/inventory"
	"github.com/spf13/cobra"
)

var (
	destLocation           string
	destPasswordCredential string
	destInit               bool
	destKeepDaily          int
	destKeepWeekly         int
	destKeepMonthly        int
)

var destinationAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Configure a new restic backup destination",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			d := inventory.Destination{
				Name: args[0], Location: destLocation, PasswordCredential: destPasswordCredential,
				KeepDaily: destKeepDaily, KeepWeekly: destKeepWeekly, KeepMonthly: destKeepMonthly,
			}
			if err := a.ops.PutDestination(cmd.Context(), d, destInit); err != nil {
				return err
			}
			cred := destPasswordCredential
			if cred == "" {
				cred = "ballast-restic-password." + args[0]
			}
			cmd.Printf("%s: configured at %s (repository password file: %s)\n", args[0], destLocation, cred)
			if destInit {
				cmd.Println("  repository initialized")
			}
			return nil
		})
	},
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
				cmd.Printf("%s\t%s\tkeep=%dd/%dw/%dm\n", d.Name, d.Location, d.KeepDaily, d.KeepWeekly, d.KeepMonthly)
			}
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
	destinationAddCmd.Flags().StringVar(&destLocation, "location", "", "restic repository location, e.g. /srv/backups or s3:https://... (required)")
	destinationAddCmd.Flags().StringVar(&destPasswordCredential, "password-credential", "", "credential file name holding the repository password (default: ballast-restic-password.<name>)")
	destinationAddCmd.Flags().BoolVar(&destInit, "init", false, "initialize the restic repository now")
	destinationAddCmd.Flags().IntVar(&destKeepDaily, "keep-daily", 7, "daily snapshots to keep")
	destinationAddCmd.Flags().IntVar(&destKeepWeekly, "keep-weekly", 4, "weekly snapshots to keep")
	destinationAddCmd.Flags().IntVar(&destKeepMonthly, "keep-monthly", 6, "monthly snapshots to keep")
	_ = destinationAddCmd.MarkFlagRequired("location")

	destinationCmd := &cobra.Command{Use: "destination", Short: "Manage backup destinations"}
	destinationCmd.AddCommand(destinationAddCmd, destinationListCmd, destinationTestCmd, destinationRemoveCmd)
	root.AddCommand(destinationCmd)
}
