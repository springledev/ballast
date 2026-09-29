package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
