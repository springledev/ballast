package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// `ballast secret` is a documented addition to spec §9's CLI surface
// (like `ballast schedule`/`ballast notify`): the credential store of
// spec §14 step 15, so a catalog {{ .Secrets.<name> }} reference (a
// database password, most commonly) has somewhere to live that is
// never the config file or the database.

var secretValueFlag string

var secretSetCmd = &cobra.Command{
	Use:   "set <service> <name>",
	Short: "Store a secret value for a catalog {{ .Secrets.<name> }} reference",
	Long: `Store a secret value for a catalog {{ .Secrets.<name> }} reference.

The value is written to a root-only credential file (never the config file or
the database, spec rule 2) and never printed back. Run it again with a new
value to rotate a password.

Prefer piping the value in over --value, which is visible in your shell
history and to anyone who can list processes:

  echo -n 'my-db-password' | ballast secret set mysql db_password

The catalog entry references it as {{ .Secrets.db_password }}, typically
inside a dump's secretEnv (see the catalog contributing guide).`,
	Example: `  echo -n 's3cr3t' | ballast secret set mysql db_password
  ballast secret set mysql db_password --value s3cr3t`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		service, name := args[0], args[1]
		value := secretValueFlag
		if value != "" {
			cmd.PrintErrln("warning: --value is visible in your shell history and to other users listing processes; prefer piping the secret on stdin")
		}
		if value == "" {
			line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if err != nil && line == "" {
				return fmt.Errorf("no value given: pass --value or pipe the secret value on stdin")
			}
			value = strings.TrimRight(line, "\r\n")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.SetSecret(cmd.Context(), service, name, value); err != nil {
				return err
			}
			cmd.Printf("%s: secret %q stored\n", service, name)
			return nil
		})
	},
}

// registerSecretCommands attaches `ballast secret ...` to root.
func registerSecretCommands(root *cobra.Command) {
	secretSetCmd.Flags().StringVar(&secretValueFlag, "value", "", "the secret value (prefer piping it on stdin instead; this flag is visible in your shell history)")

	secretCmd := &cobra.Command{Use: "secret", Short: "Manage secret values for catalog {{ .Secrets.<name> }} references"}
	secretCmd.AddCommand(secretSetCmd)
	root.AddCommand(secretCmd)
}
