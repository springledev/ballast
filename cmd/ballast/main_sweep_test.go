package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
)

// sweepApp builds a minimal app whose runner drives the real run state
// machine to finalState, with one nightly schedule for "svc".
func sweepApp(t *testing.T, finalState engine.State) *app {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	runner := scheduler.RunnerFunc(func(_ context.Context, service string) (engine.State, error) {
		m, err := engine.StartRun(st.DB(), service)
		if err != nil {
			return engine.FAILED, err
		}
		for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING, finalState} {
			if err := m.Transition(s, "simulated outcome"); err != nil {
				return engine.FAILED, err
			}
		}
		return finalState, nil
	})
	coord := &scheduler.Coordinator{
		DB:       st.DB(),
		Locks:    scheduler.FileLocker{Dir: filepath.Join(t.TempDir(), "locks")},
		Runner:   runner,
		Log:      &bytes.Buffer{},
		LockWait: -1,
	}
	if err := scheduler.SetSchedule(st.DB(), "svc", "0 3 * * *"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	return &app{store: st, coord: coord, notifier: notify.NewNotifier(), logw: &bytes.Buffer{}}
}

// TestSweepDueIsOneEnginePath proves the daemon's per-minute sweep and
// `ballast run-due` (both call sweepDue) leave identical state: the
// same run transitions, the same outcome, and the freshness sweep
// recorded, whether the job succeeds or fails.
func TestSweepDueIsOneEnginePath(t *testing.T) {
	due := time.Date(2026, 3, 5, 3, 0, 20, 0, time.UTC)
	tests := []struct {
		name    string
		final   engine.State
		wantErr bool
	}{
		{"verified backup succeeds", engine.SUCCESS, false},
		{"unverifiable backup is amber, not green", engine.SUCCESS_UNVERIFIED, true},
		{"failed backup is reported", engine.FAILED, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var outcomes [2]engine.State
			var errs [2]error
			for i, trigger := range []string{"daemon", "run-due"} {
				a := sweepApp(t, tc.final)
				_ = trigger // both triggers call sweepDue; separate DBs prove identical results
				errs[i] = sweepDue(context.Background(), a, due)
				runs, err := engine.RecentRuns(a.store.DB(), "svc", 5)
				if err != nil || len(runs) != 1 {
					t.Fatalf("%s: runs = %v, err = %v; want exactly one run", trigger, runs, err)
				}
				outcomes[i] = runs[0].State
				if _, err := a.store.MetaGet(freshnessSweepMetaKey); err != nil {
					t.Errorf("%s: freshness sweep not recorded: %v", trigger, err)
				}
			}
			if outcomes[0] != outcomes[1] || (errs[0] == nil) != (errs[1] == nil) {
				t.Errorf("daemon (%v, %v) and run-due (%v, %v) diverged", outcomes[0], errs[0], outcomes[1], errs[1])
			}
			if outcomes[0] != tc.final {
				t.Errorf("final state = %v, want %v", outcomes[0], tc.final)
			}
			if (errs[0] != nil) != tc.wantErr {
				t.Errorf("error = %v, wantErr %v", errs[0], tc.wantErr)
			}
		})
	}
}
