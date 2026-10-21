package main

import (
	"sync"
	"time"
)

// sweepStuckLimit is how long the scheduler may go without finishing a
// sweep before the heartbeat stops (spec §21.7).
const sweepStuckLimit = 10 * time.Minute

// sweepWatch tells the heartbeat whether the scheduler is making
// progress, not merely whether its clock still ticks.
//
// The loop starts a new sweep goroutine on every tick, so a hung sweep
// never stops the ticks. Individual sweeps can legitimately last hours:
// RunDue waits for the backups it starts, a finished backup may still be
// pruning old snapshots while it holds the run lock, and the repository
// health check runs at the end of a sweep. None of that is a sign of
// trouble, and none of it holds up the next minute's sweep: nothing
// serialises sweeps (each claims a service's run lock without waiting
// and skips a busy one, a destination already being checked is skipped
// as busy, and the sweep's other steps are short). A healthy daemon
// therefore finishes some sweep about every minute, even while older
// ones are busy with long work.
//
// So the rule is: the scheduler is alive while some sweep that began
// within the last sweepStuckLimit has finished. A deadlock, a lock
// taken and never released, or a database call that never returns
// stops every new sweep from finishing, and the heartbeat stops once
// the limit has passed, so the outside monitor notices. A long sweep
// that finally finishes proves nothing, because only its start time
// counts. Before any sweep has finished, the first tick's time stands
// in, which gives a starting daemon the same grace.
//
// Times are the loop's own tick times, so tests can drive them.
type sweepWatch struct {
	mu       sync.Mutex
	first    time.Time // when the first sweep began
	last     time.Time // when the newest sweep that finished began
	reported bool
}

// begin records that a sweep started at now.
func (w *sweepWatch) begin(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.first.IsZero() {
		w.first = now
	}
}

// finish records that the sweep that began at started has finished.
func (w *sweepWatch) finish(started time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if started.After(w.last) {
		w.last = started
	}
}

// stalled reports whether no sweep that began within limit of now has
// finished, and since when the scheduler last showed progress (the start
// of the newest finished sweep, or of the first sweep if none has).
func (w *sweepWatch) stalled(now time.Time, limit time.Duration) (since time.Time, stalled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	since = w.last
	if since.IsZero() {
		since = w.first
	}
	return since, now.Sub(since) > limit
}

// changed records whether the heartbeat is being held back and reports
// whether that differs from last time, so the log says it once.
func (w *sweepWatch) changed(held bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reported == held {
		return false
	}
	w.reported = held
	return true
}
