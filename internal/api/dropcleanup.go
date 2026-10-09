package api

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/strategy"
)

// DropSnapshot is one old backup that still holds a part a destination
// no longer receives.
type DropSnapshot struct {
	ID    string    `json:"id"`
	Time  time.Time `json:"time"`
	Holds []string  `json:"holds"`
}

// DropPlan says what removing the old copies of a dropped part from one
// destination would do. Nothing is deleted until DropParts is called.
type DropPlan struct {
	Service     string `json:"service"`
	Destination string `json:"destination"`
	// Parts are the parts the destination no longer receives.
	Parts []string `json:"parts"`
	// Snapshots are exactly the backups that would be removed.
	Snapshots []DropSnapshot `json:"snapshots"`
	// FreedBytes is about how much space removing them frees; it is
	// what no other backup of the service still uses.
	FreedBytes uint64 `json:"freed_bytes"`
	// Blocked is a plain-English reason nothing can be removed yet.
	Blocked string `json:"blocked,omitempty"`
	// Warning explains the side effect of removing whole backups.
	Warning string `json:"warning,omitempty"`
}

// dropRepo is the part of a restic repository the cleanup uses.
type dropRepo interface {
	Snapshots(ctx context.Context, f restic.SnapshotFilter) ([]restic.Snapshot, error)
	RawDataSize(ctx context.Context, ids []string) (uint64, error)
	ForgetSnapshots(ctx context.Context, ids []string) error
	Prune(ctx context.Context) error
}

// DroppedParts lists the parts of allParts that a target received before
// (old) and no longer receives (updated).
func DroppedParts(old, updated inventory.Target, allParts []string) []string {
	var out []string
	for _, p := range allParts {
		if old.Wants(p) && !updated.Wants(p) {
			out = append(out, p)
		}
	}
	return out
}

// partsInSnapshot names which of the service's parts a snapshot holds,
// judged by its paths: a folder part is its folder, a dump or archive is
// the file name it was written under in the run's work folder.
func partsInSnapshot(entry *catalog.Entry, paths []string) map[string]bool {
	folder := map[string]string{}
	for _, f := range entry.Backup.Files {
		n := f.Name
		if n == "" {
			n = strategy.DefaultPathPart
		}
		folder[filepath.Clean(f.Path)] = n
	}
	var names []string
	for _, d := range entry.Backup.Dumps {
		names = append(names, d.Name)
	}
	for _, v := range entry.Backup.Volumes {
		names = append(names, "volume-"+v.Name)
	}
	names = append(names, "data", "exec")
	if entry.Backup.Compose != nil {
		names = append(names, "compose-manifest", "compose-config", "compose-labels")
	}
	held := map[string]bool{}
	for _, p := range paths {
		if n, ok := folder[filepath.Clean(p)]; ok {
			held[n] = true
			continue
		}
		base := filepath.Base(p)
		for _, n := range names {
			if base == n || strings.HasPrefix(base, n+".") {
				held[n] = true
			}
		}
	}
	return held
}

// planDrop works out which snapshots hold a dropped part and how much
// space removing them frees. It refuses when the destination still
// receives the part, and when no backup without the part exists yet
// (removing everything would leave the destination empty).
func planDrop(ctx context.Context, repo dropRepo, service, dest string, target inventory.Target, entry *catalog.Entry, parts []string) (*DropPlan, error) {
	plan := &DropPlan{Service: service, Destination: dest, Parts: parts, Snapshots: []DropSnapshot{}}
	if len(parts) == 0 {
		return nil, fmt.Errorf("service %s: %w: name at least one part to remove from %s", service, ErrValidation, dest)
	}
	for _, p := range parts {
		if target.Wants(p) {
			return nil, fmt.Errorf("service %s: %w: %s still receives the part %q; stop sending it first", service, ErrValidation, dest, p)
		}
	}
	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{Tags: []string{"ballast-service:" + service}})
	if err != nil {
		return nil, fmt.Errorf("service %s: listing backups on %s: %w", service, dest, err)
	}
	dropped := map[string]bool{}
	for _, p := range parts {
		dropped[p] = true
	}
	var remaining []string
	var all []string
	for _, s := range snaps {
		all = append(all, s.ID)
		var holds []string
		for n := range partsInSnapshot(entry, s.Paths) {
			if dropped[n] {
				holds = append(holds, n)
			}
		}
		if len(holds) == 0 {
			remaining = append(remaining, s.ID)
			continue
		}
		sort.Strings(holds)
		plan.Snapshots = append(plan.Snapshots, DropSnapshot{ID: s.ID, Time: s.Time, Holds: holds})
	}
	sort.Slice(plan.Snapshots, func(i, j int) bool { return plan.Snapshots[i].Time.Before(plan.Snapshots[j].Time) })
	if len(plan.Snapshots) == 0 {
		plan.Blocked = fmt.Sprintf("%s has no old backups of %s that include %s, so there is nothing to remove.", dest, service, strings.Join(parts, ", "))
		return plan, nil
	}
	if len(remaining) == 0 {
		plan.Blocked = fmt.Sprintf("Every backup of %s on %s still includes %s. Run a backup first so a newer one without it exists, then clean up.", service, dest, strings.Join(parts, ", "))
		return plan, nil
	}
	plan.Warning = "These are whole backups: the other parts stored in them (for example older database dumps) are removed from " + dest + " too. Newer backups are not touched."
	before, err := repo.RawDataSize(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("service %s: measuring space on %s: %w", service, dest, err)
	}
	after, err := repo.RawDataSize(ctx, remaining)
	if err != nil {
		return nil, fmt.Errorf("service %s: measuring space on %s: %w", service, dest, err)
	}
	if before > after {
		plan.FreedBytes = before - after
	}
	return plan, nil
}

// dropContext loads what a drop needs for service name and destination.
func (o *Ops) dropContext(ctx context.Context, name, dest string) (inventory.Target, *catalog.Entry, dropRepo, error) {
	s, entry, err := o.resolvedEntry(ctx, name)
	if err != nil {
		return inventory.Target{}, nil, nil, err
	}
	var target inventory.Target
	found := false
	for _, t := range s.EffectiveTargets() {
		if t.Destination == dest {
			target, found = t, true
		}
	}
	if !found {
		return inventory.Target{}, nil, nil, fmt.Errorf("service %s: %w: it does not back up to %q", name, ErrValidation, dest)
	}
	d, err := inventory.GetDestination(o.DB, dest)
	if err != nil {
		return inventory.Target{}, nil, nil, wrapf("service "+name, err)
	}
	if !d.Type.IsResticRepository() {
		return inventory.Target{}, nil, nil, fmt.Errorf("service %s: %w: %s is a second copy, not a backup repository; its old copies are kept by its own \"keep newest\" setting", name, ErrValidation, dest)
	}
	repo, err := o.repoFor(d)
	if err != nil {
		return inventory.Target{}, nil, nil, fmt.Errorf("service %s: %w", name, err)
	}
	return target, entry, repo, nil
}

// PreviewDropParts shows which old backups on dest hold the dropped
// parts and how much space removing them would free. It changes nothing.
func (o *Ops) PreviewDropParts(ctx context.Context, name, dest string, parts []string) (*DropPlan, error) {
	target, entry, repo, err := o.dropContext(ctx, name, dest)
	if err != nil {
		return nil, err
	}
	return planDrop(ctx, repo, name, dest, target, entry, parts)
}

// DropParts removes exactly the snapshots ids from dest (each must be in
// the current plan for those parts) and then reclaims the space.
func (o *Ops) DropParts(ctx context.Context, name, dest string, parts, ids []string) (*DropPlan, error) {
	target, entry, repo, err := o.dropContext(ctx, name, dest)
	if err != nil {
		return nil, err
	}
	return dropParts(ctx, repo, name, dest, target, entry, parts, ids)
}

func dropParts(ctx context.Context, repo dropRepo, name, dest string, target inventory.Target, entry *catalog.Entry, parts, ids []string) (*DropPlan, error) {
	plan, err := planDrop(ctx, repo, name, dest, target, entry, parts)
	if err != nil {
		return nil, err
	}
	if plan.Blocked != "" {
		return nil, fmt.Errorf("service %s: %w: %s", name, ErrValidation, plan.Blocked)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("service %s: %w: no backups chosen to remove", name, ErrValidation)
	}
	inPlan := map[string]bool{}
	for _, s := range plan.Snapshots {
		inPlan[s.ID] = true
	}
	for _, id := range ids {
		if !inPlan[id] {
			return nil, fmt.Errorf("service %s: %w: backup %s on %s does not hold the dropped parts (or is gone); preview again", name, ErrValidation, id, dest)
		}
	}
	if err := repo.ForgetSnapshots(ctx, ids); err != nil {
		return nil, fmt.Errorf("service %s: removing old backups from %s: %w", name, dest, err)
	}
	if err := repo.Prune(ctx); err != nil {
		return nil, fmt.Errorf("service %s: removed %d old backup(s) from %s but reclaiming the space failed (it is retried by the next cleanup): %w", name, len(ids), dest, err)
	}
	return plan, nil
}
