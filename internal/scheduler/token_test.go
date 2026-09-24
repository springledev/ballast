package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadTokenFile(t *testing.T) {
	const secret = "0123456789abcdef"
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) string
		want    string
		wantErr string
	}{
		{
			name: "owner-only file, trailing newline trimmed",
			setup: func(t *testing.T, dir string) string {
				return writeFile(t, dir, secret+"\n", 0o600)
			},
			want: secret,
		},
		{
			name: "stricter than 0600 is fine",
			setup: func(t *testing.T, dir string) string {
				return writeFile(t, dir, secret, 0o400)
			},
			want: secret,
		},
		{
			name: "group-readable rejected",
			setup: func(t *testing.T, dir string) string {
				return writeFile(t, dir, secret, 0o640)
			},
			wantErr: "chmod 600",
		},
		{
			name: "world-readable rejected",
			setup: func(t *testing.T, dir string) string {
				return writeFile(t, dir, secret, 0o644)
			},
			wantErr: "chmod 600",
		},
		{
			name: "empty file rejected",
			setup: func(t *testing.T, dir string) string {
				return writeFile(t, dir, " \n", 0o600)
			},
			wantErr: "empty",
		},
		{
			name: "missing file rejected",
			setup: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "nope")
			},
			wantErr: "reading trigger token file",
		},
		{
			name: "oversized file rejected",
			setup: func(t *testing.T, dir string) string {
				return writeFile(t, dir, secret+strings.Repeat(" ", maxTokenFileSize), 0o600)
			},
			wantErr: "too large",
		},
		{
			name: "file of exactly the size limit accepted",
			setup: func(t *testing.T, dir string) string {
				return writeFile(t, dir, secret+strings.Repeat("\n", maxTokenFileSize-len(secret)), 0o600)
			},
			want: secret,
		},
		{
			// The mode checked is the target's, the file actually read.
			name: "symlink to owner-only file accepted",
			setup: func(t *testing.T, dir string) string {
				return symlinkTo(t, dir, writeFile(t, t.TempDir(), secret, 0o600))
			},
			want: secret,
		},
		{
			name: "symlink to world-readable file rejected",
			setup: func(t *testing.T, dir string) string {
				return symlinkTo(t, dir, writeFile(t, t.TempDir(), secret, 0o644))
			},
			wantErr: "chmod 600",
		},
		{
			// Must fail fast rather than block waiting for a writer.
			name: "FIFO rejected without hanging",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "fifo")
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("mkfifo: %v", err)
				}
				return path
			},
			wantErr: "not a regular file",
		},
		{
			name: "directory rejected",
			setup: func(t *testing.T, dir string) string {
				return dir
			},
			wantErr: "not a regular file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t, t.TempDir())
			got, err := ReadTokenFile(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ReadTokenFile error = %v, want it to contain %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q leaks the token", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadTokenFile: %v", err)
			}
			if got != tt.want {
				t.Errorf("ReadTokenFile = %q, want %q", got, tt.want)
			}
		})
	}
}

func writeFile(t *testing.T, dir, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	// Chmod explicitly: WriteFile's mode is filtered by the umask.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod token file: %v", err)
	}
	return path
}

func symlinkTo(t *testing.T, dir, target string) string {
	t.Helper()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return link
}
