package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCmdPrintsVersion(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"version"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != version {
		t.Errorf("version output = %q, want %q", got, version)
	}
}

func TestRootListsCoreCommands(t *testing.T) {
	cmds := rootCmd.Commands()
	found := map[string]bool{}
	for _, c := range cmds {
		found[c.Name()] = true
	}
	for _, want := range []string{"serve", "version"} {
		if !found[want] {
			t.Errorf("missing expected subcommand %q", want)
		}
	}
}
