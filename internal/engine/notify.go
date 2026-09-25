package engine

import (
	"context"
	"log"
)

// Notifier is the minimal set of notification hooks the engine calls
// on run-outcome transitions (spec §11, §14 step 10). It is kept
// deliberately small and uses only primitive types (plus the standard
// context.Context) so a concrete implementation (see internal/notify)
// never needs to import this package — internal/notify implements
// this interface structurally, with no dependency on internal/engine
// in its production code.
//
// Every method takes a context.Context so a delivery attempt (a
// synchronous HTTP call, in internal/notify's implementation) can be
// bounded by the caller: see Machine.SetNotifier.
//
// A nil Notifier (the default) is a no-op: notifications are entirely
// optional and off unless a caller explicitly wires one in with
// Machine.SetNotifier.
type Notifier interface {
	// NotifyJobFailure is called when a run reaches FAILED from any
	// state other than VERIFYING.
	NotifyJobFailure(ctx context.Context, service string, runID int64, detail string) error
	// NotifyVerificationFailure is called when a run reaches FAILED
	// while it was VERIFYING: a distinct, higher-signal event from a
	// plain job failure, since the backup itself ran but could not be
	// proven restorable (spec §11 "verification failure (immediate,
	// distinct severity)").
	NotifyVerificationFailure(ctx context.Context, service string, runID int64, detail string) error
	// NotifySuccessHeartbeat is called when a run reaches SUCCESS.
	// Per-service opt-in (and any rate limiting) is the
	// implementation's responsibility: the engine calls it
	// unconditionally on every verified success, never on
	// SUCCESS_UNVERIFIED (amber never reads as a clean heartbeat).
	NotifySuccessHeartbeat(ctx context.Context, service string, runID int64) error
}

// SetNotifier attaches a Notifier that Transition will call on FAILED
// and SUCCESS outcomes from this point on, using ctx to bound every
// delivery attempt (e.g. so a pending SIGTERM is not held up by a
// stuck HTTP call while the engine's run lock is held). A nil ctx is
// treated as context.Background().
//
// Pass nil for n to disable notifications again; a Machine has no
// Notifier by default.
func (m *Machine) SetNotifier(ctx context.Context, n Notifier) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.notifier = n
	m.notifyCtx = ctx
}

// NotifyErr returns the error from the most recent notification
// delivery attempt made by Transition, or nil if that attempt (or the
// most recent transition, if it made no attempt) succeeded or no
// Notifier is set.
//
// This is separate from Run().Error on purpose: a notification
// failure must never be mistaken for the run's own failure reason, or
// mask it. The run's state and error are already committed by the
// time a notification is even attempted.
func (m *Machine) NotifyErr() error { return m.notifyErr }

// notifyOutcome sends an optional notification for FAILED and SUCCESS
// outcomes reached by an already-committed transition. It never
// returns an error to the caller: a delivery failure is logged and
// recorded on m.notifyErr (see NotifyErr) but can no longer change
// the run, which has already been persisted.
func (m *Machine) notifyOutcome(from, to State, detail string) {
	m.notifyErr = nil
	if m.notifier == nil {
		return
	}

	ctx := m.notifyCtx
	if ctx == nil {
		ctx = context.Background()
	}

	var err error
	switch {
	case to == FAILED && from == VERIFYING:
		err = m.notifier.NotifyVerificationFailure(ctx, m.run.Service, m.run.ID, detail)
	case to == FAILED:
		err = m.notifier.NotifyJobFailure(ctx, m.run.Service, m.run.ID, detail)
	case to == SUCCESS:
		err = m.notifier.NotifySuccessHeartbeat(ctx, m.run.Service, m.run.ID)
	default:
		return
	}

	m.notifyErr = err
	if err != nil {
		log.Printf("ballast: %s run %d: notification delivery failed (run outcome unaffected): %v", m.run.Service, m.run.ID, err)
	}
}
