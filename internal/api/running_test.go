package api

import (
	"context"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
)

// TestRunningServices checks that RunningServices names exactly the
// services with a backup in progress, and nothing once it finishes
// (spec §21.5).
func TestRunningServices(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	for _, n := range []string{"a", "b"} {
		if err := inventory.PutService(h.ops.DB, inventory.Service{Name: n, CatalogEntry: "folder", Destination: "local"}); err != nil {
			t.Fatalf("PutService: %v", err)
		}
	}
	got, err := h.ops.RunningServices(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("idle RunningServices = %v, %v; want none", got, err)
	}
	m, err := engine.StartRun(h.ops.DB, "b")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// A live run's process holds the service's run lock.
	if _, ok, err := h.locks.TryLock("b"); err != nil || !ok {
		t.Fatalf("taking lock: %v, %v", ok, err)
	}
	got, err = h.ops.RunningServices(context.Background())
	if err != nil || len(got) != 1 || got[0] != "b" {
		t.Fatalf("RunningServices = %v, %v; want [b]", got, err)
	}
	if err := m.Transition(engine.FAILED, "stop"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	got, err = h.ops.RunningServices(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("finished RunningServices = %v, %v; want none", got, err)
	}
}

// TestRunningServicesIgnoresCrashedRuns: a run left unfinished by a
// crash or power loss has no process holding the service's run lock, so
// it is not running (spec §21.5); a live one still is.
func TestRunningServicesIgnoresCrashedRuns(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	for _, n := range []string{"crashed", "live"} {
		if err := inventory.PutService(h.ops.DB, inventory.Service{Name: n, CatalogEntry: "folder", Destination: "local"}); err != nil {
			t.Fatalf("PutService: %v", err)
		}
		if _, err := engine.StartRun(h.ops.DB, n); err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	}
	release, ok, err := h.locks.TryLock("live")
	if err != nil || !ok {
		t.Fatalf("taking lock: %v, %v", ok, err)
	}
	got, err := h.ops.RunningServices(context.Background())
	if err != nil || len(got) != 1 || got[0] != "live" {
		t.Fatalf("RunningServices = %v, %v; want [live] only", got, err)
	}
	release()
	got, err = h.ops.RunningServices(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("after the live run's process ended, RunningServices = %v, %v; want none", got, err)
	}
	// Asking must not take a lock away from a later real run.
	if _, ok, _ := h.locks.TryLock("crashed"); !ok {
		t.Error("RunningServices left the run lock taken")
	}
}

// TestDoctorShowsSchemaVersion checks that doctor reports the database
// schema version next to the Ballast version (spec §21.5).
func TestDoctorShowsSchemaVersion(t *testing.T) {
	h := newHarness(t)
	rep, err := h.ops.Doctor(t.Context())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	for _, c := range rep.Checks {
		if c.Area == "ballast" && c.Name == "schema" {
			if c.Status != DoctorOK || c.Detail == "" {
				t.Errorf("schema check = %+v", c)
			}
			return
		}
	}
	t.Fatal("doctor has no ballast schema check")
}
