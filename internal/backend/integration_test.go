package backend_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// These tests run the real restic binary against real storage servers
// in containers (spec §17 tests: S3 in testcontainers — SeaweedFS, since
// MinIO's community edition is no longer maintained —;
// containerized sshd for SFTP; bad-key/wrong-bucket fail loudly").
// They skip when restic, docker or the image is missing, or in -short.

const (
	seaweedImage = "chrislusf/seaweedfs:4.47"
	sftpImage    = "atmoz/sftp:alpine"
)

func requireIntegration(t *testing.T, image string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	bin := os.Getenv("BALLAST_TEST_RESTIC")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("restic"); err != nil {
			t.Skip("restic not installed; set BALLAST_TEST_RESTIC or install restic to run this integration test")
		}
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("docker image %s not available locally; skipping integration test", image)
	}
	return bin
}

// writeCred writes a 0600 credential file into dir.
func writeCred(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sampleData returns a directory with one file to back up.
func sampleData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("ballast integration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// roundTrip initializes the repository, backs up sample data, and
// reads it back — the whole path a real destination must support.
func roundTrip(t *testing.T, ctx context.Context, repo *restic.Repository) {
	t.Helper()
	if err := repo.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	res, err := repo.Backup(ctx, restic.BackupOptions{Paths: []string{sampleData(t)}})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snaps))
	}
	rb, err := repo.ReadBack(ctx, res.SnapshotID)
	if err != nil {
		t.Fatalf("ReadBack: %v", err)
	}
	if len(rb.Samples) == 0 {
		t.Fatalf("ReadBack returned no samples")
	}
}

func TestS3SeaweedFSIntegration(t *testing.T) {
	resticBin := requireIntegration(t, seaweedImage)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const rootUser, rootPass = "ballastadmin", "ballast-s3-secret-123"
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: seaweedS3Request(t, rootUser, rootPass),
		Started:          true,
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("starting seaweedfs: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "8333/tcp")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + host + ":" + port.Port()

	// Keys in Ballast's own environment must never be used instead of
	// the destination's credential file.
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient-wrong-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-wrong-secret")

	credDir := t.TempDir()
	writeCred(t, credDir, "pw", []byte("repo-password-for-tests\n"))
	good, _, err := backend.RenderCredentials(backend.AWSCredential, backend.Credentials{AccessKeyID: rootUser, SecretAccessKey: rootPass})
	if err != nil {
		t.Fatal(err)
	}
	writeCred(t, credDir, "keys-good", good)
	const wrongSecret = "definitely-the-wrong-secret"
	bad, _, err := backend.RenderCredentials(backend.AWSCredential, backend.Credentials{AccessKeyID: rootUser, SecretAccessKey: wrongSecret})
	if err != nil {
		t.Fatal(err)
	}
	writeCred(t, credDir, "keys-bad", bad)

	redact := secret.NewSet()
	r := backend.Resolver{CredDirs: []string{credDir}, Runner: &runner.Runner{Redact: redact}, ResticBin: resticBin, Redact: redact}
	open := func(bucket, cred string) *restic.Repository {
		t.Helper()
		s := backend.Settings{Endpoint: endpoint, Bucket: bucket, Path: "ballast"}
		loc, err := backend.Location(backend.S3, s)
		if err != nil {
			t.Fatal(err)
		}
		repo, err := r.Open(backend.Target{Name: "s3", Type: backend.S3, Settings: s, Location: loc, PasswordCredential: "pw", BackendCredential: cred})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return repo
	}

	t.Run("round trip", func(t *testing.T) {
		roundTrip(t, ctx, open("ballast-test", "keys-good"))
	})

	t.Run("wrong secret fails loudly and is redacted", func(t *testing.T) {
		_, err := open("ballast-test", "keys-bad").Snapshots(ctx, restic.SnapshotFilter{})
		if err == nil {
			t.Fatal("Snapshots with a wrong secret key succeeded")
		}
		if restic.IsRepoMissing(err) {
			t.Fatalf("a wrong secret key was reported as a missing repository: %v", err)
		}
		if strings.Contains(err.Error(), wrongSecret) {
			t.Fatalf("error leaks the secret key: %v", err)
		}
		if !strings.Contains(err.Error(), "secret access key") {
			t.Errorf("error does not explain the likely cause: %v", err)
		}
	})

	t.Run("wrong bucket fails loudly", func(t *testing.T) {
		_, err := open("no-such-bucket", "keys-good").Snapshots(ctx, restic.SnapshotFilter{})
		if err == nil {
			t.Fatal("Snapshots against a bucket that does not exist succeeded")
		}
		if !strings.Contains(err.Error(), "bucket does not exist") {
			t.Errorf("error does not explain the likely cause: %v", err)
		}
	})
}

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

func TestSFTPIntegration(t *testing.T) {
	resticBin := requireIntegration(t, sftpImage)
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not installed")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	keyDir := t.TempDir()
	genKey := func(name string) string {
		t.Helper()
		p := filepath.Join(keyDir, name)
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", p).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v: %s", err, out)
		}
		return p
	}
	goodKey, otherKey := genKey("good"), genKey("other")

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        sftpImage,
			Cmd:          []string{"ballast::1001::upload"},
			ExposedPorts: []string{"22/tcp"},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      goodKey + ".pub",
				ContainerFilePath: "/home/ballast/.ssh/keys/good.pub",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForLog("Server listening on 0.0.0.0 port 22").WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("starting sftp server: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := c.MappedPort(ctx, "22/tcp")
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(mapped.Port())

	credDir := t.TempDir()
	writeCred(t, credDir, "pw", []byte("repo-password-for-tests\n"))
	for name, path := range map[string]string{"key-good": goodKey, "key-other": otherKey} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data, _, err := backend.RenderCredentials(backend.SSHCredential, backend.Credentials{SSHPrivateKey: string(raw)})
		if err != nil {
			t.Fatalf("RenderCredentials(%s): %v", name, err)
		}
		writeCred(t, credDir, name, data)
	}

	knownHosts := filepath.Join(t.TempDir(), "ssh", "known_hosts")
	redact := secret.NewSet()
	r := backend.Resolver{CredDirs: []string{credDir}, Runner: &runner.Runner{Redact: redact}, ResticBin: resticBin, Redact: redact, KnownHostsFile: knownHosts}
	open := func(cred string) *restic.Repository {
		t.Helper()
		s := backend.Settings{Host: host, Port: port, User: "ballast", Path: "/upload/repo"}
		loc, err := backend.Location(backend.SFTP, s)
		if err != nil {
			t.Fatal(err)
		}
		repo, err := r.Open(backend.Target{Name: "sftp", Type: backend.SFTP, Settings: s, Location: loc, PasswordCredential: "pw", BackendCredential: cred})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return repo
	}

	t.Run("round trip", func(t *testing.T) {
		roundTrip(t, ctx, open("key-good"))
		if info, err := os.Stat(knownHosts); err != nil || info.Size() == 0 {
			t.Fatalf("server host key was not remembered in %s: %v", knownHosts, err)
		}
	})

	t.Run("unauthorized key fails loudly", func(t *testing.T) {
		_, err := open("key-other").Snapshots(ctx, restic.SnapshotFilter{})
		if err == nil {
			t.Fatal("Snapshots with a key the server does not accept succeeded")
		}
		var ee *runner.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("want a restic exit error, got %v", err)
		}
		if restic.IsRepoMissing(err) {
			t.Fatalf("a rejected key was reported as a missing repository: %v", err)
		}
		if !strings.Contains(err.Error(), "rejected the SSH key") {
			t.Errorf("error does not explain the likely cause: %v", err)
		}
	})
}
