// Package notify handles notifications for backup events: job failure,
// verification failure, freshness breaches, and success heartbeats.
//
// Supported channels: ntfy (primary), generic HTTP webhook.
// Per-tier isolation (Phase 2) must preserve notification content
// separation so a secondary-tier failure doesn't mask a primary-tier one
// (spec §11, §14 step 10).
//
// IMPORTANT: Channel secrets (ntfy topic URL, webhook URL) are never
// stored in the database (spec rule 2). Callers read them from
// root-only credential files and pass them to channel constructors.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/springledev/ballast/internal/scheduler"
)

const (
	// DefaultMaxAgeNightly is the default max-age for nightly backup freshness.
	DefaultMaxAgeNightly = 48 * time.Hour
	// DefaultMaxAgeWeekly is the default max-age for weekly backup freshness.
	DefaultMaxAgeWeekly = 9 * 24 * time.Hour
	// neverBackedUpAge is a sentinel value indicating the service has never had a successful backup.
	neverBackedUpAge = 999 * 24 * time.Hour
)

// EventType describes the kind of notification being sent.
type EventType string

const (
	// EventJobFailure is sent immediately when a job reaches FAILED state.
	EventJobFailure EventType = "job_failure"
	// EventVerificationFailure is sent when verification fails (distinct severity).
	EventVerificationFailure EventType = "verification_failure"
	// EventFreshnessBreached is sent when a service's max age is exceeded.
	EventFreshnessBreached EventType = "freshness_breached"
	// EventSuccessHeartbeat is an optional success notification (per-service config).
	EventSuccessHeartbeat EventType = "success_heartbeat"
	// EventTest is the "Ballast test notification" a channel's test-send
	// button delivers through the very same Send path as real events.
	EventTest EventType = "test"
	// EventWeeklyDigest is the optional once-a-week summary of every
	// service, sent only to channels that opted in.
	EventWeeklyDigest EventType = "weekly_digest"
)

// Event describes a notification event.
type Event struct {
	// Service is the backup service name (e.g. "immich", "vaultwarden").
	Service string
	// Type is the kind of event.
	Type EventType
	// Message is the human-readable event description.
	Message string
	// Detail includes structured information (run ID, state, etc).
	Detail string
	// Severity is "error", "warning", or "info" for display purposes.
	Severity string
	// Timestamp is when the event occurred.
	Timestamp time.Time
	// Tier names the backup tier the event is about ("restic" for the
	// primary tier). Empty means the primary tier. Every channel shows
	// it, so a problem with one copy of the data is never worded as if
	// it were about another (spec §11, rule 3).
	Tier string
}

// TierPrimary is the tier name of the restic (primary) tier.
const TierPrimary = "restic"

// TierName returns the event's tier, defaulting to the primary tier.
func (e Event) TierName() string {
	if e.Tier == "" {
		return TierPrimary
	}
	return e.Tier
}

// ForTier returns a copy of e attributed to tier, with the tier named
// in the message so it reads correctly even where only the message is
// shown.
func (e Event) ForTier(tier string) Event {
	e.Tier = tier
	if tier != "" && tier != TierPrimary {
		e.Message = fmt.Sprintf("[%s copy] %s", tier, e.Message)
	}
	return e
}

// Channel is the interface for a notification delivery mechanism.
type Channel interface {
	// Send delivers the event, returning a non-nil error if delivery
	// failed. ctx bounds how long delivery may block (e.g. so a
	// pending SIGTERM is not held up by a stuck HTTP call while the
	// engine's run lock is held).
	Send(ctx context.Context, event Event) error
	// Type returns a short label for this channel (e.g. "ntfy", "webhook").
	Type() string
}

// clientWithTimeout returns an http.Client with a reasonable timeout.
func clientWithTimeout() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
	}
}

// stripURLSecret removes the request URL from an error returned by
// http.NewRequestWithContext or (*http.Client).Do.
//
// Both wrap failures in a *url.Error carrying the full request URL —
// for a Channel, that URL is the ntfy topic or webhook URL, a secret
// read from a root-only credential file (spec rule 2) that must never
// reach the DB, logs, or an API response. *url.Error.Error() embeds
// the URL directly, so it can never be logged as-is; this unwraps to
// url.Error.Err instead, which describes the failure ("connection
// refused", "no such host", an invalid-URL parse complaint, ...)
// without repeating the URL that was being requested.
func stripURLSecret(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// NtfyChannel sends notifications via ntfy.sh HTTP POST.
// Secrets (topic URL) are passed from the caller and never logged.
type NtfyChannel struct {
	Topic string // ntfy topic URL from credential file
	// Secret is the optional access credential for a protected ntfy
	// server, from the credential store: a bearer token, or — when
	// Username is set — that user's password.
	Secret   string
	Username string
	Client   *http.Client // injected for tests; nil defaults to clientWithTimeout()
}

// NewNtfyChannel creates a channel that sends to the given ntfy topic.
// topic must be a full URL read from a root-only credential file.
func NewNtfyChannel(topic string) *NtfyChannel {
	return &NtfyChannel{
		Topic:  topic,
		Client: clientWithTimeout(),
	}
}

// Send delivers the event to ntfy.
//
// Neither the returned error nor any log line built from it may
// contain the ntfy topic URL: it is a secret read from a root-only
// credential file (spec rule 2). See stripURLSecret.
func (n *NtfyChannel) Send(ctx context.Context, event Event) error {
	if n.Topic == "" {
		return fmt.Errorf("%s: ntfy topic not configured", event.Service)
	}

	client := n.Client
	if client == nil {
		client = clientWithTimeout()
	}

	// Message body includes all details
	msgBody := event.Message
	if event.Detail != "" {
		msgBody = msgBody + "\n" + event.Detail
	}

	req, err := http.NewRequestWithContext(ctx, "POST", n.Topic, bytes.NewBufferString(msgBody))
	if err != nil {
		return fmt.Errorf("%s: creating ntfy request: %w", event.Service, stripURLSecret(err))
	}

	// Set headers per ntfy API
	title := fmt.Sprintf("[%s] [%s] %s", event.Service, event.TierName(), event.Type)
	switch event.Type {
	case EventTest:
		title = "Ballast test notification"
	case EventWeeklyDigest:
		title = "Ballast weekly digest"
	}
	req.Header.Set("Title", title)
	switch {
	case n.Secret != "" && n.Username != "":
		req.SetBasicAuth(n.Username, n.Secret)
	case n.Secret != "":
		req.Header.Set("Authorization", "Bearer "+n.Secret)
	}
	req.Header.Set("Priority", priorityFromSeverity(event.Severity))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: sending to ntfy: %w", event.Service, stripURLSecret(err))
	}
	defer resp.Body.Close()
	// Drain (without keeping) the body so the connection can be reused;
	// its contents are never included in the error (finding: response
	// bodies must not be echoed into logs).
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512)) //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: ntfy returned status %d", event.Service, resp.StatusCode)
	}
	return nil
}

// Type returns "ntfy".
func (n *NtfyChannel) Type() string { return "ntfy" }

// WebhookChannel sends notifications via a generic HTTP webhook.
// Secrets (webhook URL) are passed from the caller and never logged.
type WebhookChannel struct {
	URL    string       // webhook URL from credential file
	Client *http.Client // injected for tests; nil defaults to clientWithTimeout()
	// Secret, when set, signs each body: the X-Ballast-Signature header
	// carries "sha256=" and the hex HMAC-SHA256 of the body.
	Secret string

	lastStatus int // HTTP status of the latest delivery, shown on test-send
}

// NewWebhookChannel creates a channel that POSTs to the given URL.
// url must be read from a root-only credential file.
func NewWebhookChannel(url string) *WebhookChannel {
	return &WebhookChannel{
		URL:    url,
		Client: clientWithTimeout(),
	}
}

// webhookPayload is the JSON structure sent to webhooks.
type webhookPayload struct {
	Service   string    `json:"service"`
	Tier      string    `json:"tier"`
	Type      EventType `json:"type"`
	Severity  string    `json:"severity"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail"`
	Timestamp time.Time `json:"timestamp"`
}

// Send delivers the event as JSON to the webhook URL.
//
// Neither the returned error nor any log line built from it may
// contain the webhook URL: it is a secret read from a root-only
// credential file (spec rule 2). See stripURLSecret.
func (w *WebhookChannel) Send(ctx context.Context, event Event) error {
	if w.URL == "" {
		return fmt.Errorf("%s: webhook URL not configured", event.Service)
	}

	client := w.Client
	if client == nil {
		client = clientWithTimeout()
	}

	payload := webhookPayload{
		Service:   event.Service,
		Tier:      event.TierName(),
		Type:      event.Type,
		Severity:  event.Severity,
		Message:   event.Message,
		Detail:    event.Detail,
		Timestamp: event.Timestamp,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s: marshaling webhook payload: %w", event.Service, err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", w.URL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%s: creating webhook request: %w", event.Service, stripURLSecret(err))
	}
	req.Header.Set("Content-Type", "application/json")
	if w.Secret != "" {
		mac := hmac.New(sha256.New, []byte(w.Secret))
		mac.Write(data)
		req.Header.Set("X-Ballast-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	w.lastStatus = 0
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: sending webhook: %w", event.Service, stripURLSecret(err))
	}
	defer resp.Body.Close()
	w.lastStatus = resp.StatusCode
	// Drain (without keeping) the body so the connection can be reused;
	// its contents are never included in the error (finding: response
	// bodies must not be echoed into logs).
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512)) //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: webhook returned status %d", event.Service, resp.StatusCode)
	}
	return nil
}

// Type returns "webhook".
func (w *WebhookChannel) Type() string { return "webhook" }

// priorityFromSeverity maps severity to ntfy priority values.
func priorityFromSeverity(severity string) string {
	switch severity {
	case "error":
		return "high"
	case "warning":
		return "default"
	default:
		return "low"
	}
}

// formatDuration returns a human-readable duration (e.g. "3 days 2 hours").
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	if d == neverBackedUpAge {
		return "never"
	}

	days := d / (24 * time.Hour)
	remainder := d % (24 * time.Hour)
	hours := remainder / time.Hour

	if days > 0 && hours > 0 {
		return fmt.Sprintf("%d days %d hours", days, hours)
	}
	if days > 0 {
		return fmt.Sprintf("%d days", days)
	}
	if hours > 0 {
		return fmt.Sprintf("%d hours", hours)
	}
	minutes := remainder / time.Minute
	if minutes > 0 {
		return fmt.Sprintf("%d minutes", minutes)
	}
	return "< 1 minute"
}

// ErrNoChannels is returned by Notifier.Send when the event's service
// has no registered delivery channel. It is a loud signal, not a
// silent success: a caller like SweepFreshness must not treat a
// freshness breach as reported when nothing was actually able to
// deliver it, or a service configured after the fact would never
// hear about a breach that happened before its channels existed.
var ErrNoChannels = errors.New("no notifications are set up for this service (see Notifications in the web UI to set one up)")

// Notifier sends notifications for backup events.
type Notifier struct {
	channels map[string][]Channel // keyed by service; multiple channels per service

	// Resolve, when set, is asked for a service's channels at send time
	// whenever none were registered for it up front. This is what lets
	// a long-running `ballast serve` notify about a service added
	// after it started (from the web UI, say) — or pick up a channel
	// credential file written after startup — without a restart. An
	// error from Resolve is returned from Send, never swallowed.
	Resolve func(service string) ([]Channel, error)
}

// NewNotifier creates a new notifier with empty channel registry.
func NewNotifier() *Notifier {
	return &Notifier{
		channels: make(map[string][]Channel),
	}
}

// RegisterChannel registers a delivery channel for a service. Multiple
// channels can be registered per service; each Send call delivers to all.
func (n *Notifier) RegisterChannel(service string, ch Channel) {
	if n.channels == nil {
		n.channels = make(map[string][]Channel)
	}
	n.channels[service] = append(n.channels[service], ch)
}

// channelsFor returns the channels registered or resolved for service.
func (n *Notifier) channelsFor(service string) ([]Channel, error) {
	chs := n.channels[service]
	if len(chs) == 0 && n.Resolve != nil {
		var err error
		if chs, err = n.Resolve(service); err != nil {
			return nil, fmt.Errorf("%s: loading notification channels: %w", service, err)
		}
	}
	return chs, nil
}

// Destinations names where alerts for service go, by channel kind
// (such as "ntfy, webhook"), never by address. It is "" when nothing is
// set up or channels cannot be loaded.
func (n *Notifier) Destinations(service string) string {
	chs, err := n.channelsFor(service)
	if err != nil {
		return ""
	}
	var kinds []string
	for _, c := range chs {
		kinds = append(kinds, c.Type())
	}
	return strings.Join(uniqueStrings(kinds), ", ")
}

// Send delivers an event to all registered channels for that service.
// If delivery fails on any channel, every failure is collected and
// returned together (via errors.Join) so one channel's outage can
// never hide another's — the same isolation-of-failure principle spec
// rule 3 requires between backup tiers. All channels are attempted
// regardless of individual failures.
//
// If the service has no registered channel at all, Send returns
// ErrNoChannels rather than silently succeeding: notifications are
// optional to configure, but once a caller asks to send one, "nobody
// was listening" must be visible, not indistinguishable from success.
func (n *Notifier) Send(ctx context.Context, event Event) error {
	chs, err := n.channelsFor(event.Service)
	if err != nil {
		return err
	}
	if len(chs) == 0 {
		return fmt.Errorf("%s: %w", event.Service, ErrNoChannels)
	}

	var errs []error
	for _, ch := range chs {
		if err := ch.Send(ctx, event); err != nil {
			errs = append(errs, fmt.Errorf("%s channel: %w", ch.Type(), err))
		}
	}

	return errors.Join(errs...)
}

// SendTiers delivers one event per backup tier. Each is sent on its own,
// primary tier first, and a failure to deliver (or a problem in) one
// tier never stops or hides another (spec rule 3): every delivery
// error is returned, labelled with its tier.
func (n *Notifier) SendTiers(ctx context.Context, events []Event) error {
	ordered := make([]Event, 0, len(events))
	for _, e := range events {
		if e.TierName() == TierPrimary {
			ordered = append(ordered, e)
		}
	}
	for _, e := range events {
		if e.TierName() != TierPrimary {
			ordered = append(ordered, e)
		}
	}
	var errs []error
	for _, e := range ordered {
		if err := n.Send(ctx, e); err != nil {
			errs = append(errs, fmt.Errorf("%s tier: %w", e.TierName(), err))
		}
	}
	return errors.Join(errs...)
}

// FormatJobFailure formats a job failure event.
func FormatJobFailure(service, runID, errorMsg string) Event {
	return Event{
		Service:   service,
		Type:      EventJobFailure,
		Message:   fmt.Sprintf("Backup failed for %s: %s", service, errorMsg),
		Detail:    fmt.Sprintf("Run ID: %s", runID),
		Severity:  "error",
		Timestamp: time.Now().UTC(),
	}
}

// FormatVerificationFailure formats a verification failure event.
// runID is included in Detail (consistent with FormatJobFailure) so a
// verification-failure notification can always be traced back to the
// specific run in the DB.
func FormatVerificationFailure(service, runID, checkType, detail string) Event {
	d := fmt.Sprintf("Run ID: %s", runID)
	if detail != "" {
		d = d + "\n" + detail
	}
	return Event{
		Service:   service,
		Type:      EventVerificationFailure,
		Message:   fmt.Sprintf("Verification failed for %s: %s", service, checkType),
		Detail:    d,
		Severity:  "error",
		Timestamp: time.Now().UTC(),
	}
}

// FormatFreshnessBreached formats a freshness breach event.
func FormatFreshnessBreached(service string, maxAge, actualAge time.Duration) Event {
	return Event{
		Service:   service,
		Type:      EventFreshnessBreached,
		Message:   fmt.Sprintf("No verified backup for %s in %s (max allowed: %s)", service, formatDuration(actualAge), formatDuration(maxAge)),
		Detail:    fmt.Sprintf("Consider running a manual backup for %s", service),
		Severity:  "warning",
		Timestamp: time.Now().UTC(),
	}
}

// FormatSuccessHeartbeat formats a success notification.
func FormatSuccessHeartbeat(service string, backupAge time.Duration) Event {
	return Event{
		Service:   service,
		Type:      EventSuccessHeartbeat,
		Message:   fmt.Sprintf("Backup for %s successful and verified (age: %s)", service, formatDuration(backupAge)),
		Detail:    "",
		Severity:  "info",
		Timestamp: time.Now().UTC(),
	}
}

// cronGapWindow is how far forward from cronGapReference DefaultMaxAge
// scans a schedule to find the largest gap between its firings.
//
// It must be wide enough that every schedule which fires at all is
// seen firing at least twice, across varied month lengths — not just
// "a month" (finding: a day-of-month schedule like "0 3 31 * *" only
// fires in the seven 31-day months, so its largest real gap, Aug
// 31->Oct 31 skipping September, is ~61 days; a 40-day window only
// ever catches one such firing, so maxFiringGap's count<2 fallback
// kicked in and produced a max-age far smaller than the schedule's
// real cadence, causing a false freshness alarm every time that gap
// was crossed). A full year plus a bit guarantees every month is
// represented at least once, so any recurring monthly-or-sparser
// schedule's largest gap is fully captured. maxFiringGap jumps from one
// firing to the next rather than testing every minute of the window
// (that scan cost ~13 ms per service and made GET /api/v1/status slow,
// spec §22.3); see BenchmarkMaxFiringGap.
const cronGapWindow = 400 * 24 * time.Hour

// cronGapReference is the fixed anchor DefaultMaxAge scans forward
// from. Using a fixed time (rather than time.Now()) makes the derived
// default deterministic and independent of when the sweep runs. It is
// a Monday so a weekly or weekday schedule's sample is not an
// accident of which day the anchor happens to land on.
var cronGapReference = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// DefaultMaxAge derives the default freshness max-age for a service
// from how often its cron schedule actually fires, for use when the
// service has no explicit notification_settings.max_age_hours
// override (spec §11).
//
// Rule: over a representative ~400-day window (see cronGapWindow), find
// the largest gap between consecutive firings, then:
//
//   - gap <= 24h (nightly or more frequent)  -> DefaultMaxAgeNightly (48h)
//   - gap <= 7 days (weekly or more frequent) -> DefaultMaxAgeWeekly (9 days)
//   - otherwise (monthly or sparser)          -> gap + 2 days, so the
//     alert window always has slack past the schedule's own cadence
//     rather than firing the moment a monthly job is theoretically due
//
// Note this rule is deliberately conservative, not exact: a weekdays-only
// schedule ("0 3 * * 1-5") has a largest gap of 3 days (Friday to
// Monday), which is <=7 days and so maps to the weekly default (9
// days) rather than the nightly one — a wider alert window than a
// human scheduling by hand might pick, but never a false alarm on a
// normal weekend.
//
// An expression ParseCron cannot parse falls back to
// DefaultMaxAgeNightly: the conservative choice on bad input is to
// check often, not to go quiet.
//
// The result depends on nothing but cronExpr (the window is fixed), so
// it is remembered per expression: status pages ask for every service's
// max age on each load (spec §22.3).
func DefaultMaxAge(cronExpr string) time.Duration {
	if d, ok := defaultMaxAgeMemo.Load(cronExpr); ok {
		return d.(time.Duration)
	}
	d := defaultMaxAge(cronExpr)
	if defaultMaxAgeMemoSize.Add(1) <= defaultMaxAgeMemoLimit {
		defaultMaxAgeMemo.Store(cronExpr, d)
	}
	return d
}

// defaultMaxAgeMemo maps a cron expression to its DefaultMaxAge. It
// never goes stale: the answer is a pure function of the expression.
// defaultMaxAgeMemoLimit bounds it, since expressions come from user
// input; past it, answers are simply computed every time.
var (
	defaultMaxAgeMemo     sync.Map
	defaultMaxAgeMemoSize atomic.Int64
)

const defaultMaxAgeMemoLimit = 4096

// defaultMaxAge computes DefaultMaxAge without the memo.
func defaultMaxAge(cronExpr string) time.Duration {
	sched, err := scheduler.ParseCron(cronExpr)
	if err != nil {
		return DefaultMaxAgeNightly
	}
	gap, ok := maxFiringGap(sched)
	if !ok {
		// Fewer than two firings observed in the representative
		// window (a schedule sparser than monthly): fall back to the
		// window length itself as a conservative stand-in for the gap.
		gap = cronGapWindow
	}
	switch {
	case gap <= 24*time.Hour:
		return DefaultMaxAgeNightly
	case gap <= 7*24*time.Hour:
		return DefaultMaxAgeWeekly
	default:
		return gap + 2*24*time.Hour
	}
}

// maxFiringGap walks the firings of sched in the cronGapReference
// window (both ends included) and returns the largest gap between
// consecutive ones. ok is false when fewer than two firings occurred in
// the window, in which case gap is not meaningful. The window is UTC,
// so Schedule.Next visits exactly the minutes Schedule.Matches accepts,
// skipping whole months, days and hours that cannot fire.
func maxFiringGap(sched scheduler.Schedule) (gap time.Duration, ok bool) {
	var last time.Time
	count := 0
	end := cronGapReference.Add(cronGapWindow)
	for t, more := sched.Next(cronGapReference.Add(-time.Minute)); more && !t.After(end); t, more = sched.Next(t) {
		count++
		if count > 1 {
			if d := t.Sub(last); d > gap {
				gap = d
			}
		}
		last = t
	}
	return gap, count > 1
}

// DigestService is one service's line in the weekly digest.
type DigestService struct {
	Name string
	// Light is the service's traffic light: green, amber, red, running
	// or none (never run).
	Light string
	// VerifiedAge is how long ago the last verified backup finished,
	// or nil if there never was one.
	VerifiedAge *time.Duration
}

// FormatWeeklyDigest builds the once-a-week "Ballast is alive" summary:
// how many services are green, amber and red, how old each service's
// last verified restore is, and the free disk space for verification
// drills (disk may be empty when unknown). The worst light sets the
// severity, so a digest with a red service is not sent as low priority.
func FormatWeeklyDigest(services []DigestService, disk string) Event {
	counts := map[string]int{}
	var lines []string
	for _, s := range services {
		light := s.Light
		if light == "" {
			light = "none"
		}
		counts[light]++
		age := "no verified backup yet"
		if s.VerifiedAge != nil {
			age = "last verified restore " + formatDuration(*s.VerifiedAge) + " ago"
		}
		lines = append(lines, fmt.Sprintf("%s: %s, %s", s.Name, light, age))
	}
	if len(services) == 0 {
		lines = append(lines, "No services are set up yet.")
	}
	if disk != "" {
		lines = append(lines, "Disk space: "+disk)
	}
	severity := "info"
	switch {
	case counts["red"] > 0:
		severity = "error"
	case counts["amber"] > 0 || counts["none"] > 0:
		severity = "warning"
	}
	msg := fmt.Sprintf("Weekly digest: %d green, %d amber, %d red", counts["green"], counts["amber"], counts["red"])
	if n := counts["none"]; n > 0 {
		msg += fmt.Sprintf(", %d never backed up", n)
	}
	return Event{
		Service:   "ballast",
		Type:      EventWeeklyDigest,
		Message:   msg,
		Detail:    strings.Join(lines, "\n"),
		Severity:  severity,
		Timestamp: time.Now().UTC(),
	}
}
