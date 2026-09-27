package dockerhost

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/secret"
)

// writeScript writes an executable shell script.
func writeScript(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCred(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolverDocker(t *testing.T) {
	credDir := t.TempDir()
	state := t.TempDir()
	cert, key := testPEMs(t)
	writeCred(t, credDir, "tls.ca.pem", cert)
	writeCred(t, credDir, "tls.cert.pem", cert)
	writeCred(t, credDir, "tls.key.pem", key)
	writeCred(t, credDir, "sshkey", testSSHKey)
	redact := secret.NewSet()
	r := Resolver{CredDirs: []string{credDir}, DockerBin: "/stub/docker", SSHBin: "/usr/bin/ssh", StateDir: state, KnownHostsFile: filepath.Join(state, "kh", "known_hosts"), Redact: redact}

	tests := []struct {
		name     string
		host     Host
		wantArgs []string
		wantEnv  bool // ambient Docker settings blanked
		wantErr  string
	}{
		{"local ambient", Host{Name: "local", Kind: Local}, []string{"ps"}, false, ""},
		{"local socket", Host{Name: "sock", Kind: Local, Address: "unix:///run/d.sock"}, []string{"--host", "unix:///run/d.sock", "ps"}, true, ""},
		{"ssh", Host{Name: "nas", Kind: RemoteSSH, Address: "ssh://me@nas", Credential: "sshkey"}, []string{"--host", "ssh://me@nas", "ps"}, true, ""},
		{"ssh missing key", Host{Name: "nas2", Kind: RemoteSSH, Address: "ssh://me@nas", Credential: "nope"}, nil, false, "SSH key"},
		{"tls", Host{Name: "media", Kind: RemoteTLS, Address: "tcp://media:2376", Credential: "tls"}, []string{"--host", "tcp://media:2376", "--tlsverify",
			"--tlscacert", filepath.Join(credDir, "tls.ca.pem"), "--tlscert", filepath.Join(credDir, "tls.cert.pem"), "--tlskey", filepath.Join(credDir, "tls.key.pem"), "ps"}, true, ""},
		{"tls missing files", Host{Name: "m2", Kind: RemoteTLS, Address: "tcp://media:2376", Credential: "absent"}, nil, false, "TLS file"},
		{"tls no credential", Host{Name: "m3", Kind: RemoteTLS, Address: "tcp://media:2376"}, nil, false, "no TLS certificates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := r.Docker(tt.host)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), tt.host.Name) {
					t.Fatalf("error = %v, want containing %q and the host name", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c := d.Command(nil, "ps")
			if c.Name != "/stub/docker" || !reflect.DeepEqual(c.Args, tt.wantArgs) {
				t.Errorf("command = %s %v, want /stub/docker %v", c.Name, c.Args, tt.wantArgs)
			}
			env := strings.Join(c.Env, "\n")
			if blanked := strings.Contains(env, "DOCKER_HOST=\n") && strings.Contains(env, "DOCKER_CONTEXT="); blanked != tt.wantEnv {
				t.Errorf("ambient docker env blanked = %v, want %v (env %q)", blanked, tt.wantEnv, c.Env)
			}
			if d.Name() != tt.host.Name {
				t.Errorf("Name() = %q, want %q", d.Name(), tt.host.Name)
			}
		})
	}
	// Secret key files are registered for redaction on resolve.
	if got := redact.Redact(key); strings.Contains(got, "PRIVATE KEY-----\nM") {
		t.Error("TLS client key was not registered for redaction")
	}
	if got := redact.Redact(testSSHKey); got == testSSHKey {
		t.Error("SSH key was not registered for redaction")
	}
}

// TestSSHShim runs the generated wrapper against a fake ssh that
// prints its argv, proving the options reach ssh and the docker CLI's
// own arguments are passed through untouched.
func TestSSHShim(t *testing.T) {
	credDir, state := t.TempDir(), t.TempDir()
	writeCred(t, credDir, "k", testSSHKey)
	fakeSSH := writeScript(t, filepath.Join(t.TempDir(), "ssh"), `for a in "$@"; do printf '%s\n' "$a"; done`+"\n")
	kh := filepath.Join(state, "it's here", "known_hosts") // a quote in the path must survive
	r := Resolver{CredDirs: []string{credDir}, SSHBin: fakeSSH, StateDir: state, KnownHostsFile: kh}

	for _, tt := range []struct {
		name string
		host Host
		want []string
	}{
		{"with key", Host{Name: "nas", Kind: RemoteSSH, Address: "ssh://me@nas", Credential: "k"},
			[]string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + kh, "-o", "ConnectTimeout=20",
				"-i", filepath.Join(credDir, "k"), "-o", "IdentitiesOnly=yes", "--", "me@nas", "docker system dial-stdio"}},
		{"own keys", Host{Name: "nas2", Kind: RemoteSSH, Address: "ssh://me@nas"},
			[]string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + kh, "-o", "ConnectTimeout=20",
				"--", "me@nas", "docker system dial-stdio"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, err := r.Docker(tt.host)
			if err != nil {
				t.Fatal(err)
			}
			shim := filepath.Join(state, tt.host.Name, "ssh")
			if fi, err := os.Stat(shim); err != nil || fi.Mode().Perm() != 0o700 {
				t.Fatalf("shim %s: %v, mode %v", shim, err, fi)
			}
			var path string
			for _, e := range d.env {
				if v, ok := strings.CutPrefix(e, "PATH="); ok {
					path = v
				}
			}
			if !strings.HasPrefix(path, filepath.Dir(shim)+string(os.PathListSeparator)) {
				t.Fatalf("PATH %q does not start with the shim directory", path)
			}
			// Find "ssh" exactly as the docker CLI would: through PATH.
			t.Setenv("PATH", path)
			found, err := exec.LookPath("ssh")
			if err != nil || found != shim {
				t.Fatalf("ssh on PATH = %q (%v), want %q", found, err, shim)
			}
			out, err := exec.Command(found, "--", "me@nas", "docker system dial-stdio").Output()
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ssh argv =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Dir(kh)); err != nil {
		t.Errorf("known_hosts directory was not created: %v", err)
	}
	if _, err := (Resolver{SSHBin: fakeSSH}).Docker(Host{Name: "x", Kind: RemoteSSH, Address: "ssh://a@b"}); err == nil {
		t.Error("resolving an SSH host without a state directory: want an error")
	}
}

func TestTest(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		script   string
		want     string
		wantErr  string
		wantHint bool
	}{
		{"ok", `echo "27.3.1"`, "27.3.1", "", false},
		{"args", `[ "$1 $2 $3 $4" = "--host ssh://a@b version --format" ] && echo ok || { echo "bad args: $*" >&2; exit 1; }`, "ok", "", false},
		{"key refused", `echo "me@nas: Permission denied (publickey)." >&2; exit 255`, "", "refused the SSH key", true},
		{"no docker group", `echo "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock" >&2; exit 1`, "", "docker group", true},
		{"tls ca", `echo "tls: failed to verify certificate: x509: certificate signed by unknown authority" >&2; exit 1`, "", "CA certificate", true},
		{"unknown failure", `echo "something odd" >&2; exit 3`, "", "connection test failed", false},
		{"empty version", `exit 0`, "", "did not report a version", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := writeScript(t, filepath.Join(dir, tt.name), tt.script+"\n")
			d := Docker{HostName: "nas", bin: bin, global: []string{"--host", "ssh://a@b"}}
			res, err := Test(context.Background(), &runner.Runner{}, d)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "docker host nas") {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.ServerVersion != tt.want {
				t.Errorf("ServerVersion = %q, want %q", res.ServerVersion, tt.want)
			}
		})
	}
	_, err := Test(context.Background(), &runner.Runner{}, NewLocal(filepath.Join(dir, "missing-docker")))
	if err == nil || !strings.Contains(err.Error(), "not installed on the Ballast machine") {
		t.Errorf("missing docker binary: error = %v", err)
	}
}

func TestHint(t *testing.T) {
	if got := Hint(exec.ErrNotFound, nil); !strings.Contains(got, "not installed") {
		t.Errorf("Hint(ErrNotFound) = %q", got)
	}
	if got := Hint(errors.New("x"), []byte("Warning: REMOTE HOST IDENTIFICATION HAS CHANGED!\nHost key verification failed.")); !strings.Contains(got, "changed") {
		t.Errorf("changed host key: most specific hint should win, got %q", got)
	}
	if got := Hint(errors.New("x"), []byte("all fine")); got != "" {
		t.Errorf("unrecognized error: Hint = %q, want empty", got)
	}
}
