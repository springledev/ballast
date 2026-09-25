package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/scheduler"
)

// mockChannel is a test implementation of Channel that records every
// event it receives.
type mockChannel struct {
	events *[]Event
}

func (m *mockChannel) Send(ctx context.Context, e Event) error {
	*m.events = append(*m.events, e)
	return nil
}

func (m *mockChannel) Type() string { return "mock" }

// erroringChannel always fails with a fixed error, for testing that
// Notifier.Send joins every channel's failure rather than dropping any
// of them.
type erroringChannel struct {
	err error
	typ string
}

func (e *erroringChannel) Send(ctx context.Context, event Event) error { return e.err }
func (e *erroringChannel) Type() string                                { return e.typ }

func TestNtfyChannel_Send(t *testing.T) {
	tests := []struct {
		name       string
		topic      string
		event      Event
		statusCode int
		wantErr    bool
		wantTitle  string
	}{
		{
			name:  "success",
			topic: "https://ntfy.sh/test-topic",
			event: Event{
				Service:   "immich",
				Type:      EventJobFailure,
				Message:   "Backup failed: database lock",
				Detail:    "Run ID: 123",
				Severity:  "error",
				Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
			},
			statusCode: 200,
			wantErr:    false,
			wantTitle:  "[immich] job_failure",
		},
		{
			name:    "empty topic",
			topic:   "",
			event:   Event{Service: "test", Type: EventJobFailure},
			wantErr: true,
		},
		{
			name:       "http error",
			topic:      "https://ntfy.sh/test-topic",
			statusCode: 500,
			event: Event{
				Service: "test",
				Type:    EventJobFailure,
				Message: "Backup failed",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotReq *http.Request
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotReq = r
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			if tt.topic != "" {
				tt.topic = server.URL + "/test-topic"
			}

			ch := NewNtfyChannel(tt.topic)
			ch.Client = server.Client()

			err := ch.Send(context.Background(), tt.event)
			if (err != nil) != tt.wantErr {
				t.Errorf("Send() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr {
				return
			}

			if got := gotReq.Header.Get("Title"); got != tt.wantTitle {
				t.Errorf("Title = %q, want %q", got, tt.wantTitle)
			}
		})
	}
}

func TestNtfyChannel_PriorityMapping(t *testing.T) {
	tests := []struct {
		severity string
		wantPrio string
	}{
		{"error", "high"},
		{"warning", "default"},
		{"info", "low"},
		{"", "low"},
	}

	for _, tt := range tests {
		t.Run(tt.severity, func(t *testing.T) {
			got := priorityFromSeverity(tt.severity)
			if got != tt.wantPrio {
				t.Errorf("priorityFromSeverity(%q) = %q, want %q", tt.severity, got, tt.wantPrio)
			}
		})
	}
}

func TestWebhookChannel_Send(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		event      Event
		statusCode int
		wantErr    bool
	}{
		{
			name: "success",
			url:  "https://example.com/webhook",
			event: Event{
				Service:   "vaultwarden",
				Type:      EventVerificationFailure,
				Message:   "Archive check failed",
				Detail:    "tar: checksum mismatch",
				Severity:  "error",
				Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
			},
			statusCode: 200,
			wantErr:    false,
		},
		{
			name:    "empty url",
			url:     "",
			event:   Event{Service: "test", Type: EventJobFailure},
			wantErr: true,
		},
		{
			name:       "http error",
			url:        "https://example.com/webhook",
			statusCode: 500,
			event:      Event{Service: "test", Type: EventJobFailure, Message: "Backup failed"},
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPayload map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&gotPayload)
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			if tt.url != "" {
				tt.url = server.URL + "/webhook"
			}

			ch := NewWebhookChannel(tt.url)
			ch.Client = server.Client()

			err := ch.Send(context.Background(), tt.event)
			if (err != nil) != tt.wantErr {
				t.Errorf("Send() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestChannelSend_NeverLeaksSecretURL pins finding 1 (BLOCKER): a
// transport-level failure (closed port, unparseable URL) wraps the
// request URL into a *url.Error whose Error() text embeds the full
// URL. For a Channel, that URL is the ntfy topic or webhook URL — a
// secret from a root-only credential file (spec rule 2) — so it must
// never survive into the error Send returns. Exercised against both
// channel implementations and both failure shapes.
func TestChannelSend_NeverLeaksSecretURL(t *testing.T) {
	const secret = "s3cret-topic-9f8e7d6c2b1a"

	newChannel := map[string]func(url string) Channel{
		"ntfy":    func(url string) Channel { return NewNtfyChannel(url) },
		"webhook": func(url string) Channel { return NewWebhookChannel(url) },
	}

	for chName, newCh := range newChannel {
		t.Run(chName, func(t *testing.T) {
			t.Run("closed port", func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
				secretURL := server.URL + "/" + secret
				server.Close() // the port now refuses connections

				err := newCh(secretURL).Send(context.Background(), Event{Service: "svc", Type: EventJobFailure})
				if err == nil {
					t.Fatal("want an error from a closed port")
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks the secret URL: %v", err)
				}
			})

			t.Run("unparseable URL", func(t *testing.T) {
				// A raw control character (newline) in the URL makes
				// net/url refuse to parse it, before any connection is
				// even attempted.
				secretURL := "http://example.com/" + secret + "\n"

				err := newCh(secretURL).Send(context.Background(), Event{Service: "svc", Type: EventJobFailure})
				if err == nil {
					t.Fatal("want an error from an unparseable URL")
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks the secret URL: %v", err)
				}
			})
		})
	}
}

// TestChannelSend_ResponseBodyNotEchoed pins finding 2: a non-2xx
// response's body must never be echoed into the returned error — only
// the status code — since a server behind a secret ntfy/webhook URL
// could otherwise leak arbitrary response content (or simply noise)
// into logs.
func TestChannelSend_ResponseBodyNotEchoed(t *testing.T) {
	const bodyMarker = "response-body-should-never-appear-in-error"

	newChannel := map[string]func(url string) Channel{
		"ntfy":    func(url string) Channel { return NewNtfyChannel(url) },
		"webhook": func(url string) Channel { return NewWebhookChannel(url) },
	}

	for chName, newCh := range newChannel {
		t.Run(chName, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(bodyMarker))
			}))
			defer server.Close()

			err := newCh(server.URL).Send(context.Background(), Event{Service: "svc", Type: EventJobFailure})
			if err == nil {
				t.Fatal("want an error from a 500 response")
			}
			if strings.Contains(err.Error(), bodyMarker) {
				t.Errorf("error echoes the response body: %v", err)
			}
			if !strings.Contains(err.Error(), "500") {
				t.Errorf("error = %q, want it to at least report the status code", err.Error())
			}
		})
	}
}

func TestNotifier_MultipleChannels(t *testing.T) {
	var sent []Event
	ch1 := &mockChannel{events: &sent}
	ch2 := &mockChannel{events: &sent}

	notifier := NewNotifier()
	notifier.RegisterChannel("immich", ch1)
	notifier.RegisterChannel("immich", ch2)

	event := Event{
		Service:   "immich",
		Type:      EventJobFailure,
		Message:   "test",
		Severity:  "error",
		Timestamp: time.Now().UTC(),
	}

	err := notifier.Send(context.Background(), event)
	if err != nil {
		t.Errorf("Send() error = %v", err)
	}

	if len(sent) != 2 {
		t.Errorf("expected 2 sent events, got %d", len(sent))
	}
}

// TestNotifier_Send_JoinsAllChannelErrors pins finding 3: when
// multiple channels are registered for a service and more than one
// fails, Send must join every failure (via errors.Join) rather than
// reporting only the first or last — one channel's outage must never
// hide another's, mirroring the isolation-of-failure principle spec
// rule 3 requires between backup tiers.
func TestNotifier_Send_JoinsAllChannelErrors(t *testing.T) {
	err1 := errors.New("channel one is down")
	err2 := errors.New("channel two is down")

	notifier := NewNotifier()
	notifier.RegisterChannel("svc", &erroringChannel{err: err1, typ: "one"})
	notifier.RegisterChannel("svc", &erroringChannel{err: err2, typ: "two"})

	event := Event{Service: "svc", Type: EventJobFailure, Message: "test", Severity: "error", Timestamp: time.Now().UTC()}
	err := notifier.Send(context.Background(), event)
	if err == nil {
		t.Fatal("Send() error = nil, want a joined error from both failing channels")
	}
	if !errors.Is(err, err1) {
		t.Errorf("Send() error = %v, want it to wrap channel one's error %v", err, err1)
	}
	if !errors.Is(err, err2) {
		t.Errorf("Send() error = %v, want it to wrap channel two's error %v", err, err2)
	}
}

// TestNotifier_UnregisteredService pins finding 6: a service with no
// registered channel at all must return ErrNoChannels, not nil — a
// caller like SweepFreshness must be able to tell "nothing was
// listening" apart from a successful delivery.
func TestNotifier_UnregisteredService(t *testing.T) {
	notifier := NewNotifier()

	event := Event{
		Service:   "unknown",
		Type:      EventJobFailure,
		Message:   "test",
		Severity:  "error",
		Timestamp: time.Now().UTC(),
	}

	err := notifier.Send(context.Background(), event)
	if !errors.Is(err, ErrNoChannels) {
		t.Errorf("Send() error = %v, want ErrNoChannels", err)
	}
}

func TestFormatJobFailure(t *testing.T) {
	event := FormatJobFailure("immich", "456", "database locked")

	if event.Service != "immich" {
		t.Errorf("Service = %q, want immich", event.Service)
	}
	if event.Type != EventJobFailure {
		t.Errorf("Type = %v, want EventJobFailure", event.Type)
	}
	if event.Severity != "error" {
		t.Errorf("Severity = %q, want error", event.Severity)
	}
	if !strings.Contains(event.Message, "immich") || !strings.Contains(event.Message, "database locked") {
		t.Errorf("Message %q doesn't contain service name and error", event.Message)
	}
}

func TestFormatVerificationFailure(t *testing.T) {
	event := FormatVerificationFailure("postgres", "789", "container-restore", "ERROR: duplicate key")

	if event.Service != "postgres" {
		t.Errorf("Service = %q, want postgres", event.Service)
	}
	if event.Type != EventVerificationFailure {
		t.Errorf("Type = %v, want EventVerificationFailure", event.Type)
	}
	if event.Severity != "error" {
		t.Errorf("Severity = %q, want error", event.Severity)
	}
	if !strings.Contains(event.Message, "postgres") {
		t.Errorf("Message should contain service name, got %q", event.Message)
	}
	if !strings.Contains(event.Detail, "789") {
		t.Errorf("Detail should contain the run ID, got %q", event.Detail)
	}
	if !strings.Contains(event.Detail, "duplicate key") {
		t.Errorf("Detail should contain error detail, got %q", event.Detail)
	}
}

func TestFormatFreshnessBreached(t *testing.T) {
	maxAge := 48 * time.Hour
	actualAge := 72 * time.Hour

	event := FormatFreshnessBreached("vaultwarden", maxAge, actualAge)

	if event.Service != "vaultwarden" {
		t.Errorf("Service = %q, want vaultwarden", event.Service)
	}
	if event.Type != EventFreshnessBreached {
		t.Errorf("Type = %v, want EventFreshnessBreached", event.Type)
	}
	if event.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", event.Severity)
	}
	if !strings.Contains(event.Message, "vaultwarden") {
		t.Errorf("Message should contain service name, got %q", event.Message)
	}
}

func TestFormatSuccessHeartbeat(t *testing.T) {
	event := FormatSuccessHeartbeat("immich", 24*time.Hour)

	if event.Service != "immich" {
		t.Errorf("Service = %q, want immich", event.Service)
	}
	if event.Type != EventSuccessHeartbeat {
		t.Errorf("Type = %v, want EventSuccessHeartbeat", event.Type)
	}
	if event.Severity != "info" {
		t.Errorf("Severity = %q, want info", event.Severity)
	}
	if !strings.Contains(event.Message, "immich") {
		t.Errorf("Message should contain service name, got %q", event.Message)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input  time.Duration
		output string
	}{
		{0, "0"},
		{neverBackedUpAge, "never"},
		{1 * time.Hour, "1 hours"},
		{24 * time.Hour, "1 days"},
		{48 * time.Hour, "2 days"},
		{25 * time.Hour, "1 days 1 hours"},
		{72*time.Hour + 2*time.Hour, "3 days 2 hours"},
		{30 * time.Minute, "30 minutes"},
		{45 * time.Second, "< 1 minute"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.input)
		if got != tt.output {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.input, got, tt.output)
		}
	}
}

// TestDefaultMaxAge pins finding 7: the default max-age is derived
// from the largest gap between a cron schedule's actual firings (via
// internal/scheduler's cron parser), not from string-matching the
// expression's day-of-week field.
func TestDefaultMaxAge(t *testing.T) {
	tests := []struct {
		name string
		cron string
		want time.Duration
	}{
		{name: "hourly", cron: "0 * * * *", want: DefaultMaxAgeNightly},
		{name: "daily", cron: "0 3 * * *", want: DefaultMaxAgeNightly},
		// Every weekday: the largest gap is Friday->Monday (3 days),
		// which is <=7 days and so maps to the weekly default. This is
		// conservative, not exact — see DefaultMaxAge's doc comment.
		{name: "weekdays", cron: "0 3 * * 1-5", want: DefaultMaxAgeWeekly},
		{name: "weekly (every Monday)", cron: "0 3 * * 1", want: DefaultMaxAgeWeekly},
		{name: "invalid cron falls back to nightly", cron: "not a cron expression", want: DefaultMaxAgeNightly},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DefaultMaxAge(tt.cron)
			if got != tt.want {
				t.Errorf("DefaultMaxAge(%q) = %v, want %v", tt.cron, got, tt.want)
			}
		})
	}

	// Monthly fires roughly every 28-31 days, well past the weekly
	// default's 7-day threshold, so it must fall into the "otherwise"
	// branch (gap + 2 days) rather than being capped at the weekly
	// default.
	t.Run("monthly exceeds the weekly default", func(t *testing.T) {
		got := DefaultMaxAge("0 3 1 * *")
		if got <= DefaultMaxAgeWeekly {
			t.Errorf("DefaultMaxAge(monthly) = %v, want > %v (the weekly default)", got, DefaultMaxAgeWeekly)
		}
	})

	// Regression for finding: day-of-month 31 only exists in seven
	// months, so its largest real gap (e.g. Aug 31 -> Oct 31, skipping
	// September) is ~61 days. A scan window too narrow to see that
	// second firing (the old 40-day cronGapWindow) undercounted the
	// gap and produced a max-age far below the schedule's real
	// cadence, so the freshness sweep would false-alarm every time
	// that ~61-day gap was crossed. The window must be wide enough
	// (see cronGapWindow) to see this schedule fire at least twice.
	t.Run("day 31 monthly: gap spans a skipped 30-day month", func(t *testing.T) {
		got := DefaultMaxAge("0 3 31 * *")
		want := 60*24*time.Hour + 2*24*time.Hour // real gap (~61d) + the 2-day slack
		if got < want {
			t.Errorf("DefaultMaxAge(%q) = %v, want >= %v (real gap ~61 days + 2 days slack)", "0 3 31 * *", got, want)
		}
	})

	// Regression for the same finding, with day-of-month 30: February
	// never has a 30th, so this schedule skips February every year.
	// The gap must reflect that skipped month, not the old
	// narrow-window fallback (which would have picked the window
	// length itself, or worse, treated it as firing monthly with no
	// gap at all).
	t.Run("day 30 monthly: February has no 30th", func(t *testing.T) {
		got := DefaultMaxAge("0 3 30 * *")
		if got <= DefaultMaxAgeWeekly {
			t.Errorf("DefaultMaxAge(%q) = %v, want > %v (gap must reflect the skipped February)", "0 3 30 * *", got, DefaultMaxAgeWeekly)
		}
	})
}

// BenchmarkMaxFiringGap measures the cost of the minute-by-minute scan
// over cronGapWindow (400 days, ~576,000 Schedule.Matches calls per
// call to maxFiringGap), to confirm widening the window (finding: a
// 40-day window undercounted gaps for schedules like "0 3 31 * *")
// stays cheap. Run with: go test ./internal/notify/ -bench MaxFiringGap -run ^$
func BenchmarkMaxFiringGap(b *testing.B) {
	sched, err := scheduler.ParseCron("0 3 31 * *")
	if err != nil {
		b.Fatalf("ParseCron: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		maxFiringGap(sched)
	}
}
