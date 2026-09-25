package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupRealBackup wires a real pipeline (no stub runner), configures
// destination "local" and service "docs" (the folder catalog entry
// backing up dataDir), and runs one real backup. It returns dataDir.
func setupRealBackup(t *testing.T) string {
	t.Helper()
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
	if out, err := execCLI(t, "backup", "docs"); err != nil {
		t.Fatalf("backup: %v (output %q)", err, out)
	}
	return dataDir
}

func TestSnapshotsCommand(t *testing.T) {
	setupRealBackup(t)
	out, err := execCLI(t, "snapshots", "docs")
	if err != nil {
		t.Fatalf("snapshots: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "11111111") {
		t.Errorf("output = %q, want the stub snapshot's short id", out)
	}
}

func TestSnapshotsCommandUnknownService(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "snapshots", "ghost"); err == nil {
		t.Error("snapshots for an unconfigured service: want an error")
	}
}
