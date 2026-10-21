package engine

import (
	"testing"
)

// finishRun walks m through the happy path to SUCCESS.
func finishRun(t *testing.T, m *Machine) {
	t.Helper()
	for _, to := range []State{PREPARING, DUMPING, UPLOADING, VERIFYING, SUCCESS} {
		if err := m.Transition(to, ""); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
}

func TestLastStartedRunNeverRun(t *testing.T) {
	s := openMachineDB(t)
	got, err := LastStartedRun(s.DB(), "immich")
	if err != nil {
		t.Fatalf("LastStartedRun: %v", err)
	}
	if got != (LatestRun{}) {
		t.Errorf("LastStartedRun for unknown service = %+v, want zero", got)
	}
}

func TestLastStartedRunInProgress(t *testing.T) {
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "immich")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	got, err := LastStartedRun(s.DB(), "immich")
	if err != nil {
		t.Fatalf("LastStartedRun: %v", err)
	}
	if !got.InProgress {
		t.Error("a PENDING run must be reported in progress")
	}
	if got.RunID != m.Run().ID || !got.StartedAt.Equal(m.Run().StartedAt) {
		t.Errorf("LastStartedRun = %+v, want run %d started %v", got, m.Run().ID, m.Run().StartedAt)
	}
}

func TestLastStartedRunPicksNewestRun(t *testing.T) {
	s := openMachineDB(t)

	m1, err := StartRun(s.DB(), "immich")
	if err != nil {
		t.Fatalf("StartRun 1: %v", err)
	}
	finishRun(t, m1)

	m2, err := StartRun(s.DB(), "immich")
	if err != nil {
		t.Fatalf("StartRun 2: %v", err)
	}
	finishRun(t, m2)

	// Give the older run a start time whose text sorts *after* the
	// newer one's (same second, no fractional part vs. a fractional
	// part): ordering must follow run IDs, not timestamp text.
	if _, err := s.DB().Exec(`UPDATE runs SET started_at = ? WHERE id = ?`,
		"2026-03-05T03:00:05Z", m1.Run().ID); err != nil {
		t.Fatalf("rewriting run 1 start: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE runs SET started_at = ? WHERE id = ?`,
		"2026-03-05T03:00:05.5Z", m2.Run().ID); err != nil {
		t.Fatalf("rewriting run 2 start: %v", err)
	}

	got, err := LastStartedRun(s.DB(), "immich")
	if err != nil {
		t.Fatalf("LastStartedRun: %v", err)
	}
	if got.RunID != m2.Run().ID {
		t.Errorf("LastStartedRun picked run %d, want newest run %d", got.RunID, m2.Run().ID)
	}
	if got.InProgress {
		t.Error("a SUCCESS run must not be reported in progress")
	}
}

func TestLastStartedRunIgnoresOtherServices(t *testing.T) {
	s := openMachineDB(t)
	m, err := StartRun(s.DB(), "vaultwarden")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	finishRun(t, m)
	got, err := LastStartedRun(s.DB(), "immich")
	if err != nil {
		t.Fatalf("LastStartedRun: %v", err)
	}
	if got != (LatestRun{}) {
		t.Errorf("LastStartedRun for a different service = %+v, want zero", got)
	}
}

func TestLastStartedRunErrors(t *testing.T) {
	t.Run("unparseable start time", func(t *testing.T) {
		s := openMachineDB(t)
		m, err := StartRun(s.DB(), "immich")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if _, err := s.DB().Exec(`UPDATE runs SET started_at = 'not-a-time' WHERE id = ?`, m.Run().ID); err != nil {
			t.Fatalf("corrupting started_at: %v", err)
		}
		if _, err := LastStartedRun(s.DB(), "immich"); err == nil {
			t.Error("LastStartedRun must reject an unparseable started_at")
		}
	})

	t.Run("query failure", func(t *testing.T) {
		s := openMachineDB(t)
		s.Close()
		if _, err := LastStartedRun(s.DB(), "immich"); err == nil {
			t.Error("LastStartedRun must surface a query failure")
		}
	})
}

func TestUnfinishedServices(t *testing.T) {
	s := openMachineDB(t)
	if got, err := UnfinishedServices(s.DB()); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v; want none", got, err)
	}
	done, _ := StartRun(s.DB(), "done")
	finishRun(t, done)
	// "retried": an old unfinished run followed by a finished one.
	if _, err := StartRun(s.DB(), "retried"); err != nil {
		t.Fatal(err)
	}
	r2, _ := StartRun(s.DB(), "retried")
	finishRun(t, r2)
	if _, err := StartRun(s.DB(), "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := StartRun(s.DB(), "a"); err != nil {
		t.Fatal(err)
	}
	got, err := UnfinishedServices(s.DB())
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("UnfinishedServices = %v, %v; want [a b]", got, err)
	}
}
