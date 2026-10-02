package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenGenerateShowsOnceAndNeverLogged(t *testing.T) {
	cliEnv(t, &spyRunner{})

	out, err := execCLI(t, "token", "generate", "immich")
	if err != nil {
		t.Fatalf("token generate: %v (output %q)", err, out)
	}
	token := extractToken(out)
	if token == "" {
		t.Fatal("token generate printed nothing")
	}
	if !strings.Contains(out, "will not be shown again") {
		t.Errorf("output = %q, want a warning that it is shown once", out)
	}

	// Generating again issues a new token; the old one must not
	// resurface anywhere.
	out2, err := execCLI(t, "token", "generate", "immich")
	if err != nil {
		t.Fatalf("second token generate: %v", err)
	}
	token2 := extractToken(out2)
	if token2 == token {
		t.Error("second generation issued the same token")
	}
	if strings.Contains(out2, token) {
		t.Error("second generation output contains the first token")
	}

	// The token must never appear in unrelated command output either.
	statusOut, err := execCLI(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(statusOut, token) || strings.Contains(statusOut, token2) {
		t.Error("status output leaks a trigger token")
	}
}

// extractToken finds the one line of execCLI's combined stdout+stderr
// output that isn't a "ballast: ..." log line (openApp always logs
// which credential directory it used, spec §8, before the command's
// own output) — that line is the raw generated token.
func extractToken(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "ballast:") {
			continue
		}
		return line
	}
	return ""
}

func TestWriteAPITokenIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds", apiTokenCredential)
	for _, tok := range []string{"first", "second"} {
		if err := writeAPIToken(path, tok); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
		}
		got, _ := os.ReadFile(path)
		if string(got) != tok+"\n" {
			t.Fatalf("contents = %q", got)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}
