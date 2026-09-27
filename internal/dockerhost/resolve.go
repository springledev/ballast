package dockerhost

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/secret"
)

// Resolver turns configured Hosts into Docker handles, resolving
// credential files against the credential directories. The pipeline,
// the API and the CLI all resolve hosts through one Resolver so a host
// behaves the same everywhere.
type Resolver struct {
	// CredDirs are searched in order for each credential file.
	CredDirs []string
	// DockerBin overrides the docker binary (tests); "" = "docker".
	DockerBin string
	// SSHBin is the real ssh program remote-ssh hosts use; "" resolves
	// "ssh" against $PATH when a remote-ssh host is resolved.
	SSHBin string
	// StateDir holds generated per-host ssh wrappers
	// (<StateDir>/<host>/ssh). Required for remote-ssh hosts.
	StateDir string
	// KnownHostsFile is where remote-ssh hosts' SSH host keys are
	// remembered (trust on first use) — the same file SFTP
	// destinations use. Required for remote-ssh hosts.
	KnownHostsFile string
	// Redact receives the contents of each secret credential file, so
	// nothing a host's key contains can reach a log or error.
	Redact *secret.Set
}

// ambientDockerEnv blanks every Docker CLI setting Ballast's own
// environment could carry, so a configured host is reached exactly as
// configured: DOCKER_HOST or DOCKER_CONTEXT set for the service user
// must never redirect a remote host's commands elsewhere.
var ambientDockerEnv = []string{
	"DOCKER_HOST=",
	"DOCKER_CONTEXT=",
	"DOCKER_TLS_VERIFY=",
	"DOCKER_CERT_PATH=",
}

// Docker returns the handle for h. h must already be Normalized.
func (r Resolver) Docker(h Host) (Docker, error) {
	d := Docker{HostName: h.Name, bin: r.DockerBin}
	switch h.Kind {
	case Local:
		if h.Address != "" {
			d.global = []string{"--host", h.Address}
			d.env = append([]string(nil), ambientDockerEnv...)
		}
		return d, nil
	case RemoteSSH:
		shim, err := r.sshShim(h)
		if err != nil {
			return Docker{}, fmt.Errorf("docker host %s: %w", h.Name, err)
		}
		d.global = []string{"--host", h.Address}
		d.env = append(append([]string(nil), ambientDockerEnv...), "PATH="+filepath.Dir(shim)+string(os.PathListSeparator)+os.Getenv("PATH"))
		return d, nil
	case RemoteTLS:
		if h.Credential == "" {
			return Docker{}, fmt.Errorf("docker host %s: no TLS certificates are configured", h.Name)
		}
		ca, cert, key := TLSFiles(h.Credential)
		paths := make([]string, 0, 3)
		for _, name := range []string{ca, cert, key} {
			p := r.path(name)
			if err := runner.CheckCredentialFile(p); err != nil {
				return Docker{}, fmt.Errorf("docker host %s: TLS file: %w", h.Name, err)
			}
			paths = append(paths, p)
		}
		r.Redact.AddFile(paths[2])
		d.global = []string{"--host", h.Address, "--tlsverify",
			"--tlscacert", paths[0], "--tlscert", paths[1], "--tlskey", paths[2]}
		d.env = append([]string(nil), ambientDockerEnv...)
		return d, nil
	}
	return Docker{}, fmt.Errorf("docker host %s: unknown kind %q", h.Name, h.Kind)
}

func (r Resolver) path(name string) string {
	return filepath.Join(runner.FindCredentialDir(name, r.CredDirs...), name)
}

// sshShim writes (or rewrites) the ssh wrapper the docker CLI runs for
// a remote-ssh host and returns its path. The docker CLI always runs
// plain "ssh" from $PATH and has no way to pass ssh options, so a
// per-host wrapper named "ssh", first on the command's $PATH, adds
// them: this host's key only, never prompting (BatchMode), and host
// keys remembered in Ballast's own known_hosts on first connection —
// the same rules as SFTP destinations. It is rewritten atomically on
// every resolve, so it always matches the host's current settings.
func (r Resolver) sshShim(h Host) (string, error) {
	if r.StateDir == "" || r.KnownHostsFile == "" {
		return "", fmt.Errorf("no SSH state directory is configured (internal wiring error)")
	}
	sshBin := r.SSHBin
	if sshBin == "" {
		p, err := exec.LookPath("ssh")
		if err != nil {
			return "", fmt.Errorf("the ssh program is not installed, or not on $PATH (install openssh-client): %w", err)
		}
		sshBin = p
	}
	if abs, err := filepath.Abs(sshBin); err == nil {
		sshBin = abs
	}
	args := []string{sshBin,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + r.KnownHostsFile,
		"-o", "ConnectTimeout=20",
	}
	if h.Credential != "" {
		key := r.path(h.Credential)
		if err := runner.CheckCredentialFile(key); err != nil {
			return "", fmt.Errorf("SSH key: %w", err)
		}
		r.Redact.AddFile(key)
		args = append(args, "-i", key, "-o", "IdentitiesOnly=yes")
	}
	if err := os.MkdirAll(filepath.Dir(r.KnownHostsFile), 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(r.KnownHostsFile), err)
	}
	dir := filepath.Join(r.StateDir, h.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	script := "#!/bin/sh\n# Generated by Ballast for Docker host " + shellQuote(h.Name) +
		"; rewritten on every use, do not edit.\nexec " + strings.Join(quoted, " ") + " \"$@\"\n"
	path := filepath.Join(dir, "ssh")
	if err := writeFileAtomic(path, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("writing ssh wrapper %s: %w", path, err)
	}
	return path, nil
}

// shellQuote single-quotes s for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// TestTimeout bounds a connection test: long enough for a slow SSH
// handshake, short enough that a firewalled host fails promptly.
const TestTimeout = 45 * time.Second

// TestResult is a successful connection test's findings.
type TestResult struct {
	// ServerVersion is the remote Docker Engine's version.
	ServerVersion string `json:"server_version"`
}

// Test connects to d's daemon and asks for its version: it proves the
// address, the credentials and the daemon permissions all work, not
// just that the machine answers. Failures carry a plain-English hint.
func Test(ctx context.Context, r *runner.Runner, d Docker) (TestResult, error) {
	ctx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, nil, d.Command(nil, "version", "--format", "{{.Server.Version}}"))
	if err != nil {
		if hint := Hint(err, res.Stderr); hint != "" {
			return TestResult{}, fmt.Errorf("docker host %s: %s: %w", d.Name(), hint, err)
		}
		if ctx.Err() != nil {
			return TestResult{}, fmt.Errorf("docker host %s: no answer within %s — check the address, and that a firewall isn't blocking the connection: %w", d.Name(), TestTimeout, err)
		}
		return TestResult{}, fmt.Errorf("docker host %s: connection test failed: %w", d.Name(), err)
	}
	v := strings.TrimSpace(string(res.Stdout))
	if v == "" {
		return TestResult{}, fmt.Errorf("docker host %s: connected, but the Docker daemon did not report a version", d.Name())
	}
	return TestResult{ServerVersion: v}, nil
}

// hints maps docker/ssh error text to plain-English explanations,
// most specific first.
var hints = []struct{ match, hint string }{
	{"REMOTE HOST IDENTIFICATION HAS CHANGED", "the machine's SSH host key changed since Ballast first connected — if you reinstalled it, remove its line from Ballast's known_hosts file; otherwise treat this as a warning sign"},
	{"Host key verification failed", "the machine's SSH host key could not be verified"},
	{"Permission denied (publickey", "the machine refused the SSH key — add the key's public half to ~/.ssh/authorized_keys of that user on the Docker host"},
	{"Permission denied", "permission denied — check the user and key"},
	{"Connection closed by", "the machine closed the SSH connection before Ballast could log in — usually the SSH key isn't in that user's ~/.ssh/authorized_keys, or that user isn't allowed to log in over SSH. If a wrong key was tried moments ago, the machine may be briefly refusing new connections from Ballast: wait a minute and try again"},
	{"docker: command not found", "Docker is not installed on that machine (or not on the SSH user's $PATH)"},
	{"docker: not found", "Docker is not installed on that machine (or not on the SSH user's $PATH)"},
	{"permission denied while trying to connect to the Docker daemon", "the user can't use Docker there — add it to the docker group (sudo usermod -aG docker USER) and log in again"},
	{"Is the docker daemon running", "the Docker daemon isn't running there, or its socket isn't reachable (for Ballast in a container or LXC, mount /var/run/docker.sock)"},
	{"certificate signed by unknown authority", "the daemon's TLS certificate wasn't signed by the CA certificate you gave"},
	{"bad certificate", "the daemon rejected the client certificate — check cert.pem and key.pem come from the CA the daemon trusts"},
	{"certificate is valid for", "the daemon's TLS certificate doesn't list this address — connect using a host name or IP that the certificate was issued for"},
	{"Client sent an HTTP request to an HTTPS server", "that port expects TLS — use a remote-tls host"},
	{"server gave HTTP response to HTTPS client", "that port doesn't use TLS — the daemon is exposed without certificates, which is unsafe; set up TLS or use SSH instead"},
	{"Could not resolve hostname", "the machine's name couldn't be found — check the spelling, or use its IP address"},
	{"no such host", "the machine's name couldn't be found — check the spelling, or use its IP address"},
	{"Connection refused", "the machine refused the connection — check the port, and that SSH or the Docker daemon is listening"},
	{"connection refused", "the machine refused the connection — check the port, and that SSH or the Docker daemon is listening"},
	{"timed out", "the connection timed out — check the address, and that a firewall isn't blocking it"},
	{"No route to host", "the machine can't be reached on the network"},
}

// Hint returns a plain-English explanation for a failed docker command
// against a host, or "" when nothing specific is recognized.
func Hint(err error, stderr []byte) string {
	// ErrNotFound: not on $PATH; ErrNotExist: an absolute path that is gone.
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return "the docker command-line tool is not installed on the Ballast machine, or not on $PATH"
	}
	s := string(stderr)
	for _, h := range hints {
		if strings.Contains(s, h.match) {
			return h.hint
		}
	}
	return ""
}
