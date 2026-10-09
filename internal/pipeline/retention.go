package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
)

// repoGates keeps a prune from running under anything else this
// process does to one repository. restic prune takes an exclusive
// repository lock, so a backup of another service to the same
// destination that starts mid-prune would otherwise fail with
// "repository is already locked". Uploads and read-backs share the
// gate; retention needs it to itself. Other processes (a
// `ballast run-due` next to `ballast serve`) are covered by restic's
// own --retry-lock instead (backend.Resolver sets
// restic.DefaultRetryLock).
//
// Unlike a sync.RWMutex, a retention step that is merely waiting
// never holds back a new upload or read-back: only a prune that is
// actually running does. One service's hours-long upload therefore
// cannot leave another service's finished backup unverified while a
// third service's retention queues behind it.
var repoGates sync.Map // repository location → *gate

// repoGate returns the process-wide gate for the repository at
// location. It is keyed by location, not destination name, so two
// destinations pointing at one repository share it.
func repoGate(location string) *gate {
	g, _ := repoGates.LoadOrStore(location, &gate{})
	return g.(*gate)
}

// gate is one repository's shared/exclusive lock, which remembers
// which services hold it so a waiter can say who it waits for.
type gate struct {
	mu      sync.Mutex
	readers map[string]int // service → shared holds
	writer  string         // service running retention; "" when none
	changed chan struct{}  // closed (and replaced) on every release
}

// wake returns the channel closed on the next release. g.mu must be
// held.
func (g *gate) wake() chan struct{} {
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.changed
}

// released wakes every waiter. g.mu must be held.
func (g *gate) released() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

// holdShared takes the shared side for service, waiting only while a
// prune is actually running. waiting, when not nil, is called with the
// holder's name each time the wait starts on a new holder, and with ""
// once the gate is taken after a wait. It returns the release func,
// or ctx's error if ctx ends first.
func (g *gate) holdShared(ctx context.Context, service string, waiting func(holder string)) (func(), error) {
	last := ""
	for {
		g.mu.Lock()
		if g.writer == "" {
			if g.readers == nil {
				g.readers = map[string]int{}
			}
			g.readers[service]++
			g.mu.Unlock()
			if last != "" && waiting != nil {
				waiting("")
			}
			return func() {
				g.mu.Lock()
				defer g.mu.Unlock()
				if g.readers[service]--; g.readers[service] <= 0 {
					delete(g.readers, service)
				}
				g.released()
			}, nil
		}
		holder, ch := g.writer, g.wake()
		g.mu.Unlock()
		if holder != last && waiting != nil {
			waiting(holder)
		}
		last = holder
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// tryExclusive takes the gate for service's retention if nobody holds
// it; otherwise it returns who does ("immich", or "immich, paperless").
func (g *gate) tryExclusive(service string) (release func(), holder string, wake <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.writer != "" {
		return nil, g.writer, g.wake()
	}
	if len(g.readers) > 0 {
		names := make([]string, 0, len(g.readers))
		for n := range g.readers {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, strings.Join(names, ", "), g.wake()
	}
	g.writer = service
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.writer = ""
		g.released()
	}, "", nil
}

// retentionWaitLimit is how long a retention step waits for the
// repository to be free before it gives up until the service's next
// verified backup. Retention runs after the run's outcome is already
// recorded, so this only bounds how long the run's slot stays taken.
var retentionWaitLimit = 20 * time.Minute

// holdShared takes rt's repository gate for an upload or a read-back.
// While it waits for another service's prune, the log and the run's
// status say which service holds the repository.
func (p *Pipeline) holdShared(ctx context.Context, service string, rt *runTarget) (func(), error) {
	return repoGate(rt.repo.Location).holdShared(ctx, service, func(holder string) {
		note := ""
		if holder != "" {
			note = lockWaitNote(rt.dest.Name, holder)
			p.logf("%s: %s", service, note)
		}
		if rt.run != nil {
			if err := rt.run.SetWaiting(p.redact(note)); err != nil {
				p.logf("%s: %v", service, err)
			}
		}
	})
}

// lockWaitNote is the plain-English line for a run waiting on another
// service's use of a destination's repository.
func lockWaitNote(dest, holder string) string {
	return fmt.Sprintf("waiting for repository lock on %s (held by %s)", dest, holder)
}

// retentionGroupBy makes all of one service's snapshots a single
// retention series, whichever host label or path set they were taken
// with: restic's default ("host,paths") would split the series when
// a catalog update changes the backed-up paths, and the old series
// would then be kept forever.
const retentionGroupBy = "tags"

// applyRetention removes service's snapshots that its retention
// policy no longer keeps (`restic forget`, scoped to the service's
// own tag so a shared repository's other services are untouched),
// then reclaims their space (`restic prune`) when anything was
// removed. It runs only after a verified SUCCESS and never changes
// the run's outcome: a failure is logged, and the next verified
// backup tries again.
func (p *Pipeline) applyRetention(ctx context.Context, service, dest string, repo *restic.Repository, policy inventory.Retention) {
	release, ok := p.holdExclusive(ctx, service, dest, repo.Location)
	if !ok {
		return
	}
	defer release()

	res, err := repo.Forget(ctx, policy.Policy(), restic.ForgetOptions{
		Tags:    []string{serviceTag(service)},
		GroupBy: retentionGroupBy,
	})
	if err != nil {
		p.logf("%s: retention cleanup (forget) failed, the backup itself succeeded: %s", service, p.redact(err.Error()))
		return
	}
	if len(res.Removed) == 0 {
		return
	}
	if err := repo.Prune(ctx); err != nil {
		p.logf("%s: removed %d old backup(s) but reclaiming their space (prune) failed, the backup itself succeeded: %s", service, len(res.Removed), p.redact(err.Error()))
		return
	}
	p.logf("%s: retention kept %d backup(s) and removed %d older one(s) (keep last %d, %d daily, %d weekly, %d monthly)",
		service, len(res.Kept), len(res.Removed), policy.KeepLast, policy.KeepDaily, policy.KeepWeekly, policy.KeepMonthly)
}

// holdExclusive waits, up to retentionWaitLimit, for the repository
// at location to be free and takes it for service's retention. It
// never queues ahead of later uploads or read-backs (see repoGates):
// if the repository stays busy, retention is skipped, said so in the
// log, and tried again after the service's next verified backup.
func (p *Pipeline) holdExclusive(ctx context.Context, service, dest, location string) (func(), bool) {
	g := repoGate(location)
	limit := time.NewTimer(retentionWaitLimit)
	defer limit.Stop()
	last := ""
	for {
		release, holder, wake := g.tryExclusive(service)
		if release != nil {
			return release, true
		}
		if holder != last {
			p.logf("%s: %s before removing old backups", service, lockWaitNote(dest, holder))
			last = holder
		}
		select {
		case <-wake:
		case <-limit.C:
			p.logf("%s: skipped removing old backups this time because %s is still busy (in use by %s); this is tried again after the next verified backup", service, dest, holder)
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}

// serviceTag is the restic tag every snapshot of service carries.
func serviceTag(service string) string {
	return "ballast-service:" + service
}
