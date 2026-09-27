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
%s
  image: %s
  filterStderr:
    - "already exists"
  restartPolicy: never
`

// TestPipelineDumpPostgresIntegration runs the `dump` strategy and
// its verification end-to-end — once with the legacy single
// `container-restore` method (alias for `db-restore-drill`), and once
// declaring every built-in registry method together
// (archive-integrity + db-restore-drill + restart-smoke probing the
// live database over TCP; spec §17.6) — against a real PostgreSQL container (spec
// §14 step 11: "a dump integration test against a real Postgres
// container"), via testcontainers-go (AGENTS.md mandates it for
// Docker-dependent tests). It seeds a table, backs it up through the
// postgres catalog entry to a local restic repository, and asserts
// each run passes VERIFYING → SUCCESS with a passing record for every
// declared method plus restic's read-back.
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

	cases := []struct {
		name        string
		verifyBlock string
		wantMethods []string
	}{
		{
			name:        "legacy container-restore alias",
			verifyBlock: "  method: container-restore",
			wantMethods: []string{"db-restore-drill", "restic-read-back"},
		},
		{
			name: "every registry method together",
			verifyBlock: "  methods: [archive-integrity, db-restore-drill, restart-smoke]\n" +
				"  smoke:\n    container: " + containerName + "\n    timeout: 60s\n    tcp:\n      port: 5432",
			wantMethods: []string{"archive-integrity", "db-restore-drill", "restart-smoke", "restic-read-back"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runPostgresDumpCase(t, ctx, r, bin, containerName, image, tc.verifyBlock, tc.wantMethods)
		})
	}
}

// runPostgresDumpCase backs up the running postgres container through
// a fresh store and repository using verifyBlock, and asserts the run
// reaches SUCCESS through VERIFYING with every wantMethods record
// passing.
func runPostgresDumpCase(t *testing.T, ctx context.Context, r *runner.Runner, bin, containerName, image, verifyBlock string, wantMethods []string) {
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
		Catalog:   registryWith(t, map[string]string{"postgres.yaml": fmt.Sprintf(postgresIntegrationEntryYAML, containerName, verifyBlock, image)}),
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
	seq := transitionsOf(t, db, runs[0].ID)
	if !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions = %v, want VERIFYING then SUCCESS", seq)
	}
	results, err := engine.VerificationResults(db, runs[0].ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	passed := map[string]bool{}
	for _, res := range results {
		if !res.Passed {
			t.Errorf("verification record not passed: %+v", res)
		}
		passed[res.Method] = res.Passed
	}
	for _, m := range wantMethods {
		if !passed[m] {
			t.Errorf("no passing %s verification record (got %v)", m, passed)
		}
	}
}
