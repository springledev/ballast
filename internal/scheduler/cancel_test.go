package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
)

// slowRunner starts a real run record and then waits until its context
// ends, like a long backup, ending the run CANCELLED with the reason
// the context carries.
type slowRunner struct {
	f       *fixture
	started chan int64
	cause   chan error
}

func (r *slowRunner) Run(ctx context.Context, service string) (engine.State, error) {
	m, err := engine.StartRun(r.f.c.DB, service)
	if err != nil {
		return "", err
	}
	if err := m.Transition(engine.PREPARING, "preparing"); err != nil {
		return "", err
	}
	r.started <- m.Run().ID
	<-ctx.Done()
	r.cause <- context.Cause(ctx)
	if err := m.Transition(engine.CANCELLED, engine.CancelDetail(ctx, nil)); err != nil {
		return "", err
	}
	return engine.CANCELLED, ctx.Err()
}

func newSlowFixture(t *testing.T) (*fixture, *slowRunner) {
	t.Helper()
	f := newFixture(t)
	r := &slowRunner{f: f, started: make(chan int64, 1), cause: make(chan error, 1)}
	f.c.Runner = r
	f.c.CancelPoll = 20 * time.Millisecond
	return f, r
}

// TestCancel_FromOtherProcess_DBPoll: a stop recorded in the database
// by another process (no in-process Cancel call) stops the run within
// a poll, with the "stopped on request" cause.
func TestCancel_FromOtherProcess_DBPoll(t *testing.T) {
	f, r := newSlowFixture(t)
	done := make(chan error, 1)
	go func() { done <- f.c.RunNow(context.Background(), "immich") }()
	id := <-r.started
	if _, err := engine.RequestCancel(f.c.DB, id); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	select {
	case cause := <-r.cause:
		if !errors.Is(cause, engine.ErrCancelledByUser) {
			t.Errorf("cause = %v, want ErrCancelledByUser", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not notice the stop request")
	}
	if err := <-done; err == nil {
		t.Errorf("RunNow = %v, want a not-verified error", err)
	}
	m, err := engine.LoadRun(f.c.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Run().State; got != engine.CANCELLED {
		t.Errorf("state = %s, want CANCELLED", got)
	}
}

// TestCancel_InProcessIsImmediate: Cancel reaches a run of this
// process at once, and reports false when nothing is running.
func TestCancel_InProcessIsImmediate(t *testing.T) {
	f, r := newSlowFixture(t)
	f.c.CancelPoll = time.Hour // only the in-process path can stop it
	if f.c.Cancel("immich") {
		t.Fatal("Cancel with nothing running = true")
	}
	done, _, err := f.c.Start(context.Background(), "immich")
	if err != nil {
		t.Fatal(err)
	}
	<-r.started
	if !f.c.Cancel("immich") {
		t.Fatal("Cancel of a running backup = false")
	}
	select {
	case cause := <-r.cause:
		if !errors.Is(cause, engine.ErrCancelledByUser) {
			t.Errorf("cause = %v, want ErrCancelledByUser", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run was not stopped")
	}
	<-done
	if f.c.Cancel("immich") {
		t.Error("Cancel after the run finished = true")
	}
}

// TestCancel_ShutdownIsNotOnRequest: cancelling the parent context
// (shutdown) does not read as stopped on request.
func TestCancel_ShutdownIsNotOnRequest(t *testing.T) {
	f, r := newSlowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.c.RunNow(ctx, "immich") }()
	<-r.started
	cancel()
	if cause := <-r.cause; errors.Is(cause, engine.ErrCancelledByUser) {
		t.Errorf("cause = %v, want plain shutdown", cause)
	}
	<-done
}
