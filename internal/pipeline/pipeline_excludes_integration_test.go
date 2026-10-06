package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/runner"
)

// TestPipelineExcludesAndKeepLastIntegration runs the folder service
// three times against a real restic with skip patterns set on the
// service and a keep-the-newest-2 destination: the skipped files are
// absent from the snapshot and only two snapshots remain.
func TestPipelineExcludesAndKeepLastIntegration(t *testing.T) {
	bin := requireRestic(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	for name, body := range map[string]string{"keep.txt": "keep me", "debug.log": "noise", "sub/trace.log": "more noise", "sub/ok.txt": "ok"} {
		full := filepath.Join(dataDir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
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
	if err := inventory.PutDestination(db, inventory.Destination{Name: "local", Location: repoDir, KeepLast: 2}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	if err := inventory.PutService(db, inventory.Service{Name: "docs", CatalogEntry: "folder", Destination: "local", Excludes: []string{"*.log"}}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	r := &runner.Runner{}
	repo, err := restic.Open(repoDir, restic.Options{Runner: r, PasswordFile: filepath.Join(credDir, "ballast-restic-password.local"), ResticBin: bin})
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
	for i := 0; i < 3; i++ {
		// Each snapshot needs a change, or restic reuses the parent.
		if err := os.WriteFile(filepath.Join(dataDir, "keep.txt"), []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if state, err := p.Run(ctx, "docs"); err != nil || state != engine.SUCCESS {
			t.Fatalf("run %d: state %s, err %v", i+1, state, err)
		}
	}
	runs, err := engine.RecentRuns(db, "docs", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	if seq := transitionsOf(t, db, runs[0].ID); !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions = %v, want VERIFYING before SUCCESS", seq)
	}
	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2 (keep-last 2)", len(snaps))
	}
	entries, err := repo.LS(ctx, "latest", "")
	if err != nil {
		t.Fatalf("LS: %v", err)
	}
	var kept []string
	for _, e := range entries {
		if strings.HasSuffix(e.Path, ".log") {
			t.Errorf("skipped file %s is in the snapshot", e.Path)
		}
		kept = append(kept, e.Path)
	}
	// The preview must name exactly the files restic left out.
	pv, err := strategy.PreviewExcludes(&catalog.Entry{Service: "docs", Backup: catalog.Backup{
		Files: []catalog.FileSpec{{Path: dataDir, Exclude: []string{"*.log"}}},
	}})
	if err != nil {
		t.Fatalf("PreviewExcludes: %v", err)
	}
	for _, it := range pv.Skipped {
		for _, k := range kept {
			if k == it.Path {
				t.Errorf("preview says %s is skipped but the snapshot has it", it.Path)
			}
		}
	}
	if pv.SkippedCount != 2 {
		t.Errorf("preview skipped %d items, want the 2 .log files", pv.SkippedCount)
	}
	if joined := strings.Join(kept, " "); !strings.Contains(joined, "keep.txt") || !strings.Contains(joined, "ok.txt") {
		t.Errorf("snapshot holds %v, want keep.txt and sub/ok.txt", kept)
	}
}
