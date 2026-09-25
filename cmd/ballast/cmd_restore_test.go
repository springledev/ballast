package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreCommand(t *testing.T) {
	setupRealBackup(t)
	to := filepath.Join(t.TempDir(), "restore-out")

	out, err := execCLI(t, "restore", "docs", "--to", to)
	if err != nil {
		t.Fatalf("restore: %v (output %q)", err, out)
	}
	if !strings.Contains(out, to) {
		t.Errorf("output = %q, want it to name the target directory", out)
	}
}

func TestRestoreCommandRequiresTo(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "restore", "docs"); err == nil {
		t.Error("restore without --to: want an error")
	}
}

func TestRestoreCommandRefusesNonEmptyTarget(t *testing.T) {
	setupRealBackup(t)
	to := t.TempDir()
	if err := os.WriteFile(filepath.Join(to, "existing"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := execCLI(t, "restore", "docs", "--to", to)
	if err == nil {
		t.Error("restore into a non-empty directory: want an error (read-only-by-default)")
	}
}
