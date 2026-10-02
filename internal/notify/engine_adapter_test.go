package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
)

func TestEngineAdapter_NotifyJobFailure(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	adapter := &EngineAdapter{DB: db.DB(), Notifier: notifier}

	if err := adapter.NotifyJobFailure(context.Background(), "svc", 42, "docker exec failed"); err != nil {
		t.Fatalf("NotifyJobFailure: %v", err)
	}
	if len(ch.events) != 1 {
		t.Fatalf("events = %d, want 1", len(ch.events))
	}
	got := ch.events[0]
	if got.Type != EventJobFailure {
		t.Errorf("Type = %v, want EventJobFailure", got.Type)
	}
	if got.Service != "svc" {
		t.Errorf("Service = %q, want svc", got.Service)
	}
}

func TestEngineAdapter_NotifyVerificationFailure(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	adapter := &EngineAdapter{DB: db.DB(), Notifier: notifier}

	if err := adapter.NotifyVerificationFailure(context.Background(), "svc", 42, "checksum mismatch"); err != nil {
		t.Fatalf("NotifyVerificationFailure: %v", err)
	}
	if len(ch.events) != 1 {
		t.Fatalf("events = %d, want 1", len(ch.events))
	}
	got := ch.events[0]
	if got.Type != EventVerificationFailure {
		t.Errorf("Type = %v, want EventVerificationFailure", got.Type)
	}
	if !strings.Contains(got.Detail, "checksum mismatch") {
		t.Errorf("Detail = %q, want it to contain %q", got.Detail, "checksum mismatch")
	}
}

// TestEngineAdapter_NotifyVerificationFailure_IncludesRunID pins
// finding 11: like NotifyJobFailure/FormatJobFailure, a verification
// failure notification must carry the run ID so it can always be
// traced back to the specific run in the DB.
func TestEngineAdapter_NotifyVerificationFailure_IncludesRunID(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	adapter := &EngineAdapter{DB: db.DB(), Notifier: notifier}

	if err := adapter.NotifyVerificationFailure(context.Background(), "svc", 4242, "checksum mismatch"); err != nil {
		t.Fatalf("NotifyVerificationFailure: %v", err)
	}
	if len(ch.events) != 1 {
		t.Fatalf("events = %d, want 1", len(ch.events))
	}
	if got := ch.events[0].Detail; !strings.Contains(got, "4242") {
		t.Errorf("Detail = %q, want it to contain the run ID 4242", got)
	}
}

func TestEngineAdapter_NotifySuccessHeartbeat_OptOutByDefault(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	adapter := &EngineAdapter{DB: db.DB(), Notifier: notifier}

	// No notification_settings row at all: must stay quiet.
	if err := adapter.NotifySuccessHeartbeat(context.Background(), "svc", 1); err != nil {
		t.Fatalf("NotifySuccessHeartbeat: %v", err)
	}
	if len(ch.events) != 0 {
		t.Errorf("events = %d, want 0 (no opt-in row)", len(ch.events))
	}
}

func TestEngineAdapter_NotifySuccessHeartbeat_OptIn(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	if _, err := db.DB().Exec(
		`INSERT INTO notification_settings (service, enabled, heartbeat_enabled, updated_at) VALUES (?, 1, 1, ?)`,
		"svc", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting notification_settings: %v", err)
	}

	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	adapter := &EngineAdapter{DB: db.DB(), Notifier: notifier}

	if err := adapter.NotifySuccessHeartbeat(context.Background(), "svc", 1); err != nil {
		t.Fatalf("NotifySuccessHeartbeat: %v", err)
	}
	if len(ch.events) != 1 {
		t.Fatalf("events = %d, want 1 (opted in)", len(ch.events))
	}
	if ch.events[0].Type != EventSuccessHeartbeat {
		t.Errorf("Type = %v, want EventSuccessHeartbeat", ch.events[0].Type)
	}
}

// TestEngineAdapter_NilNotifier_ReturnsErrorInsteadOfPanicking is the
// nil guard for review finding 3: an EngineAdapter constructed without
// a Notifier (a wiring mistake, not the deliberate "notifications off"
// case of a nil engine.Notifier) must return a clear, service-scoped
// error from every notify method instead of panicking on the nil
// dereference.
func TestEngineAdapter_NilNotifier_ReturnsErrorInsteadOfPanicking(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	adapter := &EngineAdapter{DB: db.DB()} // Notifier deliberately left nil

	if err := adapter.NotifyJobFailure(context.Background(), "svc", 1, "boom"); err == nil || !errors.Is(err, errNoNotifier) {
		t.Errorf("NotifyJobFailure with nil Notifier = %v, want an error wrapping errNoNotifier", err)
	} else if !strings.Contains(err.Error(), "svc") {
		t.Errorf("NotifyJobFailure error = %q, want it to name the service", err.Error())
	}

	if err := adapter.NotifyVerificationFailure(context.Background(), "svc", 1, "boom"); err == nil || !errors.Is(err, errNoNotifier) {
		t.Errorf("NotifyVerificationFailure with nil Notifier = %v, want an error wrapping errNoNotifier", err)
	} else if !strings.Contains(err.Error(), "svc") {
		t.Errorf("NotifyVerificationFailure error = %q, want it to name the service", err.Error())
	}

	if err := adapter.NotifySuccessHeartbeat(context.Background(), "svc", 1); err == nil || !errors.Is(err, errNoNotifier) {
		t.Errorf("NotifySuccessHeartbeat with nil Notifier = %v, want an error wrapping errNoNotifier", err)
	} else if !strings.Contains(err.Error(), "svc") {
		t.Errorf("NotifySuccessHeartbeat error = %q, want it to name the service", err.Error())
	}
}

// TestEngineAdapter_NilReceiver_ReturnsErrorInsteadOfPanicking covers
// the typed-nil case: a nil *EngineAdapter stored in an
// engine.Notifier interface field (e.g. scheduler.Coordinator.Notifier)
// is not itself a nil interface, so a caller's "notifier != nil" check
// does not catch it — a bare method call would panic dereferencing a.
// Every method must instead return the errNoNotifier-wrapped error, as
// it would for a non-nil adapter with a nil Notifier.
func TestEngineAdapter_NilReceiver_ReturnsErrorInsteadOfPanicking(t *testing.T) {
	var adapter *EngineAdapter // typed nil, not &EngineAdapter{}

	if err := adapter.NotifyJobFailure(context.Background(), "svc", 1, "boom"); err == nil || !errors.Is(err, errNoNotifier) {
		t.Errorf("NotifyJobFailure on nil *EngineAdapter = %v, want an error wrapping errNoNotifier", err)
	} else if !strings.Contains(err.Error(), "svc") {
		t.Errorf("NotifyJobFailure error = %q, want it to name the service", err.Error())
	}

	if err := adapter.NotifyVerificationFailure(context.Background(), "svc", 1, "boom"); err == nil || !errors.Is(err, errNoNotifier) {
		t.Errorf("NotifyVerificationFailure on nil *EngineAdapter = %v, want an error wrapping errNoNotifier", err)
	} else if !strings.Contains(err.Error(), "svc") {
		t.Errorf("NotifyVerificationFailure error = %q, want it to name the service", err.Error())
	}

	if err := adapter.NotifySuccessHeartbeat(context.Background(), "svc", 1); err == nil || !errors.Is(err, errNoNotifier) {
		t.Errorf("NotifySuccessHeartbeat on nil *EngineAdapter = %v, want an error wrapping errNoNotifier", err)
	} else if !strings.Contains(err.Error(), "svc") {
		t.Errorf("NotifySuccessHeartbeat error = %q, want it to name the service", err.Error())
	}

	// Confirm the same nil adapter, stored in an engine.Notifier
	// interface variable, is a non-nil interface (the crux of the bug
	// this guards against): a plain "notifier != nil" check would not
	// catch it.
	var n interface {
		NotifyJobFailure(ctx context.Context, service string, runID int64, detail string) error
	} = adapter
	if isNilInterface(n) {
		t.Fatal("typed-nil *EngineAdapter stored in an interface should not compare equal to nil")
	}
}

func TestEngineAdapter_NotifySuccessHeartbeat_ExplicitlyDisabled(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	if _, err := db.DB().Exec(
		`INSERT INTO notification_settings (service, enabled, heartbeat_enabled, updated_at) VALUES (?, 1, 0, ?)`,
		"svc", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("inserting notification_settings: %v", err)
	}

	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	adapter := &EngineAdapter{DB: db.DB(), Notifier: notifier}

	if err := adapter.NotifySuccessHeartbeat(context.Background(), "svc", 1); err != nil {
		t.Fatalf("NotifySuccessHeartbeat: %v", err)
	}
	if len(ch.events) != 0 {
		t.Errorf("events = %d, want 0 (explicitly disabled)", len(ch.events))
	}
}

// isNilInterface reports whether v is a nil interface value. It takes
// an any so the comparison is made on the interface, not on the
// concrete pointer type the caller started with.
func isNilInterface(v any) bool { return v == nil }

func TestEngineAdapter_DedupAndRecovery(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	a := &EngineAdapter{DB: db.DB(), Notifier: notifier}
	ctx := context.Background()

	// A service failing every night: one alert, not a stream.
	for i := 0; i < 3; i++ {
		if err := a.NotifyJobFailure(ctx, "svc", int64(i), "boom"); err != nil {
			t.Fatal(err)
		}
	}
	if len(ch.events) != 1 {
		t.Fatalf("after 3 failures sent %d alerts, want 1", len(ch.events))
	}
	// Verified success: exactly one recovery notice (heartbeat is off).
	if err := a.NotifySuccessHeartbeat(ctx, "svc", 4); err != nil {
		t.Fatal(err)
	}
	if len(ch.events) != 2 || ch.events[1].Type != EventRecovery {
		t.Fatalf("events = %+v, want a recovery notice", ch.events)
	}
	// Staying green sends nothing more; a new failure alerts again.
	a.NotifySuccessHeartbeat(ctx, "svc", 5)                   //nolint:errcheck
	a.NotifyVerificationFailure(ctx, "svc", 6, "bad restore") //nolint:errcheck
	if len(ch.events) != 3 || ch.events[2].Type != EventVerificationFailure {
		t.Fatalf("events = %+v", ch.events)
	}
}

func TestEngineAdapter_UndeliveredAlertRetries(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	a := &EngineAdapter{DB: db.DB(), Notifier: NewNotifier()} // nobody listening
	if err := a.NotifyJobFailure(context.Background(), "svc", 1, "boom"); !errors.Is(err, ErrNoChannels) {
		t.Fatalf("err = %v, want ErrNoChannels", err)
	}
	ch := &fakeChannel{}
	a.Notifier.RegisterChannel("svc", ch)
	a.NotifyJobFailure(context.Background(), "svc", 2, "boom") //nolint:errcheck
	if len(ch.events) != 1 {
		t.Fatal("an alert nobody received must be retried, not recorded as sent")
	}
}

// TestEngineAdapter_EndToEndThroughStateMachine drives real runs through
// the engine state machine (spec §18.1): repeated failures alert once,
// and a run that passes VERIFYING and reaches SUCCESS sends exactly one
// recovery notice. CLI and scheduled runs share this path.
func TestEngineAdapter_EndToEndThroughStateMachine(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	notifier := NewNotifier()
	ch := &fakeChannel{}
	notifier.RegisterChannel("svc", ch)
	adapter := &EngineAdapter{DB: db.DB(), Notifier: notifier}

	run := func(path []engine.State, final engine.State, detail string) *engine.Machine {
		t.Helper()
		m, err := engine.StartRun(db.DB(), "svc")
		if err != nil {
			t.Fatal(err)
		}
		m.SetNotifier(context.Background(), adapter)
		for _, s := range append(path, final) {
			if err := m.Transition(s, detail); err != nil {
				t.Fatalf("Transition to %s: %v", s, err)
			}
		}
		if err := m.NotifyErr(); err != nil {
			t.Fatalf("NotifyErr: %v", err)
		}
		return m
	}
	toVerify := []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING}

	run([]engine.State{engine.PREPARING}, engine.FAILED, "night 1")
	run([]engine.State{engine.PREPARING}, engine.FAILED, "night 2")
	if len(ch.events) != 1 || ch.events[0].Type != EventJobFailure {
		t.Fatalf("two failing nights sent %+v, want one job_failure alert", ch.events)
	}
	m := run(toVerify, engine.SUCCESS, "")
	if m.Run().State != engine.SUCCESS {
		t.Fatalf("run ended %s, want SUCCESS", m.Run().State)
	}
	if len(ch.events) != 2 || ch.events[1].Type != EventRecovery {
		t.Fatalf("after a verified success sent %+v, want one recovery notice", ch.events)
	}
}
