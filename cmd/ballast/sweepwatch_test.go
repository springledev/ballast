package main

import (
	"context"
	"testing"
	"time"
)

func TestSweepWatchStuck(t *testing.T) {
	t0 := time.Date(2026, 3, 5, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		inRunDue bool
		running  bool
		age      time.Duration
		want     bool
	}{
		{"young sweep is fine", false, false, 5 * time.Minute, false},
		{"exactly at the limit is fine", false, false, sweepStuckLimit, false},
		{"old sweep outside RunDue is stuck even with a backup running", false, true, time.Hour, true},
		{"old sweep in RunDue with a live backup is waiting, not stuck", true, true, 20 * time.Hour, false},
		{"old sweep in RunDue with no backup running is stuck", true, false, time.Hour, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &sweepWatch{}
			id := w.begin(t0)
			w.setInRunDue(id, tc.inRunDue)
			_, got := w.stuck(t0.Add(tc.age), sweepStuckLimit, func() bool { return tc.running })
			if got != tc.want {
				t.Errorf("stuck = %v, want %v", got, tc.want)
			}
			w.end(id)
			if _, got := w.stuck(t0.Add(tc.age), sweepStuckLimit, func() bool { return tc.running }); got {
				t.Error("a finished sweep must not count")
			}
		})
	}
}

func TestMarkRunDueWithoutAWatchIsHarmless(t *testing.T) {
	markRunDue(context.Background(), true) // run-due CLI path: no watch in ctx
}
