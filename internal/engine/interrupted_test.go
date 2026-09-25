package engine

import (
	"context"
	"testing"
)

func TestFailInterruptedRun(t *testing.T) {
	t.Run("never run", func(t *testing.T) {
		s := openMachineDB(t)
		last, failed, err := FailInterruptedRun(context.Background(), s.DB(), "immich", nil)
		if err != nil || failed || last != (LatestRun{}) {
			t.Errorf("FailInterruptedRun = %+v, %v, %v; want zero, false, nil", last, failed, err)
		}
	})

	t.Run("finished run left alone", func(t *testing.T) {
		s := openMachineDB(t)
		m, err := StartRun(s.DB(), "immich")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		finishRun(t, m)
		_, failed, err := FailInterruptedRun(context.Background(), s.DB(), "immich", nil)
		if err != nil || failed {
			t.Errorf("FailInterruptedRun on a finished run = %v, %v; want false, nil", failed, err)
		}
		reloaded, err := LoadRun(s.DB(), m.Run().ID)
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		if reloaded.Run().State != SUCCESS {
			t.Errorf("state = %s, want SUCCESS untouched", reloaded.Run().State)
		}
	})

	for _, stopAt := range []State{PENDING, PREPARING, DUMPING, UPLOADING, VERIFYING} {
		t.Run("unfinished run in "+string(stopAt)+" is failed", func(t *testing.T) {
			s := openMachineDB(t)
			m, err := StartRun(s.DB(), "immich")
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			for _, to := range []State{PREPARING, DUMPING, UPLOADING, VERIFYING} {
				if m.Run().State == stopAt {
					break
				}
				if err := m.Transition(to, ""); err != nil {
					t.Fatalf("transition to %s: %v", to, err)
				}
			}
			last, failed, err := FailInterruptedRun(context.Background(), s.DB(), "immich", nil)
			if err != nil || !failed {
				t.Fatalf("FailInterruptedRun = %v, %v; want true, nil", failed, err)
			}
			if last.RunID != m.Run().ID {
				t.Errorf("run = %d, want %d", last.RunID, m.Run().ID)
			}
			reloaded, err := LoadRun(s.DB(), m.Run().ID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if got := reloaded.Run(); got.State != FAILED || got.Error != InterruptedDetail || got.FinishedAt == nil {
				t.Errorf("run = %+v, want FAILED with the interrupted reason and a finish time", got)
			}
		})
	}

	t.Run("lookup failure surfaces", func(t *testing.T) {
		s := openMachineDB(t)
		s.Close()
		if _, _, err := FailInterruptedRun(context.Background(), s.DB(), "immich", nil); err == nil {
			t.Error("FailInterruptedRun must surface a lookup failure")
		}
	})

	// TestFailInterruptedRun/notifies-through-a-fake-notifier pins
	// finding 4: an interrupted run must be reported through the
	// Notifier exactly like any other job failure (spec §11), since a
	// homelab owner needs to hear that Ballast died mid-backup just as
	// much as any other failure.
	t.Run("notifies through a fake notifier", func(t *testing.T) {
		s := openMachineDB(t)
		m, err := StartRun(s.DB(), "immich")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := m.Transition(PREPARING, ""); err != nil {
			t.Fatalf("Transition to PREPARING: %v", err)
		}

		n := &fakeNotifier{}
		last, failed, err := FailInterruptedRun(context.Background(), s.DB(), "immich", n)
		if err != nil || !failed {
			t.Fatalf("FailInterruptedRun = %v, %v; want true, nil", failed, err)
		}
		if len(n.jobFailures) != 1 {
			t.Fatalf("jobFailures = %d, want 1", len(n.jobFailures))
		}
		got := n.jobFailures[0]
		if got.service != "immich" || got.runID != last.RunID || got.detail != InterruptedDetail {
			t.Errorf("jobFailures[0] = %+v, want service=immich runID=%d detail=%q", got, last.RunID, InterruptedDetail)
		}
	})

	// TestFailInterruptedRun/threads-the-caller's-ctx-through-to-the-notifier
	// pins the phase-2 review fix: FailInterruptedRun must bound its
	// notification delivery with the ctx the caller passed in (e.g.
	// RunDue/RunNow's own ctx, which is cancelled on SIGTERM), not a
	// hardcoded context.Background() that would ignore shutdown.
	t.Run("threads the caller's ctx through to the notifier", func(t *testing.T) {
		s := openMachineDB(t)
		m, err := StartRun(s.DB(), "immich")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := m.Transition(PREPARING, ""); err != nil {
			t.Fatalf("Transition to PREPARING: %v", err)
		}

		type ctxKey struct{}
		ctx := context.WithValue(context.Background(), ctxKey{}, "the caller's ctx")

		n := &fakeNotifier{}
		if _, _, err := FailInterruptedRun(ctx, s.DB(), "immich", n); err != nil {
			t.Fatalf("FailInterruptedRun: %v", err)
		}
		if n.lastCtx == nil || n.lastCtx.Value(ctxKey{}) != "the caller's ctx" {
			t.Errorf("notifier saw ctx = %v, want the exact ctx FailInterruptedRun was called with, not context.Background()", n.lastCtx)
		}
	})
}
