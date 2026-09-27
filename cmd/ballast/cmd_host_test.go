package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// versionDockerStub is a fake `docker` CLI that answers "version
// --format ..." with a fixed server version, so `ballast host add`/
// `test` succeed without a real Docker daemon.
const versionDockerStub = "#!/bin/sh\n" +
	`if [ "$1" = "version" ]; then printf '26.1.4\n'; exit 0; fi` + "\n" +
	`echo unexpected docker stub args: "$@" >&2` + "\n" +
	"exit 2\n"

// failingDockerStub always fails, so a connection test fails.
const failingDockerStub = "#!/bin/sh\nexit 1\n"

// withStubDockerCLI prepends a directory holding a fake "docker"
// executable to $PATH, so `ballast host` commands that connect to a
// local-kind host work hermetically in tests.
func withStubDockerCLI(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHostAddListRemove(t *testing.T) {
	cliEnv(t, &spyRunner{})
	withStubDockerCLI(t, versionDockerStub)

	out, err := execCLI(t, "host", "add", "extra", "--kind", "local")
	if err != nil {
		t.Fatalf("host add: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "extra") || !strings.Contains(out, "26.1.4") {
		t.Errorf("output = %q, want it to name the host and the Docker version found", out)
	}

	out, err = execCLI(t, "host", "list")
	if err != nil {
		t.Fatalf("host list: %v", err)
	}
	if !strings.Contains(out, "extra") || !strings.Contains(out, "local") {
		t.Errorf("list output = %q, want the host name and kind listed", out)
	}

	if _, err := execCLI(t, "host", "remove", "extra"); err != nil {
		t.Fatalf("host remove: %v", err)
	}
	out, err = execCLI(t, "host", "list")
	if err != nil {
		t.Fatalf("host list after remove: %v", err)
	}
	if strings.Contains(out, "extra") {
		t.Errorf("list output after remove = %q, want it gone", out)
	}
}

func TestHostAddNoTest(t *testing.T) {
	cliEnv(t, &spyRunner{})
	withStubDockerCLI(t, failingDockerStub)

	out, err := execCLI(t, "host", "add", "extra", "--kind", "local", "--no-test")
	if err != nil {
		t.Fatalf("host add --no-test: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "not tested") {
		t.Errorf("output = %q, want it to say the host was not tested", out)
	}
}

func TestHostAddConnectionFailure(t *testing.T) {
	cliEnv(t, &spyRunner{})
	withStubDockerCLI(t, failingDockerStub)

	_, err := execCLI(t, "host", "add", "extra", "--kind", "local")
	if err == nil {
		t.Fatal("host add with a failing connection: want an error")
	}

	out, lerr := execCLI(t, "host", "list")
	if lerr != nil {
		t.Fatalf("host list: %v", lerr)
	}
	if strings.Contains(out, "extra") {
		t.Errorf("list output = %q, want the failed host not to have been saved", out)
	}
}

func TestHostAddRequiresKind(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "host", "add", "extra"); err == nil {
		t.Fatal("host add without --kind: want an error")
	}
}

func TestHostTest(t *testing.T) {
	cliEnv(t, &spyRunner{})
	withStubDockerCLI(t, versionDockerStub)
	if _, err := execCLI(t, "host", "add", "extra", "--kind", "local"); err != nil {
		t.Fatalf("host add: %v", err)
	}
	out, err := execCLI(t, "host", "test", "extra")
	if err != nil {
		t.Fatalf("host test: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "reachable") || !strings.Contains(out, "26.1.4") {
		t.Errorf("output = %q, want it to report reachability and the version", out)
	}
}

func TestHostRemoveLocalRejected(t *testing.T) {
	cliEnv(t, &spyRunner{})
	if _, err := execCLI(t, "host", "remove", "local"); err == nil {
		t.Fatal("host remove local: want an error")
	}
}

// fakeSSHKey is a syntactically valid, unencrypted OpenSSH private
// key used to exercise --ssh-key-file without depending on ssh-keygen
// being installed. It is not a real key that connects to anything.
const fakeSSHKey = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	"ZmFrZS1rZXktY29udGVudHMtZm9yLXRlc3Rpbmc=\n" +
	"-----END OPENSSH PRIVATE KEY-----\n"

// TestHostAddSSHKeyFileNeverLeaks checks that a Docker host's SSH key
// file contents never appear in the command's own output (spec rule
// 2): --no-test is used so no connection (and thus no SSH shim
// exec'd) is needed.
func TestHostAddSSHKeyFileNeverLeaks(t *testing.T) {
	cliEnv(t, &spyRunner{})
	keyFile := filepath.Join(t.TempDir(), "ballast_host_key")
	if err := os.WriteFile(keyFile, []byte(fakeSSHKey), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execCLI(t, "host", "add", "rpi",
		"--kind", "remote-ssh", "--address", "ballast@192.168.1.20",
		"--ssh-key-file", keyFile, "--no-test")
	if err != nil {
		t.Fatalf("host add: %v (output %q)", err, out)
	}
	if strings.Contains(out, "fake-key-contents-for-testing") || strings.Contains(out, "PRIVATE KEY") {
		t.Errorf("output leaks the SSH key: %q", out)
	}

	listOut, err := execCLI(t, "host", "list")
	if err != nil {
		t.Fatalf("host list: %v", err)
	}
	if strings.Contains(listOut, "fake-key-contents-for-testing") || strings.Contains(listOut, "PRIVATE KEY") {
		t.Errorf("list output leaks the SSH key: %q", listOut)
	}
}

func TestServiceAddHostFlag(t *testing.T) {
	withStubRestic(t)
	cliEnv(t, &spyRunner{})
	withStubDockerCLI(t, versionDockerStub)

	if _, err := execCLI(t, "destination", "add", "local", "--location", "/srv/backups"); err != nil {
		t.Fatalf("destination add: %v", err)
	}

	// An unknown Docker host is rejected.
	if _, err := execCLI(t, "service", "add", "svc", "--catalog", "folder", "--destination", "local",
		"--param", "path=/tmp", "--host", "extra"); err == nil {
		t.Fatal("service add with an unknown --host: want an error")
	}

	if _, err := execCLI(t, "host", "add", "extra", "--kind", "local"); err != nil {
		t.Fatalf("host add: %v", err)
	}
	out, err := execCLI(t, "service", "add", "svc", "--catalog", "folder", "--destination", "local",
		"--param", "path=/tmp", "--host", "extra")
	if err != nil {
		t.Fatalf("service add: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "extra") {
		t.Errorf("output = %q, want it to mention the Docker host", out)
	}

	listOut, err := execCLI(t, "service", "list")
	if err != nil {
		t.Fatalf("service list: %v", err)
	}
	if !strings.Contains(listOut, "host=extra") {
		t.Errorf("list output = %q, want it to show host=extra", listOut)
	}
}
