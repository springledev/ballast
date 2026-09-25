package main

import "github.com/spf13/cobra"

var tokenCmd = &cobra.Command{Use: "token", Short: "Manage external trigger tokens"}

// tokenGenerateCmd is `ballast token generate <service>` (spec §14
// step 11 user decision). The token is printed to stdout exactly
// once (spec rule 2): it is never logged, never re-derivable, and
// never returned by any other command or API call.
var tokenGenerateCmd = &cobra.Command{
	Use:   "generate <service>",
	Short: "Generate a new external trigger token for a service (shown once)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			token, err := a.ops.GenerateTriggerToken(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			cmd.Println(token)
			cmd.PrintErrf("ballast: save this token now — it will not be shown again. "+
				"Store it in the credential file ballast-trigger-token.%s (mode 0600).\n", args[0])
			return nil
		})
	},
}

func init() {
	tokenCmd.AddCommand(tokenGenerateCmd)
}
