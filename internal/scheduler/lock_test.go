package scheduler

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFileLockerExcludesSecondHolder(t *testing.T) {
	l := FileLocker{Dir: filepath.Join(t.TempDir(), "locks")}

	release, ok, err := l.TryLock("immich")
	if err != nil || !ok {
		t.Fatalf("first TryLock = %v, %v; want true, nil", ok, err)
	}
	if _, ok, err := l.TryLock("immich"); err != nil || ok {
		t.Fatalf("second TryLock while held = %v, %v; want false, nil", ok, err)
	}
	// Other services are independent.
	releaseOther, ok, err := l.TryLock("vaultwarden")
	if err != nil || !ok {
		t.Fatalf("TryLock of another service = %v, %v; want true, nil", ok, err)
	}
	releaseOther()

	release()
	release2, ok, err := l.TryLock("immich")
	if err != nil || !ok {
		t.Fatalf("TryLock after release = %v, %v; want true, nil", ok, err)
	}
	release2()

	info, err := os.Stat(l.Dir)
	if err != nil {
		t.Fatalf("stat lock dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("lock dir mode = %04o, want 0700", perm)
	}
}

func TestFileLockerKeepsNamesInsideDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	l := FileLocker{Dir: dir}
	release, ok, err := l.TryLock("../../escape")
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v; want true, nil", ok, err)
	}
	defer release()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading lock dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("lock dir entries = %v, want exactly one lock file inside it", entries)
	}
}

func TestFileLockerRejectsEmptyService(t *testing.T) {
	if _, _, err := (FileLocker{Dir: t.TempDir()}).TryLock(""); err == nil {
		t.Error("TryLock with an empty service should error")
	}
}

func TestFileLockerReportsUnusableDir(t *testing.T) {
	// A regular file where the lock directory should be.
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	if _, _, err := (FileLocker{Dir: notDir}).TryLock("immich"); err == nil {
		t.Error("TryLock with an unusable lock directory should error")
	}
}

// TestFileLockerReleasedWhenHolderDies proves the property the
// interrupted-run detection relies on: a lock held by a process that
// is killed is free again immediately.
func TestFileLockerReleasedWhenHolderDies(t *testing.T) {
	if os.Getenv("BALLAST_LOCK_HELPER") != "" {
		release, ok, err := FileLocker{Dir: os.Getenv("BALLAST_LOCK_HELPER")}.TryLock("immich")
		if err != nil || !ok {
			os.Exit(2)
		}
		_ = release
		os.Stdout.WriteString("locked\n")
		select {} // hold the lock until killed
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFileLockerReleasedWhenHolderDies$")
	cmd.Env = append(os.Environ(), "BALLAST_LOCK_HELPER="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	buf := make([]byte, len("locked\n"))
	if _, err := out.Read(buf); err != nil || string(buf) != "locked\n" {
		_ = cmd.Process.Kill() // best effort before failing
		t.Fatalf("helper did not take the lock: %q, %v", buf, err)
	}

	l := FileLocker{Dir: dir}
	if _, ok, _ := l.TryLock("immich"); ok {
		t.Fatal("lock should be held by the helper process")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing helper: %v", err)
	}
	// Reap it; Wait reports the kill we just sent, which is expected.
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper exited cleanly; want it killed while holding the lock")
	}
	release, ok, err := l.TryLock("immich")
	if err != nil || !ok {
		t.Fatalf("TryLock after the holder was killed = %v, %v; want true, nil", ok, err)
	}
	release()
}
