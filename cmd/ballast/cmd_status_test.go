package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/api"
)

func TestStatusCommandNoServices(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "No services configured") {
		t.Errorf("output = %q, want a beginner-friendly empty-state message", out)
	}
}

// TestStatusCommandAfterBackup runs a real backup (not a stub
// runner) so status reflects a real, verified run: the runner test
// seam is deliberately left nil here.
func TestStatusCommandAfterBackup(t *testing.T) {
	withStubRestic(t)
	data := filepath.Join(t.TempDir(), "data")
	conf := filepath.Join(t.TempDir(), "config")
	t.Setenv("BALLAST_DATA_DIR", data)
	t.Setenv("BALLAST_CONFIG_DIR", conf)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	credDir := filepath.Join(conf, "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "ballast-restic-password.local"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "path="+dataDir); err != nil {
		t.Fatalf("service add: %v", err)
	}
	out, err := execCLI(t, "backup", "docs")
	if err != nil {
		t.Fatalf("backup: %v (output %q)", err, out)
	}

	out, err = execCLI(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "docs") || !strings.Contains(out, "green") {
		t.Errorf("status output = %q, want docs shown green", out)
	}

	out, err = execCLI(t, "status", "--json", "--service", "docs")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var st []api.ServiceStatus
	if err := json.Unmarshal([]byte(stripLogLines(out)), &st); err != nil {
		t.Fatalf("status --json output not valid JSON: %v\n%s", err, out)
	}
	if len(st) != 1 || st[0].Service != "docs" {
		t.Fatalf("status --json = %+v, want [docs]", st)
	}
}
