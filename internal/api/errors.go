package api

import (
	"errors"

	"github.com/springledev/ballast/internal/scheduler"
)

// joinedLeaves flattens err through every Unwrap() error and
// Unwrap() []error chain (the latter is what errors.Join produces)
// down to its leaf errors, so callers can inspect exactly what
// happened without depending on the joined error's internal shape.
func joinedLeaves(err error) []error {
	if err == nil {
		return nil
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range u.Unwrap() {
			out = append(out, joinedLeaves(e)...)
		}
		return out
	}
	if u, ok := err.(interface{ Unwrap() error }); ok {
		if next := u.Unwrap(); next != nil {
			return joinedLeaves(next)
		}
	}
	return []error{err}
}

// InterruptedOnly reports whether err represents ONLY a report that
// an earlier run was found interrupted (scheduler.ErrPreviousRunInterrupted)
// with no other failure alongside it — i.e. the run this call itself
// started still succeeded. Callers on a synchronous run path (`ballast
// backup`, `ballast trigger`) use this to report the interruption as a
// warning instead of failing the command (spec §14 step 11 hard rule
// 6: a verified new run must not be reported as a failure just because
// an old, unrelated run was found interrupted); a genuine failure of
// the new run (found in the same joined error) still reports as a
// real error.
func InterruptedOnly(err error) bool {
	leaves := joinedLeaves(err)
	if len(leaves) == 0 {
		return false
	}
	sawInterrupted := false
	for _, e := range leaves {
		if errors.Is(e, scheduler.ErrPreviousRunInterrupted) {
			sawInterrupted = true
			continue
		}
		return false
	}
	return sawInterrupted
}
