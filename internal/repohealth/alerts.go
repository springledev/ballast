package repohealth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/notify"
)

// NotifyAlerter delivers repository-health alerts through the same
// channels as backup alerts: each service that backs up to the
// destination gets the message on its own channels. A problem is sent
// once, then as a reminder after the usual cool-down, and a recovery
// notice follows when the repository is healthy again.
type NotifyAlerter struct {
	// DB is the Ballast database.
	DB *sql.DB
	// Notifier delivers the events.
	Notifier *notify.Notifier
	// Cooldown overrides the reminder interval; zero is the default.
	Cooldown time.Duration
}

// gateKey is the alert-state key for a destination; the prefix keeps it
// apart from any service name (service names cannot contain a colon).
func gateKey(destination string) string { return "repository:" + destination }

func (a *NotifyAlerter) gate() *notify.AlertGate {
	return &notify.AlertGate{DB: a.DB, Cooldown: a.Cooldown}
}

// services lists the services that send backups to destination.
func (a *NotifyAlerter) services(destination string) ([]string, error) {
	rows, err := a.DB.Query(`SELECT name FROM services WHERE destination = ?
		UNION SELECT service FROM service_targets WHERE destination = ? ORDER BY 1`, destination, destination)
	if err != nil {
		return nil, fmt.Errorf("destination %s: finding the services that use it: %w", destination, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("destination %s: reading service name: %w", destination, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (a *NotifyAlerter) send(ctx context.Context, destination string, mk func(service string) notify.Event) error {
	svcs, err := a.services(destination)
	if err != nil {
		return err
	}
	var errs []error
	delivered := 0
	for _, s := range svcs {
		if err := a.Notifier.Send(ctx, mk(s)); err != nil {
			if !errors.Is(err, notify.ErrNoChannels) {
				errs = append(errs, err)
			}
			continue
		}
		delivered++
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if delivered == 0 {
		return notify.ErrNoChannels
	}
	return nil
}

// Alert implements Alerter.
func (a *NotifyAlerter) Alert(ctx context.Context, destination, state, detail string) error {
	g := a.gate()
	ok, err := g.ShouldAlert(gateKey(destination))
	if err != nil || !ok {
		return err
	}
	problem := "needs attention"
	if state == StateFailed {
		problem = "failed its health check"
	}
	if err := a.send(ctx, destination, func(service string) notify.Event {
		return notify.Event{
			Service:   service,
			Type:      notify.EventJobFailure,
			Message:   fmt.Sprintf("The backup destination %s %s: %s", destination, problem, detail),
			Detail:    "Repository health check for destination " + destination,
			Severity:  map[bool]string{true: "error", false: "warning"}[state == StateFailed],
			Timestamp: time.Now().UTC(),
		}
	}); err != nil {
		return err
	}
	return g.RecordAlert(gateKey(destination))
}

// Recovered implements Alerter. It does nothing unless an alert is open.
func (a *NotifyAlerter) Recovered(ctx context.Context, destination string) error {
	g := a.gate()
	open, err := g.IsAlerting(gateKey(destination))
	if err != nil || !open {
		return err
	}
	err = a.send(ctx, destination, func(service string) notify.Event {
		ev := notify.FormatRecovery(service)
		ev.Message = fmt.Sprintf("The backup destination %s passed its health check again", destination)
		ev.Detail = "No action needed."
		return ev
	})
	if err != nil && !errors.Is(err, notify.ErrNoChannels) {
		return err
	}
	return g.Clear(gateKey(destination))
}
