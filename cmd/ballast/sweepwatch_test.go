package main

import (
	"testing"
	"time"
)

func TestSweepWatchStalled(t *testing.T) {
	t0 := time.Date(2026, 3, 5, 3, 0, 0, 0, time.UTC)
	min := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	tests := []struct {
		name      string
		started   []int // minutes at which sweeps began
		finished  []int // of those, the start minutes of the ones that finished
		at        int   // minute of the check
		wantStall bool
	}{
		{"just started, nothing finished yet", []int{0}, nil, 0, false},
		{"first sweep still open at the limit", []int{0}, nil, 10, false},
		{"nothing has ever finished and the limit has passed", []int{0, 1, 2}, nil, 12, true},
		{"quick sweeps finish every minute", []int{0, 1, 2, 3}, []int{0, 1, 2}, 3, false},
		{"one old sweep open for hours while newer ones finish", []int{0, 59, 60, 61}, []int{59, 60}, 61, false},
		{"a long old sweep finishing late is no evidence", []int{0, 1}, []int{0}, 40, true},
		{"newer sweeps stopped finishing", []int{0, 1, 2, 30}, []int{0, 1, 2}, 30, true},
		{"recovered after a stall", []int{0, 20, 21}, []int{0, 20}, 21, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &sweepWatch{}
			for _, m := range tc.started {
				w.begin(min(m))
			}
			for _, m := range tc.finished {
				w.finish(min(m))
			}
			_, got := w.stalled(min(tc.at), sweepStuckLimit)
			if got != tc.wantStall {
				t.Errorf("stalled = %v, want %v", got, tc.wantStall)
			}
		})
	}
}
