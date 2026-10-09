package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LatestRun describes the most recently started run for a service, as
// the scheduler (spec §14 step 9) needs it: when it started, and
// whether it is still in progress (no terminal state reached yet).
type LatestRun struct {
	// RunID is the run's ID, or 0 when the service has never run.
	RunID int64
	// StartedAt is when the run started, or the zero Time when the
	// service has never run.
	StartedAt time.Time
	// InProgress is true while the run has not reached a terminal
	// state (finished_at is still NULL).
	InProgress bool
	// Waiting is what the in-progress run is waiting for (see
	// Machine.SetWaiting), or "" when it is not waiting or has
	// finished.
	Waiting string
}

// LastStartedRun returns the most recently started run for service,
// or a zero LatestRun when the service has no run yet. Runs are
// ordered by their autoincrement ID, not by timestamp text, so the
// newest run always wins regardless of how its timestamps format.
func LastStartedRun(db *sql.DB, service string) (LatestRun, error) {
	var (
		id       int64
		started  string
		finished sql.NullString
		waiting  sql.NullString
	)
	err := db.QueryRow(`
		SELECT r.id, r.started_at, r.finished_at, r.waiting FROM runs r
		JOIN jobs j ON j.id = r.job_id
		WHERE j.service = ?
		ORDER BY r.id DESC LIMIT 1`, service,
	).Scan(&id, &started, &finished, &waiting)
	if errors.Is(err, sql.ErrNoRows) {
		return LatestRun{}, nil
	}
	if err != nil {
		return LatestRun{}, fmt.Errorf("looking up latest run for %s: %w", service, err)
	}
	t, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return LatestRun{}, fmt.Errorf("parsing start time of run %d for %s: %w", id, service, err)
	}
	lr := LatestRun{RunID: id, StartedAt: t, InProgress: !finished.Valid}
	if lr.InProgress {
		lr.Waiting = waiting.String
	}
	return lr, nil
}
