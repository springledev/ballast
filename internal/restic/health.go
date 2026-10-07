package restic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Lock is one lock restic holds on a repository, as read from the
// repository itself.
type Lock struct {
	// ID is the lock's identifier in the repository.
	ID string
	// Time is when the lock was created or last refreshed by its owner.
	Time time.Time
	// Exclusive is true for locks that block every other command
	// (prune, check, ...) and false for shared locks (backup).
	Exclusive bool
	// Hostname is the machine that took the lock.
	Hostname string
	// Username is the account that took the lock.
	Username string
	// PID is the process that took the lock, on Hostname.
	PID int
}

// Locks lists the locks currently held on the repository. It reads
// without locking, so it works even when the repository is locked.
func (r *Repository) Locks(ctx context.Context) ([]Lock, error) {
	res, err := r.run(ctx, "list", "locks", "--no-lock", "-q")
	if err != nil {
		return nil, err
	}
	var locks []Lock
	for _, id := range strings.Fields(string(res.Stdout)) {
		cat, err := r.run(ctx, "cat", "lock", id, "--no-lock")
		if err != nil {
			// The lock may have been released between the two calls.
			continue
		}
		var raw struct {
			Time      time.Time `json:"time"`
			Exclusive bool      `json:"exclusive"`
			Hostname  string    `json:"hostname"`
			Username  string    `json:"username"`
			PID       int       `json:"pid"`
		}
		if err := json.Unmarshal(cat.Stdout, &raw); err != nil {
			return nil, fmt.Errorf("restic cat lock %s for repository %s: unreadable lock: %w", id, r.Location, err)
		}
		locks = append(locks, Lock{ID: id, Time: raw.Time, Exclusive: raw.Exclusive, Hostname: raw.Hostname, Username: raw.Username, PID: raw.PID})
	}
	return locks, nil
}

// Unlock removes stale locks with plain `restic unlock`. It never
// passes --remove-all, so restic itself only removes locks it can
// prove are stale (spec §19.7); callers decide whether to call it.
func (r *Repository) Unlock(ctx context.Context) error {
	_, err := r.run(ctx, "unlock")
	return err
}

// Check runs `restic check` over the repository structure. A non-empty
// subset (for example "5%" or "1/10") also reads that share of the
// stored data back and verifies it. It returns an error when restic
// finds a problem.
func (r *Repository) Check(ctx context.Context, subset string) error {
	args := []string{"check"}
	if subset != "" {
		if err := ValidSubset(subset); err != nil {
			return err
		}
		args = append(args, "--read-data-subset="+subset)
	}
	_, err := r.run(ctx, args...)
	return err
}

// ValidSubset reports whether s is a value restic accepts for
// --read-data-subset: a percentage ("5%"), a fraction ("1/10") or a
// size ("500M"). It is checked here so a typo never reaches restic's
// command line.
func ValidSubset(s string) error {
	bad := fmt.Errorf("%q is not a valid share of data to read back: use a percentage such as 5%%, a fraction such as 1/10, or a size such as 500M", s)
	if s == "" || len(s) > 12 {
		return bad
	}
	digits := func(t string) bool {
		if t == "" {
			return false
		}
		for _, c := range t {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	switch {
	case strings.HasSuffix(s, "%"):
		if digits(strings.TrimSuffix(s, "%")) {
			return nil
		}
	case strings.Contains(s, "/"):
		a, b, _ := strings.Cut(s, "/")
		if digits(a) && digits(b) {
			return nil
		}
	default:
		if n := len(s); n > 1 && strings.ContainsRune("kKmMgGtT", rune(s[n-1])) && digits(s[:n-1]) {
			return nil
		}
	}
	return bad
}
