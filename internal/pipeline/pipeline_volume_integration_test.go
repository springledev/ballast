package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
)

// requireDockerDaemon skips the test unless a Docker daemon answers
// and the volume helper image can be pulled.
func requireDockerDaemon(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed; skipping integration test")
	}
	if out, err := exec.CommandContext(ctx, "docker", "pull", "-q", strategy.DefaultVolumeHelperImage).CombinedOutput(); err != nil {
		t.Skipf("Docker unreachable or cannot pull %s: %v (%s)", strategy.DefaultVolumeHelperImage, err, out)
	}
}

// docker runs one docker command for test setup and returns trimmed
// stdout, failing the test on error.
func docker(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("docker %s: %v (%s)", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// seedVolume creates a volume holding a nested file owned by uid 1000
// and a container using it, removed again when the test ends.
func seedVolume(t *testing.T, ctx context.Context) (volume, container string) {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano())
	volume, container = "ballast-vol-it-"+suffix, "ballast-vol-app-"+suffix
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(c, "docker", "rm", "-f", container).Run()
		_ = exec.CommandContext(c, "docker", "volume", "rm", "-f", volume).Run()
	})
	docker(t, ctx, "volume", "create", volume)
	docker(t, ctx, "run", "--rm", "-v", volume+":/data", strategy.DefaultVolumeHelperImage, "sh", "-c",
		"mkdir -p /data/sub && echo hello-volume > /data/sub/f.txt && chown 1000:1000 /data/sub/f.txt")
	docker(t, ctx, "run", "-d", "--name", container, "-v", volume+":/data", strategy.DefaultVolumeHelperImage, "sleep", "600")
	return volume, container
}

// running reports whether the container is running.
func running(t *testing.T, ctx context.Context, container string) bool {
	return docker(t, ctx, "inspect", "-f", "{{.State.Running}}", container) == "true"
}

// TestPipelineVolumeRoundTripIntegration is the §17.5 round trip, for
// both bundled entries (stop→tar→start and live): create a volume →
// back it up (VERIFYING → SUCCESS, archive-integrity passed) → destroy the volume and its container
// → restore the archive from restic into a new volume → assert the
// content and ownership came back, and that the container was running
// again after the backup.
func TestPipelineVolumeRoundTripIntegration(t *testing.T) {
	bin := requireRestic(t)
	ctx := context.Background()
	requireDockerDaemon(t, ctx)
	for _, entry := range []string{"docker-volume", "docker-volume-live"} {
		t.Run(entry, func(t *testing.T) { volumeRoundTrip(t, ctx, bin, entry) })
	}
}

// volumeRoundTrip runs the round trip with one bundled catalog entry.
func volumeRoundTrip(t *testing.T, ctx context.Context, bin, entryName string) {
	volume, container := seedVolume(t, ctx)

	entryYAML, err := os.ReadFile(filepath.Join("..", "..", "catalog", entryName+".yaml"))
	if err != nil {
		t.Fatal(err)
	}

	credDir := t.TempDir()
	pwFile := filepath.Join(credDir, "ballast-restic-password.local")
	if err := os.WriteFile(pwFile, []byte("integration-test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	db := s.DB()
	if err := inventory.PutDestination(db, inventory.Destination{Name: "local", Location: repoDir, KeepDaily: 7}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	if err := inventory.PutService(db, inventory.Service{
		Name: "appvol", CatalogEntry: entryName, Destination: "local",
		Params: map[string]string{"volume": volume, "container": container},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	r := &runner.Runner{}
	repo, err := restic.Open(repoDir, restic.Options{Runner: r, PasswordFile: pwFile, ResticBin: bin})
	if err != nil {
		t.Fatalf("restic.Open: %v", err)
	}
	if err := repo.Init(ctx); err != nil {
		t.Fatalf("restic Init: %v", err)
	}

	p := &Pipeline{
		DB:        db,
		Catalog:   registryWith(t, map[string]string{entryName + ".yaml": string(entryYAML)}),
		Runner:    r,
		CredDir:   credDir,
		WorkRoot:  t.TempDir(),
		ResticBin: bin,
	}
	state, err := p.Run(ctx, "appvol")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != engine.SUCCESS {
		t.Fatalf("state = %s, want SUCCESS", state)
	}
	if !running(t, ctx, container) {
		t.Error("container is not running after the backup")
	}
	runs, err := engine.RecentRuns(db, "appvol", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	if seq := transitionsOf(t, db, runs[0].ID); !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions = %v, want VERIFYING before SUCCESS", seq)
	}
	results, err := engine.VerificationResults(db, runs[0].ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	archiveChecked := false
	for _, r := range results {
		if r.Method == "archive-integrity" {
			archiveChecked = r.Passed
		}
	}
	if !archiveChecked {
		t.Errorf("no passing archive-integrity record for the volume tar: %+v", results)
	}

	// Find the volume archive in the snapshot and pull it out.
	entries, err := repo.LS(ctx, "latest", "")
	if err != nil {
		t.Fatalf("restic ls: %v", err)
	}
	var archivePath string
	for _, e := range entries {
		if strings.HasSuffix(e.Path, "/volume-"+volume+".tar") {
			archivePath = e.Path
		}
	}
	if archivePath == "" {
		t.Fatalf("snapshot has no volume-%s.tar; entries: %v", volume, entries)
	}
	var archive bytes.Buffer
	if err := repo.Dump(ctx, "latest", archivePath, &archive); err != nil {
		t.Fatalf("restic dump: %v", err)
	}

	// Destroy, then restore into a fresh volume of the same name.
	docker(t, ctx, "rm", "-f", container)
	docker(t, ctx, "volume", "rm", volume)
	if err := strategy.RestoreVolume(ctx, strategy.Options{Runner: r}, volume, &archive); err != nil {
		t.Fatalf("RestoreVolume: %v", err)
	}
	got := docker(t, ctx, "run", "--rm", "-v", volume+":/data:ro", strategy.DefaultVolumeHelperImage,
		"sh", "-c", "cat /data/sub/f.txt; stat -c %u /data/sub/f.txt")
	if got != "hello-volume\n1000" {
		t.Errorf("restored volume content/owner = %q, want %q", got, "hello-volume\n1000")
	}
}

// TestVolumeStopStartRestartsOnFailureIntegration makes the archiving
// step fail after the container was stopped (the helper image does
// not exist) and requires the container to be running again.
func TestVolumeStopStartRestartsOnFailureIntegration(t *testing.T) {
	ctx := context.Background()
	requireDockerDaemon(t, ctx)
	volume, container := seedVolume(t, ctx)

	entry := volumeEntryFor(volume, container)
	s, err := strategy.For(entry, strategy.Options{
		Runner:            &runner.Runner{},
		Docker:            dockerhost.NewLocal(""),
		VolumeHelperImage: "ballast.invalid/no-such-image:never",
	})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if _, err := s.Backup(ctx, strategy.Request{WorkDir: t.TempDir()}); err == nil {
		t.Fatal("Backup succeeded with a missing helper image, want an error")
	}
	if !running(t, ctx, container) {
		t.Error("container left stopped after a failed backup")
	}
}

// volumeEntryFor builds a resolved stop-mode volume entry.
func volumeEntryFor(volume, container string) *catalog.Entry {
	return &catalog.Entry{Service: "appvol", Backup: catalog.Backup{
		Strategy:  "volume",
		Volumes:   []catalog.VolumeSpec{{Name: volume}},
		StopStart: &catalog.StopStart{Containers: []string{container}},
	}}
}
