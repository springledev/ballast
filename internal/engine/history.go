package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/store"
)

// ErrRunNotFound is returned by LoadVisibleRun when no run with that ID
// exists, or the run is older than its service's history floor.
var ErrRunNotFound = errors.New("no such run")

// ListServiceRuns returns up to limit runs of service, newest first
// (by run ID). When before is above zero only runs with an ID below it
// are returned, which is the cursor for the next page. Runs hidden by
// the service's history floor are skipped, exactly as in the status
// view.
func ListServiceRuns(db *sql.DB, service string, before int64, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `
		SELECT r.id, r.job_id, r.state, r.started_at, r.finished_at, r.error, r.snapshot_id
		FROM runs r JOIN jobs j ON j.id = r.job_id
		WHERE j.service = ? AND ` + store.RunsAfterFloorSQL
	args := []any{service}
	if before > 0 {
		q += ` AND r.id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing runs for %s: %w", service, err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows, service)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing runs for %s: %w", service, err)
	}
	return out, nil
}

// LoadVisibleRun returns run id with the state exactly as stored. It
// returns ErrRunNotFound when the run does not exist or lies before
// its service's history floor.
func LoadVisibleRun(db *sql.DB, id int64) (Run, error) {
	row := db.QueryRow(`
		SELECT r.id, r.job_id, r.state, r.started_at, r.finished_at, r.error, r.snapshot_id, j.service
		FROM runs r JOIN jobs j ON j.id = r.job_id
		WHERE r.id = ? AND `+store.RunsAfterFloorSQL, id)
	var service string
	r, err := scanRun(row, "", &service)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("run %d: %w", id, ErrRunNotFound)
	}
	if err != nil {
		return Run{}, err
	}
	r.Service = service
	return r, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRun(s rowScanner, service string, extra ...any) (Run, error) {
	var r Run
	var state, started string
	var finished, errorText, snapshotID sql.NullString
	dest := append([]any{&r.ID, &r.JobID, &state, &started, &finished, &errorText, &snapshotID}, extra...)
	if err := s.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, err
		}
		return Run{}, fmt.Errorf("reading run for %s: %w", service, err)
	}
	r.Service = service
	r.State = State(state)
	r.Error = errorText.String
	r.SnapshotID = snapshotID.String
	t, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return Run{}, fmt.Errorf("parsing started_at of run %d: %w", r.ID, err)
	}
	r.StartedAt = t
	if finished.Valid {
		f, err := time.Parse(time.RFC3339Nano, finished.String)
		if err != nil {
			return Run{}, fmt.Errorf("parsing finished_at of run %d: %w", r.ID, err)
		}
		r.FinishedAt = &f
	}
	return r, nil
}

// TransitionRecord is one logged step of a run: the state change, when
// it happened and the detail line explaining it.
type TransitionRecord struct {
	At     time.Time
	From   State
	To     State
	Detail string
}

// RecentTransitions returns the last limit transitions of runID, oldest
// first (the run's log, newest part).
func RecentTransitions(db *sql.DB, runID int64, limit int) ([]TransitionRecord, error) {
	rows, err := db.Query(`
		SELECT at, from_state, to_state, detail FROM run_transitions
		WHERE run_id = ? ORDER BY id DESC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading log of run %d: %w", runID, err)
	}
	defer rows.Close()
	var out []TransitionRecord
	for rows.Next() {
		var t TransitionRecord
		var at, from, to string
		if err := rows.Scan(&at, &from, &to, &t.Detail); err != nil {
			return nil, fmt.Errorf("reading log of run %d: %w", runID, err)
		}
		t.From, t.To = State(from), State(to)
		if t.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("parsing log time of run %d: %w", runID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading log of run %d: %w", runID, err)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
