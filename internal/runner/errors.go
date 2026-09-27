package runner

import (
	"errors"
	"fmt"
	"syscall"
)

// ErrBrokenPipe marks the distinct spec §8 condition of a reader
// closing a pipe early: the writer was cut off, which is not a command
// malfunction. Match it with errors.Is(err, ErrBrokenPipe).
var ErrBrokenPipe = errors.New("downstream reader closed the pipe early")

// ExitError reports a command that exited with a non-zero status.
type ExitError struct {
	Cmd    string // program name
	Code   int    // exit status
	Stderr string // redacted stderr tail
}

// Error describes the failure in plain English.
func (e *ExitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s exited with status %d", e.Cmd, e.Code)
	}
	return fmt.Sprintf("%s exited with status %d: %s", e.Cmd, e.Code, e.Stderr)
}

// AbortError reports a command stopped early because its own stderr
// showed a failure that retrying cannot fix (Command.AbortOn).
type AbortError struct {
	Cmd    string // program name
	Stderr string // redacted stderr tail
}

// Error describes the failure in plain English.
func (e *AbortError) Error() string {
	return fmt.Sprintf("%s was stopped early because it reported a problem that retrying will not fix: %s", e.Cmd, e.Stderr)
}

// SignalError reports a command killed by a signal other than SIGPIPE
// (a SIGPIPE death is the BrokenPipeError case instead).
type SignalError struct {
	Cmd    string         // program name
	Signal syscall.Signal // the killing signal
	Stderr string         // redacted stderr tail
}

// Error describes the failure in plain English.
func (e *SignalError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s killed by signal: %s", e.Cmd, e.Signal)
	}
	return fmt.Sprintf("%s killed by signal: %s: %s", e.Cmd, e.Signal, e.Stderr)
}

// BrokenPipeError reports a command whose reader closed its pipe
// before the command finished writing (EPIPE/SIGPIPE). It is distinct
// from ExitError: the command did not malfunction — its consumer went
// away. Matches ErrBrokenPipe.
type BrokenPipeError struct {
	Cmd string // program name of the cut-off writer
}

// Error describes the condition in plain English.
func (e *BrokenPipeError) Error() string {
	return fmt.Sprintf("%s: %s", e.Cmd, ErrBrokenPipe)
}

// Is reports whether target is ErrBrokenPipe.
func (e *BrokenPipeError) Is(target error) bool { return target == ErrBrokenPipe }
