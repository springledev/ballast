package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EventRecovery is the single "back to normal" notice sent when a
// service that was being alerted about goes green again.
const EventRecovery EventType = "recovery"

// DefaultAlertCooldown is how long a still-failing service stays quiet
// before it gets a reminder.
const DefaultAlertCooldown = 24 * time.Hour

// RoutableEvents lists the event types a channel can be limited to.
var RoutableEvents = []EventType{EventJobFailure, EventVerificationFailure, EventFreshnessBreached, EventSuccessHeartbeat, EventRecovery}

// ValidateRouting checks event and tier filters, returning them
// normalised (trimmed, de-duplicated, sorted).
func ValidateRouting(events, tiers []string) ([]string, []string, error) {
	known := map[string]bool{}
	for _, e := range RoutableEvents {
		known[string(e)] = true
	}
	events = uniqueStrings(events)
	for _, e := range events {
		if !known[e] {
			return nil, nil, fmt.Errorf("%w: %q is not an event (choose from job_failure, verification_failure, freshness_breached, success_heartbeat, recovery)", ErrInvalidChannel, e)
		}
	}
	tiers = uniqueStrings(tiers)
	for _, t := range tiers {
		if strings.ContainsAny(t, ", ") {
			return nil, nil, fmt.Errorf("%w: %q is not a backup copy name", ErrInvalidChannel, t)
		}
	}
	return events, tiers, nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return uniqueStrings(strings.Split(s, ","))
}

// routedChannel applies a channel's event and tier filters. Test
// messages always pass, so a test-send proves delivery regardless of
// routing.
type routedChannel struct {
	Channel
	events, tiers map[string]bool
}

func newRouted(ch Channel, events, tiers []string) Channel {
	if len(events) == 0 && len(tiers) == 0 {
		return ch
	}
	r := &routedChannel{Channel: ch, events: map[string]bool{}, tiers: map[string]bool{}}
	for _, e := range events {
		r.events[e] = true
	}
	for _, t := range tiers {
		r.tiers[t] = true
	}
	return r
}

// Send delivers the event only if this channel is routed to want it.
func (r *routedChannel) Send(ctx context.Context, ev Event) error {
	// Test messages and the weekly digest skip the filters: a digest is
	// governed by the channel's own digest opt-in instead.
	if ev.Type != EventTest && ev.Type != EventWeeklyDigest {
		if len(r.events) > 0 && !r.events[string(ev.Type)] {
			return nil
		}
		if len(r.tiers) > 0 && !r.tiers[ev.TierName()] {
			return nil
		}
	}
	return r.Channel.Send(ctx, ev)
}

// ChannelAddress returns a short display string for where ch delivers,
// without secrets, or "" when unknown.
func ChannelAddress(ch Channel) string {
	if r, ok := ch.(*routedChannel); ok {
		ch = r.Channel
	}
	switch c := ch.(type) {
	case *NtfyChannel:
		return DisplayURL(c.Topic)
	case *WebhookChannel:
		return DisplayURL(c.URL)
	case *EmailChannel:
		return c.Address()
	}
	return ""
}

// LastStatus returns the HTTP status of ch's most recent delivery, if
// it is a webhook (shown on test-send), else 0.
func LastStatus(ch Channel) int {
	if r, ok := ch.(*routedChannel); ok {
		ch = r.Channel
	}
	if w, ok := ch.(*WebhookChannel); ok {
		return w.lastStatus
	}
	return 0
}

// AlertGate de-duplicates failure alerts (spec §18.1): one alert, then
// a reminder per Cooldown while the service keeps failing, and one
// recovery notice when it goes green. State is in the database so the
// daemon and `run-due` agree.
type AlertGate struct {
	DB       *sql.DB
	Cooldown time.Duration
	Now      func() time.Time
}

func (g *AlertGate) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func (g *AlertGate) cooldown() time.Duration {
	if g.Cooldown > 0 {
		return g.Cooldown
	}
	return DefaultAlertCooldown
}

// ShouldAlert reports whether a failure alert for service should be
// sent now (first failure, or the cool-down has passed).
func (g *AlertGate) ShouldAlert(service string) (bool, error) {
	var last string
	err := g.DB.QueryRow(`SELECT last_alert_at FROM alert_state WHERE service = ?`, service).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("service %s: reading alert state: %w", service, err)
	}
	t, perr := time.Parse(time.RFC3339Nano, last)
	if perr != nil {
		return true, nil
	}
	return g.now().Sub(t) >= g.cooldown(), nil
}

// RecordAlert notes that an alert was just delivered for service.
func (g *AlertGate) RecordAlert(service string) error {
	_, err := g.DB.Exec(`INSERT INTO alert_state (service, last_alert_at) VALUES (?, ?)
		ON CONFLICT(service) DO UPDATE SET last_alert_at = excluded.last_alert_at`,
		service, g.now().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("service %s: saving alert state: %w", service, err)
	}
	return nil
}

// IsAlerting reports whether service has an open alert.
func (g *AlertGate) IsAlerting(service string) (bool, error) {
	var n int
	if err := g.DB.QueryRow(`SELECT COUNT(*) FROM alert_state WHERE service = ?`, service).Scan(&n); err != nil {
		return false, fmt.Errorf("service %s: reading alert state: %w", service, err)
	}
	return n > 0, nil
}

// Clear closes service's open alert.
func (g *AlertGate) Clear(service string) error {
	if _, err := g.DB.Exec(`DELETE FROM alert_state WHERE service = ?`, service); err != nil {
		return fmt.Errorf("service %s: clearing alert state: %w", service, err)
	}
	return nil
}

// FormatRecovery formats the "back to normal" notice.
func FormatRecovery(service string) Event {
	return Event{
		Service:   service,
		Type:      EventRecovery,
		Message:   fmt.Sprintf("%s is back to normal", service),
		Detail:    "The latest backup finished and was verified. No action needed.",
		Severity:  "info",
		Timestamp: time.Now().UTC(),
	}
}
