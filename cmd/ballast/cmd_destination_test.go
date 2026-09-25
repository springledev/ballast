package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDestinationAddListRemove(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})

	out, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups")
	if err != nil {
		t.Fatalf("destination add: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "local") || !strings.Contains(out, "/srv/backups") {
		t.Errorf("output = %q, want it to name the destination and location", out)
	}

	out, err = execCLI(t, "destination", "list")
	if err != nil {
		t.Fatalf("destination list: %v", err)
	}
	if !strings.Contains(out, "local") {
		t.Errorf("list output = %q, want the destination listed", out)
	}

	if _, err := execCLI(t, "destination", "remove", "local"); err != nil {
		t.Fatalf("destination remove: %v", err)
	}
	out, err = execCLI(t, "destination", "list")
	if err != nil {
		t.Fatalf("destination list after remove: %v", err)
	}
	if strings.Contains(out, "local") {
		t.Errorf("list output after remove = %q, want it gone", out)
	}
}

func TestDestinationAddInit(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "ballast-restic-password.local"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups", "--init")
	if err != nil {
		t.Fatalf("destination add --init: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "initialized") {
		t.Errorf("output = %q, want it to confirm initialization", out)
	}

	if out, err := execCLI(t, "destination", "test", "local"); err != nil {
		t.Fatalf("destination test: %v (output %q)", err, out)
	}
}

func TestDestinationAddRequiresLocation(t *testing.T) {
	cliEnv(t, &spyRunner{})
	_, err := execCLI(t, "destination", "add", "local")
	if err == nil {
		t.Fatal("destination add without --location: want an error")
	}
}

func TestDestinationRemoveInUse(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "svc", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp"); err != nil {
		t.Fatalf("service add: %v", err)
	}
	_, err := execCLI(t, "destination", "remove", "local")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("destination remove in use = %v, want an 'in use' error", err)
	}
}
