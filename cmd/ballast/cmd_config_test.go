package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCheck(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "config", "check")
	if err != nil || !strings.Contains(out, "Your configuration looks good.") {
		t.Fatalf("clean config: %q, %v", out, err)
	}

	// A bad listen address in ballast.yaml is reported, not ignored.
	cfgFile := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "ballast.yaml")
	if err := os.WriteFile(cfgFile, []byte("listen: nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execCLI(t, "config", "check"); err == nil || !strings.Contains(err.Error(), "not a valid listen address") {
		t.Fatalf("bad listen error = %v", err)
	}
}
