package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
	execrunner "github.com/springledev/keel/runner"
)

// writeAPIToken stores the access key at path, readable only by its
// owner. It writes a temp file and renames it so a crash never leaves
// a half-written key.
func writeAPIToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("saving %s: %w", path, err)
	}
	return nil
}

// tokenRotateCmd is `ballast token rotate`: a new access key, shown
// once, and every browser signed out.
var tokenRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Make a new access key and sign every browser out (shown once)",
	Args:  noArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		// A running daemon holds the key in memory, so ask it to rotate;
		// otherwise just replace the file.
		c, _, err := connectDaemon(cmd.Context(), cfg, daemonURL)
		if err != nil && err != errNoDaemon {
			return err
		}
		var tok string
		if c != nil {
			var out struct {
				Token string `json:"token"`
			}
			if err := c.call(cmd.Context(), http.MethodPost, "/api/v1/auth/rotate", nil, &out); err != nil {
				return err
			}
			tok = out.Token
		} else {
			if tok, err = api.GenerateToken(); err != nil {
				return err
			}
			path := filepath.Join(execrunner.FindCredentialDir(apiTokenCredential, cfg.CredentialDirs()...), apiTokenCredential)
			if err := writeAPIToken(path, tok); err != nil {
				return err
			}
		}
		cmd.Println(tok)
		cmd.PrintErrln("ballast: this is your new access key — save it now. Every browser has been signed out.")
		return nil
	},
}

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
	tokenCmd.AddCommand(tokenGenerateCmd, tokenRotateCmd)
}
