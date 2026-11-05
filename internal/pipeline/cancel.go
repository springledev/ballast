package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/springledev/ballast/internal/engine"
)

// lockCleanupTimeout bounds the lock tidy-up of one destination after a
// stopped run, so an unreachable destination cannot hold the run open.
const lockCleanupTimeout = 2 * time.Minute

// cancelRun ends m as CANCELLED, after tidying up what the run itself
// left behind: its own work directory (workDir, "" for none) and, when
// it got as far as uploading, any restic lock it left on each
// destination. Containers stopped for the backup have already been
// started again by the strategy, and throwaway containers removed by
// the checks, before the run gets here.
//
// Every destination is tidied and recorded on its own (rule 3): one
// whose lock cannot be removed is reported in its own row and never
// hides another's. Each target is marked CANCELLED, keeping its
// snapshot ID when its upload had finished, so retention never runs
// for a cancelled run. The recorded reason says whether the backup was
// stopped on request or because Ballast is shutting down; it never
// carries command output, so it holds no secrets. note, when set, is a
// plain-English line appended to it (redacted again here), such as what
// became of a dump Proxmox was making.
func (p *Pipeline) cancelRun(ctx context.Context, m *engine.Machine, service string, cause error, targets []*runTarget, workDir, note string) (engine.State, error) {
	reason := engine.CancelDetail(ctx, cause)
	detail := reason
	if note != "" {
		detail += ". " + p.redact(note)
	}
	if workDir != "" {
		if err := os.RemoveAll(workDir); err != nil {
			p.logf("%s: removing the work folder of the stopped backup: %v", service, err)
		}
	}
	if st := m.Run().State; st == engine.UPLOADING || st == engine.VERIFYING {
		p.tidyTargets(m, service, targets, reason == engine.CancelDetailRequested)
	}
	for _, rt := range targets {
		rt.state = engine.CANCELLED // never pruned: retention is for verified copies only
	}
	if terr := m.Transition(engine.CANCELLED, detail); terr != nil {
		p.logf("%s: recording cancellation: %v", service, terr)
	}
	p.logf("%s: run %d %s", service, m.Run().ID, detail)
	err := ctx.Err()
	if err == nil {
		err = cause
	}
	if reason == engine.CancelDetailRequested && !errors.Is(err, engine.ErrCancelledByUser) {
		err = fmt.Errorf("%w (%w)", engine.ErrCancelledByUser, err)
	}
	return engine.CANCELLED, ensureServiceContext(service, err)
}

// tidyTargets removes the lock a stopped upload or read-back may have
// left on each destination and records each destination as CANCELLED.
// It runs on fresh, bounded contexts: the run's own is already done.
func (p *Pipeline) tidyTargets(m *engine.Machine, service string, targets []*runTarget, onRequest bool) {
	why := "Stopped because Ballast was shutting down"
	if onRequest {
		why = "Stopped on request"
	}
	for _, rt := range targets {
		if rt.repo == nil {
			continue // never opened, so nothing was started there
		}
		d := why + " before this copy was finished. The last good backup there is kept."
		if rt.snapshotID != "" {
			d = why + " after this copy was uploaded; it is not counted as a good backup because the run did not finish."
		}
		if p.RepoHealth != nil {
			lctx, cancel := context.WithTimeout(context.Background(), lockCleanupTimeout)
			out, err := p.RepoHealth.ClearOwnLocks(lctx, rt.repo, rt.dest.Name)
			cancel()
			switch {
			case err != nil:
				p.logf("%s: warning: %v", service, err)
				d += " Ballast could not tidy up the lock the backup may have left there, so the next backup may have to wait for it: " + p.redact(err.Error())
			case out.Note != "":
				p.logf("%s: warning: %s", service, out.Note)
			case out.Removed:
				p.logf("%s: removed the lock the stopped backup left on %s", service, rt.dest.Name)
			}
		}
		if err := m.RecordTarget(engine.TargetResult{Destination: rt.dest.Name, State: engine.CANCELLED, SnapshotID: rt.snapshotID, Detail: d}); err != nil {
			p.logf("%s: %v", service, err)
		}
	}
}
