package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
)

// copyAlerts records the Proton Drive copy alerts a run raised.
type copyAlerts struct{ alerts, recovered []string }

func (c *copyAlerts) Alert(_ context.Context, service, dest, detail string) error {
	c.alerts = append(c.alerts, service+"/"+dest+": "+detail)
	return nil
}

func (c *copyAlerts) Recovered(_ context.Context, service, dest string) error {
	c.recovered = append(c.recovered, service+"/"+dest)
	return nil
}

// protonHarness adds a Proton Drive destination backed by the fake CLI
// (internal/protondrive/testdata) to the targets harness.
func protonHarness(t *testing.T, env ...string) (*testHarness, *Pipeline, string, *copyAlerts) {
	t.Helper()
	h, _ := targetHarness(t)
	if err := inventory.PutDestination(h.db, inventory.Destination{Name: "proton", Type: backend.ProtonDrive}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	fakeRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fakeRoot, "drive"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../protondrive/testdata/fake-proton-drive.sh")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "proton-drive")
	if err := os.WriteFile(bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	p := h.pipeline(append([]string{"FAKE_ROOT=" + fakeRoot}, env...)...)
	p.ProtonBin = bin
	alerts := &copyAlerts{}
	p.CopyAlerts = alerts
	p.Catalog = registryWith(t, map[string]string{"photos.yaml": yamlf(dumpAndFolderYAML, t.TempDir())})
	return h, p, fakeRoot, alerts
}

func putPhotos(t *testing.T, h *testHarness, targets ...inventory.Target) error {
	t.Helper()
	return inventory.PutService(h.db, inventory.Service{Name: "photos", CatalogEntry: "photos", Destination: targets[0].Destination, Targets: targets})
}

func targetStates(t *testing.T, h *testHarness) map[string]engine.TargetResult {
	t.Helper()
	runs, _ := engine.RecentRuns(h.db, "photos", 1)
	rts, err := engine.RunTargets(h.db, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]engine.TargetResult{}
	for _, r := range rts {
		out[r.Destination] = r
	}
	return out
}

func TestRunCopiesVerifiedFilesToProton(t *testing.T) {
	h, p, fakeRoot, alerts := protonHarness(t)
	if err := putPhotos(t, h, inventory.Target{Destination: "local"}, inventory.Target{Destination: "proton", Parts: []string{"data"}}); err != nil {
		t.Fatal(err)
	}
	state, err := p.Run(context.Background(), "photos")
	if state != engine.SUCCESS || err != nil {
		t.Fatalf("state = %s, err = %v; want a clean SUCCESS", state, err)
	}
	got := targetStates(t, h)
	if r := got["proton"]; r.State != engine.SUCCESS || !r.Secondary || got["local"].Secondary {
		t.Errorf("targets = %+v; want proton as a verified secondary and local as primary", got)
	}
	matches, _ := filepath.Glob(filepath.Join(fakeRoot, "drive", "my-files", "ballast", "photos", "*", "data*"))
	if len(matches) != 1 {
		t.Errorf("Proton Drive holds %v, want the data dump once", matches)
	}
	log, _ := os.ReadFile(filepath.Join(fakeRoot, "calls.log"))
	if !strings.Contains(string(log), "unsafe_file|ERROR|"+filepath.Join(p.CredDir, "proton-drive", "proton")) {
		t.Errorf("the CLI was not given the file-based session folder; calls:\n%s", log)
	}
	if len(alerts.alerts) != 0 {
		t.Errorf("unexpected alerts %v", alerts.alerts)
	}
}

func TestProtonProblemsNeverFailTheJob(t *testing.T) {
	tests := []struct {
		name    string
		env     []string
		parts   []string
		wantIn  string
		wantRaw string
	}{
		{"signed out", []string{"FAKE_SIGNED_OUT=1"}, []string{"data"}, "sign in again", ""},
		{"copy does not match", []string{"FAKE_CORRUPT=1"}, []string{"data"}, "does not match", ""},
		{"only a folder chosen", nil, []string{"library"}, "Nothing was copied", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, p, _, alerts := protonHarness(t, tt.env...)
			if err := putPhotos(t, h, inventory.Target{Destination: "local"}, inventory.Target{Destination: "proton", Parts: tt.parts}); err != nil {
				t.Fatal(err)
			}
			state, err := p.Run(context.Background(), "photos")
			if state != engine.SUCCESS || err != nil {
				t.Fatalf("state = %s, err = %v; a copy problem must not touch the job's own result", state, err)
			}
			r := targetStates(t, h)["proton"]
			if r.State != engine.SUCCESS_UNVERIFIED || !r.Secondary || !strings.Contains(r.Detail, tt.wantIn) {
				t.Errorf("proton result = %+v, want amber secondary containing %q", r, tt.wantIn)
			}
			if len(alerts.alerts) != 1 {
				t.Errorf("alerts = %v, want exactly one for the copy", alerts.alerts)
			}
		})
	}
}

func TestPrimaryFailureMeansNoCopyAttempt(t *testing.T) {
	h, p, fakeRoot, alerts := protonHarness(t)
	if err := putPhotos(t, h, inventory.Target{Destination: "bad-offsite"}, inventory.Target{Destination: "proton", Parts: []string{"data"}}); err != nil {
		t.Fatal(err)
	}
	state, _ := p.Run(context.Background(), "photos")
	if state != engine.FAILED {
		t.Fatalf("state = %s, want FAILED from the main destination", state)
	}
	if _, err := os.Stat(filepath.Join(fakeRoot, "calls.log")); err == nil {
		t.Error("Proton Drive was contacted although the main backup failed")
	}
	if r := targetStates(t, h)["proton"]; r.State != engine.SUCCESS_UNVERIFIED || !strings.Contains(r.Detail, "Not copied") {
		t.Errorf("proton result = %+v, want a 'Not copied' note", r)
	}
	if len(alerts.alerts) != 0 {
		t.Errorf("a skipped copy must not alert separately: %v", alerts.alerts)
	}
}

func TestProtonRecoveryClearsItsAlert(t *testing.T) {
	h, p, _, alerts := protonHarness(t)
	if err := putPhotos(t, h, inventory.Target{Destination: "local"}, inventory.Target{Destination: "proton", Parts: []string{"data"}}); err != nil {
		t.Fatal(err)
	}
	if state, err := p.Run(context.Background(), "photos"); state != engine.SUCCESS {
		t.Fatalf("state = %s, err = %v", state, err)
	}
	if len(alerts.recovered) != 1 || alerts.recovered[0] != "photos/proton" {
		t.Errorf("recovered = %v, want one recovery for photos/proton", alerts.recovered)
	}
}

func TestProtonCannotBeTheMainDestination(t *testing.T) {
	h, _, _, _ := protonHarness(t)
	err := putPhotos(t, h, inventory.Target{Destination: "proton"})
	if err == nil || !strings.Contains(err.Error(), "also send to") {
		t.Errorf("err = %v, want a message pointing at \"also send to\"", err)
	}
}

// TestProtonCopyAcceptance proves the acceptance criterion end to end:
// the run reaches VERIFYING then SUCCESS, the Proton Drive copy is a
// separately reported, read-back-checked tier, and nothing from the
// Proton session folder reaches the database or the log.
func TestProtonCopyAcceptance(t *testing.T) {
	h, p, fakeRoot, _ := protonHarness(t)
	var logBuf strings.Builder
	p.Log = &logBuf
	const marker = "SESSION-TOKEN-do-not-leak"
	session := filepath.Join(p.CredDir, "proton-drive", "proton")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "session.json"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := putPhotos(t, h, inventory.Target{Destination: "local"}, inventory.Target{Destination: "proton", Parts: []string{"data"}}); err != nil {
		t.Fatal(err)
	}
	state, err := p.Run(context.Background(), "photos")
	if state != engine.SUCCESS || err != nil {
		t.Fatalf("state = %s, err = %v", state, err)
	}
	runs, _ := engine.RecentRuns(h.db, "photos", 1)
	if tr := transitionsOf(t, h.db, runs[0].ID); !containsInOrder(tr, "VERIFYING", "SUCCESS") {
		t.Errorf("transitions = %v, want VERIFYING then SUCCESS", tr)
	}
	r := targetStates(t, h)["proton"]
	if r.State != engine.SUCCESS || !r.Secondary || !strings.Contains(r.Detail, "downloaded") || !strings.Contains(r.Detail, "matched") {
		t.Errorf("proton tier = %+v, want a verified, read-back-checked secondary", r)
	}
	if fi, err := os.Stat(session); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("session folder = %v, %v; want mode 0700", fi, err)
	}
	// Nothing from the session folder in the log or anywhere in the DB.
	if strings.Contains(logBuf.String(), marker) {
		t.Error("session folder contents reached the log")
	}
	for _, table := range []string{"run_targets", "verification_results", "run_transitions"} {
		rows, err := h.db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			for _, v := range vals {
				if strings.Contains(fmt.Sprint(v), marker) {
					t.Errorf("session folder contents reached table %s", table)
				}
			}
		}
		rows.Close()
	}
	_ = fakeRoot
}
