package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretSetWritesCredentialFile checks the CLI surface of spec
// §14 step 15's credential store: `ballast secret set` writes a
// root-only credential file under the config dir's writable
// credential directory, never prints the value back, and rotating the
// same secret overwrites the same file (not a second one).
func TestSecretSetWritesCredentialFile(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "mysql", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp"); err != nil {
		t.Fatalf("service add: %v", err)
	}

	out, err := execCLI(t, "secret", "set", "mysql", "db_password", "--value", "hunter2")
	if err != nil {
		t.Fatalf("secret set: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "mysql") || !strings.Contains(out, "db_password") {
		t.Errorf("output = %q, want it to name the service and secret", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("output leaked the secret value: %q", out)
	}

	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	path := filepath.Join(credDir, "ballast-secret.mysql.db_password")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file permissions = %04o, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != "hunter2" {
		t.Errorf("credential file contents = %q, %v, want hunter2", data, err)
	}

	// Rotating the same secret overwrites the same file.
	if _, err := execCLI(t, "secret", "set", "mysql", "db_password", "--value", "new-value"); err != nil {
		t.Fatalf("secret set (rotate): %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != "new-value" {
		t.Errorf("credential file after rotation = %q, %v, want new-value", data, err)
	}
	entries, err := os.ReadDir(credDir)
	if err != nil {
		t.Fatalf("reading credential dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("credential dir entries = %v, want exactly one file (rotation must not create a second)", entries)
	}
}

// TestSecretSetUnknownService: a typo in the service name is an
// error, not a silently created credential file for a service that
// isn't there.
func TestSecretSetUnknownService(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "secret", "set", "nope", "db_password", "--value", "x")
	if err == nil {
		t.Errorf("want an error for an unknown service (output %q)", out)
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to say the service was not found", err)
	}
}

// TestSecretSetRequiresValue checks that omitting both --value and
// stdin input is a loud error, not a silently empty secret.
func TestSecretSetRequiresValue(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "mysql", "--catalog", "folder", "--destination", "local", "--param", "path=/tmp"); err != nil {
		t.Fatalf("service add: %v", err)
	}
	// Give the command a closed stdin explicitly, so a bug that makes
	// it block waiting for input fails the test instead of hanging.
	rootCmd.SetIn(strings.NewReader(""))
	t.Cleanup(func() { rootCmd.SetIn(nil) })
	if _, err := execCLI(t, "secret", "set", "mysql", "db_password"); err == nil {
		t.Error("secret set with no --value and no stdin = nil error, want error")
	}
}
