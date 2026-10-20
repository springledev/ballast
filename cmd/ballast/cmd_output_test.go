package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// execCLISplit runs the CLI like execCLI but captures stdout and
// stderr separately, so a test can tell results from notices.
func execCLISplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetCLIFlags()
	rootCmd.SetContext(context.Background())
	var o, e bytes.Buffer
	rootCmd.SetOut(&o)
	rootCmd.SetErr(&e)
	rootCmd.SetArgs(args)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	err = rootCmd.Execute()
	return o.String(), e.String(), err
}

// seedTwoServices adds a destination and the services "immich" and
// "vault" so status has something to show.
func seedTwoServices(t *testing.T) {
	t.Helper()
	withStubRestic(t)
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	for _, name := range []string{"immich", "vault"} {
		if _, err := execCLI(t, "service", "add", name, "--catalog", "folder", "--destination", "local", "--param", "path=/tmp"); err != nil {
			t.Fatalf("service add %s: %v", name, err)
		}
	}
}

// TestResultsGoToStdout runs every list/show command with the two
// streams captured separately: results on stdout, no credential notice
// or other "ballast:" chatter mixed into it (spec §21.2).
func TestResultsGoToStdout(t *testing.T) {
	cliEnv(t, &spyRunner{})
	seedTwoServices(t)
	tests := []struct {
		name string
		args []string
		want string // must appear on stdout
	}{
		{"version", []string{"version"}, version},
		{"catalog list", []string{"catalog", "list"}, "immich"},
		{"service list", []string{"service", "list"}, ""},
		{"schedule list", []string{"schedule", "list"}, ""},
		{"destination list", []string{"destination", "list"}, ""},
		{"host list", []string{"host", "list"}, ""},
		{"notify channel list", []string{"notify", "channel", "list"}, ""},
		{"status", []string{"status"}, "immich"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := execCLISplit(t, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v (stderr %q)", tc.args, err, stderr)
			}
			if tc.want != "" && !strings.Contains(stdout, tc.want) {
				t.Errorf("stdout = %q, want it to contain %q (stderr %q)", stdout, tc.want, stderr)
			}
			if strings.Contains(stdout, "ballast: ") {
				t.Errorf("stdout has a notice mixed in: %q", stdout)
			}
		})
	}
}

// TestConfigureStreamsSendsResultsToStdout checks the production
// default: cobra's Print helpers write to stdout once configureStreams
// has run on the root command.
func TestConfigureStreamsSendsResultsToStdout(t *testing.T) {
	c := &cobra.Command{Use: "x"}
	configureStreams(c)
	if c.OutOrStderr() == c.ErrOrStderr() {
		t.Fatal("results and errors share one stream; results must go to stdout")
	}
}

// TestCredentialNotice covers when the "using credential directory"
// notice appears (spec §21.2).
func TestCredentialNotice(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		credDirEnv string
		want       bool
	}{
		{"default dir, plain command", []string{"service", "list"}, "", false},
		{"default dir, version", []string{"version"}, "", false},
		{"verbose", []string{"service", "list", "--verbose"}, "", true},
		{"doctor", []string{"doctor"}, "", true},
		{"non-default dir", []string{"service", "list"}, "other", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cliEnv(t, &spyRunner{})
			if tc.credDirEnv != "" {
				t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
			}
			_, stderr, _ := execCLISplit(t, tc.args...)
			got := strings.Contains(stderr, "using credential director")
			if got != tc.want {
				t.Errorf("notice shown = %v, want %v (stderr %q)", got, tc.want, stderr)
			}
		})
	}
}

// TestStatusTakesServicePositionally covers J1.
func TestStatusTakesServicePositionally(t *testing.T) {
	cliEnv(t, &spyRunner{})
	seedTwoServices(t)
	all, _, err := execCLISplit(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "vault") || !strings.Contains(all, "immich") {
		t.Fatalf("status = %q, want both services", all)
	}
	for _, args := range [][]string{{"status", "immich"}, {"status", "--service", "immich"}} {
		out, _, err := execCLISplit(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out, "immich") {
			t.Errorf("%v = %q, want immich", args, out)
		}
		if strings.Contains(out, "vault") {
			t.Errorf("%v listed every service: %q", args, out)
		}
	}
	if _, _, err := execCLISplit(t, "status", "a", "b"); err == nil {
		t.Error("status with two services should be refused")
	}
}

// TestNoArgCommandsRejectExtras walks the command tree: every leaf
// command must declare what arguments it takes, so extras are never
// dropped silently (spec §21.2).
func TestNoArgCommandsRejectExtras(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range c.Commands() {
			if len(s.Commands()) == 0 && s.Args == nil && s.Name() != "help" {
				t.Errorf("%q does not declare its arguments (cobra.NoArgs or similar)", s.CommandPath())
			}
			walk(s)
		}
	}
	walk(rootCmd)

	cliEnv(t, &spyRunner{})
	_, _, err := execCLISplit(t, "service", "list", "immich")
	if err == nil {
		t.Fatal("service list immich: want an error")
	}
	if !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("error = %q, want it to say the command takes no arguments", err)
	}
}
