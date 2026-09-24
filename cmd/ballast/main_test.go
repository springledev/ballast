package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
)

func TestVersionCmdPrintsVersion(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"version"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != version {
		t.Errorf("version output = %q, want %q", got, version)
	}
}

func TestRootListsCoreCommands(t *testing.T) {
	cmds := rootCmd.Commands()
	found := map[string]bool{}
	for _, c := range cmds {
		found[c.Name()] = true
	}
	for _, want := range []string{"serve", "version", "run-due", "trigger"} {
		if !found[want] {
			t.Errorf("missing expected subcommand %q", want)
		}
	}
}

func TestUnimplementedRunnerErrorsClearly(t *testing.T) {
	_, err := unimplementedRunner.Run(context.Background(), "immich")
	if err == nil {
		t.Error("the placeholder runner must error until the real pipeline is wired up")
	}
}

// spyRunner stands in for the real backup pipeline.
type spyRunner struct {
	ran   []string
	state engine.State
	err   error
}

func (r *spyRunner) Run(_ context.Context, service string) (engine.State, error) {
	r.ran = append(r.ran, service)
	state := r.state
	if state == "" {
		state = engine.SUCCESS
	}
	return state, r.err
}

// cliEnv points config at a fresh data dir, swaps in r as the
// pipeline, and returns the data dir. Everything is restored after
// the test.
func cliEnv(t *testing.T, r scheduler.Runner) string {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	t.Setenv("BALLAST_DATA_DIR", data)
	t.Setenv("BALLAST_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	prev := runner
	runner = r
	t.Cleanup(func() {
		runner = prev
		triggerToken, triggerTokenFile = "", ""
	})
	return data
}

// withDB opens the CLI's database in data to arrange test state.
func withDB(t *testing.T, data string, fn func(s *store.Store)) {
	t.Helper()
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(data, "ballast.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	fn(s)
}

func execCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	err := rootCmd.Execute()
	return out.String(), err
}

func TestRunDueCommand(t *testing.T) {
	r := &spyRunner{}
	data := cliEnv(t, r)
	withDB(t, data, func(s *store.Store) {
		if err := scheduler.SetSchedule(s.DB(), "immich", "* * * * *"); err != nil {
			t.Fatalf("SetSchedule: %v", err)
		}
	})

	if out, err := execCLI(t, "run-due"); err != nil {
		t.Fatalf("run-due: %v (output %q)", err, out)
	}
	if len(r.ran) != 1 || r.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", r.ran)
	}
	if _, err := os.Stat(filepath.Join(data, "locks")); err != nil {
		t.Errorf("run locks should live in the data dir: %v", err)
	}

	// A second sweep in the same minute runs nothing more.
	if _, err := execCLI(t, "run-due"); err != nil {
		t.Fatalf("second run-due: %v", err)
	}
	if len(r.ran) != 1 {
		t.Errorf("ran = %v, want no repeat within the same firing", r.ran)
	}
}

func TestRunDueCommandFailsLoudly(t *testing.T) {
	data := cliEnv(t, &spyRunner{err: errors.New("boom")})
	withDB(t, data, func(s *store.Store) {
		if err := scheduler.SetSchedule(s.DB(), "immich", "* * * * *"); err != nil {
			t.Fatalf("SetSchedule: %v", err)
		}
	})
	out, err := execCLI(t, "run-due")
	if err == nil || !strings.Contains(err.Error(), "immich") {
		t.Fatalf("run-due error = %v, want a failure naming immich", err)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("output = %q, want the failure explained", out)
	}
}

func TestTriggerCommand(t *testing.T) {
	r := &spyRunner{}
	data := cliEnv(t, r)
	var token string
	withDB(t, data, func(s *store.Store) {
		var err error
		if token, err = scheduler.GenerateTriggerToken(s.DB(), "immich"); err != nil {
			t.Fatalf("GenerateTriggerToken: %v", err)
		}
	})
	good := filepath.Join(t.TempDir(), "good")
	bad := filepath.Join(t.TempDir(), "bad")
	for path, content := range map[string]string{good: token + "\n", bad: "wrong\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	_, err := execCLI(t, "trigger", "immich", "--token-file", bad)
	if err == nil || !strings.Contains(err.Error(), "invalid or missing trigger token") {
		t.Errorf("trigger with wrong token: error = %v, want rejection", err)
	}
	triggerTokenFile = ""
	if _, err := execCLI(t, "trigger", "immich"); err == nil || !strings.Contains(err.Error(), "no trigger token given") {
		t.Errorf("trigger with no token: error = %v, want a clear message", err)
	}
	if len(r.ran) != 0 {
		t.Fatalf("ran = %v, want no runs for rejected triggers", r.ran)
	}

	out, err := execCLI(t, "trigger", "immich", "--token-file", good)
	if err != nil {
		t.Fatalf("trigger with valid token file: %v (output %q)", err, out)
	}
	if len(r.ran) != 1 || r.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", r.ran)
	}
	if strings.Contains(out, token) {
		t.Errorf("output %q leaks the token", out)
	}
}

func TestTriggerCommandReadsConfiguredCredentialDir(t *testing.T) {
	tests := []struct {
		name    string
		credDir func(t *testing.T) string // "" CREDENTIALS_DIRECTORY → config fallback
	}{
		{
			name: "systemd LoadCredential directory",
			credDir: func(t *testing.T) string {
				dir := t.TempDir()
				t.Setenv("CREDENTIALS_DIRECTORY", dir)
				return dir
			},
		},
		{
			name: "config dir fallback without systemd",
			credDir: func(t *testing.T) string {
				return filepath.Join(os.Getenv("BALLAST_CONFIG_DIR"), "credentials")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &spyRunner{}
			data := cliEnv(t, r)
			dir := tt.credDir(t)
			var token string
			withDB(t, data, func(s *store.Store) {
				var err error
				if token, err = scheduler.GenerateTriggerToken(s.DB(), "immich"); err != nil {
					t.Fatalf("GenerateTriggerToken: %v", err)
				}
			})
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "ballast-trigger-token.immich"), []byte(token+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			out, err := execCLI(t, "trigger", "immich")
			if err != nil {
				t.Fatalf("trigger: %v (output %q)", err, out)
			}
			if len(r.ran) != 1 || r.ran[0] != "immich" {
				t.Errorf("ran = %v, want [immich]", r.ran)
			}
			if !strings.Contains(out, "credential directory "+dir) {
				t.Errorf("output %q, want the credential directory used named (spec §8)", out)
			}
			if strings.Contains(out, token) {
				t.Errorf("output %q leaks the token", out)
			}
		})
	}
}

func TestTriggerCommandWithPlaceholderRunnerFails(t *testing.T) {
	data := cliEnv(t, unimplementedRunner)
	var token string
	withDB(t, data, func(s *store.Store) {
		var err error
		if token, err = scheduler.GenerateTriggerToken(s.DB(), "immich"); err != nil {
			t.Fatalf("GenerateTriggerToken: %v", err)
		}
	})
	_, err := execCLI(t, "trigger", "immich", "--token", token)
	if err == nil || !strings.Contains(err.Error(), "not wired up") {
		t.Errorf("trigger error = %v, want the placeholder's clear failure", err)
	}
}

func TestResolveTriggerToken(t *testing.T) {
	const secret = "s3cret-token"
	writeToken := func(t *testing.T, dir, name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
			t.Fatalf("writing token: %v", err)
		}
		return path
	}
	tests := []struct {
		name     string
		service  string // default immich
		flag     string
		file     func(t *testing.T) string
		credDir  func(t *testing.T) string
		want     string
		wantErr  string
		wantWarn bool
		wantLog  string
	}{
		{
			name: "token file",
			file: func(t *testing.T) string { return writeToken(t, t.TempDir(), "tok") },
			want: secret,
		},
		{
			name:     "flag still works but warns",
			flag:     secret,
			want:     secret,
			wantWarn: true,
		},
		{
			name:    "flag and file together rejected",
			flag:    secret,
			file:    func(t *testing.T) string { return writeToken(t, t.TempDir(), "tok") },
			wantErr: "not both",
		},
		{
			name: "credential file fallback, dir logged",
			credDir: func(t *testing.T) string {
				dir := t.TempDir()
				writeToken(t, dir, "ballast-trigger-token.immich")
				return dir
			},
			want:    secret,
			wantLog: "reading trigger token from credential directory",
		},
		{
			name: "another service's credential is not used",
			credDir: func(t *testing.T) string {
				dir := t.TempDir()
				writeToken(t, dir, "ballast-trigger-token.vaultwarden")
				return dir
			},
			wantErr: "ballast-trigger-token.immich",
		},
		{
			name:    "credential dir without the credential",
			credDir: func(t *testing.T) string { return t.TempDir() },
			wantErr: "no trigger token given",
		},
		{
			name:    "credential dir missing entirely",
			credDir: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			wantErr: "no trigger token given",
		},
		{
			name:    "service name that would escape the credential dir",
			service: "../immich",
			credDir: func(t *testing.T) string { return t.TempDir() },
			wantErr: "cannot be used in a credential file name",
		},
		{
			// Present but unreadable (here: a directory we cannot
			// search) must be reported as such, not as "no token".
			name: "credential present but unreadable",
			credDir: func(t *testing.T) string {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory permissions")
				}
				dir := t.TempDir()
				writeToken(t, dir, "ballast-trigger-token.immich")
				if err := os.Chmod(dir, 0o000); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Errorf("restoring permissions: %v", err)
					}
				})
				return dir
			},
			wantErr: "checking credential file",
		},
		{
			name:    "nothing given",
			credDir: func(t *testing.T) string { return t.TempDir() },
			wantErr: "no trigger token given",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var file, credDir string
			if tt.file != nil {
				file = tt.file(t)
			}
			if tt.credDir != nil {
				credDir = tt.credDir(t)
			}
			service := tt.service
			if service == "" {
				service = "immich"
			}
			var stderr bytes.Buffer
			got, err := resolveTriggerToken(service, tt.flag, file, credDir, &stderr)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q leaks the token", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTriggerToken: %v", err)
			}
			if got != secret {
				t.Errorf("token = %q, want %q", got, secret)
			}
			if gotWarn := strings.Contains(stderr.String(), "warning"); gotWarn != tt.wantWarn {
				t.Errorf("stderr = %q, want warning=%v", stderr.String(), tt.wantWarn)
			}
			if tt.wantLog != "" && !strings.Contains(stderr.String(), tt.wantLog+" "+credDir) {
				t.Errorf("stderr = %q, want %q naming %s", stderr.String(), tt.wantLog, credDir)
			}
			if strings.Contains(stderr.String(), secret) {
				t.Errorf("stderr %q leaks the token", stderr.String())
			}
		})
	}
}

func TestShutdownContextCancelledBySignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := shutdownContext(context.Background())
			defer stop()
			// Caught by shutdownContext's handler, so this does not
			// terminate the test binary.
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatalf("sending %v: %v", sig, err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("context not cancelled by %v", sig)
			}
		})
	}
}

func TestRunDueCommandPassesCancellationToRunner(t *testing.T) {
	var gotCtxErr error
	data := cliEnv(t, scheduler.RunnerFunc(func(ctx context.Context, _ string) (engine.State, error) {
		select {
		case <-ctx.Done(): // a real runner would restart stopped containers here
		case <-time.After(5 * time.Second):
			return "", errors.New("context never cancelled")
		}
		gotCtxErr = ctx.Err()
		return engine.CANCELLED, ctx.Err()
	}))
	withDB(t, data, func(s *store.Store) {
		if err := scheduler.SetSchedule(s.DB(), "immich", "* * * * *"); err != nil {
			t.Fatalf("SetSchedule: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"run-due"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	// Cobra only hands the root's context to a subcommand that has none
	// yet; earlier tests in this binary already ran run-due, so set it
	// directly. (main executes once, so production is unaffected.)
	runDueCmd.SetContext(ctx)
	t.Cleanup(func() { runDueCmd.SetContext(context.Background()) })
	time.AfterFunc(50*time.Millisecond, cancel)
	err := rootCmd.ExecuteContext(ctx)

	if !errors.Is(gotCtxErr, context.Canceled) {
		t.Errorf("runner saw ctx error %v, want context.Canceled", gotCtxErr)
	}
	if err == nil || !strings.Contains(out.String(), "shutting down") {
		t.Errorf("run-due error = %v, output %q; want the cut-short run reported", err, out.String())
	}
}

func TestRunDueCommandUsesMaxParallelJobs(t *testing.T) {
	data := cliEnv(t, &spyRunner{})
	conf := os.Getenv("BALLAST_CONFIG_DIR")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "ballast.yaml"), []byte("max_parallel_jobs: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	c, closeDB, err := openCoordinator(cfg, unimplementedRunner, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("openCoordinator: %v", err)
	}
	t.Cleanup(func() {
		if err := closeDB(); err != nil {
			t.Errorf("closing database: %v", err)
		}
	})
	if c.MaxParallel != 3 {
		t.Errorf("MaxParallel = %d, want 3 from ballast.yaml", c.MaxParallel)
	}
	if want := filepath.Join(data, "locks"); c.Locks.(scheduler.FileLocker).Dir != want {
		t.Errorf("lock dir = %v, want %s", c.Locks, want)
	}
}

func TestServeUntilDoneStopsOnCancelAndFinishesInFlightRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inHandler := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		time.Sleep(200 * time.Millisecond) // still running when shutdown starts
		fmt.Fprint(w, "done")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serveUntilDone(ctx, ln, h) }()

	type reply struct {
		body string
		err  error
	}
	replied := make(chan reply, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			replied <- reply{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		replied <- reply{string(b), err}
	}()
	<-inHandler
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serveUntilDone = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilDone did not return after its context was cancelled")
	}
	if r := <-replied; r.err != nil || r.body != "done" {
		t.Errorf("in-flight request got %q, %v; want it to finish with \"done\"", r.body, r.err)
	}
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Error("server still accepting connections after shutdown")
	}
}

func TestServeUntilDoneReportsServeFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve fails at once on a closed listener
	err = serveUntilDone(context.Background(), ln, http.NotFoundHandler())
	if err == nil || !strings.Contains(err.Error(), "web server stopped") {
		t.Errorf("serveUntilDone = %v, want the serve failure reported", err)
	}
}

// TestServeExitsOnOneSIGTERM runs the real main() with `serve` in a
// child process and sends it a single SIGTERM, as `systemctl stop`
// does: it must exit cleanly, not wait for a second signal.
func TestServeExitsOnOneSIGTERM(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMain$")
	cmd.Env = append(os.Environ(), "BALLAST_TEST_MAIN=serve")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	// Wait for the listening line, so the signal handler is installed
	// and the server is up before the signal is sent.
	listening := make(chan struct{})
	var output bytes.Buffer
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			output.WriteString(sc.Text() + "\n")
			if strings.Contains(sc.Text(), "listening on") {
				close(listening)
			}
		}
		exited <- cmd.Wait()
	}()
	select {
	case <-listening:
	case err := <-exited:
		t.Fatalf("serve exited before listening: %v\n%s", err, output.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("serve never reported listening")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("serve exited with %v after SIGTERM, want a clean exit\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve still running 5s after one SIGTERM")
	}
}

// TestHelperMain is not a real test: TestServeExitsOnOneSIGTERM runs
// the test binary with BALLAST_TEST_MAIN set to make it act as the
// ballast binary.
func TestHelperMain(t *testing.T) {
	if os.Getenv("BALLAST_TEST_MAIN") != "serve" {
		return
	}
	os.Args = []string{"ballast", "serve", "--addr", "127.0.0.1:0"}
	main() // exits 1 on error
	os.Exit(0)
}
