package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestChildDiesWhenBallastIsKilled SIGKILLs a process that is running
// a command through a Runner, the way a crashed or OOM-killed Ballast
// would be, and checks that the command dies with it instead of
// carrying on as an orphan.
func TestChildDiesWhenBallastIsKilled(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	c := helperCommand("spawner", pidPath, "60")
	parent := exec.Command(c.Name, c.Args...)
	parent.Env = append(os.Environ(), c.Env...)
	if err := parent.Start(); err != nil {
		t.Fatalf("starting parent: %v", err)
	}
	t.Cleanup(func() {
		_ = parent.Process.Kill()
		_ = parent.Wait()
	})

	child := waitForPid(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	if err := parent.Process.Kill(); err != nil {
		t.Fatalf("killing parent: %v", err)
	}
	_ = parent.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for processAlive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still running 10s after its parent was killed; it must die with its parent", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForPid waits for the pidfile helper to write its pid to path.
func waitForPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(b))
			if err != nil {
				t.Fatalf("bad pid in %s: %v", path, err)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reading %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatal("child never wrote its pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive reports whether pid is a running process. A zombie
// (dead, waiting for init to reap it) counts as dead.
func processAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the parenthesised command name, which may
	// itself contain spaces or parentheses.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] != 'Z' && s[i+2] != 'X'
}
