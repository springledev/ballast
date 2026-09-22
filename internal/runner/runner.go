// Package runner executes external commands — restic, rclone, docker,
// pg_dump — with the subprocess semantics spec §8 makes non-negotiable:
//
//   - Pipefail: a pipeline fails if ANY stage fails. The leftmost
//     failure is reported as the root cause (downstream failures are
//     usually its cascade), and no failure is ever masked by a later
//     stage's success. No shell is ever invoked — arguments go to
//     execve verbatim — so pipefail is implemented by the runner
//     itself and there is no shell-injection surface.
//   - SIGPIPE/EPIPE: a command whose reader closed the pipe early is
//     reported distinctly (ErrBrokenPipe), never conflated with a
//     command malfunction; and a real command failure is never masked
//     by the SIGPIPE cascade it caused upstream.
//   - Both exit status and stderr are captured: a non-zero exit is
//     never silently swallowed, and a redacted stderr tail rides along
//     in every error.
//   - Context cancellation kills the whole pipeline and is reported as
//     cancellation, not as a spurious "signal: killed".
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// defaultStderrCap is the default number of stderr bytes retained per
// command. The tail (most recent bytes) is kept: that is where error
// messages live.
const defaultStderrCap = 64 << 10 // 64 KiB

// Runner executes external commands with Ballast's spec §8 semantics.
// The zero value is ready to use.
type Runner struct {
	// BaseEnv is the base environment for every command; a command's
	// own Env entries override duplicate keys. Nil uses os.Environ().
	BaseEnv []string

	// Redact lists secret strings (passwords, keys) that must never
	// reach logs or error messages: every occurrence in captured
	// stderr is replaced with "[redacted]" before it can leak.
	Redact []string

	// StderrCap bounds how many bytes of stderr are retained per
	// command (the tail is kept). Zero uses a 64 KiB default.
	StderrCap int
}

// Command is one external program invocation. Name is resolved against
// $PATH when it contains no path separator. Arguments are passed to
// execve verbatim — no shell is involved.
type Command struct {
	Name string   // program name or absolute path
	Args []string // arguments, excluding argv[0]
	Env  []string // extra KEY=VALUE entries; override BaseEnv duplicates
	Dir  string   // working directory; empty means current directory
}

// Result describes how one command finished.
type Result struct {
	// Stdout is the full stdout, captured when no stdout writer was
	// given. It is raw payload data and is never redacted: do not log it.
	Stdout   []byte
	Stderr   []byte         // stderr tail (at most StderrCap bytes), redacted
	ExitCode int            // exit status; -1 when killed by a signal
	Signaled bool           // true when the command was killed by a signal
	Signal   syscall.Signal // the killing signal, when Signaled
	Duration time.Duration  // wall time from start to reap
}

// Run executes a single command to completion. stdin may be nil (the
// command reads /dev/null); stdout may be nil (stdout is captured into
// Result.Stdout). The Result is returned even on error so callers can
// inspect partial output.
func (r *Runner) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, c Command) (Result, error) {
	results, err := r.Pipeline(ctx, stdin, stdout, c)
	if len(results) == 0 {
		return Result{}, err
	}
	return results[0], err
}

// Pipeline executes stages connected stdout→stdin with real OS pipes,
// exactly like a shell pipeline with `set -o pipefail`: backpressure
// and SIGPIPE behave as the kernel dictates, every stage is waited
// for, and the pipeline fails if ANY stage fails.
//
// stdin feeds the first stage (nil = /dev/null); stdout drains the
// last stage (nil = captured into the last Result's Stdout). Every
// stage's stderr is tail-captured into its Result. The per-stage
// Results are returned even on error.
//
// Error classification, in priority order:
//
//  1. ExitError — the leftmost stage that exited non-zero (root cause).
//  2. context error — cancellation; any "signal: killed" outcome is
//     the runner's own kill.
//  3. unexpected wait errors that are not exit-status outcomes.
//  4. SignalError — a stage killed by a signal other than SIGPIPE.
//  5. output writer failure — the caller's stdout writer failed
//     (e.g. disk full); any SIGPIPE death above is its consequence.
//  6. stdin feed failure — the caller's stdin reader failed; if the
//     first stage merely closed stdin early yet exited 0 (truncated
//     input), that is a BrokenPipeError instead.
//  7. BrokenPipeError — a stage died of SIGPIPE while every downstream
//     reader exited 0 (reader closed early on purpose).
func (r *Runner) Pipeline(ctx context.Context, stdin io.Reader, stdout io.Writer, stages ...Command) ([]Result, error) {
	if len(stages) == 0 {
		return nil, fmt.Errorf("pipeline: at least one command is required")
	}
	stderrCap := r.StderrCap
	if stderrCap <= 0 {
		stderrCap = defaultStderrCap
	}

	cmds := make([]*exec.Cmd, len(stages))
	tails := make([]*tailBuffer, len(stages))
	for i, c := range stages {
		if c.Name == "" {
			return nil, fmt.Errorf("pipeline stage %d: program name must not be empty", i)
		}
		for _, e := range c.Env {
			if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
				return nil, fmt.Errorf("pipeline stage %d (%s): environment entry %q must be in KEY=VALUE form", i, c.Name, r.redact(e))
			}
		}
		cmd := exec.Command(c.Name, c.Args...)
		cmd.Dir = c.Dir
		cmd.Env = mergeEnv(r.baseEnv(), c.Env)
		tails[i] = &tailBuffer{cap: stderrCap}
		cmd.Stderr = tails[i]
		cmds[i] = cmd
	}

	// parentEnds are the parent's copies of pipe ends handed to
	// children. Once every child has started, the parent must close
	// them (each child holds its own duplicate): that is what lets EOF
	// and SIGPIPE propagate exactly like in a shell pipeline.
	var parentEnds []*os.File
	closeParentEnds := func() {
		for _, f := range parentEnds {
			f.Close()
		}
		parentEnds = nil
	}

	// Wire stage i's stdout to stage i+1's stdin with a real OS pipe.
	for i := 0; i+1 < len(stages); i++ {
		pr, pw, err := os.Pipe()
		if err != nil {
			closeParentEnds()
			return nil, fmt.Errorf("creating pipe between %s and %s: %w", stages[i].Name, stages[i+1].Name, err)
		}
		cmds[i].Stdout = pw
		cmds[i+1].Stdin = pr
		parentEnds = append(parentEnds, pr, pw)
	}

	// Pipe the caller's stdin into the first stage, when given.
	var stdinW *os.File
	if stdin != nil {
		pr, pw, err := os.Pipe()
		if err != nil {
			closeParentEnds()
			return nil, fmt.Errorf("creating stdin pipe for %s: %w", stages[0].Name, err)
		}
		cmds[0].Stdin = pr
		stdinW = pw
		parentEnds = append(parentEnds, pr) // pw is closed by the copy goroutine
	}

	// Pipe the last stage's stdout to the caller's writer (or capture).
	last := len(cmds) - 1
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeParentEnds()
		if stdinW != nil {
			stdinW.Close()
		}
		return nil, fmt.Errorf("creating stdout pipe for %s: %w", stages[last].Name, err)
	}
	cmds[last].Stdout = stdoutW
	parentEnds = append(parentEnds, stdoutW) // stdoutR is closed by the copy goroutine

	// Start every stage. On failure, kill and reap what started and
	// close every pipe end the parent still holds.
	startedAt := make([]time.Time, len(cmds))
	started := 0
	for i, cmd := range cmds {
		startedAt[i] = time.Now()
		if err := cmd.Start(); err != nil {
			for j := 0; j < started; j++ {
				cmds[j].Process.Kill()
			}
			for j := 0; j < started; j++ {
				cmds[j].Wait()
			}
			closeParentEnds()
			if stdinW != nil {
				stdinW.Close()
			}
			stdoutR.Close()
			return nil, fmt.Errorf("starting %s: %w", stages[i].Name, err)
		}
		started++
	}
	closeParentEnds()

	// Feed the first stage's stdin. A write failing with EPIPE means
	// the child closed its stdin early — recorded, not fatal here.
	stdinErrCh := make(chan error, 1)
	if stdinW != nil {
		go func() {
			_, err := io.Copy(stdinW, stdin)
			if cerr := stdinW.Close(); err == nil {
				err = cerr
			}
			stdinErrCh <- err
		}()
	} else {
		stdinErrCh <- nil
	}

	// Drain the last stage's stdout.
	var outBuf *bytes.Buffer
	outW := stdout
	if outW == nil {
		outBuf = &bytes.Buffer{}
		outW = outBuf
	}
	stdoutErrCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(outW, stdoutR)
		if cerr := stdoutR.Close(); err == nil {
			err = cerr
		}
		stdoutErrCh <- err
	}()

	// Cancellation kills the whole pipeline.
	stop := context.AfterFunc(ctx, func() {
		for _, cmd := range cmds {
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	})
	defer stop()

	// Wait for every stage (reaping is order-independent: the children
	// run concurrently and unblock each other through the pipes).
	waitErrs := make([]error, len(cmds))
	waitDur := make([]time.Duration, len(cmds))
	for i, cmd := range cmds {
		waitErrs[i] = cmd.Wait()
		waitDur[i] = time.Since(startedAt[i])
	}
	stdinErr := <-stdinErrCh
	outErr := <-stdoutErrCh

	results := make([]Result, len(cmds))
	for i := range cmds {
		results[i] = Result{
			Stderr:   []byte(r.redact(tails[i].String())),
			Duration: waitDur[i],
		}
		var ee *exec.ExitError
		switch {
		case waitErrs[i] == nil:
			// ExitCode 0, the zero value.
		case errors.As(waitErrs[i], &ee):
			results[i].ExitCode = ee.ExitCode()
			if sig, ok := exitSignal(ee); ok {
				results[i].Signaled = true
				results[i].Signal = sig
			}
		default:
			results[i].ExitCode = -1
		}
	}
	if outBuf != nil {
		results[last].Stdout = outBuf.Bytes()
	}

	return results, classify(stages, waitErrs, results, stdinErr, outErr, ctx.Err())
}

// classify applies the pipefail/SIGPIPE priority order documented on
// Pipeline and returns the pipeline's error (nil on success).
func classify(stages []Command, waitErrs []error, results []Result, stdinErr, outErr, ctxErr error) error {
	// 1. A real exit-status failure is the root cause; report the
	//    leftmost one (later failures are usually its cascade).
	for i := range stages {
		var ee *exec.ExitError
		if errors.As(waitErrs[i], &ee) && ee.ExitCode() > 0 {
			return &ExitError{Cmd: stages[i].Name, Code: ee.ExitCode(), Stderr: stderrLine(results[i].Stderr)}
		}
	}
	// 2. Cancellation: any "signal: killed" outcome is our own kill.
	if ctxErr != nil {
		return fmt.Errorf("pipeline cancelled: %w", ctxErr)
	}
	// 3. Unexpected wait errors that are not exit-status outcomes.
	for i := range stages {
		var ee *exec.ExitError
		if waitErrs[i] != nil && !errors.As(waitErrs[i], &ee) {
			return fmt.Errorf("waiting for %s: %w", stages[i].Name, waitErrs[i])
		}
	}
	// 4. Killed by a signal other than SIGPIPE.
	for i := range stages {
		if results[i].Signaled && results[i].Signal != syscall.SIGPIPE {
			return &SignalError{Cmd: stages[i].Name, Signal: results[i].Signal, Stderr: stderrLine(results[i].Stderr)}
		}
	}
	// 5. Writing the last stage's output failed (e.g. disk full). Any
	//    SIGPIPE death above is a consequence of this, not a cause.
	if outErr != nil {
		return fmt.Errorf("could not write the output of %s: %w", stages[len(stages)-1].Name, outErr)
	}
	// 6. Feeding the first stage's stdin failed.
	if stdinErr != nil {
		if isPipeClose(stdinErr) {
			// The first stage closed its stdin without reading it
			// all, yet exited successfully: the input was truncated.
			return &BrokenPipeError{Cmd: stages[0].Name}
		}
		return fmt.Errorf("could not feed standard input to %s: %w", stages[0].Name, stdinErr)
	}
	// 7. A stage died of SIGPIPE while every downstream reader exited
	//    successfully: the reader closed the pipe early on purpose.
	for i := range stages {
		if results[i].Signaled && results[i].Signal == syscall.SIGPIPE {
			return &BrokenPipeError{Cmd: stages[i].Name}
		}
	}
	return nil
}

// baseEnv returns the configured base environment, or the process's.
func (r *Runner) baseEnv() []string {
	if r.BaseEnv != nil {
		return r.BaseEnv
	}
	return os.Environ()
}

// redact scrubs every configured secret from s.
func (r *Runner) redact(s string) string {
	for _, secret := range r.Redact {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// mergeEnv combines base and extra environment entries with
// extra-wins semantics for duplicate keys.
func mergeEnv(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	overridden := make(map[string]bool, len(extra))
	for _, e := range extra {
		k, _, _ := strings.Cut(e, "=")
		overridden[k] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if !overridden[k] {
			out = append(out, e)
		}
	}
	return append(out, extra...)
}

// exitSignal extracts the killing signal from an ExitError, when the
// process was in fact killed by a signal.
func exitSignal(ee *exec.ExitError) (syscall.Signal, bool) {
	if ee.ProcessState == nil {
		return 0, false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return ws.Signal(), true
}

// isPipeClose reports whether err is one of the errors a writer sees
// when the reader has closed its end of a pipe (EPIPE), or the pipe
// was closed out from under the writer locally.
func isPipeClose(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}

// stderrLine trims a captured stderr tail for inclusion in an error.
func stderrLine(stderr []byte) string {
	return strings.TrimSpace(string(stderr))
}
