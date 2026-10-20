package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/progress"
	"github.com/springledev/ballast/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestPrintStatusPlainUpload: a destination being uploaded to shows
// percentage and bytes; one without progress shows nothing.
func TestPrintStatusPlainUpload(t *testing.T) {
	up := &progress.Upload{Destination: "b2", Percent: 43.9, BytesDone: 36 << 30, TotalBytes: 84 << 30}
	tests := []struct {
		name string
		u    *progress.Upload
		want string
	}{
		{"with progress", up, "uploading to b2: 44% (36.0 GiB of 84.0 GiB)"},
		{"without", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&buf)
			printStatusPlain(cmd, []api.ServiceStatus{{Service: "x", Tiers: []api.TierStatus{{Light: "running"}}, Targets: []api.TargetStatus{{Destination: "b2", Upload: tt.u}}}})
			if got := strings.Contains(buf.String(), "uploading to"); got != (tt.want != "") {
				t.Fatalf("output = %q", buf.String())
			}
			if tt.want != "" && !strings.Contains(buf.String(), tt.want) {
				t.Errorf("output = %q, want %q", buf.String(), tt.want)
			}
		})
	}
}

// TestStatusRunningFlag: `status --running` prints whether anything is
// running and exits non-zero only when something is (spec §21.5).
func TestStatusRunningFlag(t *testing.T) {
	data := cliEnv(t, &spyRunner{})
	seedTwoServices(t)

	out, err := execCLI(t, "status", "--running")
	if err != nil || !strings.Contains(out, "Nothing is running") {
		t.Fatalf("idle: err=%v out=%q, want exit 0 and \"Nothing is running\"", err, out)
	}

	withDB(t, data, func(s *store.Store) {
		if _, err := engine.StartRun(s.DB(), "vault"); err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	})
	out, err = execCLI(t, "status", "--running")
	if err == nil {
		t.Fatalf("running: want a non-zero exit, got nil (out %q)", out)
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Errorf("err = %v, want exitError code 1", err)
	}
	if !strings.Contains(out, "vault") || strings.Contains(out, "immich") {
		t.Errorf("out = %q, want it to name only vault", out)
	}
}

// TestWaitIdleCommand: wait-idle returns at once when idle and fails
// with a plain message when a run is still going at the timeout.
func TestWaitIdleCommand(t *testing.T) {
	data := cliEnv(t, &spyRunner{})
	seedTwoServices(t)

	if out, err := execCLI(t, "wait-idle", "--timeout", "5s"); err != nil || !strings.Contains(out, "Nothing is running") {
		t.Fatalf("idle: err=%v out=%q", err, out)
	}

	withDB(t, data, func(s *store.Store) {
		if _, err := engine.StartRun(s.DB(), "vault"); err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	})
	prev := waitIdleInterval
	waitIdleInterval = 10 * time.Millisecond
	t.Cleanup(func() { waitIdleInterval = prev })
	_, err := execCLI(t, "wait-idle", "--timeout", "50ms")
	if err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("busy: err = %v, want a timeout naming vault", err)
	}
}

// TestWaitIdleSeesRunReachVerifyingThenSuccess drives a real run through
// the state machine: it counts as running through VERIFYING, and
// wait-idle, already waiting, returns 0 once the run reaches SUCCESS.
func TestWaitIdleSeesRunReachVerifyingThenSuccess(t *testing.T) {
	data := cliEnv(t, &spyRunner{})
	seedTwoServices(t)
	prev := waitIdleInterval
	waitIdleInterval = 20 * time.Millisecond
	t.Cleanup(func() { waitIdleInterval = prev })

	var m *engine.Machine
	withDB(t, data, func(s *store.Store) {
		var err error
		if m, err = engine.StartRun(s.DB(), "vault"); err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		for _, st := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING} {
			if err := m.Transition(st, "step"); err != nil {
				t.Fatalf("Transition %s: %v", st, err)
			}
		}
		// Still VERIFYING: not idle.
		if _, err := execCLI(t, "status", "--running"); err == nil {
			t.Error("status --running in VERIFYING: want non-zero exit")
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = m.RecordVerification(engine.VerificationRecord{Method: "archive-integrity", Passed: true})
			_ = m.Transition(engine.SUCCESS, "verified")
		}()
		out, err := execCLI(t, "wait-idle", "--timeout", "10s")
		if err != nil || !strings.Contains(out, "Nothing is running") {
			t.Fatalf("wait-idle after SUCCESS: err=%v out=%q", err, out)
		}
	})
	if out, err := execCLI(t, "status", "--running"); err != nil {
		t.Errorf("status --running after SUCCESS: err=%v out=%q", err, out)
	}
}

// TestDoctorCommandShowsSchemaVersion: the plain doctor output names the
// database version.
func TestDoctorCommandShowsSchemaVersion(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "database is at version") {
		t.Errorf("doctor output = %q, want the schema version", out)
	}
}
