package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var supportBundleOutput string

// supportBundleCmd is `ballast support-bundle [--output file]` (spec
// §21.7): it writes one archive for a bug report or for asking someone
// else for help. Every secret is left out and nothing is sent anywhere.
var supportBundleCmd = &cobra.Command{
	Use:   "support-bundle",
	Short: "Write one file with version, doctor, status and recent alerts, with every secret left out, for asking for help",
	Args:  noArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			path := supportBundleOutput
			if path == "" {
				path = "ballast-support-bundle-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
			}
			files := a.ops.SupportBundle(cmd.Context())
			// O_EXCL: never overwrite someone's file; 0600: the bundle
			// describes the whole setup.
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				if errors.Is(err, os.ErrExist) {
					return fmt.Errorf("%s already exists; choose another name with --output, or remove it first", path)
				}
				return fmt.Errorf("creating the support bundle %s: %w", path, err)
			}
			gz := gzip.NewWriter(f)
			tw := tar.NewWriter(gz)
			now := time.Now()
			var werr error
			for _, bf := range files {
				if werr = tw.WriteHeader(&tar.Header{Name: bf.Name, Mode: 0o600, Size: int64(len(bf.Data)), ModTime: now}); werr != nil {
					break
				}
				if _, werr = tw.Write(bf.Data); werr != nil {
					break
				}
			}
			if werr == nil {
				werr = tw.Close()
			}
			if werr == nil {
				werr = gz.Close()
			}
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(path)
				return fmt.Errorf("writing the support bundle %s: %w", path, werr)
			}
			cmd.Printf("Wrote %s\n", path)
			cmd.Println("It holds version, doctor, status, recent alert decisions and tool versions, with passwords, keys and addresses left out. Nothing was sent anywhere; look it over, then attach it to your question.")
			return nil
		})
	},
}

func init() {
	supportBundleCmd.Flags().StringVar(&supportBundleOutput, "output", "", "file to write (default: ballast-support-bundle-<time>.tar.gz in the current folder)")
}
