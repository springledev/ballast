package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupRealProjectBackup wires a real pipeline (like setupRealBackup)
// with two "folder" services sharing one project, and runs a real
// backup of each — enough to exercise `ballast restore-project`
// end-to-end against the stub restic binary.
func setupRealProjectBackup(t *testing.T) {
	t.Helper()
	withStubRestic(t)
	conf := t.TempDir()
	data := t.TempDir()
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

	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	for _, svc := range []string{"web-config", "web-data"} {
		dir := t.TempDir()
		if _, err := execCLI(t, "service", "add", svc, "--catalog", "folder", "--destination", "local",
			"--param", "path="+dir, "--project", "myapp"); err != nil {
			t.Fatalf("service add %s: %v", svc, err)
		}
		if out, err := execCLI(t, "backup", svc); err != nil {
			t.Fatalf("backup %s: %v (output %q)", svc, err, out)
		}
	}
}

func TestRestoreProjectCommand(t *testing.T) {
	setupRealProjectBackup(t)
	to := filepath.Join(t.TempDir(), "restore-out")

	out, err := execCLI(t, "restore-project", "myapp", "--to", to)
	if err != nil {
		t.Fatalf("restore-project: %v (output %q)", err, out)
	}
	for _, svc := range []string{"web-config", "web-data"} {
		if !strings.Contains(out, svc) {
			t.Errorf("output %q missing service %s", out, svc)
		}
	}
}

func TestRestoreProjectCommandRequiresTo(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "restore-project", "myapp"); err == nil {
		t.Error("restore-project without --to: want an error")
	}
}

func TestRestoreProjectCommandUnknownProject(t *testing.T) {
	cliEnv(t, &spyRunner{})
	to := filepath.Join(t.TempDir(), "out")
	if _, err := execCLI(t, "restore-project", "ghost", "--to", to); err == nil {
		t.Error("restore-project for an unconfigured project: want an error")
	}
}

func TestRestoreProjectCommandRejectsInvalidAsOf(t *testing.T) {
	cliEnv(t, &spyRunner{})
	to := filepath.Join(t.TempDir(), "out")
	if _, err := execCLI(t, "restore-project", "myapp", "--to", to, "--as-of", "not-a-time"); err == nil {
		t.Error("restore-project with an invalid --as-of: want an error")
	}
}
