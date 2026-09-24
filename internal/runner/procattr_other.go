//go:build !linux

package runner

import "os/exec"

// setParentDeathSignal is a no-op off Linux: Ballast ships for Linux
// only, and other systems have no parent-death signal. This file keeps
// the package building for development on other platforms.
func setParentDeathSignal(cmd *exec.Cmd) {}
