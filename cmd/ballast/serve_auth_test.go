package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// serveProc is a real `ballast serve` child process (the test binary
// acting as ballast, see TestHelperMain).
type serveProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	base   string
	mu     sync.Mutex
	output bytes.Buffer
	exited chan error
}

func (p *serveProc) stderr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.String()
}

// stop sends one SIGTERM and waits for a clean exit.
func (p *serveProc) stop() {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		p.t.Fatal(err)
	}
	select {
	case <-p.exited:
	case <-time.After(5 * time.Second):
		p.t.Fatal("serve did not stop")
	}
}

var listenRE = regexp.MustCompile(`listening on (http://\S+)`)

// startServe runs `ballast serve` on a free port with the given
// config dir and credentials dir ("" leaves it unset) and waits until
// it is listening.
func startServe(t *testing.T, dataDir, cfgDir, credDir string) *serveProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMain$")
	cmd.Env = append(os.Environ(),
		"BALLAST_TEST_MAIN=serve",
		"BALLAST_DATA_DIR="+dataDir,
		"BALLAST_CONFIG_DIR="+cfgDir,
		"CREDENTIALS_DIRECTORY="+credDir,
	)
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &serveProc{t: t, cmd: cmd, exited: make(chan error, 1)}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	listening := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			p.mu.Lock()
			p.output.WriteString(sc.Text() + "\n")
			p.mu.Unlock()
			if m := listenRE.FindStringSubmatch(sc.Text()); m != nil {
				listening <- m[1]
			}
		}
		p.exited <- cmd.Wait()
	}()
	select {
	case p.base = <-listening:
	case err := <-p.exited:
		t.Fatalf("serve exited before listening: %v\n%s", err, p.stderr())
	case <-time.After(10 * time.Second):
		t.Fatalf("serve never reported listening\n%s", p.stderr())
	}
	return p
}

func (p *serveProc) status(path, bearer string) int {
	p.t.Helper()
	req, _ := http.NewRequest("GET", p.base+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestServeFirstStartGeneratesKey: with no key configured, serve
// creates one, prints it exactly once, stores it owner-only, and the
// key works; a second start keeps the same key and prints nothing.
func TestServeFirstStartGeneratesKey(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	cfg := filepath.Join(t.TempDir(), "config")

	p := startServe(t, data, cfg, "")
	keyPath := filepath.Join(cfg, "credentials", apiTokenCredential)
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key file not created at %s: %v\n%s", keyPath, err, p.stderr())
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(keyPath)
	key := strings.TrimSpace(string(raw))
	if len(key) != 64 {
		t.Fatalf("key %q is not 256 bits of hex", key)
	}
	if p.status("/api/v1/status", "") != 401 || p.status("/api/v1/status", "wrong") != 401 {
		t.Error("anonymous or wrong-key request was not rejected")
	}
	if got := p.status("/api/v1/status", key); got != 200 {
		t.Errorf("request with the printed key = %d, want 200", got)
	}
	if p.status("/api/v1/health", "") != 200 {
		t.Error("health should need no key")
	}
	// Printed once, and never again however much the server logs.
	if n := strings.Count(p.stderr(), key); n != 1 {
		t.Errorf("key appears %d times in the server's output, want exactly 1:\n%s", n, p.stderr())
	}
	p.stop()

	p2 := startServe(t, data, cfg, "")
	defer p2.stop()
	raw2, _ := os.ReadFile(keyPath)
	if strings.TrimSpace(string(raw2)) != key {
		t.Error("second start replaced the existing key")
	}
	if strings.Contains(p2.stderr(), key) || strings.Contains(p2.stderr(), "first start") {
		t.Errorf("second start printed the key again:\n%s", p2.stderr())
	}
	if p2.status("/api/v1/status", key) != 200 {
		t.Error("existing key stopped working after restart")
	}
}

// TestServeKeepsExistingKey: a key supplied by the operator (systemd
// credential) is used unchanged, never printed, and nothing is generated.
func TestServeKeepsExistingKey(t *testing.T) {
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, apiTokenCredential), []byte("operator-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config")
	p := startServe(t, filepath.Join(t.TempDir(), "data"), cfg, credDir)
	defer p.stop()
	if p.status("/api/v1/status", "operator-key") != 200 {
		t.Error("operator's key rejected")
	}
	if strings.Contains(p.stderr(), "operator-key") || strings.Contains(p.stderr(), "first start") {
		t.Errorf("serve printed or regenerated an existing key:\n%s", p.stderr())
	}
	if _, err := os.Stat(filepath.Join(cfg, "credentials", apiTokenCredential)); err == nil {
		t.Error("a second key file was generated next to the operator's")
	}
}

var hexKeyRE = regexp.MustCompile(`(?m)^[0-9a-f]{64}$`)

// TestTokenRotateViaRunningDaemon: `ballast token rotate` asks the
// running daemon, which switches keys at once and stores the new one.
func TestTokenRotateViaRunningDaemon(t *testing.T) {
	cliEnv(t, &spyRunner{})
	cfg := os.Getenv("BALLAST_CONFIG_DIR")
	data := os.Getenv("BALLAST_DATA_DIR")
	credDir := filepath.Join(cfg, "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(credDir, apiTokenCredential)
	if err := os.WriteFile(keyPath, []byte("old-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startServe(t, data, cfg, "")
	defer p.stop()
	t.Setenv("BALLAST_URL", p.base)

	out, err := execCLI(t, "token", "rotate")
	if err != nil {
		t.Fatalf("token rotate: %v\n%s", err, out)
	}
	newKey := hexKeyRE.FindString(out)
	if newKey == "" {
		t.Fatalf("no new key printed:\n%s", out)
	}
	if got := p.status("/api/v1/status", "old-key"); got != 401 {
		t.Errorf("old key after rotate = %d, want 401", got)
	}
	if got := p.status("/api/v1/status", newKey); got != 200 {
		t.Errorf("new key = %d, want 200", got)
	}
	if raw, _ := os.ReadFile(keyPath); strings.TrimSpace(string(raw)) != newKey {
		t.Errorf("key file holds %q, want the new key", raw)
	}
	if strings.Contains(p.stderr(), newKey) {
		t.Error("the new key leaked into the server's output")
	}
}

// TestTokenRotateWithoutDaemon replaces the key file directly.
func TestTokenRotateWithoutDaemon(t *testing.T) {
	cliEnv(t, &spyRunner{})
	credDir := filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(credDir, apiTokenCredential)
	if err := os.WriteFile(keyPath, []byte("old-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := execCLI(t, "token", "rotate")
	if err != nil {
		t.Fatalf("token rotate: %v\n%s", err, out)
	}
	newKey := hexKeyRE.FindString(out)
	raw, _ := os.ReadFile(keyPath)
	if newKey == "" || strings.TrimSpace(string(raw)) != newKey || newKey == "old-key" {
		t.Fatalf("file = %q, printed key = %q", raw, newKey)
	}
	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}
