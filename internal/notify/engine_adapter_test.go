package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
	if n == nil {
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
