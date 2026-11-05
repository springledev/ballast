package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/store"
)

// ErrCancelledByUser is the cause carried by a run's context when
// someone asked for the backup to stop (API, CLI or web UI), as
// opposed to Ballast itself shutting down. Read it with context.Cause.
var ErrCancelledByUser = errors.New("the backup was stopped on request")

// ErrCancelRequested is returned by Transition when a run that was
// asked to stop tries to finish as SUCCESS or SUCCESS_UNVERIFIED: a
// cancelled run is never reported as a good backup (rule 1). The owner
// then ends the run as CANCELLED.
var ErrCancelRequested = errors.New("this backup was asked to stop, so it cannot be marked as finished successfully")

// ErrRunFinished is returned by RequestCancel when the run has already
// reached a final state, so there is nothing left to stop.
var ErrRunFinished = errors.New("this backup has already finished")

// Cancellation details recorded on a CANCELLED run, in plain English.
const (
	// CancelDetailRequested says a person asked for the backup to stop.
	CancelDetailRequested = "cancelled: stopped on request"
	// CancelDetailShutdown says Ballast was shutting down.
	CancelDetailShutdown = "cancelled: Ballast is shutting down"
)

// CancelRequest is what RequestCancel did.
type CancelRequest struct {
	// RunID is the run that was asked to stop.
	RunID int64
	// Service is the service the run belongs to.
	Service string
	// AlreadyRequested is true when a stop had already been asked for;
	// the request is then a harmless repeat.
	AlreadyRequested bool
}

// RequestCancel asks the unfinished run runID to stop, by recording
// the request in the database. Whatever process owns the run (the
// daemon, `ballast backup`, `ballast run-due`) notices it within a
// second and stops the run, which then ends CANCELLED. It returns
// ErrRunNotFound for an unknown run and ErrRunFinished when the run
// has already finished. Asking twice is harmless and reported with
// AlreadyRequested.
func RequestCancel(db *sql.DB, runID int64) (_ CancelRequest, err error) {
	tx, err := db.Begin()
	if err != nil {
		return CancelRequest{}, fmt.Errorf("asking run %d to stop: %w", runID, err)
	}
	defer func() { err = store.RollbackOnError(tx, err) }()

	var (
		service   string
		finished  sql.NullString
		requested sql.NullString
	)
	err = tx.QueryRow(`SELECT j.service, r.finished_at, r.cancel_requested_at
		FROM runs r JOIN jobs j ON j.id = r.job_id WHERE r.id = ?`, runID).Scan(&service, &finished, &requested)
	if errors.Is(err, sql.ErrNoRows) {
		return CancelRequest{}, fmt.Errorf("run %d: %w", runID, ErrRunNotFound)
	}
	if err != nil {
		return CancelRequest{}, fmt.Errorf("asking run %d to stop: %w", runID, err)
	}
	out := CancelRequest{RunID: runID, Service: service, AlreadyRequested: requested.Valid}
	if finished.Valid {
		return CancelRequest{}, fmt.Errorf("service %s run %d: %w", service, runID, ErrRunFinished)
	}
	// The guard on finished_at makes this race-free with the run's own
	// final transition: either the stop is recorded while the run is
	// unfinished (and the run cannot then finish as a success), or the
	// run finished first and nothing is changed.
	res, err := tx.Exec(`UPDATE runs SET cancel_requested_at = COALESCE(cancel_requested_at, ?)
		WHERE id = ? AND finished_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), runID)
	if err != nil {
		return CancelRequest{}, fmt.Errorf("service %s: asking run %d to stop: %w", service, runID, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return CancelRequest{}, fmt.Errorf("service %s run %d: %w", service, runID, ErrRunFinished)
	}
	if err = tx.Commit(); err != nil {
		return CancelRequest{}, fmt.Errorf("service %s: asking run %d to stop: %w", service, runID, err)
	}
	return out, nil
}

// CancelRequested reports whether service's unfinished run has been
// asked to stop. It is one cheap query, safe to poll every second.
func CancelRequested(db *sql.DB, service string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM runs r JOIN jobs j ON j.id = r.job_id
		WHERE j.service = ? AND r.finished_at IS NULL AND r.cancel_requested_at IS NOT NULL
		LIMIT 1`, service).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking whether %s was asked to stop: %w", service, err)
	}
	return true, nil
}

// StoppedOnRequest reports whether ctx was cancelled because someone
// asked the backup to stop, rather than because Ballast is shutting
// down.
func StoppedOnRequest(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrCancelledByUser)
}

// CancelDetail returns the plain-English reason to record on a run
// that ends CANCELLED: whether it was stopped on request or because
// Ballast is shutting down, as told by ctx's cause. cause is the error
// that ended the run, which says "stopped on request" too when the run
// was refused a successful finish (ErrCancelRequested).
func CancelDetail(ctx context.Context, cause error) string {
	if StoppedOnRequest(ctx) || errors.Is(cause, ErrCancelRequested) {
		return CancelDetailRequested
	}
	return CancelDetailShutdown
}

// Cancelled reports whether a run that ended with cause must be
// recorded as CANCELLED rather than FAILED: its context was cancelled,
// or it was refused a successful finish because a stop was asked for.
func Cancelled(ctx context.Context, cause error) bool {
	return ctx.Err() != nil || errors.Is(cause, ErrCancelRequested)
}
