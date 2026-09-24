package engine

import "testing"

func TestFailInterruptedRun(t *testing.T) {
	t.Run("never run", func(t *testing.T) {
		s := openMachineDB(t)
		last, failed, err := FailInterruptedRun(s.DB(), "immich")
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
		_, failed, err := FailInterruptedRun(s.DB(), "immich")
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
			last, failed, err := FailInterruptedRun(s.DB(), "immich")
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
		if _, _, err := FailInterruptedRun(s.DB(), "immich"); err == nil {
			t.Error("FailInterruptedRun must surface a lookup failure")
		}
	})
}
