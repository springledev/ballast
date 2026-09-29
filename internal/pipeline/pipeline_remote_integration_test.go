package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/store"
	"github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestPipelineRemoteDestinationIntegration is the end-to-end proof for
// spec §14 step 17 / §17.1: a remote destination set up exactly the
// way the web UI and CLI do it (api.Ops.SetupDestination with typed
// settings and write-only credentials) backs a real backup job that
// reaches VERIFYING → SUCCESS, the connection test validates the
// credentials rather than mere reachability, a bad key fails setup
// and backups loudly, and no secret reaches the database, the log, an
// error or an API response. S3 runs against SeaweedFS (the code path B2
// and R2 share); SFTP against a containerized sshd.
func TestPipelineRemoteDestinationIntegration(t *testing.T) {
	bin := requireRestic(t)
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	t.Run("s3 via seaweedfs", func(t *testing.T) {
		requireDockerImage(t, seaweedImage)
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		const user, pass = "ballastadmin", "ballast-s3-secret-e2e-123"
		c := startContainer(t, ctx, seaweedS3Request(t, user, pass))
		host, port := hostPort(t, ctx, c, "8333/tcp")
		runRemoteE2E(t, ctx, bin, remoteCase{
			dest: inventory.Destination{
				Name: "s3", Type: backend.S3,
				Settings: backend.Settings{Endpoint: "http://" + host + ":" + strconv.Itoa(port), Bucket: "ballast-e2e", Path: "homelab"},
			},
			good:    backend.Credentials{AccessKeyID: user, SecretAccessKey: pass},
			bad:     backend.Credentials{AccessKeyID: user, SecretAccessKey: "wrong-secret-access-key-e2e"},
			secrets: []string{pass, "wrong-secret-access-key-e2e"},
		})
	})

	t.Run("sftp via sshd", func(t *testing.T) {
		requireDockerImage(t, "atmoz/sftp:alpine")
		if _, err := exec.LookPath("ssh-keygen"); err != nil {
			t.Skip("ssh-keygen not installed")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		keyDir := t.TempDir()
		genKey := func(name string) (path, private string) {
			path = filepath.Join(keyDir, name)
			if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
				t.Fatalf("ssh-keygen: %v: %s", err, out)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return path, string(raw)
		}
		goodPath, goodKey := genKey("good")
		_, badKey := genKey("bad")
		c := startContainer(t, ctx, testcontainers.ContainerRequest{
			Image:        "atmoz/sftp:alpine",
			Cmd:          []string{"ballast::1001::upload"},
			ExposedPorts: []string{"22/tcp"},
			Files: []testcontainers.ContainerFile{{
				HostFilePath: goodPath + ".pub", ContainerFilePath: "/home/ballast/.ssh/keys/good.pub", FileMode: 0o644,
			}},
			WaitingFor: wait.ForLog("Server listening on 0.0.0.0 port 22").WithStartupTimeout(time.Minute),
		})
		host, port := hostPort(t, ctx, c, "22/tcp")
		runRemoteE2E(t, ctx, bin, remoteCase{
			dest: inventory.Destination{
				Name: "nas", Type: backend.SFTP,
				Settings: backend.Settings{Host: host, Port: port, User: "ballast", Path: "/upload/ballast"},
			},
			good: backend.Credentials{SSHPrivateKey: goodKey},
			bad:  backend.Credentials{SSHPrivateKey: badKey},
			// The key's base64 body is what could leak; check a long
			// slice of it rather than the PEM armor lines.
			secrets: []string{keyBody(goodKey), keyBody(badKey)},
		})
	})
}

// seaweedImage is the pinned SeaweedFS release the S3 tests run against.
const seaweedImage = "chrislusf/seaweedfs:4.47"

// seaweedS3Request returns a container request for a SeaweedFS
// single-node server with its S3 API on 8333 and one identity holding
// the given key pair (a maintained, S3-compatible stand-in for B2/S3/R2
// in tests).
func seaweedS3Request(t *testing.T, accessKey, secretKey string) testcontainers.ContainerRequest {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "s3.json")
	identities := `{"identities":[{"name":"ballast","credentials":[{"accessKey":"` + accessKey + `","secretKey":"` + secretKey +
		`"}],"actions":["Admin","Read","Write","List","Tagging"]}]}`
	if err := os.WriteFile(cfg, []byte(identities), 0o644); err != nil {
		t.Fatal(err)
	}
	return testcontainers.ContainerRequest{
		Image:        seaweedImage,
		Cmd:          []string{"server", "-dir=/data", "-s3", "-s3.port=8333", "-s3.config=/etc/seaweedfs/s3.json"},
		ExposedPorts: []string{"8333/tcp"},
		Files:        []testcontainers.ContainerFile{{HostFilePath: cfg, ContainerFilePath: "/etc/seaweedfs/s3.json", FileMode: 0o644}},
		WaitingFor: wait.ForAll(
			wait.ForLog("Start Seaweed S3 API Server"),
			wait.ForListeningPort("8333/tcp"),
		).WithDeadline(2 * time.Minute),
	}
}

type remoteCase struct {
	dest      inventory.Destination
	good, bad backend.Credentials
	secrets   []string
}

func runRemoteE2E(t *testing.T, ctx context.Context, bin string, rc remoteCase) {
	t.Helper()
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "photo.txt"), []byte("irreplaceable family photo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "ballast.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	db := s.DB()

	operatorDir, ownDir := t.TempDir(), filepath.Join(t.TempDir(), "credentials")
	redact := secret.NewSet()
	var logBuf bytes.Buffer
	r := &runner.Runner{Redact: redact}
	p := &Pipeline{
		DB:             db,
		Catalog:        registryWith(t, map[string]string{"folder.yaml": yamlf(folderEntryYAML, sourceDir)}),
		Runner:         r,
		CredDir:        operatorDir,
		OwnCredDir:     ownDir,
		WorkRoot:       t.TempDir(),
		KnownHostsFile: filepath.Join(t.TempDir(), "ssh", "known_hosts"),
		Log:            &logBuf,
		Redact:         redact,
		ResticBin:      bin,
	}
	ops := &api.Ops{
		DB:                   db,
		Repo:                 func(d inventory.Destination) (*restic.Repository, error) { return p.Resolver().Open(d.Target()) },
		CredentialsDir:       ownDir,
		CredentialSearchDirs: []string{operatorDir, ownDir},
		Redact:               redact,
	}
	var errText strings.Builder
	record := func(err error) error {
		if err != nil {
			errText.WriteString(err.Error() + "\n")
		}
		return err
	}
	name := rc.dest.Name
	rc.dest.KeepDaily, rc.dest.KeepWeekly, rc.dest.KeepMonthly = 7, 4, 6

	// 1. A wrong key fails setup loudly and fast, and leaves no
	//    destination behind (the form can simply be resubmitted).
	start := time.Now()
	err = record(ops.SetupDestination(ctx, api.DestinationSetup{Destination: rc.dest, Credentials: rc.bad, Init: true}))
	if err == nil {
		t.Fatal("setup with a wrong key succeeded")
	}
	if errors.Is(err, api.ErrValidation) {
		t.Fatalf("wrong-key setup failed input validation, not at the server: %v", err)
	}
	if d := time.Since(start); d > time.Minute {
		t.Errorf("setup with a wrong key took %v; a typo must not look like a hang", d)
	}
	if _, err := inventory.GetDestination(db, name); err == nil {
		t.Error("a destination whose setup failed was kept")
	}
	t.Logf("wrong-key setup error (%v): %v", time.Since(start).Round(time.Millisecond), err)

	// 2. The right key: setup creates the repository, and the
	//    connection test passes.
	if err := record(ops.SetupDestination(ctx, api.DestinationSetup{Destination: rc.dest, Credentials: rc.good, Init: true})); err != nil {
		t.Fatalf("setup with the right key: %v", err)
	}
	if err := record(ops.TestDestination(ctx, name)); err != nil {
		t.Fatalf("connection test with the right key: %v", err)
	}

	// 3. A real backup job to it reaches VERIFYING → SUCCESS.
	if err := inventory.PutService(db, inventory.Service{Name: "photos", CatalogEntry: "folder", Destination: name}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	state, err := p.Run(ctx, "photos")
	if record(err) != nil || state != engine.SUCCESS {
		t.Fatalf("backup: state=%s err=%v, want SUCCESS", state, err)
	}
	runs, err := engine.RecentRuns(db, "photos", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns: %v, %v", runs, err)
	}
	seq := transitionsOf(t, db, runs[0].ID)
	if !containsInOrder(seq, string(engine.VERIFYING), string(engine.SUCCESS)) {
		t.Fatalf("transitions = %v, want VERIFYING before SUCCESS", seq)
	}
	results, err := engine.VerificationResults(db, runs[0].ID)
	if err != nil || len(results) == 0 {
		t.Fatalf("verification results: %v, %v", results, err)
	}
	for _, res := range results {
		if !res.Passed {
			t.Errorf("verification %s did not pass: %+v", res.Method, res)
		}
	}
	t.Logf("run %d transitions: %v; snapshot %s", runs[0].ID, seq, runs[0].SnapshotID)

	// 4. Swap in a wrong key without re-initializing (the server stays
	//    perfectly reachable): the connection test must now fail — it
	//    validates credentials, not reachability — and so must a backup.
	if err := record(ops.SetupDestination(ctx, api.DestinationSetup{Destination: rc.dest, Credentials: rc.bad})); err != nil {
		t.Fatalf("saving the wrong key: %v", err)
	}
	if err := record(ops.TestDestination(ctx, name)); err == nil {
		t.Fatal("connection test passed with a wrong key")
	} else {
		t.Logf("wrong-key connection test error: %v", err)
	}
	state, err = p.Run(ctx, "photos")
	_ = record(err)
	if state == engine.SUCCESS || state == engine.SUCCESS_UNVERIFIED {
		t.Fatalf("backup with a wrong key ended %s, want a failure", state)
	}

	// 5. No secret anywhere: API response shape, database file (incl.
	//    WAL), log, or any error text.
	list, err := ops.ListDestinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apiJSON, _ := json.Marshal(list)
	var dbBytes []byte
	for _, f := range []string{dbPath, dbPath + "-wal"} {
		b, _ := os.ReadFile(f)
		dbBytes = append(dbBytes, b...)
	}
	for _, sec := range rc.secrets {
		for where, text := range map[string]string{
			"API response": string(apiJSON), "database": string(dbBytes),
			"log": logBuf.String(), "errors": errText.String(),
		} {
			if strings.Contains(text, sec) {
				t.Errorf("a secret leaked into the %s", where)
			}
		}
	}
	// The saved credential file itself is root-only.
	d, err := inventory.GetDestination(db, name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(ownDir, d.BackendCredential))
	if err != nil {
		t.Fatalf("credential file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode = %04o, want 0600", info.Mode().Perm())
	}
}

func startContainer(t *testing.T, ctx context.Context, req testcontainers.ContainerRequest) testcontainers.Container {
	t.Helper()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("starting %s: %v", req.Image, err)
	}
	return c
}

func hostPort(t *testing.T, ctx context.Context, c testcontainers.Container, port string) (string, int) {
	t.Helper()
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.MappedPort(ctx, port)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(mp.Port())
	return host, n
}

// keyBody returns a long, distinctive slice of a PEM key's base64 body.
func keyBody(pem string) string {
	lines := strings.Split(strings.TrimSpace(pem), "\n")
	if len(lines) < 3 {
		return pem
	}
	return lines[1]
}
