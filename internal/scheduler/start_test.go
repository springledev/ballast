package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
)

// TestStartRefusesWhileRunning: Start must report ErrAlreadyRunning
// synchronously, without starting a run, when the service's lock is
// already held (spec plan D1: an API handler can return 409 at once
// instead of holding the request open).
func TestStartRefusesWhileRunning(t *testing.T) {
	f := newFixture(t)
	f.holdLock(t, "immich")

	done, interrupted, err := f.c.Start(context.Background(), "immich")
	if !errors.Is(err, ErrAlreadyRunning) || !strings.Contains(err.Error(), "immich") {
		t.Fatalf("Start error = %v, want ErrAlreadyRunning naming immich", err)
	}
	if done != nil {
		t.Errorf("done channel = %v, want nil when Start refuses synchronously", done)
	}
	if interrupted {
		t.Error("interrupted = true, want false on a synchronous refusal")
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want nothing", f.runner.ran)
	}
}

// TestStartReportsInterruptedSynchronously: an earlier unfinished run
// is detected and failed while claiming the lock, and that fact is
// reported back synchronously (before the new run even starts).
func TestStartReportsInterruptedSynchronously(t *testing.T) {
	f := newFixture(t)
	dead := f.leaveUnfinishedRun(t, "immich")

	done, interrupted, err := f.c.Start(context.Background(), "immich")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !interrupted {
		t.Error("interrupted = false, want true")
	}
	if got := runState(t, f, dead); got.State != engine.FAILED {
		t.Errorf("interrupted run state = %s, want FAILED", got.State)
	}

	select {
	case runErr := <-done:
		if runErr != nil {
			t.Errorf("done = %v, want the background run to succeed", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("done never received a result")
	}
	if len(f.runner.ran) != 1 || f.runner.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", f.runner.ran)
	}
}

// TestStartRunsInBackgroundAndReleasesLockWhenDone: Start returns
// before the run finishes, the lock stays held while it runs, and is
// released only once done fires — so a second Start refuses meanwhile
// and succeeds afterwards.
func TestStartRunsInBackgroundAndReleasesLockWhenDone(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	started := make(chan struct{})
	f.c.Runner = RunnerFunc(func(_ context.Context, service string) (engine.State, error) {
		close(started)
		<-release
		return engine.SUCCESS, nil
	})

	done, interrupted, err := f.c.Start(context.Background(), "immich")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if interrupted {
		t.Error("interrupted = true, want false (no prior run)")
	}
	<-started

	// The lock is held while the background run is in flight.
	if _, ok, _ := f.locks.TryLock("immich"); ok {
		t.Error("run lock free while the background run is still going")
	}

	close(release)
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Errorf("done = %v, want nil (SUCCESS)", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("done never received a result")
	}

	// The lock is released once the run has finished.
	relFn, ok, err := f.locks.TryLock("immich")
	if err != nil || !ok {
		t.Fatalf("run lock still held after Start's run finished: ok=%v err=%v", ok, err)
	}
	relFn()
}

// TestStartRedactsErrorsLikeRunNow: an error surfaced on done is
// redacted exactly like RunNow's returned error (spec rule 2).
func TestStartRedactsErrorsLikeRunNow(t *testing.T) {
	f := newFixture(t)
	const secret = "s3cret-token"
	f.c.Redact = []string{secret}
	f.runner.err = errors.New("boom: " + secret)

	done, _, err := f.c.Start(context.Background(), "immich")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runErr := <-done
	if runErr == nil {
		t.Fatal("done = nil, want the runner's failure")
	}
	if strings.Contains(runErr.Error(), secret) {
		t.Errorf("done error %q leaks the secret", runErr.Error())
	}
}

// TestStartRejectsUnverifiedRun mirrors TestRunNowRejectsUnverifiedRun:
// SUCCESS_UNVERIFIED must reach done as a non-nil error (the
// scheduler.Runner contract, structurally enforced by checkRunSuccess).
func TestStartRejectsUnverifiedRun(t *testing.T) {
	f := newFixture(t)
	f.runner.state = engine.SUCCESS_UNVERIFIED

	done, _, err := f.c.Start(context.Background(), "immich")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runErr := <-done
	if !errors.Is(runErr, ErrNotVerified) {
		t.Errorf("done = %v, want ErrNotVerified for SUCCESS_UNVERIFIED", runErr)
	}
}

// TestStartTriggerAuthenticatesBeforeStarting: a bad or missing token
// is rejected synchronously, with no run started, exactly like
// Trigger — StartTrigger only differs from Trigger in running the
// authenticated run through Start instead of RunNow.
func TestStartTriggerAuthenticatesBeforeStarting(t *testing.T) {
	f := newFixture(t)
	token, err := GenerateTriggerToken(f.c.DB, "immich")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}

	for _, bad := range []string{"", "wrong"} {
		done, interrupted, err := f.c.StartTrigger(context.Background(), "immich", bad)
		if !errors.Is(err, ErrInvalidTriggerToken) {
			t.Errorf("StartTrigger with token %q: error = %v, want ErrInvalidTriggerToken", bad, err)
		}
		if done != nil {
			t.Errorf("done channel = %v, want nil on a rejected token", done)
		}
		if interrupted {
			t.Error("interrupted = true, want false on a rejected token")
		}
	}
	if len(f.runner.ran) != 0 {
		t.Fatalf("ran = %v, want no runs for rejected tokens", f.runner.ran)
	}

	done, interrupted, err := f.c.StartTrigger(context.Background(), "immich", token)
	if err != nil {
		t.Fatalf("StartTrigger with valid token: %v", err)
	}
	if interrupted {
		t.Error("interrupted = true, want false (no prior run)")
	}
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Errorf("done = %v, want the background run to succeed", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("done never received a result")
	}
	if len(f.runner.ran) != 1 || f.runner.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", f.runner.ran)
	}
}

// TestStartTriggerRefusesWhileRunning: a valid token still gets
// ErrAlreadyRunning synchronously (never starting a second run) when
// the service is already busy, exactly like Start.
func TestStartTriggerRefusesWhileRunning(t *testing.T) {
	f := newFixture(t)
	token, err := GenerateTriggerToken(f.c.DB, "immich")
	if err != nil {
		t.Fatalf("GenerateTriggerToken: %v", err)
	}
	f.holdLock(t, "immich")

	done, _, err := f.c.StartTrigger(context.Background(), "immich", token)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("StartTrigger while running: error = %v, want ErrAlreadyRunning", err)
	}
	if done != nil {
		t.Errorf("done channel = %v, want nil when the lock is held", done)
	}
	if len(f.runner.ran) != 0 {
		t.Errorf("ran = %v, want nothing", f.runner.ran)
	}
}
