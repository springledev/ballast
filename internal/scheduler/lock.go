package scheduler

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// Locker hands out per-service run locks. Holding a service's lock
// is what entitles a process to start and own a run for it; see
// Coordinator.
type Locker interface {
	// TryLock takes service's lock without waiting. ok is false when
	// another holder has it. release must be called exactly once when
	// ok is true.
	TryLock(service string) (release func(), ok bool, err error)
}

// Prober is an optional Locker capability: asking whether a service's
// lock is held without ever competing with other askers. Probes take a
// shared lock while real runs hold an exclusive one, so any number of
// probes can overlap without one seeing another as a running backup.
type Prober interface {
	// Held reports whether a real run currently holds service's lock.
	Held(service string) (bool, error)
}

// FileLocker implements Locker with flock(2) on one file per service
// in Dir. The kernel drops a flock the moment its process exits, for
// any reason — crash, SIGKILL, OOM kill — so a lock can never be left
// stale, and "unfinished run, lock free" reliably means the run's
// process is gone. Locks are exclusive across processes and also
// between separate TryLock calls within one process.
type FileLocker struct {
	// Dir holds the lock files; it is created (0700) if missing.
	Dir string
}

// TryLock implements Locker.
func (l FileLocker) TryLock(service string) (func(), bool, error) {
	if service == "" {
		return nil, false, fmt.Errorf("taking run lock: service must not be empty")
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("taking run lock for %s: creating lock directory: %w", service, err)
	}
	// PathEscape turns any "/" into %2F, so the name cannot leave Dir.
	path := filepath.Join(l.Dir, url.PathEscape(service)+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("taking run lock for %s: %w", service, err)
	}
	fd := int(f.Fd())
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, false, nil
	}
	if err != nil {
		f.Close()
		return nil, false, fmt.Errorf("taking run lock for %s: %w", service, err)
	}
	// Closing the file releases the flock; the lock file itself stays
	// (removing it would race with another process opening it).
	return func() { f.Close() }, true, nil
}

// Held implements Prober. It takes the lock file's shared flock without
// waiting and lets go at once, so it never conflicts with other probes.
// It can make a real run's exclusive TryLock fail for an instant; that
// is why claims retry for a short while.
func (l FileLocker) Held(service string) (bool, error) {
	if service == "" {
		return false, fmt.Errorf("checking run lock: service must not be empty")
	}
	path := filepath.Join(l.Dir, url.PathEscape(service)+".lock")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		// No lock file means no run has ever held this lock.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking run lock for %s: %w", service, err)
	}
	defer f.Close()
	fd := int(f.Fd())
	for {
		err = syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking run lock for %s: %w", service, err)
	}
	return false, nil // closing f drops the shared lock
}
