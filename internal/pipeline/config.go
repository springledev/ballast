package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/springledev/ballast/internal/strategy"
)

// configSnapshotMethod is the verification method name of the Proxmox
// config snapshot's parse-back check.
const configSnapshotMethod = "config-snapshot"

// runConfigSnapshot backs up the Proxmox host's own settings (spec
// §19.6) through the same state machine as every other service:
// PREPARING → DUMPING (read the settings through the API, secrets left
// out) → UPLOADING (JSON files to every destination) → VERIFYING (the
// files are read back and parsed, every guest has a config in them, and
// each destination's restic read-back passes) → SUCCESS. Ballast only
// ever reads from Proxmox here.
func (p *Pipeline) runConfigSnapshot(ctx context.Context, svc inventory.Service) (engine.State, error) {
	service := svc.Name
	var targets []*runTarget
	for _, t := range svc.EffectiveTargets() {
		dest, err := inventory.GetDestination(p.DB, t.Destination)
		if err != nil {
			return "", fmt.Errorf("service %s: %w", service, err)
		}
		targets = append(targets, &runTarget{target: t, dest: dest})
	}
	m, err := engine.StartRun(p.DB, service)
	if err != nil {
		return "", fmt.Errorf("service %s: %w", service, err)
	}
	if p.Notifier != nil {
		m.SetNotifier(ctx, p.Notifier)
	}
	defer p.logNotifyErr(service, m)

	fail := func(cause error) (engine.State, error) {
		if ctx.Err() != nil {
			if terr := m.Transition(engine.CANCELLED, "cancelled: Ballast is shutting down"); terr != nil {
				p.logf("%s: recording cancellation: %v", service, terr)
			}
			return engine.CANCELLED, ensureServiceContext(service, ctx.Err())
		}
		err := ensureServiceContext(service, cause)
		detail := p.redact(err.Error())
		if terr := m.Transition(engine.FAILED, detail); terr != nil {
			p.logf("%s: recording failure: %v", service, terr)
		}
		p.logf("%s: run %d failed: %s", service, m.Run().ID, detail)
		return engine.FAILED, err
	}

	if err := m.Transition(engine.PREPARING, "preparing"); err != nil {
		return fail(err)
	}
	if len(targets) == 0 {
		return fail(fmt.Errorf("a Proxmox config snapshot needs a destination to be stored in"))
	}
	conn, err := inventory.GetProxmoxConnection(p.DB)
	if err != nil {
		return fail(fmt.Errorf("Proxmox connection: %w", err))
	}
	cl, err := p.Proxmox.Client(conn)
	if err != nil {
		return fail(err)
	}
	m.RequireVerifications(configSnapshotMethod, readBackMethod)
	for _, rt := range targets {
		rt.repo, rt.err = p.Resolver().Open(rt.dest.Target())
	}
	if allFailed(targets) {
		return fail(targetsError(targets))
	}
	workDir := filepath.Join(p.WorkRoot, service)
	if err := os.RemoveAll(workDir); err != nil {
		return fail(fmt.Errorf("preparing work directory %s: %w", workDir, err))
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fail(fmt.Errorf("preparing work directory %s: %w", workDir, err))
	}
	defer os.RemoveAll(workDir) //nolint:errcheck // best-effort tidy-up of a private folder

	if err := m.Transition(engine.DUMPING, "reading Proxmox settings"); err != nil {
		return fail(err)
	}
	snap, err := cl.SnapshotConfig(ctx)
	if err != nil {
		return fail(fmt.Errorf("%s", p.redact(err.Error())))
	}
	paths, err := snap.WriteTo(workDir)
	if err != nil {
		return fail(err)
	}
	res := &strategy.Result{}
	for _, part := range proxmox.SnapshotParts() {
		res.Artifacts = append(res.Artifacts, strategy.Artifact{Name: part, Path: paths[part], Format: "json"})
	}
	for _, rt := range targets {
		if rt.err == nil {
			rt.paths, rt.artifacts, rt.err = selectParts(res, rt.target)
		}
	}
	if allFailed(targets) {
		return fail(targetsError(targets))
	}

	if err := m.Transition(engine.UPLOADING, "uploading"); err != nil {
		return fail(err)
	}
	p.forEachTarget(targets, func(rt *runTarget) {
		br, err := p.uploadTarget(ctx, service, rt, res)
		if err != nil {
			rt.err = err
			return
		}
		rt.snapshotID = br.SnapshotID
	})
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	if allFailed(targets) {
		return fail(targetsError(targets))
	}
	for _, rt := range targets {
		if rt.snapshotID != "" {
			if err := m.SetSnapshotID(rt.snapshotID); err != nil {
				return fail(err)
			}
			break
		}
	}

	if err := m.Transition(engine.VERIFYING, "verifying"); err != nil {
		return fail(err)
	}
	// Guests are listed again now, so a guest the snapshot missed is
	// caught rather than trusted from the same read.
	current, lerr := cl.ListGuests(ctx)
	var layer proxmox.LayerResult
	if lerr != nil {
		layer = proxmox.LayerResult{Detail: "could not list the guests to check the snapshot against: " + lerr.Error()}
	} else {
		layer = proxmox.VerifySnapshotFiles(paths, current)
	}
	rec := engine.VerificationRecord{Method: configSnapshotMethod, Passed: layer.Passed, Detail: p.redact(layer.Detail)}
	if len(snap.Skipped) > 0 {
		rec.Warning = p.redact("some optional settings could not be read with this API token and are not in the snapshot: " + strings.Join(snap.Skipped, "; "))
	}
	if err := m.RecordVerification(rec); err != nil {
		return fail(err)
	}
	p.forEachTarget(targets, func(rt *runTarget) {
		rt.checks = p.targetChecks(ctx, service, rt, false)
	})
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	for _, rt := range targets {
		for _, c := range rt.checks {
			if err := m.RecordVerification(c.rec); err != nil {
				return fail(err)
			}
		}
	}
	for _, rt := range targets {
		switch failed, hard := firstHardFailure(rt.checks); {
		case rt.err != nil:
			rt.state, rt.detail = engine.FAILED, p.redact(rt.err.Error())
		case hard:
			rt.state, rt.detail = engine.FAILED, fmt.Sprintf("verification failed (%s): %s", failed.rec.Method, failed.rec.Detail)
		case anyUnverifiable(rt.checks):
			rt.state, rt.detail = engine.SUCCESS_UNVERIFIED, "the copy was uploaded but could not be fully verified: "+unverifiableSummary(rt.checks)
		default:
			rt.state, rt.detail = engine.SUCCESS, "copy verified"
		}
		if err := m.RecordTarget(engine.TargetResult{Destination: rt.dest.Name, State: rt.state, SnapshotID: rt.snapshotID, Detail: rt.detail}); err != nil {
			return fail(err)
		}
	}
	defer p.retainTargets(ctx, service, targets)

	if !layer.Passed {
		return fail(fmt.Errorf("the Proxmox settings were read but the snapshot could not be proven complete: %s", layer.Detail))
	}
	if anyTargetFailed(targets) {
		return fail(fmt.Errorf("the Proxmox settings snapshot is verified, but storing it failed: %w", targetsError(targets)))
	}
	if anyTargetUnverified(targets) {
		detail := targetsSummary(targets, engine.SUCCESS_UNVERIFIED)
		if err := m.Transition(engine.SUCCESS_UNVERIFIED, detail); err != nil {
			return fail(err)
		}
		return engine.SUCCESS_UNVERIFIED, ensureServiceContext(service, fmt.Errorf("%s", detail))
	}
	if err := m.Transition(engine.SUCCESS, "backup verified"); err != nil {
		return fail(err)
	}
	p.logf("%s: run %d succeeded and is verified", service, m.Run().ID)
	return engine.SUCCESS, nil
}
