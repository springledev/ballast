package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/springledev/ballast/internal/config"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{Use: "config", Short: "Inspect Ballast's configuration"}

// configCheckCmd is `ballast config check`: it reads ballast.yaml and
// everything it points at, and says whether the setup hangs together,
// without running or changing anything.
var configCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check that your configuration is coherent (nothing is run or changed)",
	Long: `Reads ballast.yaml and checks everything it and your saved setup point at:
the listen address, the folders Ballast uses, and every password, key and
.env file your destinations and services refer to.

It only looks; it never starts a backup and never prints a secret.
It exits with an error if anything needs fixing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			problems, notes, err := checkConfig(cmd, cfg, a)
			if err != nil {
				return err
			}
			for _, n := range notes {
				cmd.Printf("ok: %s\n", n)
			}
			for _, p := range problems {
				cmd.Printf("PROBLEM: %s\n", p)
			}
			if len(problems) > 0 {
				return fmt.Errorf("config check found %d problem(s); fix them and run `ballast config check` again", len(problems))
			}
			cmd.Println("Your configuration looks good.")
			return nil
		})
	},
}

// checkConfig gathers plain-English problems and ok-notes.
func checkConfig(cmd *cobra.Command, cfg config.Config, a *app) (problems, notes []string, err error) {
	if _, statErr := os.Stat(cfg.ConfigFilePath()); statErr == nil {
		notes = append(notes, fmt.Sprintf("read %s", cfg.ConfigFilePath()))
	} else {
		notes = append(notes, fmt.Sprintf("no %s (that is fine; built-in defaults are used)", cfg.ConfigFilePath()))
	}
	notes = append(notes, fmt.Sprintf("the web server will listen on %s", cfg.Listen))
	for _, d := range cfg.CatalogDirs {
		if st, statErr := os.Stat(d); statErr == nil && !st.IsDir() {
			problems = append(problems, fmt.Sprintf("catalog folder %s is a file, not a folder", d))
		}
	}

	hasCred := func(name string) bool {
		for _, d := range cfg.CredentialDirs() {
			if _, statErr := os.Stat(filepath.Join(d, name)); statErr == nil {
				return true
			}
		}
		return false
	}

	dests, err := a.ops.ListDestinations(cmd.Context())
	if err != nil {
		return nil, nil, fmt.Errorf("listing destinations: %w", err)
	}
	known := map[string]bool{}
	for _, d := range dests {
		known[d.Name] = true
		for _, c := range []string{d.PasswordCredential, d.BackendCredential} {
			if c != "" && !hasCred(c) {
				problems = append(problems, fmt.Sprintf("destination %q needs the credential file %q, which was not found in %s", d.Name, c, cfg.CredentialDirs()))
			}
		}
	}
	notes = append(notes, fmt.Sprintf("%d destination(s) checked", len(dests)))

	svcs, err := a.ops.ListServices(cmd.Context())
	if err != nil {
		return nil, nil, fmt.Errorf("listing services: %w", err)
	}
	for _, s := range svcs {
		if !known[s.Destination] {
			problems = append(problems, fmt.Sprintf("service %q backs up to %q, which is not a configured destination", s.Name, s.Destination))
		}
		for name, ref := range s.Secrets {
			if path, key, isEnv := inventory.ParseEnvRef(ref); isEnv {
				keys, kerr := a.ops.TestEnvSource(cmd.Context(), path)
				if kerr != nil {
					problems = append(problems, fmt.Sprintf("service %q: the .env file for %q cannot be read: %v", s.Name, name, kerr))
					continue
				}
				if !containsString(keys, key) {
					problems = append(problems, fmt.Sprintf("service %q: %s does not contain the key %s", s.Name, path, key))
				}
				continue
			}
			if !hasCred(ref) {
				problems = append(problems, fmt.Sprintf("service %q needs the credential file %q for %q, which was not found", s.Name, ref, name))
			}
		}
	}
	notes = append(notes, fmt.Sprintf("%d service(s) checked", len(svcs)))
	return problems, notes, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func init() {
	configCmd.AddCommand(configCheckCmd)
	rootCmd.AddCommand(configCmd)
}
