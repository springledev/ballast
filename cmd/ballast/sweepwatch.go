package main

import (
	"context"
	"sync"
	"time"
)

// sweepStuckLimit is how long a scheduler sweep may run without a
// backup to blame before the heartbeat stops (spec §21.7).
const sweepStuckLimit = 10 * time.Minute

// sweepWatch tells the heartbeat whether the scheduler is making
// progress, not merely whether its clock still ticks.
//
// The loop starts a new sweep goroutine on every tick, so a hung sweep
// never stops the ticks. A sweep can legitimately last hours, though:
// sweepDue runs its backups synchronously (Coordinator.RunDue waits for
// them), so a long Immich backup keeps its sweep open for as long as
// the backup runs. The rule that separates the two:
//
//   - A sweep that has been open longer than sweepStuckLimit is
//     excused only while it is inside RunDue AND a backup is really
//     running (a live process holds its run lock). It is then waiting
//     on work that is making progress.
//   - Any other sweep open longer than the limit is stuck: a lock
//     taken and never released, a deadlock, a database call that never
//     returns, or a RunDue whose backups are gone but which did not
//     come back. The heartbeat stops until it clears, so the outside
//     monitor notices.
//
// Times are the loop's own tick times, so tests can drive them.
type sweepWatch struct {
	mu       sync.Mutex
	next     int
	open     map[int]*openSweep
	reported bool
}

type openSweep struct {
	started  time.Time
	inRunDue bool
}

// begin records that a sweep started at now and returns its id.
func (w *sweepWatch) begin(now time.Time) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.open == nil {
		w.open = map[int]*openSweep{}
	}
	w.next++
	w.open[w.next] = &openSweep{started: now}
	return w.next
}

// end records that sweep id finished.
func (w *sweepWatch) end(id int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.open, id)
}

// setInRunDue records whether sweep id is inside RunDue, waiting on
// backups.
func (w *sweepWatch) setInRunDue(id int, in bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.open[id]; s != nil {
		s.inRunDue = in
	}
}

// stuck reports whether some sweep has been open longer than limit at
// now without an excuse (see sweepWatch). backupsRunning is asked only
// when a sweep inside RunDue is past the limit; if it fails or says no,
// that sweep counts as stuck. It returns the oldest stuck sweep's start.
func (w *sweepWatch) stuck(now time.Time, limit time.Duration, backupsRunning func() bool) (time.Time, bool) {
	w.mu.Lock()
	var overdue []openSweep
	for _, s := range w.open {
		if now.Sub(s.started) > limit {
			overdue = append(overdue, *s)
		}
	}
	w.mu.Unlock()
	var oldest time.Time
	found := false
	excuse, asked := false, false
	for _, s := range overdue {
		if s.inRunDue {
			if !asked {
				excuse, asked = backupsRunning(), true
			}
			if excuse {
				continue
			}
		}
		if !found || s.started.Before(oldest) {
			oldest, found = s.started, true
		}
	}
	return oldest, found
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

type sweepCtxKey struct{}

type sweepRef struct {
	w  *sweepWatch
	id int
}

// withSweep marks ctx as belonging to sweep id of w.
func withSweep(ctx context.Context, w *sweepWatch, id int) context.Context {
	return context.WithValue(ctx, sweepCtxKey{}, sweepRef{w, id})
}

// markRunDue tells the watch (if ctx carries one) that the sweep is
// entering or leaving RunDue. Outside the daemon's loop it does nothing.
func markRunDue(ctx context.Context, in bool) {
	if r, ok := ctx.Value(sweepCtxKey{}).(sweepRef); ok {
		r.w.setInRunDue(r.id, in)
	}
}
