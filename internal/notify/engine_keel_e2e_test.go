package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/store"
)

// webhookSink is a test webhook receiver that records every delivery.
type webhookSink struct {
	mu     sync.Mutex
	bodies [][]byte
	sigs   []string
}

func (w *webhookSink) handler(rw http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	w.bodies = append(w.bodies, b)
	w.sigs = append(w.sigs, r.Header.Get("X-Ballast-Signature"))
	w.mu.Unlock()
	rw.WriteHeader(http.StatusOK)
}

func (w *webhookSink) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.bodies)
}

// walkRun drives a real run through the state machine, recording the
// state after every transition, and returns the machine and the path.
func walkRun(t *testing.T, m *engine.Machine, steps ...engine.State) []engine.State {
	t.Helper()
	path := []engine.State{m.Run().State}
	for _, s := range steps {
		if err := m.Transition(s, "detail"); err != nil {
			t.Fatalf("transition to %s: %v", s, err)
		}
		path = append(path, m.Run().State)
	}
	return path
}

// TestEngineToKeelWebhookEndToEnd proves, through the real state
// machine, the real EngineAdapter and the keel-backed webhook channel,
// that (1) a run only reaches green SUCCESS by passing VERIFYING, (2) a
// verification failure is delivered as a signed webhook that still
// names the backup tier and uses Ballast's signature header, and (3)
// neither a verified success nor an unverified (amber) one raises a
// failure alert.
func TestEngineToKeelWebhookEndToEnd(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	sink := &webhookSink{}
	srv := httptest.NewServer(http.HandlerFunc(sink.handler))
	t.Cleanup(srv.Close)

	const secretKey = "test-webhook-secret"
	notifier := NewNotifier()
	hook := NewWebhookChannel(srv.URL)
	hook.Secret = secretKey
	notifier.RegisterChannel("e2e-svc", hook)
	adapter := &EngineAdapter{DB: db.DB(), Notifier: notifier}

	newRun := func() *engine.Machine {
		m, err := engine.StartRun(db.DB(), "e2e-svc")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		m.SetNotifier(context.Background(), adapter)
		return m
	}
	upTo := []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING}

	t.Run("verified success passes through VERIFYING and sends no failure alert", func(t *testing.T) {
		before := sink.count()
		path := walkRun(t, newRun(), append(upTo, engine.SUCCESS)...)
		if got := path[len(path)-2]; got != engine.VERIFYING {
			t.Errorf("state before SUCCESS = %s, want VERIFYING (path %v)", got, path)
		}
		if got := path[len(path)-1]; got != engine.SUCCESS {
			t.Errorf("final state = %s, want SUCCESS", got)
		}
		if n := sink.count() - before; n != 0 {
			t.Errorf("a verified success sent %d webhooks, want 0 (heartbeat is opt-in)", n)
		}
	})

	t.Run("unverifiable run is amber and sends no failure alert", func(t *testing.T) {
		before := sink.count()
		m := newRun()
		path := walkRun(t, m, append(upTo, engine.SUCCESS_UNVERIFIED)...)
		if got := path[len(path)-1]; got != engine.SUCCESS_UNVERIFIED || !got.Amber() {
			t.Errorf("final state = %s (amber=%v), want amber SUCCESS_UNVERIFIED", got, got.Amber())
		}
		if n := sink.count() - before; n != 0 {
			t.Errorf("an amber run sent %d webhooks, want 0", n)
		}
	})

	t.Run("verification failure is delivered signed and names the tier", func(t *testing.T) {
		before := sink.count()
		m := newRun()
		path := walkRun(t, m, append(upTo, engine.FAILED)...)
		if got := path[len(path)-2]; got != engine.VERIFYING {
			t.Errorf("state before FAILED = %s, want VERIFYING", got)
		}
		if err := m.NotifyErr(); err != nil {
			t.Fatalf("delivery error: %v", err)
		}
		if n := sink.count() - before; n != 1 {
			t.Fatalf("webhooks sent = %d, want 1", n)
		}
		body, sig := sink.bodies[before], sink.sigs[before]
		mac := hmac.New(sha256.New, []byte(secretKey))
		mac.Write(body)
		if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); sig != want {
			t.Errorf("X-Ballast-Signature = %q, want %q", sig, want)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("webhook body is not JSON: %v\n%s", err, body)
		}
		if payload["service"] != "e2e-svc" {
			t.Errorf("service = %v", payload["service"])
		}
		if payload["type"] != string(EventVerificationFailure) {
			t.Errorf("type = %v, want %s", payload["type"], EventVerificationFailure)
		}
		if payload["tier"] != TierPrimary {
			t.Errorf("tier = %v, want %q (rule 3: every event names its tier)", payload["tier"], TierPrimary)
		}
	})
}
