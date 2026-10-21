package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/progress"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
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
	holdRunLock(t, data, "vault") // a live run's process holds the lock
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
	holdRunLock(t, data, "vault") // a live run's process holds the lock
	prev := waitIdleInterval
	waitIdleInterval = 10 * time.Millisecond
	t.Cleanup(func() { waitIdleInterval = prev })
	_, err := execCLI(t, "wait-idle", "--timeout", "50ms")
	if err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("busy: err = %v, want a timeout naming vault", err)
	}
}

// holdRunLock takes service's run lock in data, as a live run's process
// does, and returns the function that gives it up (also run at cleanup).
func holdRunLock(t *testing.T, data, service string) func() {
	t.Helper()
	release, ok, err := scheduler.FileLocker{Dir: filepath.Join(data, "locks")}.TryLock(service)
	if err != nil || !ok {
		t.Fatalf("taking the run lock for %s: ok=%v err=%v", service, ok, err)
	}
	var once sync.Once
	rel := func() { once.Do(release) }
	t.Cleanup(rel)
	return rel
}

// TestWaitIdleIgnoresACrashedRun: a run left unfinished by a crash
// (nothing holds its lock) must not stall wait-idle or status --running.
func TestWaitIdleIgnoresACrashedRun(t *testing.T) {
	data := cliEnv(t, &spyRunner{})
	seedTwoServices(t)
	withDB(t, data, func(s *store.Store) {
		if _, err := engine.StartRun(s.DB(), "vault"); err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	})
	if out, err := execCLI(t, "wait-idle", "--timeout", "5s"); err != nil || !strings.Contains(out, "Nothing is running") {
		t.Fatalf("wait-idle with a crashed run: err=%v out=%q", err, out)
	}
	if out, err := execCLI(t, "status", "--running"); err != nil {
		t.Errorf("status --running with a crashed run: err=%v out=%q", err, out)
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
		release := holdRunLock(t, data, "vault") // the live run's lock
		// Still VERIFYING: not idle.
		if _, err := execCLI(t, "status", "--running"); err == nil {
			t.Error("status --running in VERIFYING: want non-zero exit")
		}
		go func() {
			defer release()
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

// TestCheckStatusExitCode: `status --check` maps the lights to exit
// codes (spec §21.2): 0 all green, 1 amber, stale, running or no
// backups yet, 2 red (red wins over amber), and one summary line.
func TestCheckStatusExitCode(t *testing.T) {
	svc := func(name, light string, stale bool) api.ServiceStatus {
		return api.ServiceStatus{Service: name, Tiers: []api.TierStatus{{Tier: "restic", Light: light, Stale: stale}}}
	}
	tests := []struct {
		name     string
		services []api.ServiceStatus
		wantCode int
		wantText string
	}{
		{"all green", []api.ServiceStatus{svc("immich", "green", false), svc("vault", "green", false)}, 0, "2 green"},
		{"ballast-self green counts too", []api.ServiceStatus{svc("ballast-self", "green", false), svc("vault", "green", false)}, 0, "2 green"},
		{"amber", []api.ServiceStatus{svc("immich", "green", false), svc("vault", "amber", false)}, 1, "1 amber (vault)"},
		{"green but stale", []api.ServiceStatus{svc("vault", "green", true)}, 1, "1 amber (vault [stale])"},
		{"running", []api.ServiceStatus{svc("vault", "running", false)}, 1, "1 running (vault)"},
		{"no backups yet", []api.ServiceStatus{svc("vault", "none", false)}, 1, "1 no backups yet (vault)"},
		{"red", []api.ServiceStatus{svc("immich", "red", false)}, 2, "1 red (immich)"},
		{"red beats amber", []api.ServiceStatus{svc("immich", "red", false), svc("vault", "amber", false)}, 2, "1 red (immich), 1 amber (vault)"},
		{"self-backup waiting for its first run is ignored", []api.ServiceStatus{svc("vault", "green", false), {Service: "ballast-self", Self: true, FirstRunPending: true, Tiers: []api.TierStatus{{Tier: "restic", Light: "none"}}}}, 0, "1 green"},
		{"self-backup that ran and failed still counts", []api.ServiceStatus{svc("vault", "green", false), {Service: "ballast-self", Self: true, Tiers: []api.TierStatus{{Tier: "restic", Light: "red"}}}}, 2, "1 red (ballast-self)"},
		{"only a waiting self-backup confirms nothing", []api.ServiceStatus{{Service: "ballast-self", Self: true, FirstRunPending: true, Tiers: []api.TierStatus{{Tier: "restic", Light: "none"}}}}, 1, "waiting for its first scheduled run"},
		{"nothing configured", nil, 1, "No services are configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, summary := checkStatus(tt.services)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d (summary %q)", code, tt.wantCode, summary)
			}
			if !strings.Contains(summary, tt.wantText) {
				t.Errorf("summary = %q, want it to contain %q", summary, tt.wantText)
			}
			if strings.Contains(summary, "\n") {
				t.Errorf("summary = %q, want one line", summary)
			}
		})
	}
}

// TestStatusCheckFlagConflicts: --check cannot be combined with
// --running (two answers in one exit code) or --json (the summary line
// would break the JSON). Both are refused with a plain message.
func TestStatusCheckFlagConflicts(t *testing.T) {
	cliEnv(t, &spyRunner{})
	for _, args := range [][]string{{"status", "--check", "--running"}, {"status", "--check", "--json"}} {
		_, err := execCLI(t, args...)
		var ee *exitError
		if err == nil || errors.As(err, &ee) || !strings.Contains(err.Error(), "--check") {
			t.Errorf("%v: err = %v, want a plain message naming --check", args, err)
		}
	}
}

// TestStatusCheckNothingConfigured: with no services the check exits 1
// (nothing is confirmed) and says so.
func TestStatusCheckNothingConfigured(t *testing.T) {
	cliEnv(t, &spyRunner{})
	out, err := execCLI(t, "status", "--check")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err = %v, want exitError code 1", err)
	}
	if !strings.Contains(out, "No services are configured") {
		t.Errorf("out = %q, want the summary line", out)
	}
}

// TestStatusCheckRunningIsNotGreen: a backup in progress is reported as
// running and exits 1, the same as any light that is not green.
func TestStatusCheckRunningIsNotGreen(t *testing.T) {
	data := cliEnv(t, &spyRunner{})
	seedTwoServices(t)
	withDB(t, data, func(s *store.Store) {
		if _, err := engine.StartRun(s.DB(), "vault"); err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	})
	out, err := execCLI(t, "status", "--check")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err = %v, want exitError code 1 (out %q)", err, out)
	}
	if !strings.Contains(out, "1 running (vault)") {
		t.Errorf("out = %q, want vault shown as running", out)
	}
}

// TestStatusCheckUnreadable: when the status cannot be read at all, the
// check says so on stderr and exits 3.
func TestStatusCheckUnreadable(t *testing.T) {
	cliEnv(t, &spyRunner{})
	// A regular file where the data directory should be: the database
	// cannot be opened.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BALLAST_DATA_DIR", filepath.Join(blocker, "data"))
	out, err := execCLI(t, "status", "--check")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("err = %v, want exitError code 3", err)
	}
	if !strings.Contains(out, "Status cannot be read") {
		t.Errorf("out = %q, want the cannot-be-read line", out)
	}
}

// TestStatusCheckGreenAfterVerifiedBackup: a real verified backup makes
// the check exit 0.
func TestStatusCheckGreenAfterVerifiedBackup(t *testing.T) {
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
	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}
	if _, err := execCLI(t, "service", "add", "docs", "--catalog", "folder", "--destination", "local", "--param", "path="+t.TempDir()); err != nil {
		t.Fatalf("service add: %v", err)
	}
	if out, err := execCLI(t, "backup", "docs"); err != nil {
		t.Fatalf("backup: %v (output %q)", err, out)
	}
	out, err := execCLI(t, "status", "--check")
	if err != nil {
		t.Fatalf("status --check: %v (out %q)", err, out)
	}
	if !strings.Contains(out, "1 green") {
		t.Errorf("out = %q, want \"1 green\"", out)
	}
}

// TestCheckStatusOnlyPendingSelfBackupSaysPending: asking about just the
// self-backup while it waits for its first run says so, instead of
// claiming nothing is configured.
func TestCheckStatusOnlyPendingSelfBackupSaysPending(t *testing.T) {
	first := time.Date(2026, 10, 21, 4, 30, 0, 0, time.UTC)
	code, text := checkStatus([]api.ServiceStatus{{Service: "ballast-self", Self: true, FirstRunPending: true, FirstRunAt: &first, Tiers: []api.TierStatus{{Tier: "restic", Light: "none"}}}})
	if code != 1 || !strings.Contains(text, "waiting for its first") || strings.Contains(text, "No services are configured") {
		t.Fatalf("checkStatus = %d, %q; want code 1 saying ballast-self is waiting for its first run", code, text)
	}
}

// TestStatusTakesServicePositionally covers J1.
func TestStatusTakesServicePositionally(t *testing.T) {
	cliEnv(t, &spyRunner{})
	seedTwoServices(t)
	all, _, err := execCLISplit(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "vault") || !strings.Contains(all, "immich") {
		t.Fatalf("status = %q, want both services", all)
	}
	for _, args := range [][]string{{"status", "immich"}, {"status", "--service", "immich"}} {
		out, _, err := execCLISplit(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out, "immich") {
			t.Errorf("%v = %q, want immich", args, out)
		}
		if strings.Contains(out, "vault") {
			t.Errorf("%v listed every service: %q", args, out)
		}
	}
	if _, _, err := execCLISplit(t, "status", "a", "b"); err == nil {
		t.Error("status with two services should be refused")
	}
}
