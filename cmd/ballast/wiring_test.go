package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/inventory"
)

// systemdCredentialsDir simulates systemd's LoadCredential=: sets
// $CREDENTIALS_DIRECTORY to a fresh directory holding files, then
// makes it read-only as systemd does, so any attempt to save a secret
// there fails the test. Returns the directory.
func systemdCredentialsDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run-credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	return dir
}

// TestSecretsBallastCreatesSurviveSystemd: under systemd,
// $CREDENTIALS_DIRECTORY is read-only and rebuilt on every start, and
// differs from the interactive credential directory (spec §8). Secrets
// Ballast creates itself — the wizard's generated repository password
// and ntfy link — must go to its own persistent directory, and every
// later run must still find them there.
func TestSecretsBallastCreatesSurviveSystemd(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the read-only credential directory")
	}
	var hits atomic.Int32
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer ntfy.Close()

	cliEnv(t, nil)
	withStubRestic(t)
	systemd := systemdCredentialsDir(t, nil)
	own := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	backupDir := newBackupSource(t)

	wizardInput(t,
		"folder", "myfolder", backupDir,
		"",              // destination name -> "local"
		"",              // backup folder -> default
		ntfy.URL+"/xyz", // ntfy link
		"",              // schedule -> nightly
	)
	out, err := execCLI(t, "wizard")
	if err != nil {
		t.Fatalf("wizard: %v (output %q)", err, out)
	}
	for _, name := range []string{"ballast-restic-password.local", "ballast-ntfy-topic.myfolder"} {
		if _, err := os.Stat(filepath.Join(own, name)); err != nil {
			t.Errorf("%s not saved in Ballast's own directory %s: %v", name, own, err)
		}
		if _, err := os.Stat(filepath.Join(systemd, name)); err == nil {
			t.Errorf("%s was written into $CREDENTIALS_DIRECTORY", name)
		}
	}
	if !strings.Contains(out, filepath.Join(own, "ballast-restic-password.local")) {
		t.Errorf("wizard output does not name where the password was saved (%s): %q", own, out)
	}
	if !strings.Contains(out, systemd) || !strings.Contains(out, own) {
		t.Errorf("log does not name both credential directories: %q", out)
	}
	if hits.Load() == 0 {
		t.Error("no ntfy notification sent: the notifier did not find the link saved in Ballast's own directory")
	}

	// A later, separate run (as the systemd timer would do) still
	// opens the repository with the saved password.
	before := hits.Load()
	if out, err := execCLI(t, "backup", "myfolder"); err != nil {
		t.Fatalf("backup after wizard: %v (output %q)", err, out)
	}
	if hits.Load() == before {
		t.Error("no ntfy notification on the later run")
	}
}

// TestProvidedPasswordIsNeverReplaced: a repository password the
// operator provided through LoadCredential= is used as is; Ballast
// must not generate a second one in its own directory.
func TestProvidedPasswordIsNeverReplaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the read-only credential directory")
	}
	cliEnv(t, nil)
	withStubRestic(t)
	systemdCredentialsDir(t, map[string]string{"ballast-restic-password.local": "operator-provided"})
	own := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	backupDir := newBackupSource(t)

	wizardInput(t, "folder", "myfolder", backupDir, "", "", "", "none")
	out, err := execCLI(t, "wizard")
	if err != nil {
		t.Fatalf("wizard: %v (output %q)", err, out)
	}
	if _, err := os.Stat(filepath.Join(own, "ballast-restic-password.local")); err == nil {
		t.Error("a new password was generated although the operator provided one")
	}
	if strings.Contains(out, "created for you") {
		t.Errorf("wizard claims it created the password: %q", out)
	}
	if strings.Contains(out, "operator-provided") {
		t.Errorf("the password itself was printed: %q", out)
	}
}

// TestDestinationSetupLockedAcrossApps: openApp must give Ops a
// destination lock that spans processes. Two apps opened on the same
// data directory stand in for two Ballast processes (say `ballast
// serve` and `ballast wizard`): while one holds a destination's lock,
// the other's PutDestination for that name must wait, not go ahead.
// Without the cross-process lock wired in, each app would only
// serialize its own calls and the second setup would run at once.
func TestDestinationSetupLockedAcrossApps(t *testing.T) {
	cliEnv(t, nil)
	withStubRestic(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	holder, err := openApp(cfg, &discardWriter{})
	if err != nil {
		t.Fatalf("openApp (holder): %v", err)
	}
	defer holder.Close()
	other, err := openApp(cfg, &discardWriter{})
	if err != nil {
		t.Fatalf("openApp (other): %v", err)
	}
	defer other.Close()

	if holder.ops.DestinationLocks == nil {
		t.Fatal("openApp left Ops.DestinationLocks unset: destination setup is only serialized within one process")
	}
	release, ok, err := holder.ops.DestinationLocks.TryLock("local")
	if err != nil || !ok {
		t.Fatalf("taking the destination lock: ok=%v err=%v", ok, err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	dest := inventory.Destination{Name: "local", Location: t.TempDir(), KeepDaily: 7}
	err = other.ops.PutDestination(ctx, dest, true /* init */)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PutDestination while another app holds the lock: err = %v, want it to wait until ctx ends (context.DeadlineExceeded)", err)
	}
	if _, err := inventory.GetDestination(other.store.DB(), "local"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("GetDestination: err = %v, want ErrNotFound (nothing may be written while the lock is held elsewhere)", err)
	}
}
