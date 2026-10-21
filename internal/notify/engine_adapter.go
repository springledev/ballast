package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/keel/secret"
)

// errNoNotifier is returned by EngineAdapter's notify methods when
// EngineAdapter.Notifier is nil: a caller wired an EngineAdapter in
// (see Machine.SetNotifier) without ever constructing a
// notify.Notifier for it — a wiring mistake, distinct from
// internal/engine's own nil Notifier (a deliberate, documented no-op;
// see engine.Notifier). Returning this loud, per-service error instead
// of panicking on the nil dereference keeps a misconfiguration
// reported (spec rule 3: fail loudly per-service) rather than crashing
// the run.
var errNoNotifier = errors.New("notify: EngineAdapter has no Notifier configured")

// Compile-time check that EngineAdapter satisfies engine.Notifier.
// internal/engine's Notifier is deliberately structural (primitive
// types only) so internal/engine itself never needs to import this
// package; this package importing internal/engine for the assertion
// below is one-directional and creates no cycle.
var _ engine.Notifier = (*EngineAdapter)(nil)

// EngineAdapter implements internal/engine's Notifier interface over a
// Notifier and the notification_settings table, so the engine's
// run-outcome hooks can deliver through ntfy/webhook channels without
// internal/engine depending on this package (the dependency runs the
// other way: this package imports internal/engine only for the
// compile-time assertion above, which internal/engine's own code
// never needs).
//
// Wire it in after starting or loading a run:
//
//	m.SetNotifier(ctx, &notify.EngineAdapter{DB: db, Notifier: notifier})
//
// A zero-value or otherwise nil Notifier field is a configuration
// error, not a silent no-op (unlike a nil engine.Notifier itself,
// which internal/engine treats as notifications-off by design): every
// method below returns an error wrapping errNoNotifier, naming the
// service, instead of panicking on the nil dereference. The methods
// are also safe to call on a nil *EngineAdapter (e.g. a typed-nil
// engine.Notifier stored in a struct field), returning the same
// errNoNotifier-wrapped error rather than panicking.
type EngineAdapter struct {
	DB       *sql.DB
	Notifier *Notifier
	// Cooldown is how long a still-failing service stays quiet before a
	// reminder; zero means DefaultAlertCooldown.
	Cooldown time.Duration
	// Redact is the app's shared secret set (it holds every channel's
	// token, topic and password once the channel is built). Delivery
	// error text is cleaned with it before it is logged or stored.
	Redact *secret.Set
	// Location is the time zone schedules are written in; nil means the
	// machine's local zone.
	Location *time.Location
	// Now overrides the clock in tests; nil means the real clock.
	Now func() time.Time
}

func (a *EngineAdapter) gate() *AlertGate {
	return &AlertGate{DB: a.DB, Cooldown: a.Cooldown, Now: a.Now, Location: a.Location}
}

// decide logs and stores one alert decision.
func (a *EngineAdapter) decide(ev Event, outcome, reason string) {
	logDecision(a.DB, AlertDecision{
		Service: ev.Service, At: a.gate().now(), Type: string(ev.Type),
		Outcome: outcome, Reason: reason,
	}, a.Notifier.Destinations(ev.Service), a.Redact)
}

// sendAlert delivers a failure event unless the service was already
// alerted about within the cool-down. The alert is recorded only once
// delivery succeeded, so an unreachable channel is retried next time.
func (a *EngineAdapter) sendAlert(ctx context.Context, ev Event, runID int64) error {
	g := a.gate()
	runAt := a.runStart(runID)
	ok, why, err := g.DecideRun(ev.Service, runAt)
	if err != nil {
		return err
	}
	if !ok {
		a.decide(ev, OutcomeHeld, why)
		return nil
	}
	if err := a.Notifier.Send(ctx, ev); err != nil {
		a.decide(ev, OutcomeFailed, err.Error())
		return err
	}
	a.decide(ev, OutcomeSent, "")
	return g.RecordAlertRun(ev.Service, runAt)
}

// runStart returns when run runID started, or the current time when the
// run cannot be found (the run has only just finished, so it is close).
func (a *EngineAdapter) runStart(runID int64) time.Time {
	var started string
	if err := a.DB.QueryRow(`SELECT started_at FROM runs WHERE id = ?`, runID).Scan(&started); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, started); err == nil {
			return t
		}
	}
	return a.gate().now()
}

// NotifyJobFailure delivers a job-failure event (spec §11 "job
// failure (immediate)").
func (a *EngineAdapter) NotifyJobFailure(ctx context.Context, service string, runID int64, detail string) error {
	if a == nil || a.Notifier == nil {
		return fmt.Errorf("%s: %w", service, errNoNotifier)
	}
	return a.sendAlert(ctx, FormatJobFailure(service, fmt.Sprint(runID), detail), runID)
}

// NotifyVerificationFailure delivers a verification-failure event,
// distinct in type and severity from a plain job failure (spec §11
// "verification failure (immediate, distinct severity)"). runID is
// included (consistent with NotifyJobFailure) so the notification can
// always be traced back to the specific run in the DB.
func (a *EngineAdapter) NotifyVerificationFailure(ctx context.Context, service string, runID int64, detail string) error {
	if a == nil || a.Notifier == nil {
		return fmt.Errorf("%s: %w", service, errNoNotifier)
	}
	return a.sendAlert(ctx, FormatVerificationFailure(service, fmt.Sprint(runID), "backup verification", detail), runID)
}

// NotifySuccessHeartbeat delivers a success heartbeat only when the
// service has opted in via notification_settings.heartbeat_enabled
// (spec §11 "success heartbeat (configurable per-service)"); a
// service with no notification_settings row at all is treated as
// opted out, so a fresh install stays quiet by default.
func (a *EngineAdapter) NotifySuccessHeartbeat(ctx context.Context, service string, runID int64) error {
	if a == nil || a.Notifier == nil {
		return fmt.Errorf("%s: %w", service, errNoNotifier)
	}
	// A verified success after an alert closes it with one recovery notice.
	g := a.gate()
	var recoverErr error
	if alerting, err := g.IsAlerting(service); err != nil {
		return err
	} else if alerting {
		rec := FormatRecovery(service)
		if recoverErr = a.Notifier.Send(ctx, rec); recoverErr == nil {
			a.decide(rec, OutcomeSent, "")
			recoverErr = g.Clear(service)
		} else {
			a.decide(rec, OutcomeFailed, recoverErr.Error())
		}
	}
	enabled, err := a.heartbeatEnabled(service)
	if err != nil {
		return errors.Join(recoverErr, err)
	}
	if !enabled {
		return recoverErr
	}
	// The run that just succeeded is the freshest possible backup, so
	// its age is effectively zero.
	return errors.Join(recoverErr, a.Notifier.Send(ctx, FormatSuccessHeartbeat(service, 0)))
}

// heartbeatEnabled reports whether service has opted in to success
// heartbeat notifications.
func (a *EngineAdapter) heartbeatEnabled(service string) (bool, error) {
	var enabled int
	err := a.DB.QueryRow(
		`SELECT heartbeat_enabled FROM notification_settings WHERE service = ?`, service,
	).Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking heartbeat opt-in for %s: %w", service, err)
	}
	return enabled != 0, nil
}
