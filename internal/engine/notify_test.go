package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/springledev/ballast/internal/store"
)

// notifyCall records one call made to a fakeNotifier method.
type notifyCall struct {
	service string
	runID   int64
	detail  string
}

// fakeNotifier is a test double for Notifier: it records every call
// it receives and, when failWith is set, returns that error from
// every method (simulating a channel that is down). lastCtx records
// the ctx argument of the most recent call, so tests can confirm a
// specific ctx (e.g. one carrying a distinguishing value, or an
// already-cancelled one) was actually threaded through rather than
// silently replaced with context.Background().
type fakeNotifier struct {
	jobFailures    []notifyCall
	verifyFailures []notifyCall
	heartbeats     []notifyCall
	failWith       error
	lastCtx        context.Context
}

func (f *fakeNotifier) NotifyJobFailure(ctx context.Context, service string, runID int64, detail string) error {
	f.lastCtx = ctx
	f.jobFailures = append(f.jobFailures, notifyCall{service, runID, detail})
	return f.failWith
}

func (f *fakeNotifier) NotifyVerificationFailure(ctx context.Context, service string, runID int64, detail string) error {
	f.verifyFailures = append(f.verifyFailures, notifyCall{service, runID, detail})
	return f.failWith
}

func (f *fakeNotifier) NotifySuccessHeartbeat(ctx context.Context, service string, runID int64) error {
	f.heartbeats = append(f.heartbeats, notifyCall{service: service, runID: runID})
	return f.failWith
}

func notifyTestDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMachine_NoNotifierIsNoop confirms a Machine with no Notifier
// set (the default) transitions normally and reports no notify error.
func TestMachine_NoNotifierIsNoop(t *testing.T) {
	db := notifyTestDB(t)
	m, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := m.Transition(FAILED, "boom"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if err := m.NotifyErr(); err != nil {
		t.Errorf("NotifyErr() = %v, want nil", err)
	}
}

// TestMachine_NotifiesJobFailure: a FAILED transition from a
// non-VERIFYING state calls NotifyJobFailure, not
// NotifyVerificationFailure.
func TestMachine_NotifiesJobFailure(t *testing.T) {
	db := notifyTestDB(t)
	m, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	n := &fakeNotifier{}
	m.SetNotifier(context.Background(), n)

	if err := m.Transition(PREPARING, ""); err != nil {
		t.Fatalf("Transition to PREPARING: %v", err)
	}
	if err := m.Transition(FAILED, "docker exec failed"); err != nil {
		t.Fatalf("Transition to FAILED: %v", err)
	}

	if len(n.jobFailures) != 1 {
		t.Fatalf("jobFailures = %d, want 1", len(n.jobFailures))
	}
	got := n.jobFailures[0]
	if got.service != "svc" || got.runID != m.Run().ID || got.detail != "docker exec failed" {
		t.Errorf("jobFailures[0] = %+v, want service=svc runID=%d detail=%q", got, m.Run().ID, "docker exec failed")
	}
	if len(n.verifyFailures) != 0 {
		t.Errorf("verifyFailures = %d, want 0", len(n.verifyFailures))
	}
	if len(n.heartbeats) != 0 {
		t.Errorf("heartbeats = %d, want 0", len(n.heartbeats))
	}
}

// TestMachine_NotifiesVerificationFailure: a FAILED transition out of
// VERIFYING calls NotifyVerificationFailure instead of
// NotifyJobFailure — a distinct event per spec §11.
func TestMachine_NotifiesVerificationFailure(t *testing.T) {
	db := notifyTestDB(t)
	m, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	n := &fakeNotifier{}
	m.SetNotifier(context.Background(), n)

	for _, s := range []State{PREPARING, DUMPING, UPLOADING, VERIFYING} {
		if err := m.Transition(s, ""); err != nil {
			t.Fatalf("Transition to %s: %v", s, err)
		}
	}
	if err := m.Transition(FAILED, "archive integrity check failed"); err != nil {
		t.Fatalf("Transition to FAILED: %v", err)
	}

	if len(n.verifyFailures) != 1 {
		t.Fatalf("verifyFailures = %d, want 1", len(n.verifyFailures))
	}
	got := n.verifyFailures[0]
	if got.service != "svc" || got.runID != m.Run().ID || got.detail != "archive integrity check failed" {
		t.Errorf("verifyFailures[0] = %+v, unexpected", got)
	}
	if len(n.jobFailures) != 0 {
		t.Errorf("jobFailures = %d, want 0 (must not also count as a plain job failure)", len(n.jobFailures))
	}
}

// TestMachine_NotifiesSuccessHeartbeat: a SUCCESS transition calls
// NotifySuccessHeartbeat.
func TestMachine_NotifiesSuccessHeartbeat(t *testing.T) {
	db := notifyTestDB(t)
	m, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	n := &fakeNotifier{}
	m.SetNotifier(context.Background(), n)

	for _, s := range []State{PREPARING, DUMPING, UPLOADING, VERIFYING, SUCCESS} {
		if err := m.Transition(s, ""); err != nil {
			t.Fatalf("Transition to %s: %v", s, err)
		}
	}

	if len(n.heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1", len(n.heartbeats))
	}
	if got := n.heartbeats[0]; got.service != "svc" || got.runID != m.Run().ID {
		t.Errorf("heartbeats[0] = %+v, unexpected", got)
	}
}

// TestMachine_SuccessUnverifiedNoHeartbeat: SUCCESS_UNVERIFIED
// (amber) must never trigger a success heartbeat — only a genuine
// verified SUCCESS does.
func TestMachine_SuccessUnverifiedNoHeartbeat(t *testing.T) {
	db := notifyTestDB(t)
	m, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	n := &fakeNotifier{}
	m.SetNotifier(context.Background(), n)

	for _, s := range []State{PREPARING, DUMPING, UPLOADING, VERIFYING, SUCCESS_UNVERIFIED} {
		if err := m.Transition(s, ""); err != nil {
			t.Fatalf("Transition to %s: %v", s, err)
		}
	}

	if len(n.heartbeats) != 0 {
		t.Errorf("heartbeats = %d, want 0 for amber SUCCESS_UNVERIFIED", len(n.heartbeats))
	}
	if len(n.jobFailures) != 0 || len(n.verifyFailures) != 0 {
		t.Errorf("SUCCESS_UNVERIFIED must not fire a failure notification either: jobFailures=%d verifyFailures=%d", len(n.jobFailures), len(n.verifyFailures))
	}
}

// TestMachine_CancelledDoesNotNotify: CANCELLED is neither a job
// failure nor a success — it must not trigger any notification.
func TestMachine_CancelledDoesNotNotify(t *testing.T) {
	db := notifyTestDB(t)
	m, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	n := &fakeNotifier{}
	m.SetNotifier(context.Background(), n)

	if err := m.Transition(CANCELLED, "user cancelled"); err != nil {
		t.Fatalf("Transition to CANCELLED: %v", err)
	}
	if len(n.jobFailures)+len(n.verifyFailures)+len(n.heartbeats) != 0 {
		t.Errorf("CANCELLED must not notify: jobFailures=%d verifyFailures=%d heartbeats=%d", len(n.jobFailures), len(n.verifyFailures), len(n.heartbeats))
	}
}

// TestMachine_NotifyFailureDoesNotMaskRunError is the crux of the
// isolation requirement: when notification delivery itself fails, the
// run's own recorded error and state must be completely unaffected,
// and the delivery failure must be observable separately (via
// NotifyErr), never folded into or replacing the run's error.
func TestMachine_NotifyFailureDoesNotMaskRunError(t *testing.T) {
	db := notifyTestDB(t)
	m, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	deliveryErr := errors.New("ntfy: connection refused")
	m.SetNotifier(context.Background(), &fakeNotifier{failWith: deliveryErr})

	const runReason = "postgres dump exited 1: connection refused"
	if err := m.Transition(FAILED, runReason); err != nil {
		t.Fatalf("Transition to FAILED must still succeed even if notify delivery fails: %v", err)
	}

	if got := m.Run().State; got != FAILED {
		t.Errorf("run state = %s, want FAILED", got)
	}
	if got := m.Run().Error; got != runReason {
		t.Errorf("run error = %q, want %q (must not be masked by the notify failure)", got, runReason)
	}
	if !errors.Is(m.NotifyErr(), deliveryErr) {
		t.Errorf("NotifyErr() = %v, want to wrap/equal %v", m.NotifyErr(), deliveryErr)
	}

	// The persisted row must agree: reload from the DB independently.
	reloaded, err := LoadRun(db.DB(), m.Run().ID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if reloaded.Run().State != FAILED || reloaded.Run().Error != runReason {
		t.Errorf("persisted run = %+v, want state=FAILED error=%q", reloaded.Run(), runReason)
	}
}

// TestMachine_NotifyErrClearsOnNextSuccessfulSend confirms NotifyErr
// reflects only the most recent delivery attempt, not a sticky
// failure from an earlier one. A single Machine notifies at most once
// (only the terminal FAILED/SUCCESS transitions call the Notifier, and
// both are terminal states), so "the next attempt" here is a second
// run's notification through the same Notifier, whose underlying
// channel has recovered between the two.
func TestMachine_NotifyErrClearsOnNextSuccessfulSend(t *testing.T) {
	db := notifyTestDB(t)
	deliveryErr := errors.New("ntfy: connection refused")
	n := &fakeNotifier{failWith: deliveryErr}

	// First run: delivery fails, NotifyErr must report it.
	m1, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	m1.SetNotifier(context.Background(), n)
	if err := m1.Transition(FAILED, "boom"); err != nil {
		t.Fatalf("Transition to FAILED: %v", err)
	}
	if !errors.Is(m1.NotifyErr(), deliveryErr) {
		t.Fatalf("NotifyErr() after a failing send = %v, want %v", m1.NotifyErr(), deliveryErr)
	}

	// Channel recovers; a second run's notification (a genuine
	// successful send, not merely a non-notifying transition) must
	// leave NotifyErr clean.
	n.failWith = nil
	m2, err := StartRun(db.DB(), "svc")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	m2.SetNotifier(context.Background(), n)
	for _, s := range []State{PREPARING, DUMPING, UPLOADING, VERIFYING, SUCCESS} {
		if err := m2.Transition(s, ""); err != nil {
			t.Fatalf("Transition to %s: %v", s, err)
		}
	}
	if len(n.heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1 (the second run's successful send)", len(n.heartbeats))
	}
	if err := m2.NotifyErr(); err != nil {
		t.Errorf("NotifyErr() after the recovered channel's successful send = %v, want nil", err)
	}
}
