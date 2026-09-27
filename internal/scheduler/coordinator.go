package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/secret"
)

// ErrAlreadyRunning reports that a service's run lock is held by a
// live run, so no second run was started.
var ErrAlreadyRunning = errors.New("a backup run for this service is already in progress")

// ErrNotVerified reports that a run did not finish as a verified success
// (spec §4.1): it ended in SUCCESS_UNVERIFIED (amber), FAILED, CANCELLED,
// or any non-terminal state. `ballast run-due` and `ballast trigger` must
// exit non-zero when wrapped in this error, never silently report as green.
var ErrNotVerified = errors.New("the backup did not finish as a verified success")

// ErrInvalidTriggerToken reports that Trigger was called with a
// token that does not match (or a service with no token configured
// at all). Callers can map it to an HTTP 401 with errors.Is instead
// of matching error text.
var ErrInvalidTriggerToken = errors.New("invalid or missing trigger token")

// ErrPreviousRunInterrupted reports that RunNow or Trigger found the
// service's previous run unfinished with no process still working on
// it (Ballast was stopped, killed, or the machine rebooted) and marked
// it FAILED. The new run still went ahead; its own outcome, if it
// failed too, is joined into the same error. Callers can tell a
// successful run that surfaced an old failure apart from a failed run
// by checking the result for this error alone.
var ErrPreviousRunInterrupted = errors.New("the previous run was interrupted before it finished and has been marked failed")

// checkRunSuccess checks whether a run finished with SUCCESS: if err is
// non-nil it returns it, if state is not SUCCESS it returns an error
// wrapping ErrNotVerified with the service name and state, otherwise it
// returns nil. This is what Runner.Run errors are checked through.
func checkRunSuccess(service string, state engine.State, err error) error {
	if err != nil {
		return err
	}
	if state != engine.SUCCESS {
		return fmt.Errorf("%s: run finished with state %s: %w", service, state, ErrNotVerified)
	}
	return nil
}

// DefaultLockWait is how long RunNow and Trigger wait for a service's
// run lock when Coordinator.LockWait is zero. A sweep holds the lock of
// a service that is not running only for the moment it takes to clean
// up after an interrupted run, so a short wait keeps a one-shot trigger
// (e.g. a udev rule firing when a drive is plugged in) from being
// turned away by that, while a genuinely running backup is still
// refused promptly.
const DefaultLockWait = 10 * time.Second

// lockPollInterval is how often a waiting RunNow retries the run lock.
const lockPollInterval = 100 * time.Millisecond

// Coordinator is the single place backup runs are started from: the
// schedule sweep (RunDue), external triggers (Trigger), and on-demand
// runs (RunNow). Every run happens while holding the service's run
// lock, which gives two guarantees across all processes on the host:
//
//   - A service never runs twice at once. Whoever takes the lock
//     first runs; everyone else skips (RunDue) or, after waiting up to
//     LockWait, gets ErrAlreadyRunning (RunNow, Trigger).
//   - A run whose process died is noticed. Taking the lock while the
//     latest run is still unfinished proves its owner is gone, so the
//     run is marked FAILED as interrupted and reported, instead of
//     blocking the service forever.
//
// Code that starts runs any other way breaks both guarantees. A
// Coordinator must not be copied after first use.
type Coordinator struct {
	DB     *sql.DB
	Locks  Locker
	Runner Runner
	// Log receives one plain-English line per notable event: skipped
	// sweeps, interrupted runs, and failures. Nil means os.Stderr, so
	// problems are never silently dropped. Writes are serialized.
	Log io.Writer
	// MaxParallel is how many different services RunDue backs up at
	// the same time (config max_parallel_jobs). Values below 1 mean 1.
	MaxParallel int
	// LockWait is how long RunNow and Trigger keep retrying a service's
	// run lock before giving up with ErrAlreadyRunning. Zero means
	// DefaultLockWait; negative means do not wait. RunDue never waits:
	// a locked service is simply skipped until the next sweep.
	LockWait time.Duration
	// Notifier, when set, is attached to a run that withClaim finds
	// interrupted before it is failed out, so the interruption is
	// reported the same way any other job failure is (spec §11). Nil
	// (the default) means interrupted runs are still failed, just not
	// notified — step 11 wires a real Notifier in.
	Notifier engine.Notifier
	// Redact lists secret strings (passwords, keys, tokens) that must
	// never reach Log or the text of an error the Coordinator returns
	// (spec rule 2). Run errors are logged and returned verbatim, so
	// this is the last line of defence should a Runner wrap unredacted
	// restic or docker stderr into its error: every occurrence is
	// replaced with "[redacted]". Errors stay matchable with errors.Is
	// and errors.As; only their text changes. Nil redacts nothing.
	Redact *secret.Set

	logMu sync.Mutex
}

// sweepOutcome is what happened to one service during RunDue.
type sweepOutcome int

const (
	sweepOK         sweepOutcome = iota // nothing to do, skipped, or ran fine
	sweepFailed                         // reported; counts toward a non-zero exit
	sweepNotStarted                     // shutdown began before its turn
)

// errShuttingDown marks a due service that was claimed but not started
// because ctx was cancelled in the meantime.
var errShuttingDown = errors.New("not started: Ballast is shutting down")

// RunDue runs every enabled schedule that has fired since the
// scheduler last acted on it, as of now. Up to MaxParallel different
// services run at once, so one long backup does not delay the rest.
// The firing is recorded before the run starts, so a job whose run
// fails is retried at its next firing, not every minute.
//
// Failures are isolated per service (spec rule 3): an invalid
// schedule, a lock or database error, an interrupted earlier run, or a
// failed run is logged and the sweep carries on. The returned error
// names every service that failed, sorted, so `ballast run-due` exits
// non-zero without one bad job hiding another. A service whose run is
// still in progress elsewhere is skipped, not failed.
//
// When ctx is cancelled (Ballast is shutting down), no further
// services are started, and runs already going receive the
// cancellation so they can clean up (e.g. restart stopped containers).
// A run cut short this way has its firing un-recorded, so the next
// sweep after restart runs it again. Services never reached are named
// in the returned error.
func (c *Coordinator) RunDue(ctx context.Context, now time.Time) error {
	jobs, err := ListSchedules(c.DB)
	if err != nil {
		return c.redactErr(fmt.Errorf("running due jobs: %w", err))
	}

	var (
		mu         sync.Mutex
		failed     []string
		notStarted []string
		wg         sync.WaitGroup
		slots      = make(chan struct{}, max(c.MaxParallel, 1))
	)
	record := func(service string, o sweepOutcome) {
		mu.Lock()
		defer mu.Unlock()
		switch o {
		case sweepFailed:
			failed = append(failed, service)
		case sweepNotStarted:
			notStarted = append(notStarted, service)
		}
	}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		gotSlot := false
		if ctx.Err() == nil {
			select {
			case slots <- struct{}{}:
				gotSlot = true
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			if gotSlot {
				<-slots
			}
			record(j.Service, sweepNotStarted)
			continue
		}
		wg.Add(1)
		go func(j ScheduledJob) {
			defer wg.Done()
			defer func() { <-slots }()
			record(j.Service, c.sweepOne(ctx, j, now))
		}(j)
	}
	wg.Wait()

	var problems []string
	if len(failed) > 0 {
		slices.Sort(failed)
		problems = append(problems, fmt.Sprintf("%d job(s) failed: %s", len(failed), strings.Join(failed, ", ")))
	}
	if len(notStarted) > 0 {
		slices.Sort(notStarted)
		problems = append(problems, fmt.Sprintf("shutting down: %d job(s) not checked or started, left for the next sweep: %s", len(notStarted), strings.Join(notStarted, ", ")))
	}
	if len(problems) > 0 {
		return c.redactErr(errors.New(strings.Join(problems, "; ")))
	}
	return nil
}

// sweepOne handles one enabled schedule for RunDue.
func (c *Coordinator) sweepOne(ctx context.Context, j ScheduledJob, now time.Time) sweepOutcome {
	sched, err := ParseCron(j.Cron)
	if err != nil {
		c.logf(j.Service, "invalid schedule: %v", err)
		return sweepFailed
	}
	// Only take the run lock when there is something to do: the
	// schedule is due, or the latest run is unfinished and may have
	// been interrupted. Probing every idle service's lock every minute
	// would make a RunNow or Trigger that lands in that moment wait for
	// nothing. Both facts are re-checked under the lock below. If the
	// check itself fails, claim anyway: the same lookups under the lock
	// report the failure, and an interrupted run is still cleared.
	if needed, err := c.sweepNeedsClaim(j.Service, sched, now); err == nil && !needed {
		return sweepOK
	}
	interrupted, err := c.withClaim(ctx, j.Service, 0, func() error {
		last, err := LastFired(c.DB, j.Service)
		if err != nil {
			return err
		}
		if !sched.IsDue(now, last) {
			return nil
		}
		if ctx.Err() != nil {
			return errShuttingDown
		}
		if err := MarkFired(c.DB, j.Service, now); err != nil {
			return err
		}
		state, runErr := c.Runner.Run(ctx, j.Service)
		checkErr := checkRunSuccess(j.Service, state, runErr)
		if ctx.Err() != nil && checkErr != nil {
			// Cut short by shutdown, not a real failure of this
			// firing: un-record it so the next sweep runs it again.
			if err := MarkFired(c.DB, j.Service, last); err != nil {
				return errors.Join(checkErr, fmt.Errorf("could not re-arm the schedule, so it will next run at its following firing: %w", err))
			}
			return fmt.Errorf("run cut short because Ballast is shutting down; it will run again at the next sweep: %w", checkErr)
		}
		return checkErr
	})
	switch {
	case errors.Is(err, errShuttingDown):
		return sweepNotStarted
	case errors.Is(err, ErrAlreadyRunning):
		// Only worth a line when this sweep would have run it;
		// otherwise a long backup would log every minute.
		if last, lerr := LastFired(c.DB, j.Service); lerr != nil || sched.IsDue(now, last) {
			c.logf(j.Service, "previous run is still in progress; skipped this sweep")
		}
		return sweepOK
	case err != nil:
		c.logf(j.Service, "%v", err)
		return sweepFailed
	case interrupted:
		return sweepFailed
	}
	return sweepOK
}

// sweepNeedsClaim reports whether RunDue has anything to do for
// service at now: its schedule is due, or its latest run is unfinished
// (possibly interrupted, which only the lock holder can tell).
func (c *Coordinator) sweepNeedsClaim(service string, sched Schedule, now time.Time) (bool, error) {
	last, err := LastFired(c.DB, service)
	if err != nil {
		return false, err
	}
	if sched.IsDue(now, last) {
		return true, nil
	}
	latest, err := engine.LastStartedRun(c.DB, service)
	if err != nil {
		return false, err
	}
	return latest.InProgress, nil
}

// RunNow runs service immediately, regardless of its schedule, and
// without moving the schedule: the next scheduled firing still runs.
// If service is busy it waits up to LockWait for it to free up, then
// returns ErrAlreadyRunning (wrapped). If the previous run turns out
// to have been interrupted, it is marked failed, the new run still
// goes ahead, and the result includes ErrPreviousRunInterrupted, so
// the old failure is never reported as a clean success.
func (c *Coordinator) RunNow(ctx context.Context, service string) error {
	return c.redactErr(c.runNow(ctx, service))
}

// runNow is RunNow without redaction of the returned error.
func (c *Coordinator) runNow(ctx context.Context, service string) error {
	interrupted, err := c.withClaim(ctx, service, c.lockWait(), func() error {
		state, runErr := c.Runner.Run(ctx, service)
		return checkRunSuccess(service, state, runErr)
	})
	if errors.Is(err, ErrAlreadyRunning) {
		return fmt.Errorf("%s: %w; not starting another", service, err)
	}
	if interrupted {
		return errors.Join(fmt.Errorf("%s: %w (a new run went ahead anyway)", service, ErrPreviousRunInterrupted), err)
	}
	return err
}

func (c *Coordinator) lockWait() time.Duration {
	switch {
	case c.LockWait == 0:
		return DefaultLockWait
	case c.LockWait < 0:
		return 0
	}
	return c.LockWait
}

// Trigger authenticates an externally-triggered run (spec §12: the
// reserved `ballast trigger` interface) and, on success, runs service
// via RunNow.
func (c *Coordinator) Trigger(ctx context.Context, service, token string) error {
	ok, err := VerifyTriggerToken(c.DB, service, token)
	if err != nil {
		return c.redactErr(fmt.Errorf("triggering %s: %w", service, err))
	}
	if !ok {
		return c.redactErr(fmt.Errorf("triggering %s: %w", service, ErrInvalidTriggerToken))
	}
	return c.RunNow(ctx, service)
}

// claim takes service's run lock (retrying for up to wait, or until
// ctx is done) and fails any run a dead process left unfinished
// (reporting whether it did). The caller owns the lock on success and
// must call release exactly once. It returns ErrAlreadyRunning if the
// lock stayed taken. This is the shared primitive behind withClaim
// (RunDue/RunNow: run synchronously, release before returning) and
// Start (release only after the background run finishes).
func (c *Coordinator) claim(ctx context.Context, service string, wait time.Duration) (release func(), interrupted bool, err error) {
	release, err = c.lock(ctx, service, wait)
	if err != nil {
		return nil, false, err
	}

	last, interrupted, err := engine.FailInterruptedRun(ctx, c.DB, service, c.Notifier)
	if err != nil {
		release()
		return nil, false, fmt.Errorf("checking for an interrupted run: %w", err)
	}
	if interrupted {
		// The dead run never got to its cleanup, so a service whose
		// strategy stops containers for the backup may still be down.
		// The scheduler cannot tell which strategy ran, so say it
		// plainly for everyone.
		c.logf(service, "the run started at %s was interrupted before it finished (Ballast was stopped, killed, or the machine rebooted); it has been marked failed. "+
			"If this service's containers are stopped during backups, check that they are running again (docker ps -a) and start them if not",
			last.StartedAt.Local().Format(time.RFC3339))
	}
	return release, interrupted, nil
}

// withClaim takes service's run lock via claim and calls fn while
// still holding it, releasing before returning. It returns
// ErrAlreadyRunning if the lock stayed taken.
func (c *Coordinator) withClaim(ctx context.Context, service string, wait time.Duration, fn func() error) (interrupted bool, err error) {
	release, interrupted, err := c.claim(ctx, service, wait)
	if err != nil {
		return false, err
	}
	defer release()
	return interrupted, fn()
}

// Start takes service's run lock (waiting up to LockWait) and then
// runs it in the background, releasing the lock only once that run
// finishes. It returns ErrAlreadyRunning synchronously if the lock
// could not be taken — an HTTP handler can report 409 immediately
// without holding the request open for an hours-long backup.
// interrupted reports, synchronously, whether an earlier run was
// found interrupted (and marked failed) while claiming the lock.
//
// done receives exactly one value: the same nil-only-on-SUCCESS error
// RunNow returns, redacted like every other Coordinator error (spec
// rule 2). ctx governs the background run exactly as it would for
// RunNow — cancelling it (e.g. on shutdown) cancels the run.
func (c *Coordinator) Start(ctx context.Context, service string) (done <-chan error, interrupted bool, err error) {
	release, interrupted, err := c.claim(ctx, service, c.lockWait())
	if errors.Is(err, ErrAlreadyRunning) {
		return nil, false, c.redactErr(fmt.Errorf("%s: %w; not starting another", service, err))
	}
	if err != nil {
		return nil, false, c.redactErr(err)
	}
	ch := make(chan error, 1)
	go func() {
		defer release()
		state, runErr := c.Runner.Run(ctx, service)
		ch <- c.redactErr(checkRunSuccess(service, state, runErr))
	}()
	return ch, interrupted, nil
}

// StartTrigger authenticates an externally-triggered run like Trigger,
// but starts it in the background via Start instead of running it
// synchronously via RunNow — for the HTTP trigger endpoint, which must
// not hold the request open for an hours-long backup. err is returned
// synchronously for a bad token (before anything is started) or a
// busy service; once started, the run's own outcome only ever reaches
// done, exactly as Start documents.
func (c *Coordinator) StartTrigger(ctx context.Context, service, token string) (done <-chan error, interrupted bool, err error) {
	ok, err := VerifyTriggerToken(c.DB, service, token)
	if err != nil {
		return nil, false, c.redactErr(fmt.Errorf("triggering %s: %w", service, err))
	}
	if !ok {
		return nil, false, c.redactErr(fmt.Errorf("triggering %s: %w", service, ErrInvalidTriggerToken))
	}
	return c.Start(ctx, service)
}

// lock takes service's run lock, retrying every lockPollInterval for up
// to wait while another holder has it.
func (c *Coordinator) lock(ctx context.Context, service string, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		release, ok, err := c.Locks.TryLock(service)
		if err != nil {
			return nil, err
		}
		if ok {
			return release, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, ErrAlreadyRunning
		}
		t := time.NewTimer(min(left, lockPollInterval))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("waiting for the run lock of %s: %w", service, ctx.Err())
		case <-t.C:
		}
	}
}

func (c *Coordinator) logf(service, format string, args ...any) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	w := c.Log
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintln(w, c.redact(fmt.Sprintf("ballast: %s: %s", service, fmt.Sprintf(format, args...))))
}

// redact replaces every occurrence of each Redact secret in s.
func (c *Coordinator) redact(s string) string {
	return c.Redact.Redact(s)
}

// redactErr returns err with any Redact secret removed from its text.
// err itself is returned when its text holds no secret.
func (c *Coordinator) redactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if clean := c.redact(msg); clean != msg {
		return &redactedError{err: err, msg: clean}
	}
	return err
}

// redactedError is an error whose text has had secrets removed. It
// unwraps to the original, so errors.Is and errors.As still work; code
// that unwraps it must not print what it finds.
type redactedError struct {
	err error
	msg string
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
