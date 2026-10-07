package protondrive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/notify"
)

// Alerter tells the user when a service's Proton Drive copy needs
// attention, and when it recovers. It is the copy tier's own alert: a
// problem here is never reported as a failed backup (spec §12).
type Alerter interface {
	// Alert reports a problem with service's copy to destination.
	Alert(ctx context.Context, service, destination, detail string) error
	// Recovered reports that the copy works again; it does nothing
	// unless an alert is open.
	Recovered(ctx context.Context, service, destination string) error
}

// NotifyAlerter delivers copy alerts through the service's own
// notification channels, once and then as a reminder after the usual
// cool-down.
type NotifyAlerter struct {
	// DB is the Ballast database.
	DB *sql.DB
	// Notifier delivers the events.
	Notifier *notify.Notifier
	// Cooldown overrides the reminder interval; zero is the default.
	Cooldown time.Duration
}

func gateKey(service, destination string) string { return "copy:" + service + ":" + destination }

func (a *NotifyAlerter) gate() *notify.AlertGate {
	return &notify.AlertGate{DB: a.DB, Cooldown: a.Cooldown}
}

// Alert implements Alerter.
func (a *NotifyAlerter) Alert(ctx context.Context, service, destination, detail string) error {
	g := a.gate()
	ok, err := g.ShouldAlert(gateKey(service, destination))
	if err != nil || !ok {
		return err
	}
	err = a.Notifier.Send(ctx, notify.Event{
		Service:   service,
		Type:      notify.EventJobFailure,
		Message:   fmt.Sprintf("The second copy of %s in Proton Drive (%s) needs attention: %s", service, destination, detail),
		Detail:    "Your main backup is not affected.",
		Severity:  "warning",
		Timestamp: time.Now().UTC(),
	})
	if err != nil {
		if errors.Is(err, notify.ErrNoChannels) {
			return nil
		}
		return fmt.Errorf("service %s: telling you about the Proton Drive copy: %w", service, err)
	}
	return g.RecordAlert(gateKey(service, destination))
}

// Recovered implements Alerter.
func (a *NotifyAlerter) Recovered(ctx context.Context, service, destination string) error {
	g := a.gate()
	open, err := g.IsAlerting(gateKey(service, destination))
	if err != nil || !open {
		return err
	}
	ev := notify.FormatRecovery(service)
	ev.Message = fmt.Sprintf("The second copy of %s in Proton Drive (%s) works again", service, destination)
	ev.Detail = "No action needed."
	if err := a.Notifier.Send(ctx, ev); err != nil && !errors.Is(err, notify.ErrNoChannels) {
		return fmt.Errorf("service %s: telling you the Proton Drive copy recovered: %w", service, err)
	}
	return g.Clear(gateKey(service, destination))
}
