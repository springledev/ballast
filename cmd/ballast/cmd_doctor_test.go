package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/api"
)

func TestDoctorCommand(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "restic") || !strings.Contains(out, "[ok]") {
		t.Errorf("output = %q, want it to list restic status", out)
	}
}

func TestDoctorCommandJSON(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "doctor", "--json")
	if err != nil {
		t.Fatalf("doctor --json: %v (output %q)", err, out)
	}
	out = stripLogLines(out)
	var report api.DoctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parsing JSON output: %v (output %q)", err, out)
	}
	if len(report.Checks) == 0 {
		t.Errorf("DoctorReport has no checks")
	}
}

func TestSelfBackupCommand(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "ballast-restic-password.local"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Add a destination to self-backup to
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups", "--init"); err != nil {
		t.Fatalf("destination add: %v", err)
	}

	out, err := execCLI(t, "self-backup", "local")
	if err != nil {
		t.Fatalf("self-backup: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "snapshot") || !strings.Contains(out, "verified") {
		t.Errorf("output = %q, want it to report snapshot ID and verified status", out)
	}
}

func TestSelfBackupCommandJSON(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "ballast-restic-password.local"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups", "--init"); err != nil {
		t.Fatalf("destination add: %v", err)
	}

	out, err := execCLI(t, "self-backup", "local", "--json")
	if err != nil {
		t.Fatalf("self-backup --json: %v (output %q)", err, out)
	}
	out = stripLogLines(out)
	var result api.SelfBackupResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parsing JSON output: %v (output %q)", err, out)
	}
	if result.Destination != "local" {
		t.Errorf("Destination = %q, want local", result.Destination)
	}
	if result.SnapshotID == "" {
		t.Errorf("SnapshotID is empty")
	}
	if !result.Verified {
		t.Errorf("Verified = false, want true")
	}
}

func TestSelfBackupUnknownDestination(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "self-backup", "nope")
	if err == nil {
		t.Error("self-backup to unknown destination: want an error")
	}
}
