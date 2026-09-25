package engine

import (
	"database/sql"
	"fmt"
	"time"
)

// VerificationRecord is one checker's result for a run's VERIFYING
// phase (spec §7: verification results are first-class records).
// internal/verify.Result is deliberately not reused here so
// internal/engine never needs to import internal/verify (the
// dependency runs the other way, same pattern as engine.Notifier):
// the pipeline package translates between the two.
type VerificationRecord struct {
	// Method is the verification method, e.g. "archive-integrity",
	// "container-restore", "restic-read-back".
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
		INSERT INTO verification_results (run_id, method, passed, warning, detail, output, duration_ms, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.run.ID, r.Method, boolToInt(r.Passed), r.Warning, r.Detail, r.Output, r.Duration.Milliseconds(), at.Format(time.RFC3339Nano),
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
		SELECT method, passed, warning, detail, output, duration_ms, recorded_at
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
		if err := rows.Scan(&r.Method, &passed, &r.Warning, &r.Detail, &r.Output, &durationMs, &recordedAt); err != nil {
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
		WHERE j.service = ?
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
