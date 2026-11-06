package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestCanTransition_AnyNonTerminalToCancelled: every unfinished state
// may move to CANCELLED, and no finished state may.
func TestCanTransition_AnyNonTerminalToCancelled(t *testing.T) {
	for _, from := range AllStates() {
		got := CanTransition(from, CANCELLED)
		if got == from.Terminal() {
			t.Errorf("CanTransition(%s, CANCELLED) = %v, want %v", from, got, !from.Terminal())
		}
	}
}

func TestRequestCancel_Running(t *testing.T) {
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "immich")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(PREPARING, ""); err != nil {
		t.Fatal(err)
	}
	if on, err := CancelRequested(s.DB(), "immich"); err != nil || on {
		t.Fatalf("CancelRequested before = %v, %v; want false", on, err)
	}
	req, err := RequestCancel(s.DB(), m.Run().ID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if req.AlreadyRequested || req.Service != "immich" || req.RunID != m.Run().ID {
		t.Errorf("RequestCancel = %+v", req)
	}
	if on, err := CancelRequested(s.DB(), "immich"); err != nil || !on {
		t.Errorf("CancelRequested after = %v, %v; want true", on, err)
	}
	if on, _ := CancelRequested(s.DB(), "vaultwarden"); on {
		t.Error("another service reads as asked to stop")
	}
}

func TestRequestCancel_Finished(t *testing.T) {
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "immich")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(FAILED, "broke"); err != nil {
		t.Fatal(err)
	}
	if _, err := RequestCancel(s.DB(), m.Run().ID); !errors.Is(err, ErrRunFinished) {
		t.Errorf("RequestCancel on a finished run = %v, want ErrRunFinished", err)
	}
	if _, err := RequestCancel(s.DB(), 9999); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("RequestCancel on an unknown run = %v, want ErrRunNotFound", err)
	}
	if on, _ := CancelRequested(s.DB(), "immich"); on {
		t.Error("a finished run reads as asked to stop")
	}
}

func TestRequestCancel_Twice(t *testing.T) {
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "immich")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RequestCancel(s.DB(), m.Run().ID); err != nil {
		t.Fatal(err)
	}
	req, err := RequestCancel(s.DB(), m.Run().ID)
	if err != nil {
		t.Fatalf("second RequestCancel: %v", err)
	}
	if !req.AlreadyRequested {
		t.Error("second RequestCancel: AlreadyRequested = false, want true")
	}
}

// TestCancelNeverSuccess: once a stop is asked for, the run can no
// longer finish green or amber; it ends CANCELLED (rule 1).
func TestCancelNeverSuccess(t *testing.T) {
	for _, to := range []State{SUCCESS, SUCCESS_UNVERIFIED} {
		t.Run(string(to), func(t *testing.T) {
			m, s, err := startRunAt(t, VERIFYING)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RequestCancel(s.DB(), m.Run().ID); err != nil {
				t.Fatal(err)
			}
			err = m.Transition(to, "done")
			if !errors.Is(err, ErrCancelRequested) {
				t.Fatalf("Transition(%s) after a stop request = %v, want ErrCancelRequested", to, err)
			}
			if !Cancelled(context.Background(), err) || CancelDetail(context.Background(), err) != CancelDetailRequested {
				t.Errorf("the refused finish does not read as stopped on request")
			}
			if err := m.Transition(CANCELLED, CancelDetail(context.Background(), err)); err != nil {
				t.Fatalf("Transition(CANCELLED): %v", err)
			}
			got, err := LoadRun(s.DB(), m.Run().ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Run().State != CANCELLED {
				t.Errorf("state = %s, want CANCELLED", got.Run().State)
			}
		})
	}
}

// TestCancelRacesCompletion races a stop request against the run's
// own successful finish, many times: either the stop is accepted and
// the run ends CANCELLED, or it is refused as already finished and the
// run is SUCCESS. Never both an accepted stop and a green run.
func TestCancelRacesCompletion(t *testing.T) {
	s := openMachineDB(t)
	for i := 0; i < 50; i++ {
		m, err := StartRun(s.DB(), "immich")
		if err != nil {
			t.Fatal(err)
		}
		for _, to := range []State{PREPARING, DUMPING, UPLOADING, VERIFYING} {
			if err := m.Transition(to, ""); err != nil {
				t.Fatal(err)
			}
		}
		// Read the ID now: a Machine is not safe for concurrent use, and
		// the API asks for a stop by run ID, without sharing the Machine.
		runID := m.Run().ID
		var (
			wg        sync.WaitGroup
			cancelErr error
			finishErr error
		)
		wg.Add(2)
		go func() { defer wg.Done(); _, cancelErr = RequestCancel(s.DB(), runID) }()
		go func() {
			defer wg.Done()
			finishErr = m.Transition(SUCCESS, "backup verified")
			if Cancelled(context.Background(), finishErr) {
				finishErr = m.Transition(CANCELLED, CancelDetail(context.Background(), finishErr))
			}
		}()
		wg.Wait()
		if finishErr != nil {
			t.Fatalf("round %d: finishing: %v", i, finishErr)
		}
		got, err := LoadRun(s.DB(), m.Run().ID)
		if err != nil {
			t.Fatal(err)
		}
		state := got.Run().State
		switch {
		case cancelErr == nil && state != CANCELLED:
			t.Fatalf("round %d: stop accepted but run ended %s", i, state)
		case errors.Is(cancelErr, ErrRunFinished) && state != SUCCESS:
			t.Fatalf("round %d: stop refused as finished but run ended %s", i, state)
		case cancelErr != nil && !errors.Is(cancelErr, ErrRunFinished):
			t.Fatalf("round %d: RequestCancel: %v", i, cancelErr)
		}
	}
}

// TestCancelDetail_NoSecrets: the recorded reasons are fixed plain
// English and carry nothing from the error that ended the run.
func TestCancelDetail_NoSecrets(t *testing.T) {
	user, stop := context.WithCancelCause(context.Background())
	stop(ErrCancelledByUser)
	shut, stop2 := context.WithCancel(context.Background())
	stop2()
	secretErr := errors.New("restic: password hunter2 rejected")
	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"on request": {user, CancelDetailRequested},
		"shutdown":   {shut, CancelDetailShutdown},
	} {
		got := CancelDetail(tc.ctx, secretErr)
		if got != tc.want {
			t.Errorf("%s: CancelDetail = %q, want %q", name, got, tc.want)
		}
	}
}
