// Package repohealth keeps each destination's restic repository
// healthy (spec §19.7): a scheduled `restic check` with its own light
// and alerts, and careful handling of stale locks. It wraps restic and
// never decides success from parsed text (rule 4).
package repohealth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/restic"
)

// Light values for a destination's repository health.
const (
	// StateUnchecked means no check has run yet.
	StateUnchecked = "unchecked"
	// StateOK means the last check passed (green).
	StateOK = "ok"
	// StateAmber means Ballast could not tell, or left something for a
	// person to look at (for example a lock it will not remove).
	StateAmber = "amber"
	// StateFailed means restic check found a problem (red).
	StateFailed = "failed"
)

const (
	// DefaultIntervalHours is how often a destination is checked: weekly.
	DefaultIntervalHours = 168
	// DefaultSubset is the share of stored data read back by default.
	DefaultSubset = "5%"
	// DefaultRunTimeout is how old a lock must be before it can count
	// as stale: longer than any backup run is expected to take.
	DefaultRunTimeout = 6 * time.Hour
)

// Status is a destination's repository health.
type Status struct {
	// Destination is the destination's name.
	Destination string `json:"destination"`
	// Enabled is false when scheduled checks are switched off.
	Enabled bool `json:"enabled"`
	// IntervalHours is how often the check runs.
	IntervalHours int `json:"interval_hours"`
	// Subset is the --read-data-subset value ("" reads no data).
	Subset string `json:"subset"`
	// State is one of the State constants.
	State string `json:"state"`
	// Light is green, amber, red or grey, for the Destinations page.
	Light string `json:"light"`
	// Detail explains the state in plain English.
	Detail string `json:"detail"`
	// LastChecked is when the last check ended (zero if never).
	LastChecked time.Time `json:"last_checked,omitempty"`
	// LastOK is when a check last passed (zero if never).
	LastOK time.Time `json:"last_ok,omitempty"`
}

// LightFor maps a state to a traffic-light colour.
func LightFor(state string) string {
	switch state {
	case StateOK:
		return "green"
	case StateAmber:
		return "amber"
	case StateFailed:
		return "red"
	}
	return "grey"
}

// Alerter delivers a repository-health alert to the services that back
// up to the destination. *Checker's default does this through notify.
type Alerter interface {
	// Alert tells people about a problem with a destination's repository.
	Alert(ctx context.Context, destination, state, detail string) error
	// Recovered clears an open alert once the repository is healthy.
	Recovered(ctx context.Context, destination string) error
}

// Checker runs repository health work. Zero values of optional fields
// use production behaviour.
type Checker struct {
	// DB is the Ballast database.
	DB *sql.DB
	// Repo opens a destination's restic repository.
	Repo func(inventory.Destination) (*restic.Repository, error)
	// Alerts, when set, is told about failures and recoveries.
	Alerts Alerter
	// Redact removes secrets from any text stored or returned.
	Redact func(string) string
	// LockTimeout is the minimum age of a stale lock; zero means
	// DefaultRunTimeout.
	LockTimeout time.Duration
	// Hostname returns this machine's name; nil uses os.Hostname.
	Hostname func() (string, error)
	// ProcessAlive reports whether a process id exists on this machine;
	// nil checks with signal 0.
	ProcessAlive func(pid int) bool
	// Now returns the current time; nil uses time.Now.
	Now func() time.Time
	// Locks, when set, keeps two Ballast processes (serve and run-due)
	// from checking the same destination at once.
	Locks interface {
		TryLock(name string) (release func(), ok bool, err error)
	}

	mu      sync.Mutex
	running map[string]bool
}

// ErrBusy is returned when a destination is already being checked.
var ErrBusy = errors.New("this destination is already being checked")

// begin marks destination as being checked, or reports ErrBusy.
func (c *Checker) begin(destination string) (func(), error) {
	c.mu.Lock()
	if c.running[destination] {
		c.mu.Unlock()
		return nil, fmt.Errorf("destination %s: %w", destination, ErrBusy)
	}
	if c.running == nil {
		c.running = map[string]bool{}
	}
	c.running[destination] = true
	c.mu.Unlock()
	done := func() { c.mu.Lock(); delete(c.running, destination); c.mu.Unlock() }
	if c.Locks == nil {
		return done, nil
	}
	release, ok, err := c.Locks.TryLock("repo-check-" + destination)
	if err != nil {
		done()
		return nil, fmt.Errorf("destination %s: %w", destination, err)
	}
	if !ok {
		done()
		return nil, fmt.Errorf("destination %s: %w (by another Ballast process)", destination, ErrBusy)
	}
	return func() { release(); done() }, nil
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Checker) redact(s string) string {
	if c.Redact != nil {
		return c.Redact(s)
	}
	return s
}

func (c *Checker) lockTimeout() time.Duration {
	if c.LockTimeout > 0 {
		return c.LockTimeout
	}
	return DefaultRunTimeout
}

func (c *Checker) alive(pid int) bool {
	if c.ProcessAlive != nil {
		return c.ProcessAlive(pid)
	}
	return processAlive(pid)
}

// processAlive reports whether pid exists. Permission errors mean the
// process exists; anything unclear counts as alive so a lock is never
// removed on a guess.
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

// Get returns a destination's health, with defaults when it has no row.
func (c *Checker) Get(destination string) (Status, error) {
	if _, err := inventory.GetDestination(c.DB, destination); err != nil {
		return Status{}, fmt.Errorf("destination %s: %w", destination, err)
	}
	st := Status{Destination: destination, Enabled: true, IntervalHours: DefaultIntervalHours, Subset: DefaultSubset, State: StateUnchecked}
	var enabled int
	var checked, ok string
	err := c.DB.QueryRow(`SELECT enabled, interval_hours, subset, state, detail, last_checked_at, last_ok_at
		FROM destination_health WHERE destination = ?`, destination).
		Scan(&enabled, &st.IntervalHours, &st.Subset, &st.State, &st.Detail, &checked, &ok)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Status{}, fmt.Errorf("destination %s: reading repository health: %w", destination, err)
	}
	if err == nil {
		st.Enabled = enabled != 0
		st.LastChecked, _ = time.Parse(time.RFC3339Nano, checked)
		st.LastOK, _ = time.Parse(time.RFC3339Nano, ok)
	}
	st.Light = LightFor(st.State)
	return st, nil
}

// List returns the health of every destination.
func (c *Checker) List() ([]Status, error) {
	dests, err := inventory.ListDestinations(c.DB)
	if err != nil {
		return nil, fmt.Errorf("listing destinations for repository health: %w", err)
	}
	out := make([]Status, 0, len(dests))
	for _, d := range dests {
		if !d.Type.IsResticRepository() {
			continue // a Proton Drive copy has no restic repository to check
		}
		st, err := c.Get(d.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// Configure changes how often a destination is checked and how much
// data is read back. An empty subset checks structure only.
func (c *Checker) Configure(destination string, enabled bool, intervalHours int, subset string) error {
	if intervalHours < 1 {
		return fmt.Errorf("destination %s: check every how many hours? it must be at least 1", destination)
	}
	if subset != "" {
		if err := restic.ValidSubset(subset); err != nil {
			return fmt.Errorf("destination %s: %w", destination, err)
		}
	}
	if _, err := inventory.GetDestination(c.DB, destination); err != nil {
		return fmt.Errorf("destination %s: %w", destination, err)
	}
	_, err := c.DB.Exec(`INSERT INTO destination_health (destination, enabled, interval_hours, subset) VALUES (?, ?, ?, ?)
		ON CONFLICT(destination) DO UPDATE SET enabled = excluded.enabled, interval_hours = excluded.interval_hours, subset = excluded.subset`,
		destination, boolInt(enabled), intervalHours, subset)
	if err != nil {
		return fmt.Errorf("destination %s: saving repository health settings: %w", destination, err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// LockOutcome is what HandleLocks found and did.
type LockOutcome struct {
	// Removed is true when restic unlock was run.
	Removed bool
	// Note is empty when there was nothing to report, otherwise a plain
	// English explanation of what was left alone and why (amber).
	Note string
}

// HandleLocks looks at the repository's locks before a run or a check.
// A lock is removed (with plain `restic unlock`, never --remove-all)
// only when it belongs to this machine, is older than the run timeout
// and its process is gone. Every other lock is left alone and
// explained (spec §19.7).
func (c *Checker) HandleLocks(ctx context.Context, repo *restic.Repository, destination string) LockOutcome {
	out, _ := c.handleLocks(ctx, repo, destination, c.lockTimeout())
	return out
}

// ClearOwnLocks tidies up after a backup that was stopped on request:
// stopping restic mid-upload can leave its lock behind. It is
// HandleLocks without the minimum age: a lock is removed (plain
// `restic unlock`, never --remove-all) only when it belongs to this
// machine and its process is gone. A lock from another machine, or
// from a process that is still running, is left alone and explained in
// the Note. The error reports a lock that could not be looked at or
// removed; callers log it and carry on, since the run's own outcome is
// already decided.
func (c *Checker) ClearOwnLocks(ctx context.Context, repo *restic.Repository, destination string) (LockOutcome, error) {
	return c.handleLocks(ctx, repo, destination, 0)
}

// handleLocks is HandleLocks and ClearOwnLocks: minAge is how old a
// lock must be before it can count as stale.
func (c *Checker) handleLocks(ctx context.Context, repo *restic.Repository, destination string, minAge time.Duration) (LockOutcome, error) {
	locks, err := repo.Locks(ctx)
	if err != nil {
		msg := c.redact(fmt.Sprintf("Could not look at the locks on %s: %v", destination, err))
		return LockOutcome{Note: msg}, errors.New(msg)
	}
	host, herr := c.hostname()
	var stale []restic.Lock
	var notes []string
	var unlockErr error
	for _, l := range locks {
		age := c.now().Sub(l.Time)
		if age < minAge {
			continue // a recent lock is normal: someone is working
		}
		switch {
		case herr != nil:
			notes = append(notes, fmt.Sprintf("a lock from %s is %s old but this machine's name could not be read, so it was left alone", l.Hostname, age.Round(time.Minute)))
		case l.Hostname != host:
			notes = append(notes, fmt.Sprintf("a lock from another machine (%s) is %s old; Ballast never removes another machine's lock, so check that machine finished and run `restic unlock` yourself", l.Hostname, age.Round(time.Minute)))
		case c.alive(l.PID):
			notes = append(notes, fmt.Sprintf("a lock from this machine is %s old but its process (%d) is still running, so it was left alone", age.Round(time.Minute), l.PID))
		default:
			stale = append(stale, l)
		}
	}
	out := LockOutcome{}
	if len(stale) > 0 {
		if err := repo.Unlock(ctx); err != nil {
			notes = append(notes, fmt.Sprintf("an old lock from this machine could not be removed: %v", err))
			unlockErr = fmt.Errorf("destination %s: removing a lock left by this machine: %w", destination, err)
		} else {
			out.Removed = true
		}
	}
	if len(notes) > 0 {
		out.Note = c.redact(fmt.Sprintf("Repository %s: %s.", destination, strings.Join(notes, "; ")))
	}
	if unlockErr != nil {
		return out, errors.New(c.redact(unlockErr.Error()))
	}
	return out, nil
}

func (c *Checker) hostname() (string, error) {
	if c.Hostname != nil {
		return c.Hostname()
	}
	return os.Hostname()
}

// Check runs the lock handling and then `restic check` on one
// destination, records the result and alerts on a problem. A failed
// check is red; an unreachable repository or a lock Ballast would not
// remove is amber, because Ballast could not tell either way.
func (c *Checker) Check(ctx context.Context, destination string) (Status, error) {
	end, err := c.begin(destination)
	if err != nil {
		return Status{}, err
	}
	defer end()
	st, err := c.Get(destination)
	if err != nil {
		return Status{}, err
	}
	dest, err := inventory.GetDestination(c.DB, destination)
	if err != nil {
		return Status{}, fmt.Errorf("destination %s: %w", destination, err)
	}
	state, detail := c.run(ctx, dest, st.Subset)
	now := c.now()
	okAt := ""
	if state == StateOK {
		okAt = now.Format(time.RFC3339Nano)
	}
	_, err = c.DB.Exec(`INSERT INTO destination_health (destination, state, detail, last_checked_at, last_ok_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(destination) DO UPDATE SET state = excluded.state, detail = excluded.detail, last_checked_at = excluded.last_checked_at,
		last_ok_at = CASE WHEN excluded.last_ok_at != '' THEN excluded.last_ok_at ELSE destination_health.last_ok_at END`,
		destination, state, detail, now.Format(time.RFC3339Nano), okAt)
	if err != nil {
		return Status{}, fmt.Errorf("destination %s: saving repository health: %w", destination, err)
	}
	if c.Alerts != nil {
		var aerr error
		if state == StateOK {
			aerr = c.Alerts.Recovered(ctx, destination)
		} else {
			aerr = c.Alerts.Alert(ctx, destination, state, detail)
		}
		if aerr != nil && !errors.Is(aerr, notify.ErrNoChannels) {
			err = fmt.Errorf("destination %s: sending the repository health alert: %w", destination, aerr)
		}
	}
	out, gerr := c.Get(destination)
	if gerr != nil {
		return Status{}, gerr
	}
	return out, err
}

// run performs the check and returns the resulting state and detail.
func (c *Checker) run(ctx context.Context, dest inventory.Destination, subset string) (string, string) {
	if c.Repo == nil {
		return StateAmber, "Ballast is not set up to open repositories (internal wiring error)."
	}
	repo, err := c.Repo(dest)
	if err != nil {
		return StateAmber, c.redact(fmt.Sprintf("Could not open %s to check it: %v", dest.Name, err))
	}
	lock := c.HandleLocks(ctx, repo, dest.Name)
	if err := repo.Check(ctx, subset); err != nil {
		msg := c.redact(err.Error())
		if lock.Note != "" {
			return StateAmber, fmt.Sprintf("The check could not finish. %s Details: %s", lock.Note, msg)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return StateAmber, "The check was interrupted before it finished: " + msg
		}
		return StateFailed, fmt.Sprintf("restic found a problem in the backup repository %s: %s", dest.Name, msg)
	}
	if lock.Note != "" {
		return StateAmber, lock.Note
	}
	if subset == "" {
		return StateOK, "The repository's structure is consistent."
	}
	return StateOK, fmt.Sprintf("The repository is consistent and %s of its data was read back and verified.", subset)
}

// SweepDue checks every enabled destination whose last check is older
// than its interval. One destination's failure never stops the next
// (rule 3); every error is returned together.
func (c *Checker) SweepDue(ctx context.Context) error {
	list, err := c.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, st := range list {
		if !st.Enabled || ctx.Err() != nil {
			continue
		}
		if !st.LastChecked.IsZero() && c.now().Sub(st.LastChecked) < time.Duration(st.IntervalHours)*time.Hour {
			continue
		}
		if _, err := c.Check(ctx, st.Destination); err != nil && !errors.Is(err, ErrBusy) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
