package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/springledev/keel/secret"
)

// secretErrChannel fails with an error that (wrongly) carries a secret
// URL, to prove the decision log scrubs it.
type secretErrChannel struct{}

func (secretErrChannel) Send(ctx context.Context, e Event) error {
	return errors.New(`Post "https://ntfy.example.com/my-secret-topic": dial tcp: connection refused`)
}
func (secretErrChannel) Type() string { return "ntfy" }

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

func TestAlertDecisions_SentHeldFailed(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	buf := captureLog(t)

	now := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ctx := context.Background()

	// Sent.
	n := NewNotifier()
	n.RegisterChannel("svc", &fakeChannel{})
	a := &EngineAdapter{DB: db.DB(), Notifier: n, Now: clock}
	if err := a.NotifyVerificationFailure(ctx, "svc", 1, "bad"); err != nil {
		t.Fatal(err)
	}
	// Held, 23h later.
	now = now.Add(23 * time.Hour)
	if err := a.NotifyVerificationFailure(ctx, "svc", 2, "bad"); err != nil {
		t.Fatal(err)
	}
	// Failed: past the cool-down, channel errors with a secret URL.
	now = now.Add(2 * time.Hour)
	n2 := NewNotifier()
	n2.RegisterChannel("svc", secretErrChannel{})
	a2 := &EngineAdapter{DB: db.DB(), Notifier: n2, Now: clock}
	if err := a2.NotifyJobFailure(ctx, "svc", 3, "bad"); err == nil {
		t.Fatal("want delivery error")
	}

	ds, err := ListAlertDecisions(db.DB(), "svc", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 {
		t.Fatalf("decisions = %d, want 3: %+v", len(ds), ds)
	}
	// Newest first.
	want := []struct{ typ, outcome string }{
		{"job_failure", "failed"},
		{"verification_failure", "held"},
		{"verification_failure", "sent"},
	}
	for i, w := range want {
		if ds[i].Type != w.typ || ds[i].Outcome != w.outcome {
			t.Errorf("decision %d = %s/%s, want %s/%s", i, ds[i].Type, ds[i].Outcome, w.typ, w.outcome)
		}
	}
	if !strings.Contains(ds[1].Reason, "alerted 23h ago, cool-down 24h") {
		t.Errorf("held reason = %q", ds[1].Reason)
	}

	logged := buf.String()
	for _, frag := range []string{"sent verification_failure for svc", "held back", "alerted 23h ago, cool-down 24h", "failed"} {
		if !strings.Contains(logged, frag) {
			t.Errorf("log missing %q:\n%s", frag, logged)
		}
	}
	// No secret in the log or the store.
	all := logged
	for _, d := range ds {
		all += d.Reason + d.Type + d.Outcome
	}
	for _, s := range []string{"my-secret-topic", "ntfy.example.com", "https://"} {
		if strings.Contains(all, s) {
			t.Errorf("secret fragment %q leaked:\n%s", s, all)
		}
	}
}

func TestAlertDecisions_KeepsOnlyLastFew(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	for i := 0; i < maxAlertDecisions+5; i++ {
		err := storeDecision(db.DB(), AlertDecision{
			Service: "svc", At: time.Unix(int64(1000+i), 0).UTC(),
			Type: "job_failure", Outcome: "sent",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ds, err := ListAlertDecisions(db.DB(), "svc", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != maxAlertDecisions {
		t.Errorf("kept %d, want %d", len(ds), maxAlertDecisions)
	}
}

// bareSecretChannel fails with an error that echoes a token and a bare
// topic with no scheme in front of them.
type bareSecretChannel struct{}

func (bareSecretChannel) Send(ctx context.Context, e Event) error {
	return errors.New("401: invalid token s3cr3t-abc for topic my-bare-topic")
}
func (bareSecretChannel) Type() string { return "ntfy" }

func TestAlertDecisions_RedactsChannelSecretsWithoutScheme(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	buf := captureLog(t)

	n := NewNotifier()
	n.RegisterChannel("svc", bareSecretChannel{})
	a := &EngineAdapter{
		DB: db.DB(), Notifier: n,
		Redact: secret.NewSet("s3cr3t-abc", "my-bare-topic"),
		Now:    func() time.Time { return time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC) },
	}
	if err := a.NotifyJobFailure(context.Background(), "svc", 1, "bad"); err == nil {
		t.Fatal("want delivery error")
	}
	ds, err := ListAlertDecisions(db.DB(), "svc", 10)
	if err != nil || len(ds) != 1 {
		t.Fatalf("decisions = %+v, %v", ds, err)
	}
	js, _ := json.Marshal(ds)
	for name, text := range map[string]string{"log": buf.String(), "db": ds[0].Reason, "api json": string(js)} {
		for _, s := range []string{"s3cr3t-abc", "my-bare-topic"} {
			if strings.Contains(text, s) {
				t.Errorf("%s leaks %q: %s", name, s, text)
			}
		}
	}
	if !strings.Contains(ds[0].Reason, "401") {
		t.Errorf("reason lost its useful part: %q", ds[0].Reason)
	}
}

func TestScrubReason_CutsOnRuneBoundary(t *testing.T) {
	got := scrubReason(strings.Repeat("é", 200)) // 400 bytes
	if !utf8.ValidString(got) {
		t.Fatalf("cut split a character: %q", got)
	}
	if len(got) > 310 {
		t.Errorf("not shortened: %d bytes", len(got))
	}
}
