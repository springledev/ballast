package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/protondrive"
	"github.com/springledev/keel/runner"
)

// protonOps points the harness at the fake Proton Drive tool shipped
// with the protondrive tests and adds a Proton Drive destination.
func protonOps(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	script, err := os.ReadFile("../protondrive/testdata/fake-proton-drive.sh")
	if err != nil {
		t.Fatal(err)
	}
	fake := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fake, "drive"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(fake, "proton-drive")
	if err := os.WriteFile(bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ROOT", fake)
	h.ops.Runner = &runner.Runner{}
	h.ops.ProtonBin = bin
	if err := inventory.PutDestination(h.ops.DB, inventory.Destination{Name: "proton", Type: backend.ProtonDrive}); err != nil {
		t.Fatal(err)
	}
	return h, fake
}

func TestTestDestinationProton(t *testing.T) {
	tests := []struct {
		name      string
		signedOut bool
		wantErr   string
	}{
		{"signed in", false, ""},
		{"signed out", true, "sign in again"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, fake := protonOps(t)
			if tt.signedOut {
				t.Setenv("FAKE_SIGNED_OUT", "1")
			}
			err := h.ops.TestDestination(context.Background(), "proton")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(fake, "drive", "my-files", "ballast")); err != nil {
					t.Errorf("the copy folder was not created: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, protondrive.ErrSignedOut) {
				t.Errorf("err = %v, want a sign-in message", err)
			}
		})
	}
}

func TestProtonSignInFlow(t *testing.T) {
	h, _ := protonOps(t)
	ctx := context.Background()
	if _, err := h.ops.ProtonSignInStatus(ctx, "proton"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("status before sign-in: err = %v, want not found", err)
	}
	if _, err := h.ops.ProtonSignIn(ctx, "proton"); err != nil {
		t.Fatal(err)
	}
	var info protondrive.LoginInfo
	for i := 0; i < 500; i++ {
		var err error
		if info, err = h.ops.ProtonSignInStatus(ctx, "proton"); err != nil {
			t.Fatal(err)
		}
		if info.Done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !info.Done || info.Error != "" || info.Link != "https://account.proton.me/device?c=1" || info.Code != "AB12-CD34" {
		t.Errorf("sign-in = %+v", info)
	}
}

func TestProtonSignInRefusesOtherDestinations(t *testing.T) {
	h, _ := protonOps(t)
	h.addDestination("local")
	_, err := h.ops.ProtonSignIn(context.Background(), "local")
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want a validation error", err)
	}
}

func TestSetupProtonDestinationNeedsNoRepository(t *testing.T) {
	h := newHarness(t)
	err := h.ops.SetupDestination(context.Background(), DestinationSetup{
		Destination: inventory.Destination{Name: "proton", Type: backend.ProtonDrive}, Init: true,
	})
	if err != nil {
		t.Fatalf("SetupDestination: %v", err)
	}
	if entries, _ := os.ReadDir(h.credDir); len(entries) != 0 {
		t.Errorf("setup wrote credential files %v; a Proton Drive copy needs none", entries)
	}
}

func TestDoctorChecksProtonTool(t *testing.T) {
	h, _ := protonOps(t)
	h.ops.ResticBin = h.resticBin
	h.ops.WorkRoot = filepath.Join(t.TempDir(), "work")
	rep, err := h.ops.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]DoctorCheck{}
	for _, c := range rep.Checks {
		got[c.Area] = c
	}
	if c := got["proton-drive"]; c.Status != DoctorOK || !strings.Contains(c.Detail, "0.8.3") {
		t.Errorf("proton-drive check = %+v", c)
	}
	h.ops.ProtonBin = filepath.Join(t.TempDir(), "missing")
	rep, _ = h.ops.Doctor(context.Background())
	for _, c := range rep.Checks {
		if c.Area == "proton-drive" && (c.Status != DoctorFail || !strings.Contains(c.Detail, protondrive.DownloadURL)) {
			t.Errorf("missing tool check = %+v, want a failure with the download link", c)
		}
	}
}

// TestStatusShowsProtonCopyAsItsOwnAmberLight: a copy that needs a new
// sign-in is amber and says so, while the service's own light stays
// green.
func TestStatusShowsProtonCopyAsItsOwnAmberLight(t *testing.T) {
	h, _ := protonOps(t)
	h.addDestination("local")
	svc := inventory.Service{Name: "svc", CatalogEntry: "folder", Destination: "local",
		Targets: []inventory.Target{{Destination: "local"}, {Destination: "proton"}}}
	if err := inventory.PutService(h.ops.DB, svc); err != nil {
		t.Fatal(err)
	}
	m, err := engine.StartRun(h.ops.DB, "svc")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []engine.State{engine.PREPARING, engine.DUMPING, engine.UPLOADING, engine.VERIFYING} {
		if err := m.Transition(s, "step"); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []engine.TargetResult{
		{Destination: "local", State: engine.SUCCESS},
		{Destination: "proton", State: engine.SUCCESS_UNVERIFIED, Secondary: true, Detail: "Proton Drive needs you to " + protondrive.SignInAgainText + "."},
	} {
		if err := m.RecordTarget(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RecordVerification(engine.VerificationRecord{Method: "restic-read-back", Passed: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(engine.SUCCESS, "ok"); err != nil {
		t.Fatalf("reaching SUCCESS: %v", err)
	}
	st, err := h.ops.Status(context.Background(), "svc", false)
	if err != nil {
		t.Fatal(err)
	}
	local, proton := st[0].Targets[0], st[0].Targets[1]
	if local.Light != "green" || local.Secondary {
		t.Errorf("local = %+v", local)
	}
	if proton.Light != "amber" || !proton.Secondary || !proton.SignInNeeded {
		t.Errorf("proton = %+v, want amber, secondary, sign-in needed", proton)
	}
	if st[0].Tiers[0].Light != "green" {
		t.Errorf("service light = %s, want green: a copy problem never changes it", st[0].Tiers[0].Light)
	}
}
