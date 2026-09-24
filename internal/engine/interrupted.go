package engine

import (
	"database/sql"
	"fmt"
)

// InterruptedDetail is the reason recorded on a run that was found
// unfinished with no Ballast process still working on it.
const InterruptedDetail = "interrupted: Ballast stopped before this run finished (crash, kill, or reboot)"

// FailInterruptedRun marks service's latest run FAILED with
// InterruptedDetail if that run never finished, and reports whether
// it did so, along with the run it looked at.
//
// Only call this while holding the service's run lock (see
// scheduler.Locker): the lock is what proves no live process owns the
// unfinished run. Without it, a run that is merely slow would be
// failed out from under its owner.
func FailInterruptedRun(db *sql.DB, service string) (LatestRun, bool, error) {
	last, err := LastStartedRun(db, service)
	if err != nil {
		return LatestRun{}, false, err
	}
	if !last.InProgress {
		return last, false, nil
	}
	m, err := LoadRun(db, last.RunID)
	if err != nil {
		return last, false, fmt.Errorf("failing interrupted run for %s: %w", service, err)
	}
	if err := m.Transition(FAILED, InterruptedDetail); err != nil {
		return last, false, fmt.Errorf("failing interrupted run %d for %s: %w", last.RunID, service, err)
	}
	return last, true, nil
}
