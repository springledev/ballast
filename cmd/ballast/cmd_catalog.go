package main

import "github.com/spf13/cobra"

var catalogCmd = &cobra.Command{Use: "catalog", Short: "Browse the service catalog"}

// catalogListCmd is `ballast catalog list` (spec §9).
var catalogListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available catalog entries",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			entries := a.ops.ListCatalog()
			if len(entries) == 0 {
				cmd.Println("No catalog entries found.")
				return nil
			}
			for _, e := range entries {
				name := e.Name
				if name == "" {
					name = e.Service
				}
				cmd.Printf("%s\t%s\t%s\n", e.Service, name, e.Description)
			}
			return nil
		})
	},
}

func init() {
	catalogCmd.AddCommand(catalogListCmd)
}
