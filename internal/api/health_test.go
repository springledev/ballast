package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/inventory"
)

func healthByArea(rep HealthReport) map[string][]HealthItem {
	out := map[string][]HealthItem{}
	for _, it := range rep.Items {
		out[it.Area] = append(out[it.Area], it)
	}
	return out
}

func TestHealthSelfBackupAge(t *testing.T) {
	tests := []struct {
		name string
		ago  time.Duration // 0 = never
		want DoctorStatus
	}{
		{"never backed up", 0, DoctorWarn},
		{"recent", 2 * time.Hour, DoctorOK},
		{"too old", SelfBackupMaxAge + time.Hour, DoctorWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			if tt.ago != 0 {
				if err := h.ops.recordSelfBackup(ctx, time.Now().Add(-tt.ago)); err != nil {
					t.Fatal(err)
				}
			}
			rep, err := h.ops.Health(ctx)
			if err != nil {
				t.Fatal(err)
			}
			items := healthByArea(rep)["self-backup"]
			if len(items) != 1 || items[0].Status != tt.want || items[0].Message == "" {
				t.Fatalf("self-backup items = %+v, want one %q", items, tt.want)
			}
			if (rep.LastSelfBackupAt == nil) != (tt.ago == 0) {
				t.Errorf("LastSelfBackupAt = %v", rep.LastSelfBackupAt)
			}
		})
	}
}

func TestSelfBackupRecordsTime(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	if _, err := h.ops.SelfBackup(context.Background(), "local"); err != nil {
		t.Fatal(err)
	}
	last, err := h.ops.lastSelfBackup(context.Background())
	if err != nil || last == nil || time.Since(*last) > time.Minute {
		t.Fatalf("last self-backup = %v, %v", last, err)
	}
}

// TestHealthReportsEveryProblem checks the disk and credential lines,
// and that a missing credential does not hide the self-backup warning.
func TestHealthReportsEveryProblem(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.ops.WorkRoot = t.TempDir()
	h.addDestination("local")
	if err := h.ops.PutService(ctx, inventory.Service{
		Name: "files", CatalogEntry: "folder", Destination: "local",
		Params: map[string]string{"path": "/data"}, Secrets: map[string]string{"token": "ballast-missing-cred"},
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := h.ops.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := healthByArea(rep)
	if len(by["disk"]) != 1 || by["disk"][0].Message == "" {
		t.Errorf("disk items = %+v", by["disk"])
	}
	if len(by["self-backup"]) != 1 || by["self-backup"][0].Status != DoctorWarn {
		t.Errorf("self-backup items = %+v", by["self-backup"])
	}
	creds := by["credentials"]
	if len(creds) != 1 || creds[0].Status != DoctorFail || !strings.Contains(creds[0].Message, "ballast-missing-cred") {
		t.Fatalf("credential items = %+v, want one failure naming the file", creds)
	}

	// Providing the file clears the problem.
	if err := os.WriteFile(filepath.Join(h.credDir, "ballast-missing-cred"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = h.ops.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := healthByArea(rep)["credentials"]; len(c) != 1 || c[0].Status != DoctorOK {
		t.Errorf("credential items after fix = %+v", c)
	}
}

func TestCredentialProblems(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.addDestination("local")
	if err := h.ops.PutService(ctx, inventory.Service{
		Name: "files", CatalogEntry: "folder", Destination: "local",
		Params: map[string]string{"path": "/data"}, Secrets: map[string]string{"token": "ballast-svc-key"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.credDir, "ballast-restic-password.local")); err != nil {
		t.Fatal(err)
	}
	probs, err := h.ops.CredentialProblems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(probs, "\n")
	for _, want := range []string{"ballast-restic-password.local", "ballast-svc-key"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q do not name %s", joined, want)
		}
	}
}
