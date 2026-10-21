package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/heartbeat"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
	"github.com/spf13/cobra"
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
	if got := strings.TrimSpace(out.String()); got != buildVersion() {
		t.Errorf("version output = %q, want %q", got, buildVersion())
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

// TestDefaultRunnerIsRealPipeline checks that when no test stub has
// been installed in the `runner` package var, openApp wires up the
// real pipeline.Pipeline (spec §14 step 11: the placeholder runner is
// gone). Triggering an unconfigured service reaches all the way into
// inventory.GetService and fails with a real "not found" error,
// proving the call went through the real pipeline rather than a
// leftover placeholder.
func TestDefaultRunnerIsRealPipeline(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	t.Setenv("BALLAST_DATA_DIR", data)
	t.Setenv("BALLAST_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if runner != nil {
		t.Fatal("runner package var must be nil by default in this test binary")
	}
	var token string
	withDB(t, data, func(s *store.Store) {
		var err error
		if token, err = scheduler.GenerateTriggerToken(s.DB(), "ghost-service"); err != nil {
			t.Fatalf("GenerateTriggerToken: %v", err)
		}
	})
	_, err := execCLI(t, "trigger", "ghost-service", "--token", token)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("trigger error = %v, want a real inventory \"not found\" error from the real pipeline", err)
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
	cfgDir := filepath.Join(t.TempDir(), "config")
	t.Setenv("BALLAST_CONFIG_DIR", cfgDir)
	t.Setenv("BALLAST_URL", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	// Point the daemon lookup at a port nothing listens on, so commands
	// that prefer a running daemon fall back to running on their own
	// even if the developer has a real daemon on the default port.
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := os.WriteFile(filepath.Join(cfgDir, "ballast.yaml"), []byte("listen: "+addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	// cobra/pflag do not reset a flag-bound package var to its default
	// when a later Execute() call omits that flag (it just keeps
	// whatever a previous test last set it to): reset every command's
	// flag vars before each invocation so tests in this binary cannot
	// leak flags into one another (e.g. one test's --init leaking into
	// the next test's plain `destination add`).
	resetCLIFlags()
	// A test that ran rootCmd.ExecuteContext with a cancelled context
	// must not leak that context into later commands.
	rootCmd.SetContext(context.Background())
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

// TestTriggerCommandInterruptedPreviousRunIsWarningNotFailure mirrors
// the same fix for `ballast backup` (spec §14 step 11 hard rule 6): a
// verified trigger run must not be reported as a failure just because
// an earlier, unrelated run was found interrupted while claiming the
// lock.
func TestTriggerCommandInterruptedPreviousRunIsWarningNotFailure(t *testing.T) {
	r := &spyRunner{}
	data := cliEnv(t, r)
	var token string
	withDB(t, data, func(s *store.Store) {
		var err error
		if token, err = scheduler.GenerateTriggerToken(s.DB(), "immich"); err != nil {
			t.Fatalf("GenerateTriggerToken: %v", err)
		}
		m, err := engine.StartRun(s.DB(), "immich")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := m.Transition(engine.PREPARING, ""); err != nil {
			t.Fatalf("transition: %v", err)
		}
	})

	out, err := execCLI(t, "trigger", "immich", "--token", token)
	if err != nil {
		t.Fatalf("trigger: %v (output %q), want success despite an interrupted earlier run", err, out)
	}
	if !strings.Contains(out, "warning") || !strings.Contains(out, "interrupted") {
		t.Errorf("output = %q, want the interrupted earlier run reported as a warning", out)
	}
	if len(r.ran) != 1 || r.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", r.ran)
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
	started := make(chan struct{})
	data := cliEnv(t, scheduler.RunnerFunc(func(ctx context.Context, _ string) (engine.State, error) {
		close(started)
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
	// Cancel only once the runner is really running, so the cut-short
	// run is what gets reported (a fixed delay raced startup work).
	go func() {
		select {
		case <-started:
			cancel()
		case <-time.After(30 * time.Second):
			cancel()
		}
	}()
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
	a, err := openApp(cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("closing database: %v", err)
		}
	})
	if a.coord.MaxParallel != 3 {
		t.Errorf("MaxParallel = %d, want 3 from ballast.yaml", a.coord.MaxParallel)
	}
	if want := filepath.Join(data, "locks"); a.coord.Locks.(scheduler.FileLocker).Dir != want {
		t.Errorf("lock dir = %v, want %s", a.coord.Locks, want)
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
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, "ballast-api-token"), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMain$")
	cmd.Env = append(os.Environ(),
		"BALLAST_TEST_MAIN=serve",
		"BALLAST_DATA_DIR="+filepath.Join(t.TempDir(), "data"),
		"BALLAST_CONFIG_DIR="+filepath.Join(t.TempDir(), "config"),
		"CREDENTIALS_DIRECTORY="+credDir,
	)
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

// TestNextMinuteBoundary: the scheduler loop's sweeps must land on the
// wall-clock minute regardless of what moment `ballast serve` itself
// started at.
func TestNextMinuteBoundary(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "mid-minute rounds up to the next boundary",
			now:  time.Date(2024, 1, 1, 10, 30, 15, 500_000_000, time.UTC),
			want: time.Date(2024, 1, 1, 10, 31, 0, 0, time.UTC),
		},
		{
			name: "exactly on a boundary still advances to the next one",
			now:  time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC),
			want: time.Date(2024, 1, 1, 10, 31, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextMinuteBoundary(tt.now); !got.Equal(tt.want) {
				t.Errorf("nextMinuteBoundary(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

// TestUntaggedBuildReportsCommit builds the real binary the way a
// plain `go build` does (no -ldflags, no tag) and checks it reports
// the git commit, never "dev" (spec §21.5). Skipped outside a git
// checkout, where the toolchain has no commit to record.
func TestUntaggedBuildReportsCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary; skipped in -short")
	}
	if err := exec.Command("git", "rev-parse", "--verify", "HEAD").Run(); err != nil {
		t.Skip("not a git checkout with a commit")
	}
	bin := filepath.Join(t.TempDir(), "ballast")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("ballast version: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if got == "dev" || got == "" {
		t.Fatalf("untagged build reports %q, want the commit", got)
	}
	if !regexp.MustCompile(`^[0-9a-f]{7,}(-dirty)?$|^v\S+$`).MatchString(got) {
		t.Errorf("version = %q, want a short commit (optionally -dirty) or a tag", got)
	}
}

// TestRunDueQuarantinesReservedServiceName: a leftover service called
// ballast-self (allowed before v0.6) must not stop every other service's
// backups. run-due starts, runs the other service, says plainly what is
// wrong, and does not schedule the settings backup on top of it.
func TestRunDueQuarantinesReservedServiceName(t *testing.T) {
	r := &spyRunner{}
	data := cliEnv(t, r)
	withDB(t, data, func(s *store.Store) {
		if err := inventory.PutDestination(s.DB(), inventory.Destination{Name: "local", Location: "/x", KeepDaily: 7}); err != nil {
			t.Fatal(err)
		}
		if err := inventory.PutService(s.DB(), inventory.Service{Name: "old-name", CatalogEntry: "folder", Destination: "local", Params: map[string]string{"path": "/x"}}); err != nil {
			t.Fatal(err)
		}
		// A v0.5 database can hold this row; it has no other table rows to move.
		if _, err := s.DB().Exec(`DELETE FROM service_targets WHERE service = 'old-name'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec(`UPDATE services SET name = 'ballast-self' WHERE name = 'old-name'`); err != nil {
			t.Fatal(err)
		}
		if err := scheduler.SetSchedule(s.DB(), "immich", "* * * * *"); err != nil {
			t.Fatal(err)
		}
	})
	_, stderr, err := execCLISplit(t, "run-due")
	if err != nil {
		t.Fatalf("run-due = %v, want it to start and run the other services", err)
	}
	if len(r.ran) != 1 || r.ran[0] != "immich" {
		t.Errorf("ran = %v, want [immich]", r.ran)
	}
	if !strings.Contains(stderr, "ballast-self") || !strings.Contains(stderr, "another name") {
		t.Errorf("stderr %q should explain the name conflict", stderr)
	}
	withDB(t, data, func(s *store.Store) {
		jobs, err := scheduler.ListSchedules(s.DB())
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if j.Service == "ballast-self" {
				t.Errorf("the settings backup was scheduled over the leftover service: %+v", j)
			}
		}
	})
}

// TestResultsGoToStdout runs every list/show command with the two
// streams captured separately: results on stdout, no credential notice
// or other "ballast:" chatter mixed into it (spec §21.2).
func TestResultsGoToStdout(t *testing.T) {
	cliEnv(t, &spyRunner{})
	seedTwoServices(t)
	tests := []struct {
		name string
		args []string
		want string // must appear on stdout
	}{
		{"version", []string{"version"}, version},
		{"catalog list", []string{"catalog", "list"}, "immich"},
		{"service list", []string{"service", "list"}, ""},
		{"schedule list", []string{"schedule", "list"}, ""},
		{"destination list", []string{"destination", "list"}, ""},
		{"host list", []string{"host", "list"}, ""},
		{"notify channel list", []string{"notify", "channel", "list"}, ""},
		{"status", []string{"status"}, "immich"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := execCLISplit(t, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v (stderr %q)", tc.args, err, stderr)
			}
			if tc.want != "" && !strings.Contains(stdout, tc.want) {
				t.Errorf("stdout = %q, want it to contain %q (stderr %q)", stdout, tc.want, stderr)
			}
			if strings.Contains(stdout, "ballast: ") {
				t.Errorf("stdout has a notice mixed in: %q", stdout)
			}
		})
	}
}

// TestConfigureStreamsSendsResultsToStdout checks the production
// default: cobra's Print helpers write to stdout once configureStreams
// has run on the root command.
func TestConfigureStreamsSendsResultsToStdout(t *testing.T) {
	c := &cobra.Command{Use: "x"}
	configureStreams(c)
	if c.OutOrStderr() == c.ErrOrStderr() {
		t.Fatal("results and errors share one stream; results must go to stdout")
	}
}

// TestCredentialNotice covers when the "using credential directory"
// notice appears (spec §21.2).
func TestCredentialNotice(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		credDirEnv string
		want       bool
	}{
		{"default dir, plain command", []string{"service", "list"}, "", false},
		{"default dir, version", []string{"version"}, "", false},
		{"verbose", []string{"service", "list", "--verbose"}, "", true},
		{"doctor", []string{"doctor"}, "", true},
		{"non-default dir", []string{"service", "list"}, "other", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cliEnv(t, &spyRunner{})
			if tc.credDirEnv != "" {
				t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
			}
			_, stderr, _ := execCLISplit(t, tc.args...)
			got := strings.Contains(stderr, "using credential director")
			if got != tc.want {
				t.Errorf("notice shown = %v, want %v (stderr %q)", got, tc.want, stderr)
			}
		})
	}
}

// TestNoArgCommandsRejectExtras walks the command tree: every leaf
// command must declare what arguments it takes, so extras are never
// dropped silently (spec §21.2).
func TestNoArgCommandsRejectExtras(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range c.Commands() {
			if len(s.Commands()) == 0 && s.Args == nil && s.Name() != "help" {
				t.Errorf("%q does not declare its arguments (cobra.NoArgs or similar)", s.CommandPath())
			}
			walk(s)
		}
	}
	walk(rootCmd)

	cliEnv(t, &spyRunner{})
	_, _, err := execCLISplit(t, "service", "list", "immich")
	if err == nil {
		t.Fatal("service list immich: want an error")
	}
	if !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("error = %q, want it to say the command takes no arguments", err)
	}
}

// driveLoop runs runScheduleLoop with a ticker the test controls and
// returns the tick channel plus a function that stops the loop and
// waits for it.
func driveLoop(t *testing.T, a *app) (chan time.Time, func()) {
	t.Helper()
	ticks := make(chan time.Time)
	old := scheduleTicker
	scheduleTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	t.Cleanup(func() { scheduleTicker = old })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runScheduleLoop(ctx, a) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop) // runs before the ticker is restored (LIFO)
	return ticks, stop
}

// TestHeartbeatFollowsSchedulerLoop proves pings happen on loop ticks at
// the configured interval and stop once the loop is stopped.
func TestHeartbeatFollowsSchedulerLoop(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	a := sweepApp(t, engine.SUCCESS)
	a.logw = &syncBuf{}
	a.heartbeat = &heartbeat.Pinger{Credential: func() (string, error) { return srv.URL, nil }, Interval: 5 * time.Minute, Meta: a.store, Log: a.logf}

	ticks, stop := driveLoop(t, a)
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ { // minutes 0..6
		ticks <- t0.Add(time.Duration(i) * time.Minute)
		// Wait for each expected ping so the goroutines the loop starts
		// per tick cannot reorder and skew the interval arithmetic.
		switch i {
		case 0:
			waitFor(t, func() bool { return hits.Load() >= 1 })
		case 5:
			waitFor(t, func() bool { return hits.Load() >= 2 })
		}
	}
	stop() // waits for every goroutine the loop started
	if got := hits.Load(); got != 2 {
		t.Fatalf("pings while loop ran = %d, want 2 (minute 0 and minute 5)", got)
	}
	if _, ok, _ := heartbeat.LastOK(a.store.DB()); !ok {
		t.Error("last success not recorded")
	}

	// The loop is stopped: nothing may ping any more.
	before := hits.Load()
	select {
	case ticks <- t0.Add(30 * time.Minute):
		t.Fatal("stopped loop still reads ticks")
	case <-time.After(50 * time.Millisecond):
	}
	if hits.Load() != before {
		t.Error("heartbeat kept pinging after the loop stopped")
	}
}

// TestHeartbeatFailureNeverAffectsBackups proves a dead monitor does not
// change a run's outcome, and that the URL never appears in logs, the
// database or the doctor report (spec rule 2).
func TestHeartbeatFailureNeverAffectsBackups(t *testing.T) {
	const token = "SECRET-HEARTBEAT-TOKEN-do-not-leak-QQ77"
	for name, status := range map[string]int{"monitor down": 0, "monitor errors": 500} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			url := srv.URL + "/ping/" + token + "?k=" + token
			if status == 0 {
				srv.Close()
			} else {
				defer srv.Close()
			}
			a := sweepApp(t, engine.SUCCESS)
			a.logw = &syncBuf{}
			a.heartbeat = &heartbeat.Pinger{Credential: func() (string, error) { return url, nil }, Meta: a.store, Log: a.logf}

			due := time.Date(2026, 3, 5, 3, 0, 20, 0, time.UTC)
			ticks, stop := driveLoop(t, a)
			ticks <- due
			waitFor(t, func() bool {
				runs, _ := engine.RecentRuns(a.store.DB(), "svc", 5)
				return len(runs) == 1 && runs[0].State == engine.SUCCESS && strings.Contains(a.logw.(*syncBuf).String(), "heartbeat:")
			})
			stop()

			runs, err := engine.RecentRuns(a.store.DB(), "svc", 5)
			if err != nil || len(runs) != 1 || runs[0].State != engine.SUCCESS {
				t.Fatalf("runs = %+v, err = %v; want one SUCCESS run", runs, err)
			}
			if !strings.Contains(a.logw.(*syncBuf).String(), "heartbeat:") {
				t.Errorf("failed ping was not logged: %q", a.logw)
			}
			if _, ok, _ := heartbeat.LastOK(a.store.DB()); ok {
				t.Error("failed ping recorded as success")
			}

			surfaces := map[string]string{"log": a.logw.(*syncBuf).String()}
			rows, err := a.store.DB().Query(`SELECT value FROM meta`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var v string
				_ = rows.Scan(&v)
				surfaces["meta"] += v + "\n"
			}
			rows.Close()
			rep, _ := a.ops.Doctor(context.Background())
			b, _ := json.Marshal(rep)
			surfaces["doctor"] = string(b)
			for where, text := range surfaces {
				if strings.Contains(text, token) {
					t.Errorf("heartbeat token leaked into %s: %s", where, text)
				}
			}
		})
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// syncBuf is a log sink safe for the concurrent sweeps a loop starts.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// pingMinutes drives the loop with one tick per minute from 03:00:20 for
// minutes 0..last, waiting for each ping in waitAt, and returns how many
// pings reached the monitor once the loop has been stopped.
func pingMinutes(t *testing.T, a *app, hits *atomic.Int32, last int, waitAt map[int]int32, between func(i int)) int32 {
	t.Helper()
	ticks, stop := driveLoop(t, a)
	t0 := time.Date(2026, 3, 5, 3, 0, 20, 0, time.UTC)
	for i := 0; i <= last; i++ {
		ticks <- t0.Add(time.Duration(i) * time.Minute)
		if between != nil {
			between(i)
		}
		if want, ok := waitAt[i]; ok {
			waitFor(t, func() bool { return hits.Load() >= want })
		}
	}
	stop()
	return hits.Load()
}

func heartbeatServer(t *testing.T, hits *atomic.Int32) *heartbeat.Pinger {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	return &heartbeat.Pinger{Credential: func() (string, error) { return srv.URL, nil }, Interval: 5 * time.Minute}
}

// lastFinished reports when the newest sweep that completed began.
func lastFinished(a *app) time.Time {
	a.sweeps.mu.Lock()
	defer a.sweeps.mu.Unlock()
	return a.sweeps.last
}

// waitSweepFinished waits until the sweep started at tick time at has
// completed, so the next virtual minute sees it, as it would in real
// time.
func waitSweepFinished(t *testing.T, a *app, at time.Time) {
	t.Helper()
	waitFor(t, func() bool { return !lastFinished(a).Before(at) })
}

// TestHeartbeatStopsWhenEverySweepHangs: the loop keeps ticking, but
// every sweep blocks on something they all need (a database lock, a
// deadlock), so none completes and the heartbeat stops once the limit
// has passed (spec §21.7: "a stuck loop").
func TestHeartbeatStopsWhenEverySweepHangs(t *testing.T) {
	var hits atomic.Int32
	a := sweepApp(t, engine.SUCCESS)
	a.logw = &syncBuf{}
	a.heartbeat = heartbeatServer(t, &hits)
	var shared sync.Mutex // what every sweep needs
	shared.Lock()
	old := sweepRun
	sweepRun = func(ctx context.Context, _ *app, _ time.Time) error {
		got := make(chan struct{})
		go func() { shared.Lock(); shared.Unlock(); close(got) }()
		select {
		case <-got:
		case <-ctx.Done(): // shutdown, so the loop can stop
		}
		return nil
	}
	t.Cleanup(func() { sweepRun = old })
	t.Cleanup(shared.Unlock) // frees the helper goroutines

	got := pingMinutes(t, a, &hits, 20, map[int]int32{0: 1, 5: 2, 10: 3}, nil)
	if got != 3 {
		t.Fatalf("pings = %d, want 3 (minutes 0, 5 and 10, then none once no sweep has finished for %s)", got, sweepStuckLimit)
	}
	if !strings.Contains(a.logw.(*syncBuf).String(), "heartbeat is paused") {
		t.Errorf("log = %q, want one line saying the heartbeat is paused", a.logw)
	}
}

// longFirstSweep makes sweepRun block the sweep of the first tick in
// after (simulating long work that follows RunDue, or a long backup)
// while every later sweep runs the real sweepDue.
func longFirstSweep(t *testing.T, after func(ctx context.Context, a *app)) {
	t.Helper()
	old := sweepRun
	var started atomic.Bool
	sweepRun = func(ctx context.Context, a *app, now time.Time) error {
		first := !started.Swap(true)
		err := old(ctx, a, now)
		if first {
			after(ctx, a)
		}
		return err
	}
	t.Cleanup(func() { sweepRun = old })
}

// pingWhileFirstSweepIsLong runs 26 virtual minutes in which the first
// sweep never finishes and every later one does, and returns the pings.
func pingWhileFirstSweepIsLong(t *testing.T, a *app, hits *atomic.Int32) int32 {
	t.Helper()
	t0 := time.Date(2026, 3, 5, 3, 0, 20, 0, time.UTC)
	return pingMinutes(t, a, hits, 25, map[int]int32{0: 1, 5: 2, 10: 3, 15: 4, 20: 5, 25: 6}, func(i int) {
		if i > 0 {
			waitSweepFinished(t, a, t0.Add(time.Duration(i)*time.Minute))
		}
	})
}

// TestHeartbeatContinuesDuringALongHealthCheck: the repository health
// check and the other work sweepDue does after RunDue can take half an
// hour. That is not a stuck daemon, because newer sweeps go on
// finishing.
func TestHeartbeatContinuesDuringALongHealthCheck(t *testing.T) {
	var hits atomic.Int32
	a := sweepApp(t, engine.SUCCESS)
	a.logw = &syncBuf{}
	a.heartbeat = heartbeatServer(t, &hits)
	longFirstSweep(t, func(ctx context.Context, _ *app) { <-ctx.Done() })

	if got := pingWhileFirstSweepIsLong(t, a, &hits); got != 6 {
		t.Fatalf("pings = %d, want 6 (every 5 minutes while the health check runs)", got)
	}
	if strings.Contains(a.logw.(*syncBuf).String(), "heartbeat is paused") {
		t.Errorf("log = %q, the heartbeat must not pause for a long health check", a.logw)
	}
}

// TestHeartbeatContinuesDuringALongPruneAfterSuccess: retention's
// forget --prune runs after the run is marked SUCCESS, still holding
// the run lock, and can take tens of minutes against a remote
// repository.
func TestHeartbeatContinuesDuringALongPruneAfterSuccess(t *testing.T) {
	var hits atomic.Int32
	a := sweepApp(t, engine.SUCCESS)
	a.logw = &syncBuf{}
	a.ops.Coord = a.coord
	a.heartbeat = heartbeatServer(t, &hits)
	inner := a.coord.Runner
	a.coord.Runner = scheduler.RunnerFunc(func(ctx context.Context, service string) (engine.State, error) {
		state, err := inner.Run(ctx, service) // marked SUCCESS
		<-ctx.Done()                          // ...then pruning, lock still held
		return state, err
	})

	if got := pingWhileFirstSweepIsLong(t, a, &hits); got != 6 {
		t.Fatalf("pings = %d, want 6 (every 5 minutes while the prune runs)", got)
	}
}

// TestHeartbeatContinuesDuringALongBackup: a sweep waiting on a backup
// for hours does not stop the heartbeat, because the next minute's
// sweep skips the busy service and finishes.
func TestHeartbeatContinuesDuringALongBackup(t *testing.T) {
	var hits atomic.Int32
	a := sweepApp(t, engine.SUCCESS)
	a.logw = &syncBuf{}
	a.ops.Coord = a.coord
	a.heartbeat = heartbeatServer(t, &hits)
	a.coord.Runner = scheduler.RunnerFunc(func(ctx context.Context, service string) (engine.State, error) {
		m, err := engine.StartRun(a.store.DB(), service)
		if err != nil {
			return engine.FAILED, err
		}
		<-ctx.Done() // a backup that outlasts the whole test
		_ = m.Transition(engine.FAILED, "test over")
		return engine.FAILED, ctx.Err()
	})

	if got := pingWhileFirstSweepIsLong(t, a, &hits); got != 6 {
		t.Fatalf("pings = %d, want 6 (every 5 minutes while the backup runs)", got)
	}
}
