package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/keel/runner"
)

// recNotifier records which services the engine reported on.
type recNotifier struct {
	mu       sync.Mutex
	failures []string
	hearts   []string
}

func (n *recNotifier) NotifyJobFailure(_ context.Context, service string, _ int64, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failures = append(n.failures, service)
	return nil
}

func (n *recNotifier) NotifyVerificationFailure(ctx context.Context, service string, id int64, d string) error {
	return n.NotifyJobFailure(ctx, service, id, d)
}

func (n *recNotifier) NotifySuccessHeartbeat(_ context.Context, service string, _ int64) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.hearts = append(n.hearts, service)
	return nil
}

// selfHarness is a harness with two destinations, a work directory and
// one ordinary service that has already failed once.
func selfHarness(t *testing.T) (*harness, *recNotifier) {
	t.Helper()
	h := newHarness(t)
	h.addDestination("local")
	h.addDestination("offsite")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	h.ops.Log = &strings.Builder{}
	n := &recNotifier{}
	h.ops.SelfNotifier = n
	if err := inventory.PutService(h.ops.DB, inventory.Service{Name: "docs", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}); err != nil {
		t.Fatal(err)
	}
	m, err := engine.StartRun(h.ops.DB, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(engine.FAILED, "earlier failure"); err != nil {
		t.Fatal(err)
	}
	return h, n
}

func statusOf(t *testing.T, h *harness, name string) ServiceStatus {
	t.Helper()
	sts, err := h.ops.Status(context.Background(), name, true)
	if err != nil || len(sts) != 1 {
		t.Fatalf("Status(%q) = %v, %v", name, sts, err)
	}
	return sts[0]
}

func countCalls(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestRunSelfBackupReachesVerifiedAndGetsItsOwnLight(t *testing.T) {
	h, n := selfHarness(t)
	docsBefore := statusOf(t, h, "docs")

	state, err := h.ops.RunSelfBackup(context.Background())
	if err != nil || state != engine.SUCCESS {
		t.Fatalf("RunSelfBackup = %v, %v; want SUCCESS", state, err)
	}
	rows, err := h.ops.DB.Query(`SELECT t.to_state FROM run_transitions t JOIN runs r ON r.id = t.run_id JOIN jobs j ON j.id = r.job_id WHERE j.service = ? ORDER BY t.id`, SelfBackupService)
	if err != nil {
		t.Fatal(err)
	}
	var path []string
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			t.Fatal(err)
		}
		path = append(path, to)
	}
	rows.Close()
	if got, want := strings.Join(path, ">"), "PENDING>PREPARING>DUMPING>UPLOADING>VERIFYING>SUCCESS"; got != want {
		t.Fatalf("self-backup transitions = %s, want %s", got, want)
	}
	st := statusOf(t, h, SelfBackupService)
	if !st.Self || len(st.Tiers) != 1 || st.Tiers[0].Light != "green" {
		t.Fatalf("self status = %+v, want green self entry", st)
	}
	if len(st.Targets) != 2 || st.Targets[0].Light != "green" || st.Targets[1].Light != "green" {
		t.Fatalf("targets = %+v, want one green light per destination", st.Targets)
	}
	var readBacks int
	for _, v := range st.Verification {
		if v.Method == "restic-read-back" && v.Passed {
			readBacks++
		}
	}
	if readBacks != 2 {
		t.Fatalf("verification = %+v, want a passing read-back per destination", st.Verification)
	}
	if st.FreshnessAgeSeconds == nil {
		t.Fatal("no freshness age for the self-backup")
	}
	if len(n.hearts) != 1 || n.hearts[0] != SelfBackupService {
		t.Fatalf("heartbeats = %v", n.hearts)
	}
	// It is on the dashboard list too, after the services.
	all, err := h.ops.Status(context.Background(), "", false)
	if err != nil || len(all) != 2 || all[1].Service != SelfBackupService {
		t.Fatalf("Status(all) = %v, %v", all, err)
	}
	// Retention ran for the self-backup tag only, after the read-back.
	calls := h.resticInvocations(t)
	if got := countCalls(calls, "forget --json"); got != 2 {
		t.Fatalf("forget calls = %d, want 2 (one per destination):\n%s", got, strings.Join(calls, "\n"))
	}
	for _, l := range calls {
		if strings.HasPrefix(l, "forget") && !strings.Contains(l, "--tag "+SelfBackupTag) {
			t.Errorf("forget not scoped to the self-backup tag: %s", l)
		}
	}
	firstForget := -1
	for i, l := range calls {
		if strings.HasPrefix(l, "forget") && firstForget < 0 {
			firstForget = i
		}
	}
	if firstForget < 0 || countCalls(calls[:firstForget], "ls ") == 0 {
		t.Errorf("retention ran before a read-back:\n%s", strings.Join(calls, "\n"))
	}
	if after := statusOf(t, h, "docs"); after.Tiers[0].Light != docsBefore.Tiers[0].Light || after.Tiers[0].LastRunID != docsBefore.Tiers[0].LastRunID {
		t.Errorf("a service's light changed: %+v -> %+v", docsBefore.Tiers[0], after.Tiers[0])
	}
}

func TestRunSelfBackupFailureAlertsWithoutTouchingServicesAndNeverPrunes(t *testing.T) {
	for _, tc := range []struct{ name, failCmd string }{
		{"upload fails", "backup"},
		{"read-back fails", "ls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STUB_FAIL_CMD", tc.failCmd)
			h, n := selfHarness(t)
			docsBefore := statusOf(t, h, "docs")

			state, err := h.ops.RunSelfBackup(context.Background())
			if err == nil || state != engine.FAILED {
				t.Fatalf("RunSelfBackup = %v, %v; want FAILED with an error", state, err)
			}
			if !strings.Contains(err.Error(), SelfBackupService) {
				t.Errorf("error does not name the self-backup: %v", err)
			}
			if st := statusOf(t, h, SelfBackupService); st.Tiers[0].Light != "red" {
				t.Fatalf("self light = %s, want red", st.Tiers[0].Light)
			}
			if len(n.failures) != 1 || n.failures[0] != SelfBackupService {
				t.Fatalf("failure alerts = %v, want exactly one for %s", n.failures, SelfBackupService)
			}
			if len(n.hearts) != 0 {
				t.Errorf("a failed self-backup sent a success heartbeat")
			}
			if got := countCalls(h.resticInvocations(t), "forget"); got != 0 {
				t.Errorf("retention ran after an unverified self-backup (%d forget calls)", got)
			}
			after := statusOf(t, h, "docs")
			if after.Tiers[0].Light != docsBefore.Tiers[0].Light || after.Tiers[0].LastRunID != docsBefore.Tiers[0].LastRunID || after.Tiers[0].LastError != docsBefore.Tiers[0].LastError {
				t.Errorf("a service's status changed: %+v -> %+v", docsBefore.Tiers[0], after.Tiers[0])
			}
		})
	}
}

func TestRunSelfBackupWithoutDestinationFails(t *testing.T) {
	h := newHarness(t)
	h.ops.WorkRoot = t.TempDir()
	h.ops.Log = &strings.Builder{}
	state, err := h.ops.RunSelfBackup(context.Background())
	if err == nil || state != engine.FAILED || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("RunSelfBackup = %v, %v; want FAILED asking for a destination", state, err)
	}
}

func TestEnsureSelfBackupScheduleNeverRunsBySurprise(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	if err := h.ops.EnsureSelfBackupSchedule(ctx, now); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := scheduler.ListSchedules(h.ops.DB); len(jobs) != 0 {
		t.Fatalf("schedule created with no destination: %v", jobs)
	}

	h.addDestination("local")
	if err := h.ops.EnsureSelfBackupSchedule(ctx, now); err != nil {
		t.Fatal(err)
	}
	jobs, _ := scheduler.ListSchedules(h.ops.DB)
	if len(jobs) != 1 || jobs[0].Service != SelfBackupService || jobs[0].Cron != DefaultSelfBackupCron || !jobs[0].Enabled {
		t.Fatalf("schedules = %+v, want the default enabled self-backup schedule", jobs)
	}
	// A sweep right after creation runs nothing...
	coord := &scheduler.Coordinator{DB: h.ops.DB, Locks: h.locks, Runner: h.runner}
	if err := coord.RunDue(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(h.runner.ran) != 0 {
		t.Fatalf("creating the schedule ran %v by surprise", h.runner.ran)
	}
	// ...and the next scheduled time does.
	if err := coord.RunDue(ctx, time.Date(2026, 10, 21, 4, 31, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if len(h.runner.ran) != 1 || h.runner.ran[0] != SelfBackupService {
		t.Fatalf("ran = %v, want one scheduled self-backup", h.runner.ran)
	}

	// Opting out sticks: a disabled schedule is neither recreated nor re-enabled.
	if err := h.ops.SetScheduleEnabled(ctx, SelfBackupService, false); err != nil {
		t.Fatal(err)
	}
	if err := h.ops.EnsureSelfBackupSchedule(ctx, now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := scheduler.ListSchedules(h.ops.DB); len(jobs) != 1 || jobs[0].Enabled {
		t.Fatalf("opt-out not respected: %+v", jobs)
	}
}

func TestPutServiceRefusesTheSelfBackupName(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	err := inventory.PutService(h.ops.DB, inventory.Service{Name: SelfBackupService, CatalogEntry: "folder", Destination: "local"})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("PutService(%q) = %v, want a reserved-name error", SelfBackupService, err)
	}
}

// TestRunSelfBackupRetentionRealRestic runs the scheduled self-backup
// several times against a real repository: prune keeps only the policy's
// snapshots of the self-backup tag and leaves a service's snapshot alone.
func TestRunSelfBackupRetentionRealRestic(t *testing.T) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic not installed")
	}
	h := newHarness(t)
	repoDir := filepath.Join(t.TempDir(), "repo")
	pwFile := filepath.Join(h.credDir, "ballast-restic-password.real")
	if err := os.WriteFile(pwFile, []byte("pw-for-retention-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inventory.PutDestination(h.ops.DB, inventory.Destination{Name: "real", Location: repoDir, PasswordCredential: "ballast-restic-password.real", KeepLast: 2}); err != nil {
		t.Fatal(err)
	}
	r := &runner.Runner{BaseEnv: append(os.Environ(), "RESTIC_CACHE_DIR="+t.TempDir())}
	h.ops.Repo = func(d inventory.Destination) (*restic.Repository, error) {
		return restic.Open(d.Location, restic.Options{Runner: r, PasswordFile: pwFile, ResticBin: bin})
	}
	h.ops.Log = &strings.Builder{}
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	ctx := context.Background()
	repo, _ := h.ops.Repo(inventory.Destination{Location: repoDir})
	if err := repo.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// A service's snapshot in the same repository.
	other := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "f"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Backup(ctx, restic.BackupOptions{Paths: []string{other}, Tags: []string{"ballast-service:docs"}, Host: "docs"}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		if state, err := h.ops.RunSelfBackup(ctx); err != nil || state != engine.SUCCESS {
			t.Fatalf("run %d: %v, %v", i, state, err)
		}
	}
	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var self, svc int
	for _, s := range snaps {
		for _, tag := range s.Tags {
			switch tag {
			case SelfBackupTag:
				self++
			case "ballast-service:docs":
				svc++
			}
		}
	}
	if self != 2 || svc != 1 {
		t.Fatalf("after retention: %d self-backup snapshots (want 2), %d service snapshots (want 1)", self, svc)
	}
}

// TestStatusSelfBackupFirstRunPending: a self-backup that is scheduled
// but has never run is listed as waiting for its first run, with the
// time; once it has run it is an ordinary light.
func TestStatusSelfBackupFirstRunPending(t *testing.T) {
	h, _ := selfHarness(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	h.ops.Now = func() time.Time { return now }
	if err := h.ops.EnsureSelfBackupSchedule(ctx, now); err != nil {
		t.Fatal(err)
	}
	all, err := h.ops.Status(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var self *ServiceStatus
	for i := range all {
		if all[i].Self {
			self = &all[i]
		}
	}
	if self == nil {
		t.Fatal("scheduled self-backup is not listed")
	}
	if !self.FirstRunPending || self.FirstRunAt == nil {
		t.Fatalf("self = pending %v at %v, want pending with a time", self.FirstRunPending, self.FirstRunAt)
	}
	if self.FirstRunAt.Hour() != 4 || self.FirstRunAt.Minute() != 30 {
		t.Errorf("first run at %v, want 04:30", self.FirstRunAt)
	}

	if _, err := h.ops.RunSelfBackup(ctx); err != nil {
		t.Fatal(err)
	}
	all, err = h.ops.Status(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.Self && (s.FirstRunPending || s.FirstRunAt != nil || s.Tiers[0].Light != "green") {
			t.Errorf("after a run: %+v, want a plain green light", s)
		}
	}
}

// selfStatus returns the self-backup's entry from the full status, or
// nil when it is not listed.
func selfStatus(t *testing.T, h *harness) *ServiceStatus {
	t.Helper()
	all, err := h.ops.Status(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if all[i].Self {
			return &all[i]
		}
	}
	return nil
}

// TestSelfBackupFirstRunPendingHasADeadline: "waiting for the first run"
// holds only until the first scheduled firing (plus an hour of grace).
// After that, no run at all is an amber "never ran", not silence, so
// `status --check` notices a self-backup that never starts.
func TestSelfBackupFirstRunPendingHasADeadline(t *testing.T) {
	h, _ := selfHarness(t)
	ctx := context.Background()
	created := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := h.ops.EnsureSelfBackupSchedule(ctx, created); err != nil {
		t.Fatal(err)
	}
	// First firing is 04:30 the next morning.
	tests := []struct {
		name        string
		now         time.Time
		wantPending bool
		wantLight   string
	}{
		{"before the first firing", time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC), true, "none"},
		{"inside the grace period", time.Date(2026, 10, 21, 5, 15, 0, 0, time.UTC), true, "none"},
		{"past the grace period", time.Date(2026, 10, 21, 5, 45, 0, 0, time.UTC), false, "amber"},
		{"days later", time.Date(2026, 10, 25, 5, 45, 0, 0, time.UTC), false, "amber"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.ops.Now = func() time.Time { return tt.now }
			self := selfStatus(t, h)
			if self == nil {
				t.Fatal("self-backup not listed")
			}
			if self.FirstRunPending != tt.wantPending || self.Tiers[0].Light != tt.wantLight {
				t.Fatalf("pending=%v light=%s, want pending=%v light=%s", self.FirstRunPending, self.Tiers[0].Light, tt.wantPending, tt.wantLight)
			}
			if !tt.wantPending && !strings.Contains(self.Tiers[0].LastError, "never") {
				t.Errorf("no explanation that it never ran: %+v", self.Tiers[0])
			}
		})
	}
}

// TestSelfBackupOptOutIsNotListed: a disabled schedule (the opt-out)
// means the settings backup is not listed at all.
func TestSelfBackupOptOutIsNotListed(t *testing.T) {
	h, _ := selfHarness(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	h.ops.Now = func() time.Time { return now.AddDate(0, 0, 30) }
	if err := h.ops.EnsureSelfBackupSchedule(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.SetEnabled(h.ops.DB, SelfBackupService, false); err != nil {
		t.Fatal(err)
	}
	if self := selfStatus(t, h); self != nil {
		t.Fatalf("opted-out self-backup is listed: %+v", self)
	}
}

// TestRunSelfBackupSetupFailureLeavesARedRun: a failure before the
// destinations are even reached (no work folder) still records a failed
// run, so the light goes red instead of the failure leaving no trace.
func TestRunSelfBackupSetupFailureLeavesARedRun(t *testing.T) {
	h, _ := selfHarness(t)
	h.ops.WorkRoot = ""
	h.ops.Log = &strings.Builder{}
	state, err := h.ops.RunSelfBackup(context.Background())
	if err == nil || state != engine.FAILED {
		t.Fatalf("RunSelfBackup = %v, %v; want FAILED", state, err)
	}
	last, lerr := engine.LastStartedRun(h.ops.DB, SelfBackupService)
	if lerr != nil || last.RunID == 0 {
		t.Fatalf("no run recorded for the failed attempt: %+v, %v", last, lerr)
	}
	st := statusOf(t, h, SelfBackupService)
	if st.Tiers[0].Light != "red" {
		t.Fatalf("light = %s, want red", st.Tiers[0].Light)
	}
}
