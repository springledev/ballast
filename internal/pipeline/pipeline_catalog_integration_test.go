package pipeline

import (
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
	"github.com/springledev/keel/runner"

	bundledcatalog "github.com/springledev/ballast/catalog"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// immichDBImage is the Immich Postgres image the bundled
// catalog/immich.yaml declares as its verify.image default. Using the
// exact same image for the fixture container and for the
// container-restore check is what proves the vector-search extension
// (vchord) round-trips through pg_dumpall/psql — a plain postgres
// image lacks it entirely, so a restore there would pass for the
// wrong reason (spec §7.2).
const immichDBImage = "ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0"

// requireDockerImage skips the test unless docker can already see the
// named image locally. The test suite is not responsible for pulling
// multi-hundred-MB images; it only runs the real thing when the image
// is already there.
func requireDockerImage(t *testing.T, image string) {
	t.Helper()
	cmd := exec.Command("docker", "image", "inspect", image)
	if err := cmd.Run(); err != nil {
		t.Skipf("docker image %s not available locally; skipping integration test", image)
	}
}

// bundledRegistry loads the real, shipped catalog (catalog/*.yaml via
// its go:embed FS), not an inline test fixture — the whole point of
// this file is exercising the actual bundled entries end-to-end.
func bundledRegistry(t *testing.T) *catalog.Registry {
	t.Helper()
	reg, err := catalog.LoadRegistry(bundledcatalog.Bundled, nil)
	if err != nil {
		t.Fatalf("LoadRegistry(bundled): %v", err)
	}
	return reg
}

// newPipelineHarness sets up a fresh store, credential directory and
// local restic repository shared by the catalog integration tests
// below. It returns the Pipeline pre-wired with the real bundled
// catalog registry.
func newPipelineHarness(t *testing.T, resticBin string) (*Pipeline, *restic.Repository) {
	t.Helper()
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, "ballast-restic-password.local"), []byte("integration-test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")

	s, err := store.Open(filepath.Join(t.TempDir(), "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	db := s.DB()

	if err := inventory.PutDestination(db, inventory.Destination{
		Name: "local", Location: repoDir, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6,
	}); err != nil {
		t.Fatalf("PutDestination: %v", err)
	}

	r := &runner.Runner{}
	repo, err := restic.Open(repoDir, restic.Options{
		Runner:       r,
		PasswordFile: filepath.Join(credDir, "ballast-restic-password.local"),
		ResticBin:    resticBin,
	})
	if err != nil {
		t.Fatalf("restic.Open: %v", err)
	}
	if err := repo.Init(context.Background()); err != nil {
		t.Fatalf("restic Init: %v", err)
	}

	p := &Pipeline{
		DB:        db,
		Catalog:   bundledRegistry(t),
		Runner:    r,
		CredDir:   credDir,
		WorkRoot:  t.TempDir(),
		ResticBin: resticBin,
	}
	return p, repo
}

// terminateContainer stops and removes a testcontainers-go container,
// tolerating it already being gone.
func terminateContainer(c testcontainers.Container) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.Terminate(ctx)
}

// TestPipelineImmichCatalogIntegration runs the real bundled
// catalog/immich.yaml entry end-to-end (spec §14: no E2E test may
// pass without reaching VERIFYING -> SUCCESS): a throwaway Immich
// Postgres container is seeded with the vchord vector-search
// extension, a table with a vector column, a row, and a vchordrq
// index, plus a small photo-library directory; the pipeline backs
// both up via restic and verifies the dump with a real
// container-restore. This is the scenario the ContainerRestore
// package doc calls out by name: starting the throwaway container
// with the image's OWN default command (not a bare "postgres"
// argument) is what lets the restored server load vchord at all.
//
// It skips cleanly when docker, restic, or the Immich Postgres image
// is not available, and under -short.
func TestPipelineImmichCatalogIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping catalog integration test in -short mode")
	}
	bin := requireRestic(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed; skipping integration test")
	}
	requireDockerImage(t, immichDBImage)
	ctx := context.Background()

	containerName := fmt.Sprintf("ballast-immich-integration-%d", time.Now().UnixNano())
	req := testcontainers.ContainerRequest{
		Image: immichDBImage,
		Name:  containerName,
		Env: map[string]string{
			"POSTGRES_PASSWORD": "postgres",
			"POSTGRES_DB":       "immich",
		},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor:   wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(120 * time.Second),
	}
	dbC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Skipf("could not start the Immich postgres fixture container (Docker unreachable?): %v", err)
	}
	t.Cleanup(func() { terminateContainer(dbC) })

	r := &runner.Runner{}
	seed := runner.Command{
		Name: "docker",
		Args: []string{"exec", containerName, "psql", "-U", "postgres", "-d", "immich", "-c",
			"CREATE EXTENSION vchord CASCADE; " +
				"CREATE TABLE ballast_integration_items (id serial primary key, embedding vector(3)); " +
				"INSERT INTO ballast_integration_items (embedding) VALUES ('[1,2,3]'); " +
				"CREATE INDEX ON ballast_integration_items USING vchordrq (embedding vector_l2_ops);"},
	}
	if res, err := r.Run(ctx, nil, nil, seed); err != nil {
		t.Fatalf("seeding Immich fixture database: %v (stderr: %s)", err, res.Stderr)
	}

	libraryDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(libraryDir, "photo1.jpg"), []byte("fake-photo-bytes-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(libraryDir, "album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libraryDir, "album", "photo2.jpg"), []byte("fake-photo-bytes-2"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, repo := newPipelineHarness(t, bin)
	if err := inventory.PutService(p.DB, inventory.Service{
		Name:         "immich",
		CatalogEntry: "immich",
		Destination:  "local",
		Params: map[string]string{
			"library_dir":  libraryDir,
			"db_container": containerName,
			// db_user and db_image are left unset: the catalog's own
			// defaults (postgres / immichDBImage) apply.
		},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	state, err := p.Run(ctx, "immich")
	if err != nil {
		t.Fatalf("Run: %v (state %s)", err, state)
	}
	if state != engine.SUCCESS {
		t.Fatalf("state = %s, want SUCCESS", state)
	}

	runs, err := engine.RecentRuns(p.DB, "immich", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	run := runs[0]
	if run.SnapshotID == "" {
		t.Error("runs.snapshot_id not set")
	}

	results, err := engine.VerificationResults(p.DB, run.ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	found := false
	for _, res := range results {
		if res.Method == "db-restore-drill" {
			found = true
			if !res.Passed {
				t.Errorf("db-restore-drill record not passed: %+v", res)
			}
		}
	}
	if !found {
		t.Error("no db-restore-drill verification record recorded")
	}

	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snaps))
	}
	joined := strings.Join(snaps[0].Paths, "\n")
	if !strings.Contains(joined, libraryDir) {
		t.Errorf("snapshot paths = %v, want the library dir %q among them", snaps[0].Paths, libraryDir)
	}
	// The database dump is an artifact file under the run's work
	// directory (<WorkRoot>/immich/database.sql), not the library
	// path, so it never appears verbatim in snaps[0].Paths; its
	// presence is what container-restore above already proved.
	dumpArtifact := filepath.Join(p.WorkRoot, "immich", "database.sql")
	if info, err := os.Stat(dumpArtifact); err != nil || info.Size() == 0 {
		t.Errorf("expected a non-empty dump artifact at %s, got stat err %v", dumpArtifact, err)
	}
}

// TestPipelineVaultwardenCatalogIntegration runs the real bundled
// catalog/vaultwarden.yaml entry end-to-end against a fixture
// container standing in for Vaultwarden: stop-start archives its
// bind-mounted data directory, and archive-integrity verifies the
// resulting tar. It asserts the run reaches SUCCESS and that the
// container is running again afterwards — the stop-start strategy's
// "must never leave a service down" cleanup guarantee (spec §4.1).
//
// A second case makes the data directory unreadable so the archive
// step fails mid-run, and asserts the container is still restarted
// even though the run does not reach SUCCESS.
func TestPipelineVaultwardenCatalogIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping catalog integration test in -short mode")
	}
	bin := requireRestic(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed; skipping integration test")
	}
	const fixtureImage = "busybox"
	if err := exec.Command("docker", "pull", fixtureImage).Run(); err != nil {
		requireDockerImage(t, fixtureImage)
	}

	t.Run("success restarts the container", func(t *testing.T) {
		ctx := context.Background()
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dataDir, "db.sqlite3"), []byte("fake-sqlite-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dataDir, "attachments"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "attachments", "file1"), []byte("fake-attachment-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		containerName := fmt.Sprintf("ballast-vaultwarden-integration-%d", time.Now().UnixNano())
		fixC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image: fixtureImage,
				Name:  containerName,
				Cmd:   []string{"sh", "-c", "mkdir -p /www && echo ok > /www/alive && exec httpd -f -p 80 -h /www"},
			},
			Started: true,
		})
		if err != nil {
			t.Skipf("could not start the vaultwarden fixture container: %v", err)
		}
		t.Cleanup(func() { terminateContainer(fixC) })

		p, repo := newPipelineHarness(t, bin)
		if err := inventory.PutService(p.DB, inventory.Service{
			Name:         "vaultwarden",
			CatalogEntry: "vaultwarden",
			Destination:  "local",
			Params: map[string]string{
				"container": containerName,
				"data_dir":  dataDir,
			},
		}); err != nil {
			t.Fatalf("PutService: %v", err)
		}

		state, err := p.Run(ctx, "vaultwarden")
		if err != nil {
			t.Fatalf("Run: %v (state %s)", err, state)
		}
		if state != engine.SUCCESS {
			t.Fatalf("state = %s, want SUCCESS", state)
		}

		runs, err := engine.RecentRuns(p.DB, "vaultwarden", 1)
		if err != nil || len(runs) != 1 {
			t.Fatalf("RecentRuns: %v, %v", runs, err)
		}
		results, err := engine.VerificationResults(p.DB, runs[0].ID)
		if err != nil {
			t.Fatalf("VerificationResults: %v", err)
		}
		found := false
		for _, res := range results {
			if res.Method == "archive-integrity" {
				found = true
				if !res.Passed {
					t.Errorf("archive-integrity record not passed: %+v", res)
				}
			}
		}
		if !found {
			t.Error("no archive-integrity verification record recorded")
		}
		foundSmoke := false
		for _, res := range results {
			if res.Method == "restart-smoke" {
				foundSmoke = true
				if !res.Passed {
					t.Errorf("restart-smoke record not passed: %+v", res)
				}
			}
		}
		if !foundSmoke {
			t.Error("no restart-smoke verification record recorded")
		}

		if !dockerContainerRunning(t, containerName) {
			t.Error("fixture container is not running after the backup; stop-start must restart it")
		}

		snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{})
		if err != nil {
			t.Fatalf("Snapshots: %v", err)
		}
		if len(snaps) != 1 {
			t.Fatalf("snapshot count = %d, want 1", len(snaps))
		}
	})

	t.Run("archive failure still restarts the container", func(t *testing.T) {
		ctx := context.Background()
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dataDir, "db.sqlite3"), []byte("fake-sqlite-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		containerName := fmt.Sprintf("ballast-vaultwarden-failure-%d", time.Now().UnixNano())
		fixC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image: fixtureImage,
				Name:  containerName,
				Cmd:   []string{"sleep", "3600"},
			},
			Started: true,
		})
		if err != nil {
			t.Skipf("could not start the vaultwarden fixture container: %v", err)
		}
		t.Cleanup(func() { terminateContainer(fixC) })

		// Make the data directory unreadable so the stop-start
		// strategy's archive step fails while the container is down.
		if err := os.Chmod(dataDir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })

		p, _ := newPipelineHarness(t, bin)
		if err := inventory.PutService(p.DB, inventory.Service{
			Name:         "vaultwarden-fail",
			CatalogEntry: "vaultwarden",
			Destination:  "local",
			Params: map[string]string{
				"container": containerName,
				"data_dir":  dataDir,
			},
		}); err != nil {
			t.Fatalf("PutService: %v", err)
		}

		state, err := p.Run(ctx, "vaultwarden-fail")
		if err == nil {
			t.Fatal("Run: want an error when the data directory cannot be archived")
		}
		if state == engine.SUCCESS {
			t.Fatalf("state = %s, want anything but SUCCESS", state)
		}

		if !dockerContainerRunning(t, containerName) {
			t.Error("fixture container is not running after a failed backup; stop-start must still restart it")
		}
	})
}

// dockerContainerRunning reports whether the named container is
// currently running, via `docker inspect -f {{.State.Running}}`.
func dockerContainerRunning(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", name, err)
	}
	return strings.TrimSpace(string(out)) == "true"
}
