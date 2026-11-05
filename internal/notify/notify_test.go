package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
			wantTitle:  "[immich] [restic] job_failure",
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
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Priority") }))
			defer srv.Close()
			if err := NewNtfyChannel(srv.URL+"/t").Send(context.Background(), Event{Service: "s", Type: EventJobFailure, Severity: tt.severity}); err != nil {
				t.Fatal(err)
			}
			if got != tt.wantPrio {
				t.Errorf("Priority for %q = %q, want %q", tt.severity, got, tt.wantPrio)
			}
		})
	}
}

// TestNtfyTitlesAndBody pins what ntfy users see: the title Ballast
// has always sent (now via keel's Event.Title) and the body.
func TestNtfyTitlesAndBody(t *testing.T) {
	tests := []struct {
		name      string
		event     Event
		wantTitle string
		wantBody  string
	}{
		{"primary tier", Event{Service: "immich", Type: EventJobFailure, Message: "m", Detail: "d"}, "[immich] [restic] job_failure", "m\nd"},
		{"secondary tier", Event{Service: "immich", Type: EventJobFailure, Message: "m"}.ForTier("rclone"), "[immich] [rclone] job_failure", "[rclone copy] m"},
		{"test", Event{Service: "ballast", Type: EventTest, Message: "Ballast test notification"}, "Ballast test notification", "Ballast test notification"},
		{"digest", Event{Service: "ballast", Type: EventWeeklyDigest, Message: "w"}, "Ballast weekly digest", "w"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var title, body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				title = r.Header.Get("Title")
				b, _ := io.ReadAll(r.Body)
				body = string(b)
			}))
			defer srv.Close()
			if err := NewNtfyChannel(srv.URL+"/t").Send(context.Background(), tt.event); err != nil {
				t.Fatal(err)
			}
			if title != tt.wantTitle || body != tt.wantBody {
				t.Errorf("title, body = %q, %q; want %q, %q", title, body, tt.wantTitle, tt.wantBody)
			}
		})
	}
}

// preB2WebhookPayload is the webhook body Ballast sent before
// delivery moved to keel (internal/notify/notify.go at b41ee01).
type preB2WebhookPayload struct {
	Service   string    `json:"service"`
	Tier      string    `json:"tier"`
	Type      EventType `json:"type"`
	Severity  string    `json:"severity"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail"`
	Timestamp time.Time `json:"timestamp"`
}

// TestWebhookPayloadShape pins the webhook body to the pre-keel
// payload (same keys and values, tier included for every tier) and
// the signature header name.
func TestWebhookPayloadShape(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := Event{Service: "immich", Type: EventJobFailure, Message: "m", Detail: "d", Severity: "error", Timestamp: ts}
	for _, ev := range []Event{base, base.ForTier("rclone")} {
		t.Run(ev.TierName(), func(t *testing.T) {
			var body []byte
			var sigHeaders []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				for k := range r.Header {
					if strings.Contains(strings.ToLower(k), "signature") {
						sigHeaders = append(sigHeaders, k)
					}
				}
			}))
			defer srv.Close()
			w := NewWebhookChannel(srv.URL)
			w.Secret = "k"
			if err := w.Send(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
			old, err := json.Marshal(preB2WebhookPayload{Service: ev.Service, Tier: ev.TierName(), Type: ev.Type, Severity: ev.Severity, Message: ev.Message, Detail: ev.Detail, Timestamp: ev.Timestamp})
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("body %s: %v", body, err)
			}
			if err := json.Unmarshal(old, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("payload = %s\nwant (pre-keel) %s", body, old)
			}
			if len(sigHeaders) != 1 || sigHeaders[0] != "X-Ballast-Signature" {
				t.Errorf("signature headers = %v, want [X-Ballast-Signature]", sigHeaders)
			}
		})
	}
}

func TestWebhookLastStatus(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		w := NewWebhookChannel(srv.URL)
		w.Client = srv.Client()
		_ = w.Send(context.Background(), Event{Service: "s"})
		srv.Close()
		if w.lastStatus != code {
			t.Errorf("lastStatus = %d, want %d", w.lastStatus, code)
		}
		// A transport failure clears it.
		_ = w.Send(context.Background(), Event{Service: "s"})
		if w.lastStatus != 0 {
			t.Errorf("after a failed connection lastStatus = %d, want 0", w.lastStatus)
		}
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
				_ = json.NewDecoder(r.Body).Decode(&gotPayload)
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

// TestNotifier_Resolve: a service with no channel registered up front
// (e.g. one added from the web UI after `ballast serve` started) is
// looked up at send time, and a lookup failure or empty result is
// reported, never treated as delivered.
func TestNotifier_Resolve(t *testing.T) {
	lookupErr := errors.New("credential file unreadable")
	tests := []struct {
		name      string
		resolve   func(string) ([]Channel, error)
		wantSent  int
		wantErrIs error
	}{
		{
			name:     "resolved channel receives the event",
			resolve:  nil, // filled in below: needs the shared sent slice
			wantSent: 1,
		},
		{
			name:      "lookup error is returned",
			resolve:   func(string) ([]Channel, error) { return nil, lookupErr },
			wantErrIs: lookupErr,
		},
		{
			name:      "no channels found is still ErrNoChannels",
			resolve:   func(string) ([]Channel, error) { return nil, nil },
			wantErrIs: ErrNoChannels,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent []Event
			n := NewNotifier()
			n.Resolve = tt.resolve
			if n.Resolve == nil {
				n.Resolve = func(svc string) ([]Channel, error) {
					if svc != "added-later" {
						t.Errorf("Resolve(%q), want added-later", svc)
					}
					return []Channel{&mockChannel{events: &sent}}, nil
				}
			}
			err := n.Send(context.Background(), Event{Service: "added-later", Type: EventJobFailure, Timestamp: time.Now().UTC()})
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Errorf("Send() error = %v, want %v", err, tt.wantErrIs)
				}
			} else if err != nil {
				t.Errorf("Send() error = %v", err)
			}
			if len(sent) != tt.wantSent {
				t.Errorf("sent %d events, want %d", len(sent), tt.wantSent)
			}
		})
	}
}

// TestNotifier_RegisteredChannelsSkipResolve: up-front registration
// wins; Resolve is only a fallback.
func TestNotifier_RegisteredChannelsSkipResolve(t *testing.T) {
	var sent []Event
	n := NewNotifier()
	n.RegisterChannel("svc", &mockChannel{events: &sent})
	n.Resolve = func(string) ([]Channel, error) {
		t.Error("Resolve called for a service with registered channels")
		return nil, nil
	}
	if err := n.Send(context.Background(), Event{Service: "svc", Type: EventJobFailure, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if len(sent) != 1 {
		t.Errorf("sent %d events, want 1", len(sent))
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

// maxFiringGapByMinute is the original minute-by-minute scan of the
// cronGapReference window, kept as the reference maxFiringGap must
// agree with.
func maxFiringGapByMinute(sched scheduler.Schedule) (gap time.Duration, ok bool) {
	var last time.Time
	count := 0
	end := cronGapReference.Add(cronGapWindow)
	for t := cronGapReference; !t.After(end); t = t.Add(time.Minute) {
		if !sched.Matches(t) {
			continue
		}
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

// TestMaxFiringGapMatchesMinuteScan pins that jumping from firing to
// firing (spec §22.3, the fast status page) finds exactly the gap the
// minute-by-minute scan found, so no service's max age changes.
func TestMaxFiringGapMatchesMinuteScan(t *testing.T) {
	if testing.Short() {
		t.Skip("minute-by-minute reference scan skipped in -short mode")
	}
	for _, expr := range []string{
		"* * * * *", "*/15 * * * *", "0 * * * *", "30 */6 * * *", "0 3 * * *",
		"59 23 * * *", "0 0 * * *", "0 3 * * 1-5", "0 3 * * 1", "0 3 * * 0,6",
		"0 3 1 * *", "0 3 31 * *", "0 3 29 2 *", "0 3 1 1 *", "0 3 15 * 1",
		"0 3 */10 * *", "5,35 1-4 * 3-9 *", "0 12 1-7 * 0",
	} {
		t.Run(expr, func(t *testing.T) {
			sched, err := scheduler.ParseCron(expr)
			if err != nil {
				t.Fatalf("ParseCron: %v", err)
			}
			gotGap, gotOK := maxFiringGap(sched)
			wantGap, wantOK := maxFiringGapByMinute(sched)
			if gotGap != wantGap || gotOK != wantOK {
				t.Errorf("maxFiringGap(%q) = %v, %v; minute scan = %v, %v", expr, gotGap, gotOK, wantGap, wantOK)
			}
		})
	}
}

// BenchmarkMaxFiringGap measures one maxFiringGap call over the 400-day
// cronGapWindow. Status derives every service's max age with it, so it
// must stay far below a millisecond (spec §22.3).
// Run with: go test ./internal/notify/ -bench MaxFiringGap -run ^$
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

// recordingChannel records every event it is sent and can fail on demand.
type recordingChannel struct {
	got  []Event
	fail map[string]bool // tier -> fail
}

func (r *recordingChannel) Send(_ context.Context, e Event) error {
	r.got = append(r.got, e)
	if r.fail[e.TierName()] {
		return errors.New("boom")
	}
	return nil
}
func (r *recordingChannel) Type() string { return "rec" }

func TestSendTiersIsolationAndOrder(t *testing.T) {
	tests := []struct {
		name      string
		fail      map[string]bool
		wantErrIn []string
	}{
		{"all ok", nil, nil},
		{"secondary fails, primary still delivered", map[string]bool{"rclone": true}, []string{"rclone tier"}},
		{"primary fails, secondary still delivered", map[string]bool{"restic": true}, []string{"restic tier"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &recordingChannel{fail: tt.fail}
			n := NewNotifier()
			n.RegisterChannel("svc", ch)
			base := FormatJobFailure("svc", "1", "disk full")
			err := n.SendTiers(context.Background(), []Event{base.ForTier("rclone"), base.ForTier("restic")})
			if len(ch.got) != 2 {
				t.Fatalf("delivered %d events, want 2 (no tier may block another)", len(ch.got))
			}
			if ch.got[0].TierName() != "restic" || ch.got[1].TierName() != "rclone" {
				t.Errorf("order = %s,%s; want primary first", ch.got[0].TierName(), ch.got[1].TierName())
			}
			if strings.Contains(ch.got[0].Message, "copy]") || !strings.Contains(ch.got[1].Message, "[rclone copy]") {
				t.Errorf("tier wording not separated: %q / %q", ch.got[0].Message, ch.got[1].Message)
			}
			if len(tt.wantErrIn) == 0 && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			for _, w := range tt.wantErrIn {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want mention of %q", err, w)
				}
			}
		})
	}
}

func TestFormatWeeklyDigest(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		name         string
		services     []DigestService
		disk         string
		wantMessage  string
		wantSeverity string
		wantDetail   []string
	}{
		{"all green", []DigestService{{Name: "a", Light: "green", VerifiedAge: &day}}, "40.0 GiB free",
			"Weekly digest: 1 green, 0 amber, 0 red", "info", []string{"a: green, last verified restore 1 days ago", "Disk space: 40.0 GiB free"}},
		{"red wins", []DigestService{{Name: "a", Light: "amber"}, {Name: "b", Light: "red"}}, "",
			"Weekly digest: 0 green, 1 amber, 1 red", "error", []string{"b: red, no verified backup yet"}},
		{"never run", []DigestService{{Name: "a"}}, "",
			"Weekly digest: 0 green, 0 amber, 0 red, 1 never backed up", "warning", []string{"a: none"}},
		{"no services", nil, "", "Weekly digest: 0 green, 0 amber, 0 red", "info", []string{"No services are set up yet."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := FormatWeeklyDigest(tt.services, tt.disk)
			if ev.Type != EventWeeklyDigest || ev.Message != tt.wantMessage || ev.Severity != tt.wantSeverity {
				t.Errorf("event = %+v", ev)
			}
			for _, w := range tt.wantDetail {
				if !strings.Contains(ev.Detail, w) {
					t.Errorf("detail %q missing %q", ev.Detail, w)
				}
			}
		})
	}
}

func TestNtfyDigestTitle(t *testing.T) {
	var title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { title = r.Header.Get("Title") }))
	defer srv.Close()
	if err := NewNtfyChannel(srv.URL+"/t").Send(context.Background(), FormatWeeklyDigest(nil, "")); err != nil {
		t.Fatal(err)
	}
	if title != "Ballast weekly digest" {
		t.Errorf("title = %q", title)
	}
}
