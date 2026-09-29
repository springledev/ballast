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

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/keel/dockerhost"
	"github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"
)

// dindImage is the "second Docker host": a Docker daemon in a
// container, reached only over SSH, exactly as Ballast reaches a
// Docker daemon in another LXC or VM.
const dindImage = "docker:28-dind"

// remoteDumpEntryYAML dumps a folder from a container by label
// discovery, so both `docker ps` and `docker exec` must reach the
// remote daemon for the run to succeed.
const remoteDumpEntryYAML = `
apiVersion: ballast.dev/v1
service: remoteapp
version: 1
discovery:
  app:
    type: docker-label
    match: ballast.test=remote-app
backup:
  strategy: dump
  dumps:
    - name: data
      container: "{{ .Discovery.app }}"
      command: ["tar", "-C", "/data", "-cf", "-", "."]
      format: tar
verify:
  method: archive-integrity
`

// TestPipelineDockerHostSSHIntegration backs up a container that only
// exists on a second Docker daemon, reached through a remote-ssh
// Docker host with its own key and a trust-on-first-use known_hosts
// file, and requires the run to reach VERIFYING → SUCCESS with the
// remote container's data in the snapshot. A wrong key must fail the
// connection test with the plain-English hint, and nothing may reach
// the local daemon.
func TestPipelineDockerHostSSHIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed; skipping integration test")
	}
	requireDockerImage(t, dindImage)
	requireDockerImage(t, "busybox:latest")
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not installed")
	}
	resticBin := requireRestic(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	keyDir := t.TempDir()
	genKey := func(name string) (path, pub string) {
		path = filepath.Join(keyDir, name)
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v: %s", err, out)
		}
		raw, err := os.ReadFile(path + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		return path, strings.TrimSpace(string(raw))
	}
	goodKey, goodPub := genKey("good")
	badKey, _ := genKey("bad")

	// sshd is added to the stock dind image at start: root logs in by
	// key only, and dockerd then runs as the image normally would.
	// PerSourcePenalties is off because the test deliberately tries a
	// wrong key right before the right one, and OpenSSH 9.8+ would
	// otherwise refuse the follow-up connection for a while (the hint
	// for "Connection closed" tells users about that case).
	setup := `set -e
apk add --no-cache openssh-server >/dev/null
ssh-keygen -A >/dev/null
sed -i 's/^root:!/root:*/' /etc/shadow
mkdir -p /root/.ssh && chmod 700 /root/.ssh
printf '%s\n' "$AUTHORIZED_KEY" > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys
/usr/sbin/sshd -o PermitRootLogin=prohibit-password -o PasswordAuthentication=no -o PerSourcePenalties=no
exec dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock`
	dind := startContainer(t, ctx, testcontainers.ContainerRequest{
		Image:        dindImage,
		Entrypoint:   []string{"sh", "-c", setup},
		Env:          map[string]string{"AUTHORIZED_KEY": goodPub, "DOCKER_TLS_CERTDIR": ""},
		ExposedPorts: []string{"22/tcp"},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.Privileged = true
		},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("22/tcp"),
			wait.ForLog("API listen on /var/run/docker.sock"),
		).WithDeadline(4 * time.Minute),
	})
	host, port := hostPort(t, ctx, dind, "22/tcp")

	p, repo := newPipelineHarness(t, resticBin)
	redact := secret.NewSet()
	var logBuf bytes.Buffer
	r := &runner.Runner{Redact: redact}
	ownDir := filepath.Join(t.TempDir(), "credentials")
	p.Runner, p.Redact, p.Log, p.OwnCredDir = r, redact, &logBuf, ownDir
	p.Catalog = registryWith(t, map[string]string{"remoteapp.yaml": remoteDumpEntryYAML})
	p.KnownHostsFile = filepath.Join(t.TempDir(), "ssh", "known_hosts")
	p.DockerStateDir = filepath.Join(t.TempDir(), "docker-hosts")
	resolver := p.DockerHosts()
	resolver.CredDirs = []string{p.CredDir, ownDir}
	// Hosts are set up exactly as the web UI and CLI do it.
	ops := &api.Ops{DB: p.DB, Catalog: p.Catalog, Runner: r, DockerHosts: resolver, CredentialsDir: ownDir,
		CredentialSearchDirs: []string{p.CredDir, ownDir}, Redact: redact}
	var errText strings.Builder
	record := func(err error) error {
		if err != nil {
			errText.WriteString(err.Error() + "\n")
		}
		return err
	}
	goodPEM, badPEM := string(mustRead(t, goodKey)), string(mustRead(t, badKey))
	nasHost := dockerhost.Host{Name: "nas", Kind: dockerhost.RemoteSSH, Address: "root@" + host + ":" + strconv.Itoa(port)}
	credFile := filepath.Join(ownDir, "ballast-dockerhost.nas")

	// 1. A key the machine doesn't accept: setup fails with a hint and
	// leaves nothing behind (no host, no key file).
	_, err := ops.SetupDockerHost(ctx, api.DockerHostSetup{Host: nasHost, Credentials: dockerhost.Credentials{SSHPrivateKey: badPEM}})
	if record(err) == nil || !strings.Contains(err.Error(), "SSH key") {
		t.Fatalf("setup with an unauthorized key = %v, want a hint about the SSH key", err)
	}
	if _, gerr := ops.GetDockerHost(ctx, "nas"); !errors.Is(gerr, inventory.ErrNotFound) {
		t.Errorf("failed setup saved the host anyway: %v", gerr)
	}
	if _, serr := os.Stat(credFile); !os.IsNotExist(serr) {
		t.Errorf("failed setup left its key file behind: %v", serr)
	}

	// 2. The right key: the connection test passes (key + daemon
	// access, not just reachability) and the host key is remembered.
	res, err := ops.SetupDockerHost(ctx, api.DockerHostSetup{Host: nasHost, Credentials: dockerhost.Credentials{SSHPrivateKey: goodPEM}})
	if record(err) != nil {
		t.Fatalf("setup with the authorized key: %v", err)
	}
	if res.Test == nil || res.Test.ServerVersion == "" {
		t.Fatalf("setup result %+v has no Docker version", res)
	}
	if res.Host.Address != "ssh://"+nasHost.Address {
		t.Errorf("saved address = %q", res.Host.Address)
	}
	if kh, err := os.ReadFile(p.KnownHostsFile); err != nil || len(kh) == 0 {
		t.Errorf("host key was not remembered in %s: %v", p.KnownHostsFile, err)
	}
	if fi, err := os.Stat(credFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file %s: %v (want mode 0600)", credFile, err)
	}
	remote, err := resolver.Docker(res.Host)
	if err != nil {
		t.Fatal(err)
	}

	// Seed the remote daemon: stream busybox into it over SSH (no
	// registry access needed) and start the labeled app container.
	if _, err := r.Pipeline(ctx, nil, nil,
		dockerhost.Docker{}.Command(nil, "save", "busybox:latest"),
		remote.Command(nil, "load"),
	); err != nil {
		t.Fatalf("loading busybox into the remote daemon: %v", err)
	}
	if _, err := r.Run(ctx, nil, nil, remote.Command(nil, "run", "-d", "--name", "remote-app-1", "--label", "ballast.test=remote-app",
		"busybox:latest", "sh", "-c", "mkdir -p /data && echo hello-from-the-second-host > /data/proof.txt && exec sleep 3600")); err != nil {
		t.Fatalf("starting the remote app container: %v", err)
	}
	// It must not exist locally, or a misrouted exec could pass.
	if err := exec.Command("docker", "inspect", "remote-app-1").Run(); err == nil {
		t.Fatal("a container named remote-app-1 exists on the local daemon; the routing check would be meaningless")
	}
	// Detection lists the remote host's containers, not local ones.
	det, err := ops.DetectServices(ctx, "nas")
	if record(err) != nil || len(det.Containers) != 1 || det.Containers[0].Name != "remote-app-1" {
		t.Errorf("DetectServices(nas) = %+v, %v; want only remote-app-1", det.Containers, err)
	}

	// 3. Backup + verification on the second host: VERIFYING → SUCCESS.
	if err := record(ops.PutService(ctx, inventory.Service{Name: "remoteapp", CatalogEntry: "remoteapp", Destination: "local", DockerHost: "nas"})); err != nil {
		t.Fatal(err)
	}
	state, err := p.Run(ctx, "remoteapp")
	if record(err) != nil || state != engine.SUCCESS {
		t.Fatalf("Run = %s, %v; want SUCCESS (VERIFYING passed)", state, err)
	}
	var sawVerifying bool
	for _, tr := range transitionsOf(t, p.DB, 1) {
		sawVerifying = sawVerifying || tr == string(engine.VERIFYING)
	}
	if !sawVerifying {
		t.Error("run reached SUCCESS without a recorded VERIFYING transition")
	}
	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{Tags: []string{"ballast-service:remoteapp"}, Latest: 1})
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots = %v, %v; want one", snaps, err)
	}
	restored := t.TempDir()
	if err := repo.Restore(ctx, restic.RestoreOptions{SnapshotID: snaps[0].ID, TargetDir: restored}); err != nil {
		t.Fatalf("restoring the snapshot: %v", err)
	}
	var tarPath string
	_ = filepath.Walk(restored, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == "data.tar" {
			tarPath = p
		}
		return nil
	})
	out, err := exec.Command("tar", "-xOf", tarPath, "./proof.txt").Output()
	if err != nil || strings.TrimSpace(string(out)) != "hello-from-the-second-host" {
		t.Fatalf("restored dump content = %q, %v; want the remote container's file", out, err)
	}

	// 4. The key is later swapped for one the machine refuses (saved
	// untested): the connection test and the next run both fail loudly.
	if _, err := ops.SetupDockerHost(ctx, api.DockerHostSetup{Host: nasHost, Credentials: dockerhost.Credentials{SSHPrivateKey: badPEM}, SkipTest: true}); record(err) != nil {
		t.Fatal(err)
	}
	if _, err := ops.TestDockerHost(ctx, "nas"); record(err) == nil || !strings.Contains(err.Error(), "SSH key") {
		t.Fatalf("connection test with an unauthorized key = %v, want a hint about the SSH key", err)
	}
	state, err = p.Run(ctx, "remoteapp")
	if record(err) == nil || state != engine.FAILED {
		t.Fatalf("Run with an unauthorized key = %s, %v; want FAILED", state, err)
	}

	// 5. The built-in local kind with an explicit socket path.
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		if _, err := ops.SetupDockerHost(ctx, api.DockerHostSetup{Host: dockerhost.Host{Name: "sock", Kind: dockerhost.Local, Address: "/var/run/docker.sock"}}); record(err) != nil {
			t.Errorf("local host on an explicit socket: %v", err)
		}
	}

	// No key ever reaches an API response, the database, the log or
	// an error message.
	hosts, _ := ops.ListDockerHosts(ctx)
	apiJSON, _ := json.Marshal([]any{hosts, res})
	var dbFile string
	if err := p.DB.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbFile); err != nil {
		t.Fatal(err)
	}
	var dbBytes []byte
	for _, f := range []string{dbFile, dbFile + "-wal"} {
		b, _ := os.ReadFile(f)
		dbBytes = append(dbBytes, b...)
	}
	for _, key := range []string{goodPEM, badPEM} {
		for where, text := range map[string]string{
			"API response": string(apiJSON), "database": string(dbBytes),
			"log": logBuf.String(), "errors": errText.String(),
		} {
			if strings.Contains(text, keyBody(key)) {
				t.Errorf("an SSH private key leaked into the %s", where)
			}
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
