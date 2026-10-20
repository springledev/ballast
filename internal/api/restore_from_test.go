package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/keel/runner"
)

// fromStub is a fake restic whose only snapshot ID differs per
// destination, so a test can tell which repository a restore read.
func fromStub(id string) string {
	return fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$STUB_LOG"
case "$1" in
  snapshots) printf '[{"time":"2024-01-01T00:00:00Z","id":"%[1]s","short_id":"%[2]s","hostname":"t","tags":["ballast","ballast-service:svc"],"paths":["/d"]}]\n' ;;
  restore) exit 0 ;;
  *) exit 0 ;;
esac
`, strings.Repeat(id, 64), strings.Repeat(id, 8))
}

// twoDestinationHarness wires destinations "local" and "offsite", each
// with its own stub repository (distinct snapshot ID, own log), plus
// "other", which exists but is not a target of service svc.
func twoDestinationHarness(t *testing.T) (*harness, map[string]string) {
	t.Helper()
	h := newHarness(t)
	logs := map[string]string{}
	bins := map[string]string{}
	for name, id := range map[string]string{"local": "a", "offsite": "b", "other": "c"} {
		h.addDestination(name)
		dir := t.TempDir()
		bins[name] = filepath.Join(dir, "restic")
		if err := os.WriteFile(bins[name], []byte(fromStub(id)), 0o755); err != nil {
			t.Fatal(err)
		}
		logs[name] = filepath.Join(dir, "log")
	}
	h.ops.Repo = func(dest inventory.Destination) (*restic.Repository, error) {
		return restic.Open(dest.Location, restic.Options{
			Runner:       &runner.Runner{BaseEnv: append(os.Environ(), "STUB_LOG="+logs[dest.Name])},
			PasswordFile: filepath.Join(h.credDir, dest.PasswordCredential),
			ResticBin:    bins[dest.Name],
		})
	}
	if err := inventory.PutService(h.ops.DB, inventory.Service{
		Name: "svc", CatalogEntry: "folder", Project: "stack", Destination: "local",
		Targets: []inventory.Target{{Destination: "local"}, {Destination: "offsite"}},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	return h, logs
}

func logOf(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestRestoreFromDestination(t *testing.T) {
	tests := []struct {
		name     string
		from     string
		wantErr  bool
		wantRepo string // destination whose snapshot must be restored
		wantID   string
	}{
		{"default is the main destination", "", false, "local", strings.Repeat("a", 64)},
		{"second target", "offsite", false, "offsite", strings.Repeat("b", 64)},
		{"main destination by name", "local", false, "local", strings.Repeat("a", 64)},
		{"not a target of the service", "other", true, "", ""},
		{"unknown destination", "ghost", true, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, logs := twoDestinationHarness(t)
			to := filepath.Join(t.TempDir(), "out")
			err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc", From: tt.from, To: to})
			if tt.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("Restore = %v, want ErrValidation", err)
				}
				for n, l := range logs {
					if strings.Contains(logOf(t, l), "restore") {
						t.Errorf("refused restore still ran restic restore on %s", n)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if got := logOf(t, logs[tt.wantRepo]); !strings.Contains(got, "restore --no-lock "+tt.wantID) {
				t.Errorf("repo %s log = %q, want a restore of %s", tt.wantRepo, got, tt.wantID)
			}
			for n, l := range logs {
				if n != tt.wantRepo && strings.Contains(logOf(t, l), "restore") {
					t.Errorf("restore also touched %s", n)
				}
			}
		})
	}
}

// A snapshot ID from one repository is not found in another: the
// service-tag guard resolves inside the chosen repository only.
func TestRestoreFromResolvesSnapshotInsideChosenRepository(t *testing.T) {
	h, _ := twoDestinationHarness(t)
	to := filepath.Join(t.TempDir(), "out")
	err := h.ops.Restore(context.Background(), RestoreRequest{Service: "svc", From: "offsite", Snapshot: strings.Repeat("a", 8), To: to})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("Restore = %v, want ErrValidation", err)
	}
}

func TestSnapshotsFromDestination(t *testing.T) {
	h, _ := twoDestinationHarness(t)
	snaps, err := h.ops.SnapshotsFrom(context.Background(), "svc", "offsite")
	if err != nil || len(snaps) != 1 || snaps[0].ShortID != strings.Repeat("b", 8) {
		t.Fatalf("SnapshotsFrom offsite = %v, %v", snaps, err)
	}
	if _, err := h.ops.SnapshotsFrom(context.Background(), "svc", "other"); !errors.Is(err, ErrValidation) {
		t.Errorf("SnapshotsFrom non-target = %v, want ErrValidation", err)
	}
	def, err := h.ops.Snapshots(context.Background(), "svc")
	if err != nil || def[0].ShortID != strings.Repeat("a", 8) {
		t.Errorf("default Snapshots = %v, %v", def, err)
	}
}

func TestRestoreProjectFromDestination(t *testing.T) {
	h, logs := twoDestinationHarness(t)
	// A second project member that only has the local destination.
	if err := inventory.PutService(h.ops.DB, inventory.Service{
		Name: "localonly", CatalogEntry: "folder", Destination: "local", Project: "stack",
	}); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(t.TempDir(), "out")
	rep, err := h.ops.RestoreProject(context.Background(), RestoreProjectRequest{Project: "stack", From: "offsite", To: to})
	if err == nil || !strings.Contains(err.Error(), "localonly") || strings.Contains(err.Error(), "svc,") {
		t.Fatalf("error = %v, want only localonly named", err)
	}
	for _, r := range rep.Results {
		switch r.Service {
		case "svc":
			if r.Error != "" || r.SnapshotID != strings.Repeat("b", 8) {
				t.Errorf("svc result = %+v, want restored from offsite", r)
			}
		case "localonly":
			if r.Error == "" {
				t.Errorf("localonly should be refused, got %+v", r)
			}
		}
	}
	if strings.Contains(logOf(t, logs["local"]), "restore") {
		t.Error("local repository must not be restored from")
	}
}
