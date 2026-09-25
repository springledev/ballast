package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCredFile creates a credential file with exactly the given
// permissions (WriteFile's perm is umask-masked, so chmod after).
func writeCredFile(t *testing.T, dir, name, contents string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing credential fixture: %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod credential fixture: %v", err)
	}
	return path
}

func TestLoadCredentialValue(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, dir string) (credDir, fileName string)
		want       string
		wantErr    bool
		wantErrSub string
	}{
		{
			name: "happy path returns trimmed contents",
			setup: func(t *testing.T, dir string) (string, string) {
				writeCredFile(t, dir, "ntfy-topic", "https://ntfy.sh/my-topic\n", 0o600)
				return dir, "ntfy-topic"
			},
			want: "https://ntfy.sh/my-topic",
		},
		{
			name: "stricter permissions accepted",
			setup: func(t *testing.T, dir string) (string, string) {
				writeCredFile(t, dir, "webhook-url", "https://example.com/hook", 0o400)
				return dir, "webhook-url"
			},
			want: "https://example.com/hook",
		},
		{
			name: "missing file",
			setup: func(t *testing.T, dir string) (string, string) {
				return dir, "nope"
			},
			wantErr:    true,
			wantErrSub: "nope",
		},
		{
			name: "group readable is rejected",
			setup: func(t *testing.T, dir string) (string, string) {
				writeCredFile(t, dir, "leaky", "s3cr3t-topic-url", 0o640)
				return dir, "leaky"
			},
			wantErr:    true,
			wantErrSub: "chmod 600",
		},
		{
			name: "world readable is rejected",
			setup: func(t *testing.T, dir string) (string, string) {
				writeCredFile(t, dir, "leaky2", "s3cr3t-topic-url", 0o604)
				return dir, "leaky2"
			},
			wantErr:    true,
			wantErrSub: "chmod 600",
		},
		{
			name: "empty file is rejected",
			setup: func(t *testing.T, dir string) (string, string) {
				writeCredFile(t, dir, "empty", "", 0o600)
				return dir, "empty"
			},
			wantErr:    true,
			wantErrSub: "empty",
		},
		{
			name: "whitespace-only file is rejected",
			setup: func(t *testing.T, dir string) (string, string) {
				writeCredFile(t, dir, "blank", "   \n\t  \n", 0o600)
				return dir, "blank"
			},
			wantErr:    true,
			wantErrSub: "empty",
		},
		{
			name: "path traversal in file name is rejected",
			setup: func(t *testing.T, dir string) (string, string) {
				return dir, "../etc/passwd"
			},
			wantErr: true,
		},
		{
			name: "relative credential directory is rejected",
			setup: func(t *testing.T, dir string) (string, string) {
				return "relative/dir", "topic"
			},
			wantErr: true,
		},
		{
			name: "empty credential directory is rejected",
			setup: func(t *testing.T, dir string) (string, string) {
				return "", "topic"
			},
			wantErr: true,
		},
		{
			name: "directory instead of file is rejected",
			setup: func(t *testing.T, dir string) (string, string) {
				if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
					t.Fatal(err)
				}
				return dir, "subdir"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			credDir, fileName := tt.setup(t, dir)

			got, err := LoadCredentialValue(credDir, fileName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadCredentialValue() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.wantErrSub != "" && !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErrSub)
				}
				if strings.Contains(err.Error(), "s3cr3t-topic-url") {
					t.Errorf("error leaks secret contents: %q", err.Error())
				}
				return
			}
			if got != tt.want {
				t.Errorf("LoadCredentialValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLoadCredentialValueNeverLeaksOnMissingFile pins the "never in
// errors/logs" requirement for the not-found path specifically: the
// error must name the file, never its would-be contents.
func TestLoadCredentialValueNeverLeaksOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadCredentialValue(dir, "ntfy-topic")
	if err == nil {
		t.Fatal("missing credential file must be a loud error")
	}
	if !strings.Contains(err.Error(), "ntfy-topic") {
		t.Errorf("error = %q, want the file name for context", err.Error())
	}
}
