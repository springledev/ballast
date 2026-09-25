package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/store"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const postgresIntegrationEntryYAML = `
apiVersion: ballast.dev/v1
service: postgres
version: 1
backup:
  strategy: dump
  dumps:
    - name: database
      container: %s
      command: ["pg_dumpall", "-U", "postgres"]
      format: sql
verify:
  method: container-restore
  image: %s
  filterStderr:
    - "already exists"
  restartPolicy: never
`

// TestPipelineDumpPostgresIntegration runs the `dump` strategy and
// `container-restore` verification end-to-end against a real
// PostgreSQL container (spec §14 step 11: "a dump integration test
// against a real Postgres container"), via testcontainers-go
// (AGENTS.md mandates it for Docker-dependent tests). It seeds a
// table, backs it up through the postgres catalog entry to a local
// restic repository, and asserts the run reaches SUCCESS with a
// passing container-restore verification record.
//
// It skips cleanly, naming the reason, when restic or Docker is not
// available.
func TestPipelineDumpPostgresIntegration(t *testing.T) {
	bin := requireRestic(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed; skipping integration test")
	}
	ctx := context.Background()

	const image = "postgres:16"
	containerName := fmt.Sprintf("ballast-pg-integration-%d", time.Now().UnixNano())
	req := testcontainers.ContainerRequest{
		Image:        image,
		Name:         containerName,
		Env:          map[string]string{"POSTGRES_PASSWORD": "postgres"},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor:   wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(120 * time.Second),
	}
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Skipf("could not start a postgres container (Docker unreachable or no network access to pull %s): %v", image, err)
	}
	t.Cleanup(func() {
		tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = pgC.Terminate(tctx)
	})

	r := &runner.Runner{}
	seed := runner.Command{
		Name: "docker",
		Args: []string{"exec", containerName, "psql", "-U", "postgres", "-c",
			"CREATE TABLE ballast_integration_test (id int); INSERT INTO ballast_integration_test VALUES (1);"},
	}
	if res, err := r.Run(ctx, nil, nil, seed); err != nil {
		t.Fatalf("seeding postgres: %v (stderr: %s)", err, res.Stderr)
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
	if err := inventory.PutService(db, inventory.Service{Name: "postgres", CatalogEntry: "postgres", Destination: "local"}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

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
		Catalog:   registryWith(t, map[string]string{"postgres.yaml": fmt.Sprintf(postgresIntegrationEntryYAML, containerName, image)}),
		Runner:    r,
		CredDir:   credDir,
		WorkRoot:  t.TempDir(),
		ResticBin: bin,
	}

	state, err := p.Run(ctx, "postgres")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != engine.SUCCESS {
		t.Fatalf("state = %s, want SUCCESS", state)
	}

	runs, err := engine.RecentRuns(db, "postgres", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	results, err := engine.VerificationResults(db, runs[0].ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	found := false
	for _, res := range results {
		if res.Method == "container-restore" {
			found = true
			if !res.Passed {
				t.Errorf("container-restore record not passed: %+v", res)
			}
		}
	}
	if !found {
		t.Error("no container-restore verification record recorded")
	}
}
