package engine

import (
	"context"
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
// notifier, when non-nil, is attached to the run before the FAILED
// transition so the interruption is reported the same way any other
// job failure is (spec §11): an interrupted run is a real failure a
// homelab owner needs to hear about, not a silent internal cleanup
// step. Pass nil to skip notification (e.g. in tests, or before step
// 11 wires a real Notifier in).
//
// ctx bounds that notification delivery attempt, exactly as
// Machine.SetNotifier documents: pass the caller's own context (e.g.
// the one RunDue/RunNow received) so a pending SIGTERM is not held up
// by a stuck HTTP call. A nil ctx is treated as context.Background().
//
// Only call this while holding the service's run lock (see
// scheduler.Locker): the lock is what proves no live process owns the
// unfinished run. Without it, a run that is merely slow would be
// failed out from under its owner.
func FailInterruptedRun(ctx context.Context, db *sql.DB, service string, notifier Notifier) (LatestRun, bool, error) {
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
	if notifier != nil {
		m.SetNotifier(ctx, notifier)
	}
	if err := m.Transition(FAILED, InterruptedDetail); err != nil {
		return last, false, fmt.Errorf("failing interrupted run %d for %s: %w", last.RunID, service, err)
	}
	return last, true, nil
}
