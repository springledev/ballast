package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/store"
)

// requireRestic returns a real restic binary or skips the test, the
// same convention internal/restic's own integration tests use.
func requireRestic(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("BALLAST_TEST_RESTIC"); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic not installed; set BALLAST_TEST_RESTIC or install restic to run this integration test")
	}
	return bin
}

// TestPipelineFilesIntegration runs a real `files` backup end-to-end
// against a real restic binary and a local-disk repository (spec §14
// step 11: "a files backup end-to-end to a local restic repo recorded
// SUCCESS only after VERIFYING"). It skips cleanly when restic is not
// installed.
func TestPipelineFilesIntegration(t *testing.T) {
	bin := requireRestic(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "hello.txt"), []byte("hello, ballast\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, "ballast-restic-password.local"), []byte("integration-test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")

	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	db := s.DB()

	if err := inventory.PutDestination(db, inventory.Destination{
		Name: "local", Location: repoDir, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6,
	}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	if err := inventory.PutService(db, inventory.Service{Name: "docs", CatalogEntry: "folder", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	r := &runner.Runner{}
	repo, err := restic.Open(repoDir, restic.Options{
		Runner:       r,
		PasswordFile: filepath.Join(credDir, "ballast-restic-password.local"),
		ResticBin:    bin,
	})
	if err != nil {
		t.Fatalf("restic.Open: %v", err)
	}
	if err := repo.Init(ctx); err != nil {
		t.Fatalf("restic Init: %v", err)
	}

	p := &Pipeline{
		DB:        db,
		Catalog:   registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, dataDir)}),
		Runner:    r,
		CredDir:   credDir,
		WorkRoot:  t.TempDir(),
		ResticBin: bin,
	}

	state, err := p.Run(ctx, "docs")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != engine.SUCCESS {
		t.Fatalf("state = %s, want SUCCESS", state)
	}

	runs, err := engine.RecentRuns(db, "docs", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	run := runs[0]
	if run.SnapshotID == "" {
		t.Error("runs.snapshot_id not set")
	}
	seq := transitionsOf(t, db, run.ID)
	if !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions = %v, want VERIFYING before SUCCESS", seq)
	}

	results, err := engine.VerificationResults(db, run.ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no verification results recorded")
	}
	for _, res := range results {
		if !res.Passed {
			t.Errorf("verification record %s not passed: %+v", res.Method, res)
		}
	}

	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snaps))
	}
}
