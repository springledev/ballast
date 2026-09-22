package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeCred creates a credential file with exactly the given
// permissions (WriteFile's perm is umask-masked, so chmod after).
func writeCred(t *testing.T, dir, name string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("s3cr3t"), 0o600); err != nil {
		t.Fatalf("writing credential fixture: %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod credential fixture: %v", err)
	}
	return path
}

func TestFileEnvHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "restic-password", 0o600)
	writeCred(t, dir, "b2-key", 0o400)

	env, err := FileEnv(dir, map[string]string{
		"RESTIC_PASSWORD_FILE": "restic-password",
		"B2_APPLICATION_KEY":   "b2-key",
	})
	if err != nil {
		t.Fatalf("FileEnv: %v", err)
	}
	// Entries come out sorted by key, pointing at the files by path.
	want := []string{
		"B2_APPLICATION_KEY=" + filepath.Join(dir, "b2-key"),
		"RESTIC_PASSWORD_FILE=" + filepath.Join(dir, "restic-password"),
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
}

// TestFileEnvNeverReadsContents: only paths may leave the function —
// the secret content must not appear in the returned environment.
func TestFileEnvNeverReadsContents(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "pw", 0o600)
	env, err := FileEnv(dir, map[string]string{"RESTIC_PASSWORD_FILE": "pw"})
	if err != nil {
		t.Fatalf("FileEnv: %v", err)
	}
	for _, e := range env {
		if strings.Contains(e, "s3cr3t") {
			t.Errorf("env entry leaks secret contents: %q", e)
		}
	}
}

func TestFileEnvMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := FileEnv(dir, map[string]string{"RESTIC_PASSWORD_FILE": "nope"})
	if err == nil {
		t.Fatal("missing credential file must be a loud error")
	}
	if !strings.Contains(err.Error(), "RESTIC_PASSWORD_FILE") {
		t.Errorf("error = %q, want the env var name for context", err)
	}
}

// TestFileEnvRejectsLoosePermissions pins spec §"Non-negotiable
// rules" #2: a credential file readable by group or other is a loud
// error, never a warning.
func TestFileEnvRejectsLoosePermissions(t *testing.T) {
	cases := []struct {
		name string
		perm os.FileMode
	}{
		{"world readable", 0o604},
		{"group readable", 0o640},
		{"world writable", 0o602},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCred(t, dir, "pw", tc.perm)
			_, err := FileEnv(dir, map[string]string{"RESTIC_PASSWORD_FILE": "pw"})
			if err == nil {
				t.Fatalf("permissions %04o must be rejected", tc.perm)
			}
			if !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("error = %q, want a plain-English fix hint", err)
			}
		})
	}
}

func TestFileEnvRejectsNonAbsoluteDir(t *testing.T) {
	if _, err := FileEnv("relative/dir", map[string]string{"K": "f"}); err == nil {
		t.Error("relative credential directory must be rejected")
	}
}

func TestFileEnvRejectsEmptyDir(t *testing.T) {
	if _, err := FileEnv("", map[string]string{"K": "f"}); err == nil {
		t.Error("empty credential directory must be rejected")
	}
}

// TestFileEnvRejectsPathEscape: a file name must be a plain name
// inside the credential directory, not a path that escapes it.
func TestFileEnvRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := FileEnv(dir, map[string]string{"K": "../etc/passwd"}); err == nil {
		t.Error("file name escaping the credential directory must be rejected")
	}
}

func TestFileEnvRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := FileEnv(dir, map[string]string{"K": "subdir"}); err == nil {
		t.Error("a directory must be rejected as a credential file")
	}
}

func TestFileEnvEmptyMapping(t *testing.T) {
	env, err := FileEnv(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("FileEnv with no files: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("env = %v, want empty", env)
	}
}
