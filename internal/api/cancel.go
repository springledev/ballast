package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
)

// ErrNothingRunning reports that a stop was asked for a service that
// has no backup running right now (409).
var ErrNothingRunning = errors.New("no backup of this service is running right now, so there is nothing to stop")

// ErrNotCancellable reports a run that cannot be stopped from Ballast
// (409): Ballast's own backup, which is short and finishes on its own.
var ErrNotCancellable = errors.New("Ballast's own backup cannot be stopped; it is short and finishes on its own")

// ErrAlreadyFinished reports that the run asked to stop had already
// finished (409). A run that has finished may still be tidying up old
// backups; that is never interrupted.
var ErrAlreadyFinished = errors.New("this backup has already finished, so there is nothing to stop (it may still be tidying up old backups, which is never interrupted)")

// CancelResult is what a stop request did.
type CancelResult struct {
	// RunID is the run asked to stop.
	RunID int64 `json:"run_id"`
	// Service is the service the run belongs to.
	Service string `json:"service"`
	// AlreadyRequested is true when a stop had already been asked for;
	// the request was a harmless repeat.
	AlreadyRequested bool `json:"already_requested"`
	// Message says what happens next, in plain English.
	Message string `json:"message"`
}

// CancelRun asks run id to stop. The request is recorded in the
// database, so it reaches the run whichever process is doing it, and
// the run then ends CANCELLED, never as a success: stopped containers
// are started again, the repository is left unlocked, and the last
// good backup is kept. Asking twice is harmless. It returns
// inventory.ErrNotFound for an unknown run, ErrAlreadyFinished when
// the run has finished, ErrNothingRunning when its process is gone, and
// ErrNotCancellable for Ballast's own backup.
func (o *Ops) CancelRun(ctx context.Context, id int64) (CancelResult, error) {
	run, err := engine.LoadVisibleRun(o.DB, id)
	if err != nil {
		if isRunNotFound(err) {
			return CancelResult{}, fmt.Errorf("run %d: %w", id, inventory.ErrNotFound)
		}
		return CancelResult{}, o.redactErr(err)
	}
	return o.cancel(ctx, run)
}

// CancelService asks service's running backup to stop, like CancelRun.
// It returns ErrNothingRunning when no backup of it is running.
func (o *Ops) CancelService(ctx context.Context, service string) (CancelResult, error) {
	if service == SelfBackupService {
		return CancelResult{}, fmt.Errorf("%s: %w", service, ErrNotCancellable)
	}
	if _, err := inventory.GetService(o.DB, service); err != nil {
		return CancelResult{}, wrapf("service "+service, err)
	}
	last, err := engine.LastStartedRun(o.DB, service)
	if err != nil {
		return CancelResult{}, o.redactErr(fmt.Errorf("service %s: %w", service, err))
	}
	if !last.InProgress {
		return CancelResult{}, fmt.Errorf("service %s: %w", service, ErrNothingRunning)
	}
	run, err := engine.LoadVisibleRun(o.DB, last.RunID)
	if err != nil {
		return CancelResult{}, o.redactErr(fmt.Errorf("service %s: %w", service, err))
	}
	return o.cancel(ctx, run)
}

// cancel records the stop request for run and, when this process is
// running it, stops it at once.
func (o *Ops) cancel(ctx context.Context, run engine.Run) (CancelResult, error) {
	service := run.Service
	if service == SelfBackupService {
		return CancelResult{}, fmt.Errorf("%s: %w", service, ErrNotCancellable)
	}
	if run.FinishedAt != nil {
		return CancelResult{}, fmt.Errorf("service %s run %d: %w", service, run.ID, ErrAlreadyFinished)
	}
	// A run left unfinished by a process that died has nobody to stop
	// it; it is marked failed the next time the service is backed up.
	if o.Coord != nil {
		busy, err := o.Coord.IsRunning(ctx, service)
		if err != nil {
			return CancelResult{}, o.redactErr(fmt.Errorf("service %s: %w", service, err))
		}
		if !busy {
			return CancelResult{}, fmt.Errorf("service %s: %w (its last run did not finish because Ballast stopped while it was running; it will be marked failed at the next backup)", service, ErrNothingRunning)
		}
	}
	req, err := engine.RequestCancel(o.DB, run.ID)
	switch {
	case errors.Is(err, engine.ErrRunFinished):
		return CancelResult{}, fmt.Errorf("service %s run %d: %w", service, run.ID, ErrAlreadyFinished)
	case errors.Is(err, engine.ErrRunNotFound):
		return CancelResult{}, fmt.Errorf("run %d: %w", run.ID, inventory.ErrNotFound)
	case err != nil:
		return CancelResult{}, o.redactErr(err)
	}
	if o.Coord != nil {
		o.Coord.Cancel(service)
	}
	res := CancelResult{RunID: run.ID, Service: service, AlreadyRequested: req.AlreadyRequested,
		Message: "Stopping the backup of " + service + ". Anything it stopped is started again, and the last good backup is kept."}
	if req.AlreadyRequested {
		res.Message = "The backup of " + service + " is already stopping. The last good backup is kept."
	}
	return res, nil
}
