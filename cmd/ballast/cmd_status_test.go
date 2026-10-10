package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
)

func TestStatusCommandNoServices(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "No services configured") {
		t.Errorf("output = %q, want a beginner-friendly empty-state message", out)
	}
}

// TestStatusCommandAfterBackup runs a real backup (not a stub
// runner) so status reflects a real, verified run: the runner test
// seam is deliberately left nil here.
func TestStatusCommandAfterBackup(t *testing.T) {
	withStubRestic(t)
	data := filepath.Join(t.TempDir(), "data")
	conf := filepath.Join(t.TempDir(), "config")
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
	dataDir := t.TempDir()

	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "path="+dataDir); err != nil {
		t.Fatalf("service add: %v", err)
	}
	out, err := execCLI(t, "backup", "docs")
	if err != nil {
		t.Fatalf("backup: %v (output %q)", err, out)
	}

	out, err = execCLI(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "docs") || !strings.Contains(out, "green") {
		t.Errorf("status output = %q, want docs shown green", out)
	}

	out, err = execCLI(t, "status", "--json", "--service", "docs")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var st []api.ServiceStatus
	if err := json.Unmarshal([]byte(stripLogLines(out)), &st); err != nil {
		t.Fatalf("status --json output not valid JSON: %v\n%s", err, out)
	}
	if len(st) != 1 || st[0].Service != "docs" {
		t.Fatalf("status --json = %+v, want [docs]", st)
	}
}

// TestPrintStatusPlainWaiting: a running backup that waits for another
// service's use of the repository says so under its light.
func TestPrintStatusPlainWaiting(t *testing.T) {
	const note = "waiting for repository lock on b2 (held by immich)"
	tests := []struct {
		name    string
		tier    api.TierStatus
		wantOut bool
	}{
		{name: "waiting", tier: api.TierStatus{Tier: "restic", Light: "running", Waiting: note}, wantOut: true},
		{name: "not waiting", tier: api.TierStatus{Tier: "restic", Light: "running"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&buf)
			printStatusPlain(cmd, []api.ServiceStatus{{Service: "vaultwarden", Tiers: []api.TierStatus{tt.tier}}})
			if got := strings.Contains(buf.String(), note); got != tt.wantOut {
				t.Errorf("output = %q, want note shown = %v", buf.String(), tt.wantOut)
			}
		})
	}
}

// TestPrintStatusPlainPhase: a running backup shows its phase and how
// long it has been running; a finished one shows neither.
func TestPrintStatusPlainPhase(t *testing.T) {
	tests := []struct {
		name string
		tier api.TierStatus
		want string
	}{
		{name: "running", tier: api.TierStatus{Tier: "restic", Light: "running", Phase: "uploading to the destination", ElapsedSeconds: 125}, want: "now: uploading to the destination (running for 2m5s)"},
		{name: "finished", tier: api.TierStatus{Tier: "restic", Light: "green"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&buf)
			printStatusPlain(cmd, []api.ServiceStatus{{Service: "x", Tiers: []api.TierStatus{tt.tier}}})
			if got := strings.Contains(buf.String(), "now:"); got != (tt.want != "") {
				t.Fatalf("output = %q", buf.String())
			}
			if tt.want != "" && !strings.Contains(buf.String(), tt.want) {
				t.Errorf("output = %q, want %q", buf.String(), tt.want)
			}
		})
	}
}
