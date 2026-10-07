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
	"github.com/springledev/keel/runner"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// runBundledEntry runs one real bundled catalog entry through the
// pipeline and asserts VERIFYING -> SUCCESS with the wanted passing
// verification methods. secrets maps a secret name to its value, which
// is written to a credential file in the run's credential directory.
func runBundledEntry(t *testing.T, entryName string, params, secrets map[string]string, wantMethods ...string) {
	t.Helper()
	bin := requireRestic(t)
	p, _ := newPipelineHarness(t, bin)
	refs := map[string]string{}
	for name, value := range secrets {
		file := "ballast-test-" + name
		if err := os.WriteFile(filepath.Join(p.CredDir, file), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		refs[name] = file
	}
	if err := inventory.PutService(p.DB, inventory.Service{
		Name: entryName, CatalogEntry: entryName, Destination: "local", Params: params, Secrets: refs,
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	state, err := p.Run(context.Background(), entryName)
	if err != nil || state != engine.SUCCESS {
		t.Fatalf("Run = %s, %v; want SUCCESS", state, err)
	}
	runs, err := engine.RecentRuns(p.DB, entryName, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	if seq := transitionsOf(t, p.DB, runs[0].ID); !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Errorf("transitions = %v, want VERIFYING then SUCCESS", seq)
	}
	results, err := engine.VerificationResults(p.DB, runs[0].ID)
	if err != nil {
		t.Fatalf("VerificationResults: %v", err)
	}
	passed := map[string]bool{}
	for _, res := range results {
		if !res.Passed {
			t.Errorf("verification record not passed: %+v", res)
		}
		passed[res.Method] = true
	}
	for _, m := range wantMethods {
		if !passed[m] {
			t.Errorf("no passing %s record (got %v)", m, passed)
		}
	}
}

// startFixture starts a throwaway container and removes it afterwards.
func startFixture(t *testing.T, req testcontainers.ContainerRequest) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed; skipping integration test")
	}
	requireDockerImage(t, req.Image)
	c, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	if err != nil {
		t.Skipf("could not start %s: %v", req.Image, err)
	}
	t.Cleanup(func() { terminateContainer(c) })
}

func dockerExec(t *testing.T, args ...string) {
	t.Helper()
	res, err := (&runner.Runner{}).Run(context.Background(), nil, nil, runner.Command{Name: "docker", Args: append([]string{"exec"}, args...)})
	if err != nil {
		t.Fatalf("docker exec %v: %v (stderr: %s)", args, err, res.Stderr)
	}
}

// TestPipelinePostgresDBUserIntegration backs up a Postgres whose only
// superuser is "app" (no "postgres" role) through the bundled
// postgres.yaml with db_user=app, and proves the restore drill passes.
func TestPipelinePostgresDBUserIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	name := fmt.Sprintf("ballast-pg-app-%d", time.Now().UnixNano())
	startFixture(t, testcontainers.ContainerRequest{
		Image: "postgres:16", Name: name,
		Env:        map[string]string{"POSTGRES_USER": "app", "POSTGRES_PASSWORD": "x", "POSTGRES_DB": "appdb"},
		WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(120 * time.Second),
	})
	dockerExec(t, name, "psql", "-U", "app", "-d", "appdb", "-c", "CREATE TABLE t(id int); INSERT INTO t VALUES (1);")
	runBundledEntry(t, "postgres", map[string]string{"container": name, "image": "postgres:16", "db_user": "app"}, nil, "db-restore-drill", "restic-read-back")
}

// TestPipelineMariaDBAppUserIntegration backs up one database as an app
// user with the password as a secret, on both a MySQL image and a
// MariaDB 11 image (which ships only mariadb-dump and mariadb-admin, so
// this proves the dump probe and the restore-drill dialect probe).
func TestPipelineMariaDBAppUserIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, image := range []string{"mysql:8", "mariadb:11"} {
		t.Run(image, func(t *testing.T) {
			name := fmt.Sprintf("ballast-sql-app-%d", time.Now().UnixNano())
			startFixture(t, testcontainers.ContainerRequest{
				Image: image, Name: name,
				Env: map[string]string{
					"MYSQL_ROOT_PASSWORD": "rootpw", "MYSQL_DATABASE": "appdb",
					"MYSQL_USER": "app", "MYSQL_PASSWORD": "app-pw-123",
				},
				ExposedPorts: []string{"3306/tcp"},
				WaitingFor:   wait.ForListeningPort("3306/tcp").WithStartupTimeout(180 * time.Second),
			})
			// Seed through the same client the image ships.
			dockerExec(t, "-e", "MYSQL_PWD=app-pw-123", name, "sh", "-c",
				`c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec $c -u app appdb -e "CREATE TABLE t(id int); INSERT INTO t VALUES (1);"`)
			runBundledEntry(t, "mariadb-db",
				map[string]string{"container": name, "image": image, "db_user": "app", "db_name": "appdb"},
				map[string]string{"db_password": "app-pw-123"}, "db-restore-drill", "restic-read-back")
		})
	}
}

// TestPipelineGiteaIntegration backs up a real Gitea (official root
// image, which refuses to dump as root) with the bundled gitea.yaml and
// proves the zip is read back and restic re-reads the snapshot.
func TestPipelineGiteaIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	name := fmt.Sprintf("ballast-gitea-%d", time.Now().UnixNano())
	startFixture(t, testcontainers.ContainerRequest{
		Image: "gitea/gitea:1.22", Name: name,
		Env: map[string]string{
			"GITEA__database__DB_TYPE":      "sqlite3",
			"GITEA__security__INSTALL_LOCK": "true",
			"GITEA__server__DISABLE_SSH":    "true",
		},
		ExposedPorts: []string{"3000/tcp"},
		WaitingFor:   wait.ForListeningPort("3000/tcp").WithStartupTimeout(180 * time.Second),
	})
	// A brand-new Gitea has no repositories folder until the first
	// repository is created; make it so `gitea dump` has something to read.
	dockerExec(t, "-u", "git", name, "sh", "-c", "mkdir -p /data/git/repositories/ballast && echo hi > /data/git/repositories/ballast/f")
	runBundledEntry(t, "gitea", map[string]string{"container": name}, nil, "archive-integrity", "restic-read-back")
}

// TestPipelineHostConfigIntegration backs up the bundled host-config
// entry (this machine's own units and files) and proves it verifies.
func TestPipelineHostConfigIntegration(t *testing.T) {
	for _, p := range []string{"/etc/systemd/system", "/usr/local/bin", "/etc/fstab", "/etc/hosts"} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s missing on this machine: %v", p, err)
		}
	}
	runBundledEntry(t, "host-config", nil, nil, "restic-read-back")
}
