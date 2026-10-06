package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	excludeSet     []string
	excludeClear   bool
	excludePreview bool
)

// serviceExcludesCmd is `ballast service excludes <name>`: show, change
// or preview the list of things a service's backups skip.
var serviceExcludesCmd = &cobra.Command{
	Use:   "excludes <name>",
	Short: "Show, change or preview what a service's backups skip",
	Long: `Show, change or preview what a service's backups skip.

A pattern is a file or folder name that may use * and ? wildcards, for
example "*.log" or "node_modules". A pattern without a slash matches
that name anywhere inside the backed-up folder; one that starts with a
slash matches from the top of the folder only.

With no flags this prints the current list. Give --skip (repeatable) to
replace the list, or --clear to empty it. --preview shows what the list
(the new one if you also gave --skip) would leave out of the next backup,
without backing anything up.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if excludeClear && len(excludeSet) > 0 {
			return fmt.Errorf("give either --clear or --skip, not both")
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
			if excludeClear || len(excludeSet) > 0 {
				s.Excludes = nil
				if !excludeClear {
					s.Excludes = append([]string(nil), excludeSet...)
				}
				if excludePreview {
					// Preview the draft before it is saved.
					if err := printExcludePreview(cmd, a, args[0], s.Excludes); err != nil {
						return err
					}
				}
				if err := a.ops.PutService(cmd.Context(), s); err != nil {
					return err
				}
				cmd.Printf("%s: saved\n", args[0])
			} else if excludePreview {
				if err := printExcludePreview(cmd, a, args[0], s.Excludes); err != nil {
					return err
				}
			}
			if len(s.Excludes) == 0 {
				cmd.Printf("%s: nothing extra is skipped (the catalog entry's own list still applies)\n", args[0])
				return nil
			}
			cmd.Printf("%s skips:\n", args[0])
			for _, p := range s.Excludes {
				cmd.Printf("  %s\n", p)
			}
			return nil
		})
	},
}

// printExcludePreview prints what patterns would leave out of
// service's next backup.
func printExcludePreview(cmd *cobra.Command, a *app, service string, patterns []string) error {
	pv, err := a.ops.PreviewExcludes(cmd.Context(), service, patterns)
	if err != nil {
		return err
	}
	cmd.Printf("Would skip %d item(s), %s; %s would still be backed up.\n", pv.SkippedCount, humanBytes(pv.SkippedBytes), humanBytes(pv.KeptBytes))
	for _, it := range pv.Skipped {
		cmd.Printf("  %s (%s)\n", it.Path, humanBytes(it.Bytes))
	}
	if pv.Note != "" {
		cmd.Println(pv.Note)
	}
	return nil
}

// humanBytes formats n as a short size such as "1.5 GB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func registerExcludeFlags() {
	serviceExcludesCmd.Flags().StringArrayVar(&excludeSet, "skip", nil, "pattern to skip (repeatable); replaces the current list")
	serviceExcludesCmd.Flags().BoolVar(&excludeClear, "clear", false, "empty the list")
	serviceExcludesCmd.Flags().BoolVar(&excludePreview, "preview", false, "show what would be skipped, without backing up")
}
