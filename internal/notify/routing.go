package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/springledev/ballast/internal/scheduler"
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

// AlertGate de-duplicates failure alerts (spec §18.1, §21.1): one
// alert, then one reminder per scheduled failure while the service
// keeps failing (or per Cooldown when it has no schedule), and one
// recovery notice when it goes green. State is in the database so the
// daemon and `run-due` agree.
type AlertGate struct {
	DB       *sql.DB
	Cooldown time.Duration
	Now      func() time.Time
	// Location is the time zone the schedules are written in; nil means
	// the machine's local zone, like the scheduler itself.
	Location *time.Location
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

func (g *AlertGate) loc() *time.Location {
	if g.Location != nil {
		return g.Location
	}
	return time.Local
}

// firing returns the scheduled firing that a run starting at t belongs
// to (the latest firing at or before t), as a UTC timestamp. It is ""
// when service has no enabled, readable schedule.
func (g *AlertGate) firing(service string, t time.Time) string {
	var expr string
	var enabled int
	err := g.DB.QueryRow(`SELECT cron_expr, enabled FROM schedules WHERE service = ?`, service).Scan(&expr, &enabled)
	if err != nil || enabled == 0 {
		return ""
	}
	sch, err := scheduler.ParseCron(expr)
	if err != nil {
		return ""
	}
	f, ok := sch.Prev(t.In(g.loc()))
	if !ok {
		return ""
	}
	return f.UTC().Format(time.RFC3339)
}

// ShouldAlert reports whether a failure alert for service should be
// sent now (first failure, or the cool-down has passed).
func (g *AlertGate) ShouldAlert(service string) (bool, error) {
	ok, _, err := g.Decide(service)
	return ok, err
}

// Decide is DecideRun for a run happening right now.
func (g *AlertGate) Decide(service string) (bool, string, error) {
	return g.DecideRun(service, g.now())
}

// DecideRun reports whether a failure alert should be sent for a run
// of service that started at runAt, with a plain-English reason when it
// is held back.
//
// A service with a schedule gets one alert per scheduled firing: a new
// failed run reminds when it belongs to a later firing than the last
// alert, and is held (a manual retry, say) when it belongs to the same
// one. A service without a schedule waits out the cool-down (24 hours
// by default).
func (g *AlertGate) DecideRun(service string, runAt time.Time) (bool, string, error) {
	var last, window string
	err := g.DB.QueryRow(`SELECT last_alert_at, last_window FROM alert_state WHERE service = ?`, service).Scan(&last, &window)
	if errors.Is(err, sql.ErrNoRows) {
		return true, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("service %s: reading alert state: %w", service, err)
	}
	t, perr := time.Parse(time.RFC3339Nano, last)
	if perr != nil {
		return true, "", nil
	}
	if cur := g.firing(service, runAt); cur != "" {
		if window == "" {
			// An alert recorded before windows were kept: work it out
			// from when it was sent.
			window = g.firing(service, t)
		}
		if cur != window {
			return true, "", nil
		}
		return false, fmt.Sprintf("already alerted for this scheduled run (alerted %s ago)", shortDuration(g.now().Sub(t))), nil
	}
	ago := g.now().Sub(t)
	cd := g.cooldown()
	if ago >= cd {
		return true, "", nil
	}
	return false, fmt.Sprintf("alerted %s ago, cool-down %s", shortDuration(ago), shortDuration(cd)), nil
}

// shortDuration renders d as whole hours, or minutes under an hour.
func shortDuration(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// RecordAlert notes that an alert was just delivered for service.
func (g *AlertGate) RecordAlert(service string) error {
	return g.RecordAlertRun(service, g.now())
}

// RecordAlertRun notes that an alert was just delivered for a run of
// service that started at runAt, so later failures of the same
// scheduled firing are held.
func (g *AlertGate) RecordAlertRun(service string, runAt time.Time) error {
	_, err := g.DB.Exec(`INSERT INTO alert_state (service, last_alert_at, last_window) VALUES (?, ?, ?)
		ON CONFLICT(service) DO UPDATE SET last_alert_at = excluded.last_alert_at, last_window = excluded.last_window`,
		service, g.now().Format(time.RFC3339Nano), g.firing(service, runAt))
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
