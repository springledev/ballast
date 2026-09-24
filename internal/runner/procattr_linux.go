package runner

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal makes the kernel SIGKILL cmd's process as soon
// as the thread that started it exits. Ballast's run locks are flocks
// the kernel drops when Ballast dies, so without this a SIGKILLed
// Ballast could leave restic or docker working while the next sweep,
// seeing the lock free, starts a second run. Pdeathsig is tied to the
// starting OS thread, not the process: Pipeline locks its goroutine to
// that thread until every stage is reaped.
func setParentDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
