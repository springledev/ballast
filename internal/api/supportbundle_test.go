package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/springledev/keel/secret"
)

// TestBundleScrubber proves the bundle scrubs credential-file contents
// the app never loaded into its redaction set, and leaves short values
// and ordinary text alone.
func TestBundleScrubber(t *testing.T) {
	dir := t.TempDir()
	for name, v := range map[string]string{"a": "LONG-UNLOADED-SECRET-123\n", "b": "tiny\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o := &Ops{Redact: secret.NewSet("LOADED-SECRET-456"), CredentialSearchDirs: []string{dir, filepath.Join(dir, "missing")}}
	tests := []struct{ name, in, want string }{
		{"loaded", "x LOADED-SECRET-456 y", "x " + secret.Placeholder + " y"},
		{"unloaded credential file", "x LONG-UNLOADED-SECRET-123 y", "x " + secret.Placeholder + " y"},
		{"short value left alone", "a tiny word", "a tiny word"},
	}
	s := o.bundleScrubber()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Redact(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
