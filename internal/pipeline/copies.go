package pipeline

import (
	"context"
	"path/filepath"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/protondrive"
	"github.com/springledev/ballast/internal/strategy"
)

// copyTarget is a report-only second copy (a Proton Drive destination)
// of one run (spec §12, §19.8). It is kept apart from the restic
// targets so it can never fail the run, hide a restic result or be
// hidden by one.
type copyTarget struct {
	target inventory.Target
	dest   inventory.Destination
}

// ProtonClient returns the Proton Drive client for dest: its session
// folder is a 0700 folder under the credential directory.
func (p *Pipeline) ProtonClient(dest inventory.Destination) *protondrive.Client {
	dir := p.OwnCredDir
	if dir == "" {
		dir = p.CredDir
	}
	return &protondrive.Client{Bin: p.ProtonBin, StateDir: protondrive.StateDirFor(dir, dest.Name), Runner: p.Runner}
}

// runCopies sends the run's verified artifacts to every Proton Drive
// target, once the main backup has a verified result, and records each
// outcome as a secondary (report-only) result. It returns nothing on
// purpose: nothing here may change the run's own outcome.
func (p *Pipeline) runCopies(ctx context.Context, m *engine.Machine, service string, targets []*runTarget, copies []*copyTarget, res *strategy.Result) {
	if len(copies) == 0 || ctx.Err() != nil {
		return
	}
	// Copy only what restic holds verified: a failed main backup means
	// no copy is attempted, and a main backup nobody could verify has
	// nothing verified to copy.
	mainOK := false
	for _, rt := range targets {
		if rt.state == engine.FAILED {
			mainOK = false
			break
		}
		if rt.state == engine.SUCCESS {
			mainOK = true
		}
	}
	for _, ct := range copies {
		var r protondrive.Result
		if !mainOK {
			r.Detail = "Not copied to Proton Drive this time because the main backup did not finish verified."
		} else {
			r = p.copyOne(ctx, service, ct, res)
		}
		state := engine.SUCCESS_UNVERIFIED // amber: a copy problem is never red (spec §12)
		if r.Verified {
			state = engine.SUCCESS
		}
		detail := p.redact(r.Detail)
		if err := m.RecordTarget(engine.TargetResult{Destination: ct.dest.Name, State: state, Detail: detail, Secondary: true}); err != nil {
			p.logf("%s: recording the Proton Drive copy to %s: %v", service, ct.dest.Name, err)
		}
		p.logf("%s: Proton Drive copy to %s: %s", service, ct.dest.Name, detail)
		p.alertCopy(ctx, service, ct.dest.Name, r.Verified, mainOK, detail)
	}
}

// skipCopies records, for a run that failed before the copy stage, that
// the second copy was not attempted. It is amber like every copy
// problem, and says why.
func (p *Pipeline) skipCopies(m *engine.Machine, service string, copies []*copyTarget) {
	recorded := map[string]bool{}
	if rows, err := engine.RunTargets(p.DB, m.Run().ID); err == nil {
		for _, r := range rows {
			recorded[r.Destination] = true
		}
	}
	for _, ct := range copies {
		if recorded[ct.dest.Name] {
			continue // the copy stage already reported this one
		}
		err := m.RecordTarget(engine.TargetResult{Destination: ct.dest.Name, State: engine.SUCCESS_UNVERIFIED, Secondary: true,
			Detail: "Not copied to Proton Drive this time because the main backup did not finish."})
		if err != nil {
			p.logf("%s: recording the skipped Proton Drive copy to %s: %v", service, ct.dest.Name, err)
		}
	}
}

// alertCopy raises or clears the copy's own alert. A copy skipped
// because the main backup failed is not alerted (the main backup's own
// alert says so).
func (p *Pipeline) alertCopy(ctx context.Context, service, dest string, ok, attempted bool, detail string) {
	if p.CopyAlerts == nil || !attempted {
		return
	}
	var err error
	if ok {
		err = p.CopyAlerts.Recovered(ctx, service, dest)
	} else {
		err = p.CopyAlerts.Alert(ctx, service, dest, detail)
	}
	if err != nil {
		p.logf("%s: Proton Drive copy alert: %v", service, err)
	}
}

// copyOne copies one target's parts. Every failure is a Result that is
// not Verified.
func (p *Pipeline) copyOne(ctx context.Context, service string, ct *copyTarget, res *strategy.Result) protondrive.Result {
	paths, arts, err := selectParts(res, ct.target)
	if err != nil {
		return protondrive.Result{Detail: p.redact(err.Error())}
	}
	req := protondrive.Request{
		Service:  service,
		RunTime:  time.Now().UTC(),
		Folder:   protondrive.FolderFor(ct.dest.Settings.Path),
		MaxBytes: int64(ct.dest.Settings.MaxArtifactMB) << 20,
		KeepLast: ct.target.EffectiveRetention(ct.dest).KeepLast,
		WorkDir:  filepath.Join(p.WorkRoot, service),
	}
	for _, a := range arts {
		req.Files = append(req.Files, protondrive.File{Name: filepath.Base(a.Path), Path: a.Path})
	}
	for _, path := range paths {
		name := res.PathParts[path]
		if name == "" {
			name = filepath.Base(path)
		}
		req.SkippedFolders = append(req.SkippedFolders, name)
	}
	return p.ProtonClient(ct.dest).Copy(ctx, req)
}
