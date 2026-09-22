package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is not a test: it is the fake external command the
// runner tests execute (the test binary re-invoking itself), so tests
// are hermetic and need neither restic nor docker nor coreutils. The
// first argument after "--" selects the behavior.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	behavior := args[0]
	args = args[1:]
	switch behavior {
	case "out": // out <text>...: print to stdout, exit 0
		fmt.Fprint(os.Stdout, strings.Join(args, " "))
		os.Exit(0)
	case "err": // err <text>...: print to stderr, exit 0
		fmt.Fprint(os.Stderr, strings.Join(args, " "))
		os.Exit(0)
	case "fail": // fail <code> [msg...]: print msg to stderr, exit code
		code := 1
		fmt.Sscan(args[0], &code)
		if len(args) > 1 {
			fmt.Fprintln(os.Stderr, strings.Join(args[1:], " "))
		}
		os.Exit(code)
	case "cat": // copy stdin to stdout, exit 0
		io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	case "noread": // noread [code]: exit without reading stdin
		code := 0
		if len(args) > 0 {
			fmt.Sscan(args[0], &code)
		}
		os.Exit(code)
	case "head": // head <n>: read n bytes of stdin, then exit 0
		n := 0
		fmt.Sscan(args[0], &n)
		io.CopyN(io.Discard, os.Stdin, int64(n))
		os.Exit(0)
	case "producer": // producer <n>: write n bytes to stdout, exit 0
		n := 0
		fmt.Sscan(args[0], &n)
		chunk := bytes.Repeat([]byte("x"), 4096)
		for n > 0 {
			m := min(n, len(chunk))
			// A write to fd 1 that fails with EPIPE makes the Go
			// runtime raise SIGPIPE: the process dies by signal,
			// exactly like a real command whose reader went away.
			if _, err := os.Stdout.Write(chunk[:m]); err != nil {
				fmt.Fprintln(os.Stderr, "producer write error:", err)
				os.Exit(1)
			}
			n -= m
		}
		os.Exit(0)
	case "sleeper": // sleeper <seconds>: sleep, then exit 0
		sec := 0
		fmt.Sscan(args[0], &sec)
		time.Sleep(time.Duration(sec) * time.Second)
		os.Exit(0)
	case "suicide": // suicide <sigterm|sigkill>: die by signal
		sig := syscall.SIGTERM
		if len(args) > 0 && args[0] == "sigkill" {
			sig = syscall.SIGKILL
		}
		syscall.Kill(syscall.Getpid(), sig)
		os.Exit(1)
	case "env": // env <key>...: print each value on its own line
		for _, k := range args {
			fmt.Fprintln(os.Stdout, os.Getenv(k))
		}
		os.Exit(0)
	case "bigerr": // bigerr <n>: write numbered lines to stderr, exit 1
		n := 0
		fmt.Sscan(args[0], &n)
		for i := 0; i < n; i++ {
			fmt.Fprintf(os.Stderr, "line-%06d\n", i)
		}
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper behavior:", behavior)
		os.Exit(2)
	}
}

// helperCommand builds a Command that re-invokes the test binary as a
// fake external program with the given behavior.
func helperCommand(behavior string, args ...string) Command {
	return Command{
		Name: os.Args[0],
		Args: append([]string{"-test.run=TestHelperProcess", "--", behavior}, args...),
		Env:  []string{"GO_WANT_HELPER_PROCESS=1"},
	}
}

// bigInput returns more data than a pipe buffer holds (64 KiB), so a
// reader that does not read forces an EPIPE on the writer.
func bigInput() io.Reader {
	return strings.NewReader(strings.Repeat("z", 1<<20))
}

func TestRunCapturesOutput(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), nil, nil, helperCommand("out", "hello", "world"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Stdout) != "hello world" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hello world")
	}
	if res.ExitCode != 0 || res.Signaled {
		t.Errorf("ExitCode = %d, Signaled = %v; want clean exit 0", res.ExitCode, res.Signaled)
	}
	if res.Duration <= 0 {
		t.Error("Duration must be positive")
	}
}

func TestRunCapturesStderrOnSuccess(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), nil, nil, helperCommand("err", "some warning"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Spec §8: stderr is checked even when the exit status is 0 — the
	// runner's job is to capture it so callers can check.
	if string(res.Stderr) != "some warning" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "some warning")
	}
}

func TestRunExitError(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), nil, nil, helperCommand("fail", "42", "boom"))
	if err == nil {
		t.Fatal("Run must fail on non-zero exit")
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error type = %T, want *ExitError", err)
	}
	if ee.Code != 42 {
		t.Errorf("ExitError.Code = %d, want 42", ee.Code)
	}
	if !strings.Contains(ee.Stderr, "boom") {
		t.Errorf("ExitError.Stderr = %q, want it to contain %q", ee.Stderr, "boom")
	}
	if !strings.Contains(err.Error(), "42") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error text = %q, want exit code and stderr tail", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("Result.ExitCode = %d, want 42", res.ExitCode)
	}
}

func TestRunSignaled(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), nil, nil, helperCommand("suicide", "sigterm"))
	if err == nil {
		t.Fatal("Run must fail when the command is killed by a signal")
	}
	var se *SignalError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *SignalError", err)
	}
	if se.Signal != syscall.SIGTERM {
		t.Errorf("SignalError.Signal = %v, want SIGTERM", se.Signal)
	}
	if !strings.Contains(err.Error(), "killed by signal") {
		t.Errorf("error text = %q, want a plain-English signal description", err)
	}
	if !res.Signaled || res.ExitCode != -1 {
		t.Errorf("Result = Signaled %v ExitCode %d, want Signaled true ExitCode -1", res.Signaled, res.ExitCode)
	}
}

// TestRunExitErrorNoStderr: with an empty stderr the error text still
// names the exit status, without a dangling colon.
func TestRunExitErrorNoStderr(t *testing.T) {
	r := &Runner{}
	_, err := r.Run(context.Background(), nil, nil, helperCommand("fail", "3"))
	if err == nil {
		t.Fatal("Run must fail")
	}
	if !strings.Contains(err.Error(), "exited with status 3") {
		t.Errorf("error text = %q, want the exit status", err)
	}
	if strings.HasSuffix(strings.TrimSpace(err.Error()), ":") {
		t.Errorf("error text = %q must not dangle a colon without stderr", err)
	}
}

func TestRunStreamsStdin(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), strings.NewReader("piped data"), nil, helperCommand("cat"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Stdout) != "piped data" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "piped data")
	}
}

// TestRunStdinClosedEarly pins the dangerous case for backups: the
// command exited 0 WITHOUT consuming all of its input (truncated
// input). That must be reported — distinctly, as ErrBrokenPipe, not as
// success and not as a command failure.
func TestRunStdinClosedEarly(t *testing.T) {
	r := &Runner{}
	_, err := r.Run(context.Background(), bigInput(), nil, helperCommand("noread", "0"))
	if !errors.Is(err, ErrBrokenPipe) {
		t.Fatalf("error = %v, want ErrBrokenPipe", err)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		t.Error("broken pipe must not be reported as a command exit failure")
	}
}

// TestRunStdinEpipeMaskedByChildFailure: when the child fails, the
// EPIPE on our stdin write is a consequence, not the cause — the
// child's exit failure must be reported.
func TestRunStdinEpipeMaskedByChildFailure(t *testing.T) {
	r := &Runner{}
	_, err := r.Run(context.Background(), bigInput(), nil, helperCommand("noread", "5"))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 5 {
		t.Fatalf("error = %v, want *ExitError with code 5", err)
	}
	if errors.Is(err, ErrBrokenPipe) {
		t.Error("child failure must not be masked as a broken pipe")
	}
}

// flakyWriter accepts the first n bytes, then fails with err.
type flakyWriter struct {
	n   int
	err error
}

func (w *flakyWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, w.err
	}
	if len(p) > w.n {
		n := w.n
		w.n = 0
		return n, w.err
	}
	w.n -= len(p)
	return len(p), nil
}

// TestRunStdoutWriterFailure: when the consumer of the command's
// output fails (e.g. disk full writing the dump file), the command
// dies of SIGPIPE — but the reported cause must be the writer failure,
// never "signal: broken pipe".
func TestRunStdoutWriterFailure(t *testing.T) {
	r := &Runner{}
	diskFull := errors.New("no space left on device")
	w := &flakyWriter{n: 512, err: diskFull}
	_, err := r.Run(context.Background(), nil, w, helperCommand("producer", "1000000"))
	if !errors.Is(err, diskFull) {
		t.Fatalf("error = %v, want it to wrap the writer failure", err)
	}
	if errors.Is(err, ErrBrokenPipe) {
		t.Error("writer failure must not be reported as a broken pipe")
	}
	var se *SignalError
	if errors.As(err, &se) {
		t.Error("writer failure must not be reported as a signal death")
	}
}

func TestRunFullStdoutCapture(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), nil, nil, helperCommand("producer", "100000"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Stdout) != 100000 {
		t.Errorf("captured stdout = %d bytes, want 100000", len(res.Stdout))
	}
}

func TestRunCancellation(t *testing.T) {
	r := &Runner{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, nil, nil, helperCommand("sleeper", "30"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %v; the kill must be prompt", elapsed)
	}
}

func TestPipelineCancellation(t *testing.T) {
	r := &Runner{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := r.Pipeline(ctx, nil, nil, helperCommand("sleeper", "30"), helperCommand("sleeper", "30"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestRunStartFailure(t *testing.T) {
	r := &Runner{}
	_, err := r.Run(context.Background(), nil, nil, Command{Name: "ballast-no-such-binary-xyz"})
	if err == nil {
		t.Fatal("Run of a missing binary must fail")
	}
	if !strings.Contains(err.Error(), "ballast-no-such-binary-xyz") {
		t.Errorf("error = %q, want it to name the missing program", err)
	}
}

// TestPipelineStartFailureKillsStartedStages: when a later stage fails
// to start, every already-started stage must be killed and reaped —
// promptly, not after the started stage finishes on its own.
func TestPipelineStartFailureKillsStartedStages(t *testing.T) {
	r := &Runner{}
	start := time.Now()
	_, err := r.Pipeline(context.Background(), nil, nil,
		helperCommand("sleeper", "30"),
		Command{Name: "ballast-no-such-binary-xyz"})
	if err == nil {
		t.Fatal("Pipeline must fail when a stage cannot start")
	}
	if !strings.Contains(err.Error(), "ballast-no-such-binary-xyz") {
		t.Errorf("error = %q, want it to name the stage that failed to start", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("start failure took %v; the already-started stage must be killed and reaped promptly", elapsed)
	}
}

// errReader yields n bytes, then fails with a non-EPIPE I/O error.
type errReader struct {
	n   int
	err error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, r.err
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	r.n -= len(p)
	return len(p), nil
}

// TestRunStdinReaderFailure: a genuine I/O error from the caller's
// stdin reader (e.g. the dump file became unreadable mid-stream) must
// be reported as a feed failure — distinctly from EPIPE.
func TestRunStdinReaderFailure(t *testing.T) {
	r := &Runner{}
	diskErr := errors.New("reading dump file: input/output error")
	_, err := r.Run(context.Background(), &errReader{n: 100, err: diskErr}, nil, helperCommand("cat"))
	if !errors.Is(err, diskErr) {
		t.Fatalf("error = %v, want it to wrap the stdin reader failure", err)
	}
	if !strings.Contains(err.Error(), "could not feed standard input") {
		t.Errorf("error = %q, want a plain-English feed-failure description", err)
	}
	if errors.Is(err, ErrBrokenPipe) {
		t.Error("a genuine reader I/O error must not be reported as a broken pipe")
	}
}

// TestPipelineRejectsMalformedEnv: an environment entry that is not in
// KEY=VALUE form is a caller bug and must fail loudly before anything
// starts, not leak silently into the child.
func TestPipelineRejectsMalformedEnv(t *testing.T) {
	r := &Runner{}
	c := helperCommand("out", "unreachable")
	c.Env = append(c.Env, "NO-EQUALS-SIGN")
	if _, err := r.Pipeline(context.Background(), nil, nil, c); err == nil {
		t.Fatal("malformed environment entry must be rejected")
	} else if !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Errorf("error = %q, want a plain-English KEY=VALUE hint", err)
	}
}

func TestRunEnvPassing(t *testing.T) {
	r := &Runner{}
	c := helperCommand("env", "BALLAST_TEST_VAR")
	c.Env = append(c.Env, "BALLAST_TEST_VAR=hello-env")
	res, err := r.Run(context.Background(), nil, nil, c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "hello-env" {
		t.Errorf("child saw BALLAST_TEST_VAR=%q, want %q", res.Stdout, "hello-env")
	}
}

// TestBaseEnvOverride: a command's Env entries override duplicate keys
// in the runner's BaseEnv; non-duplicate keys pass through.
func TestBaseEnvOverride(t *testing.T) {
	r := &Runner{BaseEnv: []string{"BASE_A=1", "BASE_B=2"}}
	c := helperCommand("env", "BASE_A", "BASE_B")
	c.Env = append(c.Env, "BASE_B=3")
	res, err := r.Run(context.Background(), nil, nil, c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if len(got) != 2 || got[0] != "1" || got[1] != "3" {
		t.Errorf("env = %v, want [1 3] (base kept, override won)", got)
	}
}

// TestRedaction pins spec §"Non-negotiable rules" #2: secrets never
// reach logs or error messages. A secret printed to stderr must be
// scrubbed from both the Result and the error text.
func TestRedaction(t *testing.T) {
	r := &Runner{Redact: []string{"s3cr3t-token"}}
	res, err := r.Run(context.Background(), nil, nil, helperCommand("fail", "1", "password is s3cr3t-token ok"))
	if err == nil {
		t.Fatal("Run must fail")
	}
	if strings.Contains(err.Error(), "s3cr3t-token") {
		t.Errorf("error text leaks the secret: %q", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("error text = %q, want [redacted] marker", err)
	}
	if strings.Contains(string(res.Stderr), "s3cr3t-token") {
		t.Errorf("Result.Stderr leaks the secret: %q", res.Stderr)
	}
}

// TestStderrTailCap: stderr is bounded, and the TAIL (where the error
// messages are) is what survives.
func TestStderrTailCap(t *testing.T) {
	r := &Runner{StderrCap: 1024}
	res, err := r.Run(context.Background(), nil, nil, helperCommand("bigerr", "10000"))
	if err == nil {
		t.Fatal("Run must fail")
	}
	if len(res.Stderr) > 1024 {
		t.Errorf("captured stderr = %d bytes, want <= 1024", len(res.Stderr))
	}
	if !strings.Contains(string(res.Stderr), "line-009999") {
		t.Errorf("captured stderr must keep the tail (last line); got %d bytes ending %q",
			len(res.Stderr), res.Stderr[max(0, len(res.Stderr)-40):])
	}
	if strings.Contains(string(res.Stderr), "line-000000") {
		t.Error("captured stderr must drop the head once over capacity")
	}
}

func TestPipelineHappyPath(t *testing.T) {
	r := &Runner{}
	results, err := r.Pipeline(context.Background(), strings.NewReader("through the pipe"), nil,
		helperCommand("cat"), helperCommand("cat"), helperCommand("cat"))
	if err != nil {
		t.Fatalf("Pipeline: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	for i, res := range results {
		if res.ExitCode != 0 || res.Signaled {
			t.Errorf("stage %d: ExitCode = %d, Signaled = %v; want clean exit 0", i, res.ExitCode, res.Signaled)
		}
	}
	if string(results[2].Stdout) != "through the pipe" {
		t.Errorf("final stdout = %q, want %q", results[2].Stdout, "through the pipe")
	}
}

// TestPipelineUpstreamFailureIsNotMasked is the core pipefail
// guarantee: the last stage exits 0, yet the pipeline must fail
// because an upstream stage failed.
func TestPipelineUpstreamFailureIsNotMasked(t *testing.T) {
	r := &Runner{}
	_, err := r.Pipeline(context.Background(), nil, nil,
		helperCommand("fail", "2", "dump went wrong"), helperCommand("cat"))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("error = %v, want *ExitError with code 2", err)
	}
	if !strings.Contains(err.Error(), "dump went wrong") {
		t.Errorf("error = %q, want the failing stage's stderr", err)
	}
}

// TestPipelineMiddleFailure: a failing middle stage must be reported
// even though both its neighbors exit 0.
func TestPipelineMiddleFailure(t *testing.T) {
	r := &Runner{}
	_, err := r.Pipeline(context.Background(), nil, nil,
		helperCommand("cat"), helperCommand("fail", "7"), helperCommand("cat"))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("error = %v, want *ExitError with code 7", err)
	}
}

// TestPipelineLeftmostFailureIsRootCause: when several stages fail,
// the leftmost is reported — downstream failures are its cascade
// (pg_dump dying makes restic fail too; pg_dump is the story).
func TestPipelineLeftmostFailureIsRootCause(t *testing.T) {
	r := &Runner{}
	_, err := r.Pipeline(context.Background(), nil, nil,
		helperCommand("fail", "2"), helperCommand("fail", "3"))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("error = %v, want *ExitError with code 2 (leftmost)", err)
	}
}

// TestPipelineSigpipeAttributedToDownstreamFailure: the producer dies
// of SIGPIPE because the consumer failed without reading. The reported
// cause must be the consumer's exit failure — the SIGPIPE is a
// symptom, and must not mask it.
func TestPipelineSigpipeAttributedToDownstreamFailure(t *testing.T) {
	r := &Runner{}
	results, err := r.Pipeline(context.Background(), nil, nil,
		helperCommand("producer", "10000000"), helperCommand("noread", "3"))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("error = %v, want *ExitError with code 3", err)
	}
	if errors.Is(err, ErrBrokenPipe) {
		t.Error("the consumer's failure must not be masked as a broken pipe")
	}
	// The producer's SIGPIPE death is still recorded per stage.
	if !results[0].Signaled || results[0].Signal != syscall.SIGPIPE {
		t.Errorf("producer result = Signaled %v Signal %v, want SIGPIPE death recorded",
			results[0].Signaled, results[0].Signal)
	}
}

// TestPipelineReaderClosedEarly is the spec §8 "EPIPE-closed reader"
// case: the consumer deliberately stopped reading and exited 0, the
// producer died of SIGPIPE. This is loud (data was truncated) but
// distinct from a command malfunction.
func TestPipelineReaderClosedEarly(t *testing.T) {
	r := &Runner{}
	_, err := r.Pipeline(context.Background(), nil, nil,
		helperCommand("producer", "10000000"), helperCommand("head", "100"))
	if !errors.Is(err, ErrBrokenPipe) {
		t.Fatalf("error = %v, want ErrBrokenPipe", err)
	}
	if !strings.Contains(err.Error(), "closed the pipe early") {
		t.Errorf("error text = %q, want a plain-English broken-pipe description", err)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		t.Error("reader-closed-early must not be reported as a command exit failure")
	}
}

func TestPipelineRejectsNoStages(t *testing.T) {
	r := &Runner{}
	if _, err := r.Pipeline(context.Background(), nil, nil); err == nil {
		t.Error("empty pipeline must be rejected")
	}
}

func TestPipelineRejectsEmptyName(t *testing.T) {
	r := &Runner{}
	if _, err := r.Pipeline(context.Background(), nil, nil, Command{Name: ""}); err == nil {
		t.Error("empty program name must be rejected")
	}
}
