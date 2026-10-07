package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/springledev/ballast/internal/strategy"
)

// guestConfigMethod is the verification method name of a guest job's
// restorability drill (the dump is present at the size Proxmox
// reported and its guest settings read back).
const guestConfigMethod = "guest-config"

// runGuest backs up one Proxmox guest through the same state machine as
// every other service (spec §19.6): PREPARING → DUMPING (vzdump) →
// UPLOADING (the optional copy of the dump to restic destinations) →
// VERIFYING → SUCCESS. The guest-config drill must pass for SUCCESS,
// and so must every copy's own restic read-back. Pruning of old dumps
// only ever deletes volumes Ballast recorded creating.
func (p *Pipeline) runGuest(ctx context.Context, svc inventory.Service) (engine.State, error) {
	service := svc.Name
	var targets []*runTarget
	for _, t := range svc.EffectiveTargets() {
		dest, err := inventory.GetDestination(p.DB, t.Destination)
		if err != nil {
			return "", fmt.Errorf("service %s: %w", service, err)
		}
		targets = append(targets, &runTarget{target: t, dest: dest})
	}
	if svc.Guest == nil {
		return "", fmt.Errorf("service %s: this Proxmox guest job has no settings saved", service)
	}
	guest := *svc.Guest
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
	conn, err := inventory.GetProxmoxConnection(p.DB)
	if err != nil {
		return fail(fmt.Errorf("Proxmox connection: %w", err))
	}
	cl, err := p.Proxmox.Client(conn)
	if err != nil {
		return fail(err)
	}
	opts := proxmox.GuestBackupOptions{Node: svc.ProxmoxNode, VMID: svc.ProxmoxVMID, Mode: guest.Mode, Storage: guest.Storage}
	if err := opts.Validate(); err != nil {
		return fail(err)
	}
	if len(targets) > 0 {
		if fi, err := os.Stat(guest.CopyPath); err != nil || !fi.IsDir() {
			return fail(fmt.Errorf("the folder %q where Proxmox's %s storage appears on this machine is not readable, so the dump cannot be copied to a destination", guest.CopyPath, guest.Storage))
		}
	}
	required := []string{guestConfigMethod}
	if len(targets) > 0 {
		required = append(required, readBackMethod)
	}
	m.RequireVerifications(required...)
	for _, rt := range targets {
		rt.repo, rt.err = p.Resolver().Open(rt.dest.Target())
	}
	if len(targets) > 0 && allFailed(targets) {
		return fail(targetsError(targets))
	}

	if err := m.Transition(engine.DUMPING, "dumping"); err != nil {
		return fail(err)
	}
	res, err := cl.CreateDump(ctx, opts, p.pollInterval())
	if err != nil {
		return fail(fmt.Errorf("%s", p.redact(err.Error())))
	}
	if err := inventory.RecordProxmoxDump(p.DB, service, res.Node, res.Storage, res.Volume, m.Run().ID, res.Size); err != nil {
		return fail(err)
	}

	if err := m.Transition(engine.UPLOADING, "uploading"); err != nil {
		return fail(err)
	}
	if len(targets) > 0 {
		file, err := dumpFile(guest.CopyPath, res)
		for _, rt := range targets {
			if err != nil && rt.err == nil {
				rt.err = err
			} else if rt.err == nil {
				rt.paths = []string{file}
			}
		}
		p.forEachTarget(targets, func(rt *runTarget) {
			br, err := p.uploadTarget(ctx, service, rt, &strategy.Result{})
			if err != nil {
				rt.err = err
				return
			}
			rt.snapshotID = br.SnapshotID
		})
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		for _, rt := range targets {
			if rt.snapshotID != "" {
				if err := m.SetSnapshotID(rt.snapshotID); err != nil {
					return fail(err)
				}
				break
			}
		}
	}

	if err := m.Transition(engine.VERIFYING, "verifying"); err != nil {
		return fail(err)
	}
	layer := cl.VerifyGuestConfig(ctx, res)
	rec := engine.VerificationRecord{Method: guestConfigMethod, Passed: layer.Passed, Detail: p.redact(layer.Detail)}
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

	// Old dumps go only once this one is proven restorable, and only
	// ones Ballast made. A failure here is reported but never changes
	// the outcome of the backup itself.
	if layer.Passed {
		defer p.pruneGuestDumps(ctx, cl, svc, guest)
	}

	if !layer.Passed {
		return fail(fmt.Errorf("the Proxmox backup was made but could not be proven restorable: %s", layer.Detail))
	}
	if anyTargetFailed(targets) {
		return fail(fmt.Errorf("the Proxmox backup is verified, but copying it failed: %w", targetsError(targets)))
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

// pruneGuestDumps applies keep-last to the dumps this service created.
func (p *Pipeline) pruneGuestDumps(ctx context.Context, cl *proxmox.Client, svc inventory.Service, guest inventory.GuestJob) {
	if guest.KeepLast <= 0 {
		return
	}
	owned, err := inventory.OwnedProxmoxDumps(p.DB, svc.Name, svc.ProxmoxNode, guest.Storage)
	if err != nil {
		p.logf("%s: %v", svc.Name, err)
		return
	}
	pruned, gone, err := cl.PruneOwned(ctx, svc.ProxmoxNode, guest.Storage, svc.ProxmoxVMID, owned, guest.KeepLast, p.pollInterval())
	if mErr := inventory.MarkProxmoxDumpsPruned(p.DB, svc.ProxmoxNode, append(pruned, gone...)); mErr != nil {
		p.logf("%s: %v", svc.Name, mErr)
	}
	if err != nil {
		p.logf("%s: removing old Proxmox backups failed, the backup itself succeeded: %s", svc.Name, p.redact(err.Error()))
		return
	}
	if len(pruned) > 0 {
		p.logf("%s: removed %d old Proxmox backup(s) made by Ballast (keeping the newest %d)", svc.Name, len(pruned), guest.KeepLast)
	}
}

// pollInterval is how often a running Proxmox task is checked.
func (p *Pipeline) pollInterval() time.Duration {
	if p.ProxmoxPoll > 0 {
		return p.ProxmoxPoll
	}
	return proxmox.DefaultPollInterval
}

// dumpFile finds the new dump's file under dir (a folder where the
// dump storage appears on this machine): directly in it, or in its
// dump/ subfolder like a Proxmox directory storage. Its size must
// match what Proxmox reported.
func dumpFile(dir string, res proxmox.GuestBackupResult) (string, error) {
	_, name, ok := strings.Cut(res.Volume, "/")
	if !ok {
		name = res.Volume
	}
	name = filepath.Base(name)
	for _, c := range []string{filepath.Join(dir, name), filepath.Join(dir, "dump", name)} {
		fi, err := os.Stat(c)
		if err != nil {
			continue
		}
		if res.Size > 0 && fi.Size() != res.Size {
			return "", fmt.Errorf("the dump file %s is %d bytes but Proxmox reported %d; it may still be syncing", c, fi.Size(), res.Size)
		}
		return c, nil
	}
	return "", fmt.Errorf("the dump file %s was not found under %s (looked in it and in its dump/ folder); check the folder the storage appears in", name, dir)
}
