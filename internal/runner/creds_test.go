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

// TestCheckCredentialFile is a direct table-driven test of
// CheckCredentialFile (finding 10): it is exported specifically so
// other packages (e.g. internal/notify, for secrets read directly
// into memory rather than pointed at by an env var) can apply the
// exact same rule FileEnv uses, so this pins that rule independent of
// FileEnv's own tests.
func TestCheckCredentialFile(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) string // returns the path to check
		wantErr bool
	}{
		{
			name: "regular file, 0600",
			setup: func(t *testing.T, dir string) string {
				return writeCred(t, dir, "pw", 0o600)
			},
			wantErr: false,
		},
		{
			name: "regular file, stricter than 0600",
			setup: func(t *testing.T, dir string) string {
				return writeCred(t, dir, "pw", 0o400)
			},
			wantErr: false,
		},
		{
			name: "group readable is rejected",
			setup: func(t *testing.T, dir string) string {
				return writeCred(t, dir, "pw", 0o640)
			},
			wantErr: true,
		},
		{
			name: "world readable is rejected",
			setup: func(t *testing.T, dir string) string {
				return writeCred(t, dir, "pw", 0o604)
			},
			wantErr: true,
		},
		{
			name: "world writable is rejected",
			setup: func(t *testing.T, dir string) string {
				return writeCred(t, dir, "pw", 0o602)
			},
			wantErr: true,
		},
		{
			name: "missing file",
			setup: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "does-not-exist")
			},
			wantErr: true,
		},
		{
			name: "directory instead of a regular file",
			setup: func(t *testing.T, dir string) string {
				sub := filepath.Join(dir, "subdir")
				if err := os.Mkdir(sub, 0o700); err != nil {
					t.Fatal(err)
				}
				return sub
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.setup(t, dir)
			err := CheckCredentialFile(path)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckCredentialFile(%q) error = %v, wantErr %v", path, err, tt.wantErr)
			}
		})
	}
}

func TestLoadCredentialValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-password")
	if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadCredentialValue(dir, "mysql-password")
	if err != nil {
		t.Fatalf("LoadCredentialValue() error = %v, want nil", err)
	}
	if got != "hunter2" {
		t.Errorf("LoadCredentialValue() = %q, want %q (trimmed)", got, "hunter2")
	}

	if _, err := LoadCredentialValue(dir, "missing"); err == nil {
		t.Error("LoadCredentialValue(missing file) = nil error, want error")
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentialValue(dir, "empty"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("LoadCredentialValue(empty file) error = %v, want mentioning empty", err)
	}

	if _, err := LoadCredentialValue(dir, "../escape"); err == nil {
		t.Error("LoadCredentialValue(path escape) = nil error, want error")
	}
	if _, err := LoadCredentialValue("relative/dir", "x"); err == nil {
		t.Error("LoadCredentialValue(relative dir) = nil error, want error")
	}

	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentialValue(dir, "loose"); err == nil {
		t.Error("LoadCredentialValue(loose permissions) = nil error, want error")
	}
}

func TestFindCredentialDir(t *testing.T) {
	systemd, own := t.TempDir(), t.TempDir()
	write := func(dir, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(systemd, "both")
	write(own, "both")
	write(own, "own-only")

	tests := []struct {
		name string
		file string
		dirs []string
		want string
	}{
		{"operator-provided wins", "both", []string{systemd, own}, systemd},
		{"falls back to Ballast's own", "own-only", []string{systemd, own}, own},
		{"missing names first dir", "missing", []string{systemd, own}, systemd},
		{"empty dirs skipped", "missing", []string{"", own}, own},
		{"no dirs", "missing", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FindCredentialDir(tt.file, tt.dirs...); got != tt.want {
				t.Errorf("FindCredentialDir(%q, %v) = %q, want %q", tt.file, tt.dirs, got, tt.want)
			}
		})
	}
}
