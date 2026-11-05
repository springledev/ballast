package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/springledev/ballast/internal/catalog"
)

var catalogCmd = &cobra.Command{Use: "catalog", Short: "Browse the service catalog"}

// catalogListCmd is `ballast catalog list` (spec §9).
var catalogListCmd = &cobra.Command{
	Use:   "list",
	Args:  noArgs,
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

// catalogValidateCmd is `ballast catalog validate <file>` (spec §9): a
// thin wrapper over catalog.LoadFile, which already does schema
// validation and typed parsing (catalog is data, not code).
var catalogValidateCmd = &cobra.Command{
	Use:   "validate <file>",
	Short: "Check a catalog YAML file for schema and definition errors",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		entry, err := catalog.LoadFile(args[0])
		if err != nil {
			return err
		}
		cmd.Printf("%s is a valid catalog entry for service %q.\n", args[0], entry.Service)
		return nil
	},
}

// catalogInstallCmd is `ballast catalog install <file>` (spec §9): it
// validates the file exactly like catalog validate, then copies it
// into the first configured user catalog directory (config.CatalogDirs,
// created if missing) as <service>.yaml, so it is picked up by
// catalog.LoadRegistry on the next command/serve start. Overwriting an
// existing file for the same service name is allowed (this is also how
// you update an already-installed entry).
var catalogInstallCmd = &cobra.Command{
	Use:   "install <file>",
	Short: "Validate a catalog YAML file and add it to your catalog directory",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		entry, err := catalog.LoadFile(args[0])
		if err != nil {
			return err
		}
		dir := cfg.CatalogDirs[0]
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating catalog directory %s: %w", dir, err)
		}
		data, err := os.ReadFile(args[0])
		if err != nil {
			return fmt.Errorf("reading %s: %w", args[0], err)
		}
		dest := filepath.Join(dir, entry.Service+".yaml")
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", dest, err)
		}
		cmd.Printf("Installed catalog entry %q to %s.\n", entry.Service, dest)
		cmd.Println("It will be picked up the next time a ballast command or `ballast serve` starts.")
		return nil
	},
}

func init() {
	catalogCmd.AddCommand(catalogListCmd, catalogValidateCmd, catalogInstallCmd)
}
