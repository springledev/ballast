// Package scheduler decides which configured backup jobs are due to
// run and authenticates externally triggered runs (spec §9, §14 step
// 9; the trigger side is also the Phase-2 reserved interface of
// spec §12).
//
// This package never runs Docker, restic, or any backup strategy
// itself: callers supply a Runner that actually executes a job by
// service name, so due-job selection, run locking, and trigger-token
// auth are fully unit-testable with a stub Runner, independent of the
// real execution pipeline (spec §12: v0.1 ships and tests the trigger
// contract with a stub). Every run is started through a Coordinator.
package scheduler

import (
	"context"

	"github.com/springledev/ballast/internal/engine"
)

// Runner executes one backup job by service name and returns its final state.
//
// Run must return the final State of the run, along with any error that
// occurred. A nil error is only valid with State == SUCCESS. If State is
// SUCCESS_UNVERIFIED (amber) or any non-terminal state, Run must return
// a non-nil error. This is enforced structurally by the Coordinator:
// returning SUCCESS_UNVERIFIED with a nil error will be caught and an
// appropriate error will be returned to the caller. Run must also honour
// ctx: it is cancelled when Ballast shuts down, and cleanup such as
// restarting stopped containers depends on it.
type Runner interface {
	Run(ctx context.Context, service string) (engine.State, error)
}

// RunnerFunc adapts a plain function to Runner.
type RunnerFunc func(ctx context.Context, service string) (engine.State, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, service string) (engine.State, error) {
	return f(ctx, service)
}

// ScheduledJob is one configured recurring backup job.
type ScheduledJob struct {
	Service string
	Cron    string
	Enabled bool
}
