package engine

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/store"
)

// VerificationRecord is one checker's result for a run's VERIFYING
// phase (spec §7: verification results are first-class records).
// internal/verify.Result is deliberately not reused here so
// internal/engine never needs to import internal/verify (the
// dependency runs the other way, same pattern as engine.Notifier):
// the pipeline package translates between the two.
type VerificationRecord struct {
	// Target names the destination this result belongs to; empty means
	// the run as a whole (a restore drill runs once per artifact, not
	// once per destination).
	Target string
	// Method is the verification method, e.g. "archive-integrity",
	// "db-restore-drill", "restic-read-back", "restart-smoke".
	Method string
	// Passed is the verdict. Anything that is not Passed and not
	// Warning-only fails the VERIFYING phase.
	Passed bool
	// Warning is a non-fatal notice worth surfacing; empty means none.
	Warning string
	// Detail is a plain-English summary of what was checked or why it
	// failed. Must never contain secrets (caller obligation, like
	// Machine.Transition's detail).
	Detail string
	// Output is the (already-redacted) captured output the verdict
	// rests on.
	Output string
	// Duration is the check's wall time.
	Duration time.Duration
	// RecordedAt is when the check completed. The zero value is
	// replaced with time.Now().UTC() by RecordVerification.
	RecordedAt time.Time
}

// SetSnapshotID records the restic snapshot ID this run produced. It
// may be called any time after StartRun; typically right after a
// successful upload (UPLOADING).
func (m *Machine) SetSnapshotID(id string) error {
	if _, err := m.store.Exec(`UPDATE runs SET snapshot_id = ? WHERE id = ?`, id, m.run.ID); err != nil {
		return fmt.Errorf("recording snapshot ID for %s run %d: %w", m.run.Service, m.run.ID, err)
	}
	m.run.SnapshotID = id
	return nil
}

// SetWaiting records what the run is waiting for right now (a plain
// English note such as "waiting for repository lock on b2 (held by
// immich)"), or clears it when note is empty, so a run that is stalled
// on something outside its control says why while it is in progress.
// note must already be redacted. Unlike the rest of Machine, SetWaiting
// touches only the database row, so it may be called from the
// goroutines that upload and verify a run's destinations in parallel.
func (m *Machine) SetWaiting(note string) error {
	var v any
	if note != "" {
		v = note
	}
	if _, err := m.store.Exec(`UPDATE runs SET waiting = ? WHERE id = ?`, v, m.run.ID); err != nil {
		return fmt.Errorf("recording what %s run %d is waiting for: %w", m.run.Service, m.run.ID, err)
	}
	return nil
}

// RecordVerification persists one verification check result for this
// run (spec §7: verification results are first-class records, shown
// in the GUI and `ballast status --verify`). Detail and Output are
// persisted verbatim: the caller must redact them first, exactly like
// Transition's detail parameter.
func (m *Machine) RecordVerification(r VerificationRecord) error {
	at := r.RecordedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := m.store.Exec(`
		INSERT INTO verification_results (run_id, method, passed, warning, detail, output, duration_ms, recorded_at, target)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.run.ID, r.Method, boolToInt(r.Passed), r.Warning, r.Detail, r.Output, r.Duration.Milliseconds(), at.Format(time.RFC3339Nano), r.Target,
	)
	if err != nil {
		return fmt.Errorf("recording verification result %q for %s run %d: %w", r.Method, m.run.Service, m.run.ID, err)
	}
	return nil
}

// boolToInt renders a bool as a SQLite integer 0/1.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// VerificationResults returns every verification record for runID, in
// the order they were recorded.
func VerificationResults(db *sql.DB, runID int64) ([]VerificationRecord, error) {
	rows, err := db.Query(`
		SELECT method, passed, warning, detail, output, duration_ms, recorded_at, target
		FROM verification_results WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing verification results for run %d: %w", runID, err)
	}
	defer rows.Close()

	var out []VerificationRecord
	for rows.Next() {
		var r VerificationRecord
		var passed int
		var durationMs int64
		var recordedAt string
		if err := rows.Scan(&r.Method, &passed, &r.Warning, &r.Detail, &r.Output, &durationMs, &recordedAt, &r.Target); err != nil {
			return nil, fmt.Errorf("scanning verification result for run %d: %w", runID, err)
		}
		r.Passed = passed != 0
		r.Duration = time.Duration(durationMs) * time.Millisecond
		t, err := time.Parse(time.RFC3339Nano, recordedAt)
		if err != nil {
			return nil, fmt.Errorf("parsing verification result time for run %d: %w", runID, err)
		}
		r.RecordedAt = t
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing verification results for run %d: %w", runID, err)
	}
	return out, nil
}

// RecentRuns returns the most recent runs for service, newest first,
// up to limit (limit <= 0 defaults to 10). Used by the status API/CLI
// to show run history without loading every run ever made.
func RecentRuns(db *sql.DB, service string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.Query(`
		SELECT r.id, r.job_id, r.state, r.started_at, r.finished_at, r.error, r.snapshot_id
		FROM runs r JOIN jobs j ON j.id = r.job_id
		WHERE j.service = ? AND `+store.RunsAfterFloorSQL+`
		ORDER BY r.id DESC LIMIT ?`, service, limit)
	if err != nil {
		return nil, fmt.Errorf("listing recent runs for %s: %w", service, err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		var state, started string
		var finished, errorText, snapshotID sql.NullString
		if err := rows.Scan(&r.ID, &r.JobID, &state, &started, &finished, &errorText, &snapshotID); err != nil {
			return nil, fmt.Errorf("scanning recent run for %s: %w", service, err)
		}
		r.Service = service
		r.State = State(state)
		r.Error = errorText.String
		r.SnapshotID = snapshotID.String
		t, err := time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, fmt.Errorf("parsing started_at for %s run %d: %w", service, r.ID, err)
		}
		r.StartedAt = t
		if finished.Valid {
			f, err := time.Parse(time.RFC3339Nano, finished.String)
			if err != nil {
				return nil, fmt.Errorf("parsing finished_at for %s run %d: %w", service, r.ID, err)
			}
			r.FinishedAt = &f
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing recent runs for %s: %w", service, err)
	}
	return out, nil
}

// RequireVerifications declares the verification methods this run's
// catalog entry asked for (spec §17.6). Once set, Transition refuses
// SUCCESS unless every one of them has at least one recorded result
// for this run and all of that method's results passed — so a method
// that was skipped, never implemented, or silently dropped can never
// turn a run green. Calling it again replaces the list.
func (m *Machine) RequireVerifications(methods ...string) {
	m.required = append([]string(nil), methods...)
}

// checkRequired enforces RequireVerifications against the persisted
// results of this run.
func (m *Machine) checkRequired() error {
	if len(m.required) == 0 {
		return nil
	}
	recs, err := VerificationResults(m.store, m.run.ID)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(recs))
	failed := make(map[string]bool)
	for _, r := range recs {
		seen[r.Method] = true
		if !r.Passed {
			failed[r.Method] = true
		}
	}
	for _, method := range m.required {
		if !seen[method] {
			return fmt.Errorf("cannot mark %s run %d as verified: verification method %q never ran", m.run.Service, m.run.ID, method)
		}
		if failed[method] {
			return fmt.Errorf("cannot mark %s run %d as verified: verification method %q did not pass", m.run.Service, m.run.ID, method)
		}
	}
	return nil
}

// TargetResult is one destination's outcome within a run (spec §19.4):
// each target uploads, verifies and reports on its own, so one target's
// failure never hides another's result.
type TargetResult struct {
	// Destination names the destination.
	Destination string
	// State is SUCCESS, SUCCESS_UNVERIFIED or FAILED for this target.
	State State
	// SnapshotID is the restic snapshot this target received; empty
	// when its upload failed.
	SnapshotID string
	// Detail is a plain-English summary (the failure reason, if any).
	// Must not contain secrets.
	Detail string
	// Secondary marks a report-only second copy (spec §12): its outcome
	// is shown and alerted on its own and never changes the run's state.
	Secondary bool
}

// RecordTarget stores one destination's outcome for this run, replacing
// an earlier record for the same destination.
func (m *Machine) RecordTarget(t TargetResult) error {
	_, err := m.store.Exec(`
		INSERT INTO run_targets (run_id, destination, state, snapshot_id, detail, tier)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, destination) DO UPDATE SET state = excluded.state, snapshot_id = excluded.snapshot_id, detail = excluded.detail, tier = excluded.tier`,
		m.run.ID, t.Destination, string(t.State), t.SnapshotID, t.Detail, tierName(t.Secondary))
	if err != nil {
		return fmt.Errorf("recording target %s for %s run %d: %w", t.Destination, m.run.Service, m.run.ID, err)
	}
	return nil
}

// RunTargets returns every destination outcome recorded for runID, in
// the order they were recorded.
func RunTargets(db *sql.DB, runID int64) ([]TargetResult, error) {
	rows, err := db.Query(`SELECT destination, state, snapshot_id, detail, tier FROM run_targets WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing targets of run %d: %w", runID, err)
	}
	defer rows.Close()
	var out []TargetResult
	for rows.Next() {
		var t TargetResult
		var state, tier string
		if err := rows.Scan(&t.Destination, &state, &t.SnapshotID, &t.Detail, &tier); err != nil {
			return nil, fmt.Errorf("scanning target of run %d: %w", runID, err)
		}
		t.State = State(state)
		t.Secondary = tier == "secondary"
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing targets of run %d: %w", runID, err)
	}
	return out, nil
}

func tierName(secondary bool) string {
	if secondary {
		return "secondary"
	}
	return "primary"
}
