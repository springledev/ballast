package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/springledev/ballast/internal/dockerhost"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/secret"
)

// fakeSSHPrivateKey is a syntactically valid, unencrypted OpenSSH
// private key (the shape backend.RenderCredentials checks: BEGIN/END
// markers and a "PRIVATE KEY" body), used to exercise a docker host's
// SSH credential without depending on ssh-keygen being installed. It
// is not a real key that connects to anything.
const fakeSSHPrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	"ZmFrZS1rZXktY29udGVudHMtZm9yLXRlc3Rpbmc=\n" +
	"-----END OPENSSH PRIVATE KEY-----"

// versionDockerStub is a fake `docker` CLI that answers "version
// --format ..." with a fixed server version, so a connection test
// succeeds without a real Docker daemon. For a remote-ssh host, the
// real docker CLI would shell out through the generated ssh wrapper,
// but this stub is invoked directly and never looks at $PATH, so the
// wrapper is written but never exercised.
const versionDockerStub = "#!/bin/sh\n" +
	`if [ "$1" = "--host" ] && [ "$3" = "version" ]; then printf '26.1.4\n'; exit 0; fi` + "\n" +
	`if [ "$1" = "version" ]; then printf '26.1.4\n'; exit 0; fi` + "\n" +
	"echo unexpected docker stub args: \"$@\" >&2\nexit 2\n"

func writeDockerStub(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	return path
}

// sshHostResolver configures h.ops.DockerHosts with a temp SSH state
// directory and known_hosts file, so a remote-ssh host resolves
// without a real ssh installation (the generated wrapper script is
// never executed by the stub docker binaries these tests use).
func sshHostResolver(t *testing.T, dockerBin string) dockerhost.Resolver {
	t.Helper()
	return dockerhost.Resolver{
		CredDirs:       []string{t.TempDir()},
		DockerBin:      dockerBin,
		SSHBin:         "/usr/bin/ssh",
		StateDir:       t.TempDir(),
		KnownHostsFile: filepath.Join(t.TempDir(), "known_hosts"),
	}
}

// TestSetupDockerHostSSHSkipTest checks that an SSH credential is
// written to a 0600 file named after the default credential base name
// and that the key is registered for redaction, without a connection
// test running.
func TestSetupDockerHostSSHSkipTest(t *testing.T) {
	h := newHarness(t)
	res, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
		Host:        dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
		Credentials: dockerhost.Credentials{SSHPrivateKey: fakeSSHPrivateKey},
		SkipTest:    true,
	})
	if err != nil {
		t.Fatalf("SetupDockerHost: %v", err)
	}
	if res.Test != nil {
		t.Errorf("Test = %+v, want nil (skip_test)", res.Test)
	}
	wantCred := dockerhost.DefaultCredentialName("rpi")
	if res.Host.Credential != wantCred {
		t.Errorf("Credential = %q, want %q", res.Host.Credential, wantCred)
	}

	path := filepath.Join(h.credDir, wantCred)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file mode = %v, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading credential file: %v", err)
	}
	if got := string(data); got != fakeSSHPrivateKey+"\n" {
		t.Errorf("credential file contents = %q, want the key", got)
	}

	saved, err := inventory.GetDockerHost(h.ops.DB, "rpi")
	if err != nil {
		t.Fatalf("GetDockerHost: %v", err)
	}
	if saved.Credential != wantCred {
		t.Errorf("saved Credential = %q, want %q", saved.Credential, wantCred)
	}
}

// TestSetupDockerHostSSHSkipTestRedacts checks that the SSH key is
// added to o.Redact the moment it's saved, so it can never leak
// through a log line or error message from that point on (spec rule
// 2).
func TestSetupDockerHostSSHSkipTestRedacts(t *testing.T) {
	h := newHarness(t)
	redact := secret.NewSet()
	h.ops.Redact = redact
	if _, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
		Host:        dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
		Credentials: dockerhost.Credentials{SSHPrivateKey: fakeSSHPrivateKey},
		SkipTest:    true,
	}); err != nil {
		t.Fatalf("SetupDockerHost: %v", err)
	}
	if got := redact.Redact("here is a secret: " + fakeSSHPrivateKey + " end"); got == "here is a secret: "+fakeSSHPrivateKey+" end" {
		t.Errorf("Redact() did not redact the SSH key: %q", got)
	}
}

// TestSetupDockerHostTLSMissingCerts checks that a remote-tls host
// with no certificates given fails validation instead of saving a
// broken host.
func TestSetupDockerHostTLSMissingCerts(t *testing.T) {
	h := newHarness(t)
	_, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
		Host:     dockerhost.Host{Name: "nas", Kind: dockerhost.RemoteTLS, Address: "192.168.1.30"},
		SkipTest: true,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if _, gerr := inventory.GetDockerHost(h.ops.DB, "nas"); !errors.Is(gerr, inventory.ErrNotFound) {
		t.Errorf("host was saved despite missing certificates: %v", gerr)
	}
}

// TestSetupDockerHostRenameLocalRejected checks that the built-in
// local host can't be turned into a remote host.
func TestSetupDockerHostRenameLocalRejected(t *testing.T) {
	h := newHarness(t)
	_, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
		Host:     dockerhost.Host{Name: dockerhost.LocalName, Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
		SkipTest: true,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	saved, gerr := inventory.GetDockerHost(h.ops.DB, dockerhost.LocalName)
	if gerr != nil {
		t.Fatalf("GetDockerHost: %v", gerr)
	}
	if saved.Kind != dockerhost.Local {
		t.Errorf("local host Kind = %q, want it to stay local", saved.Kind)
	}
}

// TestSetupDockerHostConnectionFailureRollsBack checks that a failed
// connection test removes the credential file it just wrote (rather
// than leaving an unreachable host half-configured) and saves no row.
func TestSetupDockerHostConnectionFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	failingDocker := writeDockerStub(t, "#!/bin/sh\nexit 1\n")
	h.ops.DockerHosts = sshHostResolver(t, failingDocker)
	h.ops.DockerHosts.CredDirs = []string{h.credDir}
	h.ops.Runner = &runner.Runner{}

	_, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
		Host:        dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
		Credentials: dockerhost.Credentials{SSHPrivateKey: fakeSSHPrivateKey},
	})
	if err == nil {
		t.Fatal("SetupDockerHost: want an error from the failing connection test")
	}

	if _, gerr := inventory.GetDockerHost(h.ops.DB, "rpi"); !errors.Is(gerr, inventory.ErrNotFound) {
		t.Errorf("a host row was saved despite the failed connection test: %v", gerr)
	}
	path := filepath.Join(h.credDir, dockerhost.DefaultCredentialName("rpi"))
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("credential file %s was not rolled back: %v", path, serr)
	}
}

// TestSetupDockerHostSuccessReportsVersion checks the successful path:
// a connection test against a stub docker reporting a version saves
// the host and returns that version in the result.
func TestSetupDockerHostSuccessReportsVersion(t *testing.T) {
	h := newHarness(t)
	h.ops.DockerHosts = sshHostResolver(t, writeDockerStub(t, versionDockerStub))
	h.ops.DockerHosts.CredDirs = []string{h.credDir}
	h.ops.Runner = &runner.Runner{}

	res, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
		Host: dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
	})
	if err != nil {
		t.Fatalf("SetupDockerHost: %v", err)
	}
	if res.Test == nil || res.Test.ServerVersion != "26.1.4" {
		t.Fatalf("Test = %+v, want ServerVersion 26.1.4", res.Test)
	}
	if _, gerr := inventory.GetDockerHost(h.ops.DB, "rpi"); gerr != nil {
		t.Errorf("GetDockerHost: %v", gerr)
	}
}

// TestDeleteDockerHostInUse checks that a host backing a service can't
// be removed.
func TestDeleteDockerHostInUse(t *testing.T) {
	h := newHarness(t)
	h.addDestination("local")
	if _, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
		Host:     dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
		SkipTest: true,
	}); err != nil {
		t.Fatalf("SetupDockerHost: %v", err)
	}
	if err := inventory.PutService(h.ops.DB, inventory.Service{
		Name: "svc", CatalogEntry: "folder", Destination: "local", DockerHost: "rpi",
		Params: map[string]string{"path": t.TempDir()},
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	err := h.ops.DeleteDockerHost(context.Background(), "rpi")
	if !errors.Is(err, inventory.ErrInUse) {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
}

// TestDeleteDockerHostRemovesCredentialFiles checks that deleting a
// host Ballast wrote credentials for also removes those files, not
// just the database row.
func TestDeleteDockerHostRemovesCredentialFiles(t *testing.T) {
	h := newHarness(t)
	if _, err := h.ops.SetupDockerHost(context.Background(), DockerHostSetup{
		Host:        dockerhost.Host{Name: "rpi", Kind: dockerhost.RemoteSSH, Address: "ballast@192.168.1.20"},
		Credentials: dockerhost.Credentials{SSHPrivateKey: fakeSSHPrivateKey},
		SkipTest:    true,
	}); err != nil {
		t.Fatalf("SetupDockerHost: %v", err)
	}
	path := filepath.Join(h.credDir, dockerhost.DefaultCredentialName("rpi"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("credential file missing before delete: %v", err)
	}

	if err := h.ops.DeleteDockerHost(context.Background(), "rpi"); err != nil {
		t.Fatalf("DeleteDockerHost: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("credential file %s still exists after delete: %v", path, err)
	}
	if _, gerr := inventory.GetDockerHost(h.ops.DB, "rpi"); !errors.Is(gerr, inventory.ErrNotFound) {
		t.Errorf("host row still exists after delete: %v", gerr)
	}
}
