package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/heartbeat"
	"github.com/springledev/keel/runner"
)

func TestDoctorReportHealthy(t *testing.T) {
	tests := []struct {
		name   string
		checks []DoctorCheck
		want   bool
	}{
		{"empty", nil, true},
		{"warning only", []DoctorCheck{{Status: DoctorOK}, {Status: DoctorWarn}}, true},
		{"one failure", []DoctorCheck{{Status: DoctorOK}, {Status: DoctorFail}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (DoctorReport{Checks: tt.checks}).Healthy(); got != tt.want {
				t.Fatalf("Healthy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDoctorReportsEveryArea(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf '27.1.0\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.ops.Runner = &runner.Runner{}
	h.ops.ResticBin = h.resticBin
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")

	rep, err := h.ops.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	got := map[string]DoctorStatus{}
	for _, c := range rep.Checks {
		got[c.Area+"/"+c.Name] = c.Status
		if c.Detail == "" {
			t.Errorf("check %s/%s has no detail", c.Area, c.Name)
		}
	}
	for _, key := range []string{"restic/restic", "docker-host/local", "destination/local", "disk/" + h.ops.WorkRoot} {
		if got[key] != DoctorOK {
			t.Errorf("check %s = %q, want ok (all: %v)", key, got[key], rep.Checks)
		}
	}
	for _, c := range rep.Checks {
		if c.Area == "proxmox" {
			t.Errorf("proxmox is not configured but was checked: %+v", c)
		}
	}
}

func TestDoctorFailureDoesNotHideOthers(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.Runner = &runner.Runner{}
	h.ops.ResticBin = filepath.Join(t.TempDir(), "no-such-restic")
	h.ops.Repo = nil // every destination check fails too

	rep, err := h.ops.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.Healthy() {
		t.Fatal("report should be unhealthy")
	}
	areas := map[string]bool{}
	for _, c := range rep.Checks {
		areas[c.Area] = true
	}
	for _, a := range []string{"restic", "docker-host", "destination"} {
		if !areas[a] {
			t.Errorf("area %s missing after earlier failures: %+v", a, rep.Checks)
		}
	}
}

func TestDiskCheckUsesNearestExistingParent(t *testing.T) {
	st, detail := diskCheck(filepath.Join(t.TempDir(), "not", "yet", "there"))
	if st == DoctorFail && !strings.Contains(detail, "free") {
		t.Fatalf("unexpected detail %q", detail)
	}
	if detail == "" {
		t.Fatal("empty detail")
	}
}

func TestDoctorHeartbeatLine(t *testing.T) {
	const secretURL = "https://hc.example/ping/SECRET-HB-TOKEN-do-not-leak"
	recent := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	tests := []struct {
		name       string
		configured bool
		lastOK     string
		want       DoctorStatus
		wantText   string
	}{
		{"not configured", false, "", DoctorOK, "No heartbeat monitor"},
		{"never reached", true, "", DoctorWarn, "not been reached"},
		{"stale", true, old, DoctorWarn, old},
		{"recent", true, recent, DoctorOK, recent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			cred := t.TempDir()
			h.ops.CredentialSearchDirs = []string{cred}
			if tt.configured {
				if err := os.WriteFile(filepath.Join(cred, heartbeat.CredentialName), []byte(secretURL), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.lastOK != "" {
				if _, err := h.ops.DB.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, heartbeat.MetaKeyLastOK, tt.lastOK); err != nil {
					t.Fatal(err)
				}
			}
			var found *DoctorCheck
			rep, _ := h.ops.Doctor(context.Background())
			for i, c := range rep.Checks {
				if c.Area == "heartbeat" {
					found = &rep.Checks[i]
				}
			}
			if found == nil {
				t.Fatal("no heartbeat line in doctor")
			}
			if found.Status != tt.want || !strings.Contains(found.Detail, tt.wantText) {
				t.Errorf("got %s %q, want %s containing %q", found.Status, found.Detail, tt.want, tt.wantText)
			}
			b, _ := json.Marshal(rep)
			if strings.Contains(string(b), "SECRET-HB-TOKEN") {
				t.Error("doctor output leaks the heartbeat URL")
			}
		})
	}
}

// TestDoctorShowsBallastVersion checks that doctor names the running
// version as its first line, so a support request says what build it is.
func TestDoctorShowsBallastVersion(t *testing.T) {
	h := newHarness(t)
	h.ops.Version = "v0.5-35dad2f"
	rep, err := h.ops.Doctor(t.Context())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	for _, c := range rep.Checks {
		if c.Area == "ballast" && c.Name == "version" {
			if c.Status != DoctorOK || c.Detail != "Ballast v0.5-35dad2f is running." {
				t.Errorf("version check = %+v", c)
			}
			return
		}
	}
	t.Fatal("doctor has no ballast version check")
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
