package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	secretpkg "github.com/springledev/keel/secret"
)

// spyRunner stands in for the real backup pipeline (spec §12: test
// the reserved interfaces with stubs). It records each run and can
// check that the run lock is held while it runs.
type spyRunner struct {
	mu        sync.Mutex
	ran       []string
	state     engine.State // returned state (default SUCCESS)
	err       error
	checkLock Locker // if set, Run asserts the service lock is held
	lockFree  []string
}

func (r *spyRunner) Run(_ context.Context, service string) (engine.State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, service)
	if r.checkLock != nil {
		if release, ok, _ := r.checkLock.TryLock(service); ok {
			release()
			r.lockFree = append(r.lockFree, service)
		}
	}
	state := r.state
	if state == "" {
		state = engine.SUCCESS
	}
	return state, r.err
}

type fixture struct {
	c      *Coordinator
	runner *spyRunner
	log    *bytes.Buffer
	locks  FileLocker
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := openTestDB(t)
	locks := FileLocker{Dir: filepath.Join(t.TempDir(), "locks")}
	runner := &spyRunner{checkLock: locks}
	log := &bytes.Buffer{}
	return &fixture{
		// No lock wait by default so refusals are immediate; tests of
		// the wait set LockWait themselves.
		c:      &Coordinator{DB: s.DB(), Locks: locks, Runner: runner, Log: log, LockWait: -1},
		runner: runner,
		log:    log,
		locks:  locks,
	}
}

func (f *fixture) schedule(t *testing.T, service, cron string) {
	t.Helper()
	if err := SetSchedule(f.c.DB, service, cron); err != nil {
		t.Fatalf("SetSchedule(%s): %v", service, err)
	}
}

// holdLock simulates a live run in another process.
func (f *fixture) holdLock(t *testing.T, service string) {
	t.Helper()
	release, ok, err := f.locks.TryLock(service)
	if err != nil || !ok {
		t.Fatalf("holding lock for %s: %v, %v", service, ok, err)
	}
	t.Cleanup(release)
}

// leaveUnfinishedRun simulates a run whose process died mid-backup.
func (f *fixture) leaveUnfinishedRun(t *testing.T, service string) *engine.Machine {
	t.Helper()
	m, err := engine.StartRun(f.c.DB, service)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := m.Transition(engine.PREPARING, ""); err != nil {
		t.Fatalf("transition: %v", err)
	}
	return m
}

func runState(t *testing.T, f *fixture, m *engine.Machine) engine.Run {
	t.Helper()
	reloaded, err := engine.LoadRun(f.c.DB, m.Run().ID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return reloaded.Run()
}

var nightly = time.Date(2026, 3, 5, 3, 0, 20, 0, time.UTC)

func TestRunDueRunsDueJobsUnderLockAndReportsFailures(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "vaultwarden", "* * * * *")
	f.schedule(t, "immich", "* * * * *")
	f.runner.err = errors.New("boom")

	err := f.c.RunDue(context.Background(), nightly)
	if err == nil || err.Error() != "2 job(s) failed: immich, vaultwarden" {
		t.Fatalf("RunDue error = %v, want both failures named in order", err)
	}
	if got := strings.Join(f.runner.ran, ","); got != "immich,vaultwarden" {
		t.Errorf("ran = %v, want both jobs run (in name order) despite the first failing", f.runner.ran)
	}
	if len(f.runner.lockFree) != 0 {
		t.Errorf("runs without the service lock held: %v", f.runner.lockFree)
	}
	if !strings.Contains(f.log.String(), "boom") {
		t.Errorf("log = %q, want the failure", f.log.String())
	}
}

func TestRunDueSkipsDisabledSchedules(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "* * * * *")
	if err := SetEnabled(f.c.DB, "immich", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := f.c.RunDue(context.Background(), nightly); err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want nothing (schedule disabled)", f.runner.ran)
	}
}

func TestRunDueIsolatesBadSchedule(t *testing.T) {
	f := newFixture(t)
	// SetSchedule rejects bad expressions; simulate stale stored data.
	if _, err := f.c.DB.Exec(
		`INSERT INTO schedules (service, cron_expr, enabled, updated_at) VALUES ('broken', 'not a cron', 1, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seeding broken schedule: %v", err)
	}
	f.schedule(t, "fine", "* * * * *")

	err := f.c.RunDue(context.Background(), nightly)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("RunDue error = %v, want it to name the broken schedule", err)
	}
	if len(f.runner.ran) != 1 || f.runner.ran[0] != "fine" {
		t.Errorf("ran = %v, want [fine] despite the other job's bad schedule", f.runner.ran)
	}
}

func TestRunDueFollowsSchedule(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	ctx := context.Background()

	// A new schedule runs at the first sweep.
	if err := f.c.RunDue(ctx, nightly); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// Same firing: a second sweep in the same minute, and later that
	// day, must not run it again.
	for _, at := range []time.Time{nightly.Add(20 * time.Second), nightly.Add(12 * time.Hour)} {
		if err := f.c.RunDue(ctx, at); err != nil {
			t.Fatalf("sweep at %v: %v", at, err)
		}
	}
	// Next night's firing runs it again.
	if err := f.c.RunDue(ctx, nightly.Add(24*time.Hour)); err != nil {
		t.Fatalf("next-night sweep: %v", err)
	}
	if len(f.runner.ran) != 2 {
		t.Errorf("ran = %v, want exactly two runs (one per nightly firing)", f.runner.ran)
	}
}

func TestRunDueDoesNotRetryFailedRunEveryMinute(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	f.runner.err = errors.New("catalog entry missing")
	ctx := context.Background()

	if err := f.c.RunDue(ctx, nightly); err == nil {
		t.Fatal("first sweep should report the failure")
	}
	for i := 1; i <= 5; i++ {
		if err := f.c.RunDue(ctx, nightly.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if len(f.runner.ran) != 1 {
		t.Errorf("ran = %v, want one attempt per firing, not one per sweep", f.runner.ran)
	}
}

func TestRunNowDoesNotShiftSchedule(t *testing.T) {
	// A manual or external-trigger run must not count as the scheduled
	// firing: with runs-based bookkeeping, a trigger inside a
	// repeated daylight-saving hour could re-arm a fixed-time job.
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	ctx := context.Background()
	if err := f.c.RunDue(ctx, nightly); err != nil { // consume the first-sweep run
		t.Fatalf("first sweep: %v", err)
	}
	if err := f.c.RunNow(ctx, "immich"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	fired, err := LastFired(f.c.DB, "immich")
	if err != nil {
		t.Fatalf("LastFired: %v", err)
	}
	if !fired.Equal(nightly) {
		t.Errorf("LastFired = %v after RunNow, want it unchanged at %v", fired, nightly)
	}
}

func TestRunDueSkipsServiceRunningElsewhere(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	f.holdLock(t, "immich")
	f.leaveUnfinishedRun(t, "immich") // the live run's DB row

	if err := f.c.RunDue(context.Background(), nightly); err != nil {
		t.Fatalf("RunDue: %v (a live run elsewhere is a skip, not a failure)", err)
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want no second run", f.runner.ran)
	}
	if !strings.Contains(f.log.String(), "still in progress") {
		t.Errorf("log = %q, want a skip note", f.log.String())
	}
}

func TestRunDueQuietWhileRunningAndNotDue(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	if err := MarkFired(f.c.DB, "immich", nightly); err != nil {
		t.Fatalf("MarkFired: %v", err)
	}
	f.holdLock(t, "immich")
	if err := f.c.RunDue(context.Background(), nightly.Add(time.Hour)); err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if f.log.Len() != 0 {
		t.Errorf("log = %q, want nothing: the job was not due anyway", f.log.String())
	}
}

func TestRunDueFailsInterruptedRunAndCarriesOn(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	dead := f.leaveUnfinishedRun(t, "immich") // no lock held: its process is gone

	err := f.c.RunDue(context.Background(), nightly)
	if err == nil || !strings.Contains(err.Error(), "immich") {
		t.Fatalf("RunDue error = %v, want the interrupted run reported as a failure", err)
	}
	if got := runState(t, f, dead); got.State != engine.FAILED || got.Error != engine.InterruptedDetail {
		t.Errorf("interrupted run = %+v, want FAILED as interrupted", got)
	}
	if !strings.Contains(f.log.String(), "interrupted") {
		t.Errorf("log = %q, want the interruption explained", f.log.String())
	}
	if !strings.Contains(f.log.String(), "check that they are running again") {
		t.Errorf("log = %q, want a hint that stopped containers may still be down", f.log.String())
	}
	if len(f.runner.ran) != 1 {
		t.Errorf("ran = %v, want the due job still run after clearing the dead run", f.runner.ran)
	}

	// Reported once: the next sweep finds nothing wrong.
	f.log.Reset()
	if err := f.c.RunDue(context.Background(), nightly.Add(time.Minute)); err != nil {
		t.Errorf("next sweep: %v, want no repeat report", err)
	}
}

// fakeNotifier is a minimal engine.Notifier that records job-failure
// calls, used to confirm Coordinator.Notifier is actually threaded
// through to engine.FailInterruptedRun (see withClaim).
type fakeNotifier struct {
	mu       sync.Mutex
	failures []string // "service:runID:detail"
}

func (n *fakeNotifier) NotifyJobFailure(_ context.Context, service string, runID int64, detail string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failures = append(n.failures, fmt.Sprintf("%s:%d:%s", service, runID, detail))
	return nil
}

func (n *fakeNotifier) NotifyVerificationFailure(_ context.Context, service string, runID int64, detail string) error {
	return nil
}

func (n *fakeNotifier) NotifySuccessHeartbeat(_ context.Context, service string, runID int64) error {
	return nil
}

// TestRunDuePassesNotifierToInterruptedRun pins review finding 4:
// Coordinator.Notifier must be passed through to
// engine.FailInterruptedRun (withClaim), so a run found interrupted
// during a sweep is reported through the same Notifier as any other
// job failure (spec §11), instead of only being logged.
func TestRunDuePassesNotifierToInterruptedRun(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	dead := f.leaveUnfinishedRun(t, "immich")
	notifier := &fakeNotifier{}
	f.c.Notifier = notifier

	if err := f.c.RunDue(context.Background(), nightly); err == nil || !strings.Contains(err.Error(), "immich") {
		t.Fatalf("RunDue error = %v, want the interrupted run reported as a failure", err)
	}

	want := fmt.Sprintf("immich:%d:%s", dead.Run().ID, engine.InterruptedDetail)
	if len(notifier.failures) != 1 || notifier.failures[0] != want {
		t.Errorf("notifier failures = %v, want exactly [%q]", notifier.failures, want)
	}
}

func TestRunDueNeverFailsLiveRun(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	f.holdLock(t, "immich")
	live := f.leaveUnfinishedRun(t, "immich")

	// Even days later, a run whose owner still holds the lock is left
	// alone: a slow first backup is not a stuck one.
	if err := f.c.RunDue(context.Background(), nightly.Add(72*time.Hour)); err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if got := runState(t, f, live); got.State != engine.PREPARING {
		t.Errorf("live run state = %s, want it untouched (PREPARING)", got.State)
	}
}

func TestRunDueSurfacesInfrastructureFailures(t *testing.T) {
	tests := []struct {
		name    string
		breakIt func(t *testing.T, f *fixture)
	}{
		{"schedule listing fails", func(t *testing.T, f *fixture) { mustExec(t, f, `DROP TABLE schedules`) }},
		{"run lookup fails", func(t *testing.T, f *fixture) { mustExec(t, f, `DROP TABLE runs`) }},
		{"last-firing lookup fails", func(t *testing.T, f *fixture) { mustExec(t, f, `UPDATE schedules SET last_fired_at = 'garbage'`) }},
		{"lock directory unusable", func(t *testing.T, f *fixture) {
			notDir := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(notDir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			f.c.Locks = FileLocker{Dir: notDir}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.schedule(t, "immich", "* * * * *")
			tt.breakIt(t, f)
			if err := f.c.RunDue(context.Background(), nightly); err == nil {
				t.Fatal("RunDue must fail loudly when it cannot tell whether a job should run")
			}
			if len(f.runner.ran) != 0 {
				t.Errorf("ran = %v, want nothing run", f.runner.ran)
			}
		})
	}
}

func mustExec(t *testing.T, f *fixture, q string) {
	t.Helper()
	if _, err := f.c.DB.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestRunNowRefusesWhileRunning(t *testing.T) {
	f := newFixture(t)
	f.holdLock(t, "immich")
	err := f.c.RunNow(context.Background(), "immich")
	if !errors.Is(err, ErrAlreadyRunning) || !strings.Contains(err.Error(), "immich") {
		t.Fatalf("RunNow error = %v, want ErrAlreadyRunning naming immich", err)
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want nothing", f.runner.ran)
	}
}

func TestRunNowClearsInterruptedRunThenRuns(t *testing.T) {
	f := newFixture(t)
	dead := f.leaveUnfinishedRun(t, "immich")
	// The new run succeeds, but the old failure must not be reported as
	// a clean success.
	err := f.c.RunNow(context.Background(), "immich")
	if !errors.Is(err, ErrPreviousRunInterrupted) || !strings.Contains(err.Error(), "immich") {
		t.Fatalf("RunNow error = %v, want ErrPreviousRunInterrupted naming immich", err)
	}
	if got := runState(t, f, dead); got.State != engine.FAILED {
		t.Errorf("interrupted run state = %s, want FAILED", got.State)
	}
	if len(f.runner.ran) != 1 || len(f.runner.lockFree) != 0 {
		t.Errorf("ran = %v (lock free during %v), want one run under the lock", f.runner.ran, f.runner.lockFree)
	}
}

func TestRunNowReportsInterruptedRunAndOwnFailure(t *testing.T) {
	f := newFixture(t)
	f.leaveUnfinishedRun(t, "immich")
	f.runner.err = errors.New("boom")

	err := f.c.RunNow(context.Background(), "immich")
	if !errors.Is(err, ErrPreviousRunInterrupted) || !errors.Is(err, f.runner.err) {
		t.Fatalf("RunNow error = %v, want both the interrupted run and this run's failure", err)
	}

	// Reported once: the next run has nothing old to report.
	f.runner.err = nil
	if err := f.c.RunNow(context.Background(), "immich"); err != nil {
		t.Errorf("next RunNow: %v, want no repeat report", err)
	}
}

func TestRunNowWaitsForBriefLockHold(t *testing.T) {
	f := newFixture(t)
	f.c.LockWait = 5 * time.Second
	release, ok, err := f.locks.TryLock("immich")
	if err != nil || !ok {
		t.Fatalf("taking lock: %v, %v", ok, err)
	}
	// A sweep clearing an interrupted run holds the lock only briefly.
	time.AfterFunc(150*time.Millisecond, release)

	if err := f.c.RunNow(context.Background(), "immich"); err != nil {
		t.Fatalf("RunNow: %v, want it to wait out the brief hold and run", err)
	}
	if len(f.runner.ran) != 1 {
		t.Errorf("ran = %v, want one run", f.runner.ran)
	}
}

func TestRunNowGivesUpAfterLockWait(t *testing.T) {
	f := newFixture(t)
	f.c.LockWait = 150 * time.Millisecond
	f.holdLock(t, "immich")

	start := time.Now()
	err := f.c.RunNow(context.Background(), "immich")
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("RunNow error = %v, want ErrAlreadyRunning", err)
	}
	if waited := time.Since(start); waited < f.c.LockWait {
		t.Errorf("gave up after %v, want at least LockWait (%v)", waited, f.c.LockWait)
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want nothing", f.runner.ran)
	}
}

func TestRunNowLockWaitStopsOnCancel(t *testing.T) {
	f := newFixture(t)
	f.c.LockWait = time.Minute
	f.holdLock(t, "immich")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	err := f.c.RunNow(ctx, "immich")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunNow error = %v, want context.Canceled", err)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("waited %v after cancellation, want a prompt return", waited)
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want nothing", f.runner.ran)
	}
}

func TestCoordinatorLockWait(t *testing.T) {
	tests := []struct {
		set  time.Duration
		want time.Duration
	}{
		{0, DefaultLockWait},
		{-1, 0},
		{-time.Hour, 0},
		{3 * time.Second, 3 * time.Second},
	}
	for _, tt := range tests {
		c := &Coordinator{LockWait: tt.set}
		if got := c.lockWait(); got != tt.want {
			t.Errorf("LockWait %v: lockWait() = %v, want %v", tt.set, got, tt.want)
		}
	}
}

// countingLocker wraps a Locker and records every TryLock call.
type countingLocker struct {
	Locker
	mu    sync.Mutex
	tried []string
}

func (l *countingLocker) TryLock(service string) (func(), bool, error) {
	l.mu.Lock()
	l.tried = append(l.tried, service)
	l.mu.Unlock()
	return l.Locker.TryLock(service)
}

func TestRunDueLeavesIdleServicesUnlocked(t *testing.T) {
	f := newFixture(t)
	counter := &countingLocker{Locker: f.locks}
	f.c.Locks = counter
	f.schedule(t, "idle", "0 3 * * *")    // not due, last run finished
	f.schedule(t, "crashed", "0 3 * * *") // not due, run left unfinished
	f.schedule(t, "due", "* * * * *")     // due
	for _, svc := range []string{"idle", "crashed"} {
		if err := MarkFired(f.c.DB, svc, nightly); err != nil {
			t.Fatalf("MarkFired: %v", err)
		}
	}
	f.leaveUnfinishedRun(t, "crashed")

	err := f.c.RunDue(context.Background(), nightly.Add(time.Hour))
	if err == nil || err.Error() != "1 job(s) failed: crashed" {
		t.Fatalf("RunDue error = %v, want only the interrupted run reported", err)
	}
	slices.Sort(counter.tried)
	if got := strings.Join(counter.tried, ","); got != "crashed,due" {
		t.Errorf("locks tried = %v, want only the due service and the one with an unfinished run", counter.tried)
	}
	if got := strings.Join(f.runner.ran, ","); got != "due" {
		t.Errorf("ran = %v, want [due]", f.runner.ran)
	}
}

func TestRunNowConcurrentCallsRunOnce(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	finish := make(chan struct{})
	f.c.Runner = RunnerFunc(func(context.Context, string) (engine.State, error) {
		close(started)
		<-finish
		return engine.SUCCESS, nil
	})

	first := make(chan error, 1)
	go func() { first <- f.c.RunNow(context.Background(), "immich") }()
	<-started
	// The second caller arrives mid-run, as a timer sweep racing a
	// udev trigger would.
	if err := f.c.RunNow(context.Background(), "immich"); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("concurrent RunNow error = %v, want ErrAlreadyRunning", err)
	}
	close(finish)
	if err := <-first; err != nil {
		t.Errorf("first RunNow: %v", err)
	}
}

func TestTrigger(t *testing.T) {
	f := newFixture(t)
	token, err := GenerateTriggerToken(f.c.DB, "immich")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	ctx := context.Background()

	for _, bad := range []string{"", "wrong"} {
		if err := f.c.Trigger(ctx, "immich", bad); err == nil || !strings.Contains(err.Error(), "invalid or missing trigger token") {
			t.Errorf("Trigger with token %q: error = %v, want rejection", bad, err)
		}
	}
	if len(f.runner.ran) != 0 {
		t.Fatalf("ran = %v, want no runs for rejected tokens", f.runner.ran)
	}

	if err := f.c.Trigger(ctx, "immich", token); err != nil {
		t.Fatalf("Trigger with valid token: %v", err)
	}
	if len(f.runner.ran) != 1 || f.runner.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", f.runner.ran)
	}

	f.holdLock(t, "immich")
	if err := f.c.Trigger(ctx, "immich", token); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("Trigger while running: error = %v, want ErrAlreadyRunning", err)
	}
}

// concurrencyRunner blocks every run until release is closed and
// records the peak number of runs in flight.
type concurrencyRunner struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	ran      []string
	started  chan string
	release  chan struct{}
	state    engine.State
	err      error
}

func newConcurrencyRunner() *concurrencyRunner {
	return &concurrencyRunner{started: make(chan string, 16), release: make(chan struct{})}
}

func (r *concurrencyRunner) Run(_ context.Context, service string) (engine.State, error) {
	r.mu.Lock()
	r.inFlight++
	r.peak = max(r.peak, r.inFlight)
	r.ran = append(r.ran, service)
	state := r.state
	if state == "" {
		state = engine.SUCCESS
	}
	r.mu.Unlock()
	r.started <- service
	<-r.release
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return state, r.err
}

func TestRunDueRunsServicesInParallelUpToLimit(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("max %d", limit), func(t *testing.T) {
			f := newFixture(t)
			for _, svc := range []string{"immich", "paperless", "vaultwarden"} {
				f.schedule(t, svc, "0 3 * * *")
			}
			r := newConcurrencyRunner()
			r.err = errors.New("boom") // every run logs concurrently: exercises the log lock under -race
			f.c.Runner = r
			f.c.MaxParallel = limit

			done := make(chan error, 1)
			go func() { done <- f.c.RunDue(context.Background(), nightly) }()
			// Wait until the limit is reached, then give any
			// over-limit run a chance to (wrongly) start.
			for i := 0; i < limit; i++ {
				select {
				case <-r.started:
				case <-time.After(10 * time.Second):
					close(r.release)
					<-done // the sweep has finished writing the log
					t.Fatalf("only %d of %d runs started; log:\n%s", i, limit, f.log.String())
				}
			}
			select {
			case svc := <-r.started:
				t.Fatalf("%s started beyond the limit of %d", svc, limit)
			case <-time.After(50 * time.Millisecond):
			}
			close(r.release)
			err := <-done
			if err == nil || !strings.Contains(err.Error(), "3 job(s) failed: immich, paperless, vaultwarden") {
				t.Errorf("RunDue error = %v, want all three failures named in order", err)
			}
			if r.peak != limit || len(r.ran) != 3 {
				t.Errorf("peak concurrency = %d over %v, want %d with all three run", r.peak, r.ran, limit)
			}
		})
	}
}

func TestRunDueCancelledBeforeStart(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	f.schedule(t, "vaultwarden", "0 3 * * *")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := f.c.RunDue(ctx, nightly)
	if err == nil || !strings.Contains(err.Error(), "shutting down: 2 job(s) not checked or started, left for the next sweep: immich, vaultwarden") {
		t.Fatalf("RunDue error = %v, want both services reported as left for the next sweep", err)
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want nothing started while shutting down", f.runner.ran)
	}
	for _, svc := range []string{"immich", "vaultwarden"} {
		if fired, _ := LastFired(f.c.DB, svc); !fired.IsZero() {
			t.Errorf("%s firing recorded at %v, want none: it never ran", svc, fired)
		}
	}
}

func TestRunDueCancelledMidRunRetriesAfterRestart(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	f.schedule(t, "vaultwarden", "0 3 * * *")
	// Yesterday's firing, which must be restored for immich.
	yesterday := nightly.Add(-24 * time.Hour)
	for _, svc := range []string{"immich", "vaultwarden"} {
		if err := MarkFired(f.c.DB, svc, yesterday); err != nil {
			t.Fatalf("MarkFired: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	f.c.Runner = RunnerFunc(func(ctx context.Context, service string) (engine.State, error) {
		close(started) // only immich may start: a second call panics
		<-ctx.Done()   // a real runner cleans up here, then returns
		return engine.CANCELLED, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- f.c.RunDue(ctx, nightly) }()
	<-started
	cancel() // systemd sends SIGTERM mid-backup

	err := <-done
	if err == nil ||
		!strings.Contains(err.Error(), "1 job(s) failed: immich") ||
		!strings.Contains(err.Error(), "left for the next sweep: vaultwarden") {
		t.Fatalf("RunDue error = %v, want immich cut short and vaultwarden left for later", err)
	}
	if !strings.Contains(f.log.String(), "cut short because Ballast is shutting down") {
		t.Errorf("log = %q, want the cut-short run explained", f.log.String())
	}
	for _, svc := range []string{"immich", "vaultwarden"} {
		if fired, _ := LastFired(f.c.DB, svc); !fired.Equal(yesterday) {
			t.Errorf("%s last firing = %v, want it left at %v so the next sweep retries", svc, fired, yesterday)
		}
	}

	// After restart, the next sweep picks both up.
	r := &spyRunner{}
	f.c.Runner = r
	if err := f.c.RunDue(context.Background(), nightly.Add(5*time.Minute)); err != nil {
		t.Fatalf("sweep after restart: %v", err)
	}
	if strings.Join(r.ran, ",") != "immich,vaultwarden" {
		t.Errorf("ran after restart = %v, want both", r.ran)
	}
}

func TestRunDueKeepsFiringWhenRunFinishedDespiteShutdown(t *testing.T) {
	// The run completed (nil error) just as shutdown began: that
	// firing really was served and must stay recorded.
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	ctx, cancel := context.WithCancel(context.Background())
	f.c.Runner = RunnerFunc(func(context.Context, string) (engine.State, error) {
		cancel()
		return engine.SUCCESS, nil
	})
	if err := f.c.RunDue(ctx, nightly); err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if fired, _ := LastFired(f.c.DB, "immich"); !fired.Equal(nightly) {
		t.Errorf("last firing = %v, want %v", fired, nightly)
	}
}

func TestRunDueReArmsWhenRunReturnsUnverifiedStateDuringShutdown(t *testing.T) {
	// A runner returning (CANCELLED, nil) during shutdown should have
	// its firing re-armed: it's not a success, just an interruption.
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	ctx, cancel := context.WithCancel(context.Background())
	f.c.Runner = RunnerFunc(func(context.Context, string) (engine.State, error) {
		cancel()
		return engine.CANCELLED, nil
	})
	if err := f.c.RunDue(ctx, nightly); err == nil {
		t.Errorf("RunDue error = %v, want an error (CANCELLED is not SUCCESS)", err)
	}
	// The firing should be un-recorded (left at its previous state or zero)
	// so the next sweep retries it.
	if fired, _ := LastFired(f.c.DB, "immich"); !fired.IsZero() {
		t.Errorf("last firing = %v, want zero (re-armed for next sweep)", fired)
	}
}

func TestRunDueReportsInterruptedServiceOnceEvenIfAlsoFailing(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	dead := f.leaveUnfinishedRun(t, "immich")
	mustExec(t, f, `UPDATE schedules SET last_fired_at = 'garbage'`)

	err := f.c.RunDue(context.Background(), nightly)
	if err == nil || err.Error() != "1 job(s) failed: immich" {
		t.Fatalf("RunDue error = %v, want immich named exactly once", err)
	}
	want := dead.Run().StartedAt.Local().Format(time.RFC3339)
	if !strings.Contains(f.log.String(), "the run started at "+want+" was interrupted") {
		t.Errorf("log = %q, want the interrupted run's start time %s", f.log.String(), want)
	}
	if !strings.Contains(f.log.String(), "parsing last scheduled firing") {
		t.Errorf("log = %q, want the lookup failure too", f.log.String())
	}
}

func TestCoordinatorNilLogWritesToStderr(t *testing.T) {
	f := newFixture(t)
	f.c.Log = nil
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = prev })

	f.c.logf("immich", "hello %d", 42)
	got, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ballast: immich: hello 42\n" {
		t.Errorf("stderr = %q, want the message", got)
	}
}

// TestCoordinatorRedactsSecrets guards spec rule 2 at the last point
// before output: a Runner that wraps unredacted tool stderr into its
// error must not get the secret into the log or a returned error.
func TestCoordinatorRedactsSecrets(t *testing.T) {
	const secret = "hunter2-restic-password"
	leak := fmt.Errorf("restic: wrong password %q", secret)

	tests := []struct {
		name        string
		interrupted bool // leave an unfinished run first
		run         func(f *fixture) error
	}{
		{name: "RunDue log", run: func(f *fixture) error {
			return f.c.RunDue(context.Background(), nightly)
		}},
		{name: "RunNow error", run: func(f *fixture) error {
			return f.c.RunNow(context.Background(), "immich")
		}},
		{name: "RunNow error after an interrupted run", interrupted: true, run: func(f *fixture) error {
			return f.c.RunNow(context.Background(), "immich")
		}},
		{name: "Trigger error", run: func(f *fixture) error {
			token, err := GenerateTriggerToken(f.c.DB, "immich")
			if err != nil {
				t.Fatalf("GenerateTriggerToken: %v", err)
			}
			return f.c.Trigger(context.Background(), "immich", token)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.schedule(t, "immich", "0 3 * * *")
			f.c.Redact = secretpkg.NewSet("", secret) // an empty entry must not blank everything
			f.runner.err = leak
			if tt.interrupted {
				f.leaveUnfinishedRun(t, "immich")
			}

			err := tt.run(f)
			if err == nil {
				t.Fatal("want the run's failure reported")
			}
			if len(f.runner.ran) != 1 {
				t.Fatalf("ran = %v, want one run", f.runner.ran)
			}
			for what, text := range map[string]string{"log": f.log.String(), "error": err.Error()} {
				if strings.Contains(text, secret) {
					t.Errorf("%s leaks the secret: %q", what, text)
				}
			}
			if !strings.Contains(f.log.String()+err.Error(), "[redacted]") {
				t.Errorf("log %q / error %q: want the secret shown as [redacted]", f.log.String(), err)
			}
			if tt.name != "RunDue log" && !errors.Is(err, leak) {
				t.Errorf("error %v no longer matches the run's error with errors.Is", err)
			}
			if tt.interrupted && !errors.Is(err, ErrPreviousRunInterrupted) {
				t.Errorf("error %v lost ErrPreviousRunInterrupted", err)
			}
		})
	}
}

func TestCoordinatorRedactErr(t *testing.T) {
	c := &Coordinator{Redact: secretpkg.NewSet("s3cret")}
	if c.redactErr(nil) != nil {
		t.Error("redactErr(nil) must be nil")
	}
	clean := errors.New("no secrets here")
	if got := c.redactErr(clean); got != clean {
		t.Errorf("redactErr of a clean error = %v, want it returned unchanged", got)
	}
	dirty := errors.New("key s3cret and again s3cret")
	got := c.redactErr(dirty)
	if want := "key [redacted] and again [redacted]"; got.Error() != want {
		t.Errorf("redactErr text = %q, want %q", got.Error(), want)
	}
	if !errors.Is(got, dirty) {
		t.Error("redacted error must still match the original with errors.Is")
	}
}

func TestRunNowRejectsUnverifiedRun(t *testing.T) {
	tests := []struct {
		name            string
		state           engine.State
		err             error
		wantNotVerified bool
		wantNil         bool
	}{
		{"SUCCESS_UNVERIFIED returns ErrNotVerified", engine.SUCCESS_UNVERIFIED, nil, true, false},
		{"FAILED returns error", engine.FAILED, nil, true, false},
		{"CANCELLED returns error", engine.CANCELLED, nil, true, false},
		{"SUCCESS with nil returns nil", engine.SUCCESS, nil, false, true},
		{"runner error takes precedence", engine.SUCCESS, errors.New("boom"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.runner.state = tt.state
			f.runner.err = tt.err

			err := f.c.RunNow(context.Background(), "immich")
			if tt.wantNil {
				if err != nil {
					t.Errorf("RunNow error = %v, want nil for SUCCESS", err)
				}
			} else if tt.wantNotVerified {
				if !errors.Is(err, ErrNotVerified) {
					t.Errorf("RunNow error = %v, want to match ErrNotVerified", err)
				}
			} else if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Errorf("RunNow error = %v, want to match runner error %v", err, tt.err)
				}
			} else {
				t.Errorf("RunNow error = %v, unexpected error type", err)
			}
		})
	}
}

func TestRunDueRejectsUnverifiedRuns(t *testing.T) {
	tests := []struct {
		name  string
		state engine.State
		err   error
	}{
		{"SUCCESS_UNVERIFIED fails the run", engine.SUCCESS_UNVERIFIED, nil},
		{"FAILED fails the run", engine.FAILED, nil},
		{"CANCELLED fails the run", engine.CANCELLED, nil},
		{"runner error fails the run", engine.SUCCESS, errors.New("boom")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.schedule(t, "immich", "0 3 * * *")
			f.runner.state = tt.state
			f.runner.err = tt.err

			err := f.c.RunDue(context.Background(), nightly)
			// RunDue returns a summary error listing failed services,
			// not the specific failure type. Check that it's non-nil
			// and names the failed service.
			if tt.state == engine.SUCCESS && tt.err == nil {
				if err != nil {
					t.Errorf("RunDue error = %v, want nil for SUCCESS without error", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "immich") {
					t.Errorf("RunDue error = %v, want error naming immich", err)
				}
			}
		})
	}
}

func TestRunDueSkipsNewScheduleAfterManualBackup(t *testing.T) {
	f := newFixture(t)
	f.schedule(t, "immich", "0 3 * * *")
	db := f.c.DB
	ts := nightly.Add(8 * time.Hour).Format(time.RFC3339Nano)
	db.Exec(`INSERT INTO jobs (service, created_at) VALUES ('immich', ?)`, ts)
	if _, err := db.Exec(`INSERT INTO runs (job_id, state, started_at, finished_at)
		SELECT id, 'SUCCESS', ?, ? FROM jobs WHERE service='immich'`, ts, ts); err != nil {
		t.Fatal(err)
	}
	if err := f.c.RunDue(context.Background(), nightly.Add(9*time.Hour)); err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want nothing: backed up an hour ago by hand", f.runner.ran)
	}
}

// TestRecoverInterruptedFailsDeadRunsAtStartup: a run a crashed daemon
// left unfinished is failed at startup even for a service with no
// schedule (RunDue never looks at those), while a run whose lock is
// held is left alone.
func TestRecoverInterruptedFailsDeadRunsAtStartup(t *testing.T) {
	f := newFixture(t)
	dead := f.leaveUnfinishedRun(t, "manual-only") // no schedule, no lock
	f.holdLock(t, "live")
	live := f.leaveUnfinishedRun(t, "live")

	n, err := f.c.RecoverInterrupted(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("RecoverInterrupted = %d, %v; want 1, nil", n, err)
	}
	if got := runState(t, f, dead); got.State != engine.FAILED || got.Error != engine.InterruptedDetail {
		t.Errorf("dead run = %+v, want FAILED as interrupted", got)
	}
	if got := runState(t, f, live); got.State != engine.PREPARING {
		t.Errorf("live run state = %s, want it untouched", got.State)
	}
	if n, err := f.c.RecoverInterrupted(context.Background()); err != nil || n != 0 {
		t.Errorf("second call = %d, %v; want 0, nil", n, err)
	}
}
