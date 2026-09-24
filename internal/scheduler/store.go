package scheduler

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SetSchedule upserts and enables the cron expression for service.
// The expression must parse (spec: invalid schedules must be rejected
// loudly, not silently skipped later by run-due).
//
// A schedule that has never fired is due straight away, so the next
// `ballast run-due` sweep backs the service up once right away, and
// the cron expression takes over from then on. A new service gets a
// verified backup without waiting for its first scheduled time.
// Replacing an existing schedule keeps its last firing, so it does not
// trigger an extra run.
func SetSchedule(db *sql.DB, service, cronExpr string) error {
	if service == "" {
		return fmt.Errorf("setting schedule: service must not be empty")
	}
	if _, err := ParseCron(cronExpr); err != nil {
		return fmt.Errorf("setting schedule for %s: %w", service, err)
	}
	_, err := db.Exec(`
		INSERT INTO schedules (service, cron_expr, enabled, updated_at) VALUES (?, ?, 1, ?)
		ON CONFLICT(service) DO UPDATE SET cron_expr = excluded.cron_expr, enabled = 1, updated_at = excluded.updated_at`,
		service, cronExpr, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("setting schedule for %s: %w", service, err)
	}
	return nil
}

// SetEnabled toggles whether service's schedule is swept by run-due,
// without discarding the configured cron expression. It errors if no
// schedule has been configured for service yet.
func SetEnabled(db *sql.DB, service string, enabled bool) error {
	res, err := db.Exec(
		`UPDATE schedules SET enabled = ?, updated_at = ? WHERE service = ?`,
		boolToInt(enabled), time.Now().UTC().Format(time.RFC3339Nano), service,
	)
	if err != nil {
		return fmt.Errorf("setting schedule enabled=%v for %s: %w", enabled, service, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("setting schedule enabled=%v for %s: no schedule configured", enabled, service)
	}
	return nil
}

// ListSchedules returns every configured schedule, ordered by service
// name so sweeps and their output are deterministic.
func ListSchedules(db *sql.DB) ([]ScheduledJob, error) {
	rows, err := db.Query(`SELECT service, cron_expr, enabled FROM schedules ORDER BY service`)
	if err != nil {
		return nil, fmt.Errorf("listing schedules: %w", err)
	}
	defer rows.Close()

	var jobs []ScheduledJob
	for rows.Next() {
		var j ScheduledJob
		var enabled int
		if err := rows.Scan(&j.Service, &j.Cron, &enabled); err != nil {
			return nil, fmt.Errorf("listing schedules: %w", err)
		}
		j.Enabled = enabled != 0
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing schedules: %w", err)
	}
	return jobs, nil
}

// LastFired returns when the scheduler last acted on a firing of
// service's schedule, or the zero Time if it never has (including
// when no schedule is configured).
func LastFired(db *sql.DB, service string) (time.Time, error) {
	var fired sql.NullString
	err := db.QueryRow(`SELECT last_fired_at FROM schedules WHERE service = ?`, service).Scan(&fired)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !fired.Valid) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("looking up last scheduled firing for %s: %w", service, err)
	}
	t, err := time.Parse(time.RFC3339Nano, fired.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing last scheduled firing for %s: %w", service, err)
	}
	return t, nil
}

// MarkFired records that the scheduler acted on a firing of service's
// schedule at at. A zero at clears the record, as if the schedule had
// never fired. It errors if no schedule is configured for service.
func MarkFired(db *sql.DB, service string, at time.Time) error {
	var value any // NULL for a zero at
	if !at.IsZero() {
		value = at.UTC().Format(time.RFC3339Nano)
	}
	res, err := db.Exec(`UPDATE schedules SET last_fired_at = ? WHERE service = ?`, value, service)
	if err != nil {
		return fmt.Errorf("recording scheduled firing for %s: %w", service, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("recording scheduled firing for %s: no schedule configured", service)
	}
	return nil
}

// GenerateTriggerToken creates a new random external-trigger token
// for service and persists only its SHA-256 hash (spec rule 2:
// secrets never appear in the DB, logs, or API responses). The
// returned token is shown to the caller exactly once — Ballast cannot
// recover it later, only reissue a new one.
func GenerateTriggerToken(db *sql.DB, service string) (string, error) {
	if service == "" {
		return "", fmt.Errorf("generating trigger token: service must not be empty")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating trigger token for %s: %w", service, err)
	}
	token := hex.EncodeToString(raw)

	_, err := db.Exec(`
		INSERT INTO trigger_tokens (service, token_hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT(service) DO UPDATE SET token_hash = excluded.token_hash, created_at = excluded.created_at`,
		service, hashToken(token), time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return "", fmt.Errorf("storing trigger token for %s: %w", service, err)
	}
	return token, nil
}

// VerifyTriggerToken reports whether token is the current
// externally-supplied trigger token for service. The stored hash is
// compared in constant time; a missing token or missing service both
// verify false rather than erroring, so callers cannot distinguish
// "wrong token" from "no token configured" by error type alone. A
// service with no token still goes through the same hash-and-compare
// work (against noTokenHash), so response time does not reveal which
// services have a token either.
func VerifyTriggerToken(db *sql.DB, service, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var stored string
	err := db.QueryRow(`SELECT token_hash FROM trigger_tokens WHERE service = ?`, service).Scan(&stored)
	found := err == nil
	switch {
	case errors.Is(err, sql.ErrNoRows):
		stored = noTokenHash
	case err != nil:
		return false, fmt.Errorf("looking up trigger token for %s: %w", service, err)
	}
	given := hashToken(token)
	match := subtle.ConstantTimeCompare([]byte(stored), []byte(given)) == 1
	return found && match, nil
}

// noTokenHash stands in for the stored hash when a service has no
// trigger token. It has the length of a real hash but is not hex, so
// no token can hash to it; VerifyTriggerToken also refuses a missing
// row regardless of the comparison.
var noTokenHash = strings.Repeat("-", sha256.Size*2)

// hashToken returns the hex-encoded SHA-256 hash of token. Only the
// hash is ever persisted; the raw token is never stored or logged.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
