package pipeline

import (
	"context"
	"sync"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
)

// repoLocks serializes pruning against everything else this process
// does to one repository. restic prune takes an exclusive repository
// lock, so a backup of another service to the same destination that
// starts mid-prune would otherwise fail with "repository is already
// locked". Backups hold the shared side; retention holds the
// exclusive side. Other processes (a `ballast run-due` next to
// `ballast serve`) are covered by restic's own --retry-lock instead
// (backend.Resolver sets restic.DefaultRetryLock).
var repoLocks sync.Map // repository location → *sync.RWMutex

// repoLock returns the process-wide lock for the repository at
// location. It is keyed by location, not destination name, so two
// destinations pointing at one repository share it.
func repoLock(location string) *sync.RWMutex {
	l, _ := repoLocks.LoadOrStore(location, &sync.RWMutex{})
	return l.(*sync.RWMutex)
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
func (p *Pipeline) applyRetention(ctx context.Context, service string, repo *restic.Repository, policy inventory.Retention) {
	lock := repoLock(repo.Location)
	lock.Lock()
	defer lock.Unlock()

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

// serviceTag is the restic tag every snapshot of service carries.
func serviceTag(service string) string {
	return "ballast-service:" + service
}
