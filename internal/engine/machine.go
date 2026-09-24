package engine

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/store"
)

// Run is one persisted execution of a job, held at its current
// state-machine position. All mutations go through the Machine so that
// illegal transitions are impossible.
type Run struct {
	ID         int64
	JobID      int64
	Service    string
	State      State
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
}

// Machine is a handle over a persisted run. It enforces the §4.1 state
// machine: every mutation either follows the legal adjacency table or
// is rejected, and every accepted transition (including the initial
// entry into PENDING) is recorded with a timestamp and detail line.
//
// A Machine is not safe for concurrent use: use it from a single
// goroutine, or serialize calls externally. The DB serializes writers
// and stale snapshots are rejected, but the in-memory snapshot is not
// mutex-guarded.
type Machine struct {
	store *sql.DB
	run   Run
}

// StartRun creates (if needed) the job for service, creates a new run
// in PENDING state, and records the initial PENDING→PENDING transition,
// all inside one transaction so the runs row and its first transition
// row are never observed separately.
func StartRun(db *sql.DB, service string) (_ *Machine, err error) {
	if service == "" {
		return nil, fmt.Errorf("starting run: service must not be empty")
	}
	jobID, err := upsertJob(db, service)
	if err != nil {
		return nil, fmt.Errorf("starting run for %s: %w", service, err)
	}

	now := time.Now().UTC()
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting run for %s: %w", service, err)
	}
	defer func() { err = store.RollbackOnError(tx, err) }()

	res, err := tx.Exec(
		`INSERT INTO runs (job_id, state, started_at) VALUES (?, ?, ?)`,
		jobID, string(PENDING), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("creating run for %s: %w", service, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("creating run for %s: %w", service, err)
	}

	m := &Machine{store: db, run: Run{ID: id, JobID: jobID, Service: service, State: PENDING, StartedAt: now}}
	if err = m.recordTx(tx, PENDING, PENDING, "run started", now); err != nil {
		return nil, fmt.Errorf("starting run for %s: recording initial transition: %w", service, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("starting run for %s: %w", service, err)
	}
	m.applySnapshot(PENDING, now, "run started")
	return m, nil
}

// LoadRun resumes the machine for an existing run, e.g. after a
// process restart. It errors if the run is in an illegal stored state.
func LoadRun(db *sql.DB, runID int64) (*Machine, error) {
	row := db.QueryRow(`
		SELECT r.id, r.job_id, j.service, r.state, r.started_at, r.finished_at, r.error
		FROM runs r JOIN jobs j ON j.id = r.job_id WHERE r.id = ?`, runID)

	r := Run{ID: runID}
	var state string
	var started string
	var finished sql.NullString
	var errorText sql.NullString
	if err := row.Scan(&r.ID, &r.JobID, &r.Service, &state, &started, &finished, &errorText); err != nil {
		return nil, fmt.Errorf("loading run %d: %w", runID, err)
	}
	r.Error = errorText.String
	r.State = State(state)
	if !valid(r.State) {
		return nil, fmt.Errorf("loading run %d: unrecognized stored state %q", runID, state)
	}
	startT, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return nil, fmt.Errorf("loading run %d: parsing started_at: %w", runID, err)
	}
	r.StartedAt = startT
	if finished.Valid {
		f, perr := time.Parse(time.RFC3339Nano, finished.String)
		if perr != nil {
			return nil, fmt.Errorf("loading run %d: parsing finished_at: %w", runID, perr)
		}
		r.FinishedAt = &f
	}
	return &Machine{store: db, run: r}, nil
}

// Run returns a snapshot of the machine's current run.
func (m *Machine) Run() Run { return m.run }

// Transition moves the run from its current state to to, when legal,
// persisting state, finished_at and the transition record. detail is
// the structured log line explaining the transition (e.g. the command
// that failed). detail must be non-empty for FAILED and CANCELLED so
// every bad outcome carries a reason.
//
// detail is persisted verbatim in the DB: never pass command output
// that may contain secrets through it — pass a redacted summary (redaction
// is a caller obligation; see spec §11).
//
// CALLER CLEANUP OBLIGATION (spec §4.1): CANCELLED is reachable from
// any state, and any state that stops containers or creates throwaway
// containers MUST guarantee their cleanup via defer before a
// transition can reach a cancellable state — the machine records the
// transition but does not perform cleanup itself.
func (m *Machine) Transition(to State, detail string) error {
	if !valid(to) {
		return unrecognizedErr(to)
	}
	from := m.run.State
	if !CanTransition(from, to) {
		return fmt.Errorf("illegal transition %s -> %s for job %s run %d", from, to, m.run.Service, m.run.ID)
	}
	if (to == FAILED || to == CANCELLED) && detail == "" {
		return fmt.Errorf("transition to %s requires a detail (reason)", to)
	}

	if err := m.record(from, to, detail); err != nil {
		return err
	}
	m.run.State = to
	return nil
}

// record persists a transition atomically using a fresh transaction.
func (m *Machine) record(from, to State, detail string) (err error) {
	tx, err := m.store.Begin()
	if err != nil {
		return fmt.Errorf("recording transition %s->%s for service %s (run %d): %w", from, to, m.run.Service, m.run.ID, err)
	}
	defer func() { err = store.RollbackOnError(tx, err) }()
	now := time.Now().UTC()
	if err = m.recordTx(tx, from, to, detail, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("recording transition %s->%s for service %s (run %d): %w", from, to, m.run.Service, m.run.ID, err)
	}
	m.applySnapshot(to, now, detail)
	return nil
}

// recordTx is the transactional core of record: it updates the runs
// row and inserts the transition row with duration. It is used both by
// record (inside its own transaction) and by StartRun (inside the
// run-creation transaction) so a run is never persisted without its
// initial history row (spec §4.1).
func (m *Machine) recordTx(tx *sql.Tx, from, to State, detail string, now time.Time) error {
	ts := now.Format(time.RFC3339Nano)

	// Duration is the time spent in the previous state; read the last
	// transition's timestamp first.
	var prevAt string
	if err := tx.QueryRow(
		`SELECT at FROM run_transitions WHERE run_id = ? ORDER BY id DESC LIMIT 1`,
		m.run.ID,
	).Scan(&prevAt); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("recording transition %s->%s for service %s (run %d): reading previous transition: %w", from, to, m.run.Service, m.run.ID, err)
	}
	durationMs := int64(0)
	if prevAt != "" {
		prevt, perr := time.Parse(time.RFC3339Nano, prevAt)
		if perr != nil {
			return fmt.Errorf("recording transition %s->%s for service %s (run %d): parsing previous transition time %q: %w", from, to, m.run.Service, m.run.ID, prevAt, perr)
		}
		durationMs = now.Sub(prevt).Milliseconds()
		if durationMs < 0 {
			durationMs = 0
		}
	}

	// Guard is structural: the UPDATE only applies if the DB still
	// holds the state this machine believes; zero rows means stale.
	res, err := tx.Exec(
		`UPDATE runs SET state = ?, finished_at = ?, error = ? WHERE id = ? AND state = ?`,
		string(to),
		finishedAtValue(to, now),
		errorIfBad(to, detail),
		m.run.ID,
		string(from),
	)
	if err != nil {
		return fmt.Errorf("recording transition %s->%s for service %s (run %d): %w", from, to, m.run.Service, m.run.ID, err)
	}
	if affected, aerr := res.RowsAffected(); aerr == nil && affected == 0 {
		return fmt.Errorf("recording transition for service %s (run %d): database state is not %s (stale machine?)", m.run.Service, m.run.ID, from)
	}
	if _, err = tx.Exec(
		`INSERT INTO run_transitions (run_id, from_state, to_state, at, duration_ms, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		m.run.ID, string(from), string(to), ts, durationMs, detail,
	); err != nil {
		return fmt.Errorf("recording transition %s->%s for service %s (run %d): %w", from, to, m.run.Service, m.run.ID, err)
	}
	return nil
}

// applySnapshot updates the in-memory run only after the transaction
// that persisted the transition has committed successfully.
func (m *Machine) applySnapshot(to State, now time.Time, detail string) {
	if to.Terminal() {
		finished := now
		m.run.FinishedAt = &finished
	}
	if to == FAILED {
		m.run.Error = detail
	}
}

// finishedAtValue returns now's timestamp when to is terminal (the run
// finished), else nil to leave finished_at NULL.
func finishedAtValue(to State, now time.Time) any {
	if to.Terminal() {
		return now.Format(time.RFC3339Nano)
	}
	return nil
}

// errorIfBad returns detail as the persisted run error when to is a
// bad outcome; otherwise the run error stays empty.
func errorIfBad(to State, detail string) any {
	if to == FAILED {
		return detail
	}
	return nil
}

// upsertJob returns the id of the job for service, creating it when
// this is the service's first run. Uses INSERT OR IGNORE + re-query so
// a concurrent first run cannot trip the UNIQUE constraint.
func upsertJob(db *sql.DB, service string) (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM jobs WHERE service = ?`, service).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("looking up job for %s: %w", service, err)
	}
	if _, err = db.Exec(
		`INSERT OR IGNORE INTO jobs (service, created_at) VALUES (?, ?)`,
		service, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return 0, fmt.Errorf("creating job for %s: %w", service, err)
	}
	err = db.QueryRow(`SELECT id FROM jobs WHERE service = ?`, service).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("looking up just-created job for %s: %w", service, err)
	}
	return id, nil
}

// unrecognizedErr is a lowercase wrapper of State's parse error used by
// Transition to keep error text consistent.
func unrecognizedErr(s State) error {
	return fmt.Errorf("unrecognized job state %q", string(s))
}
