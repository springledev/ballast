package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/strategy"
)

// runTarget is one destination's working state during a run (spec
// §19.4). Each target uploads, verifies and reports on its own.
type runTarget struct {
	target inventory.Target
	dest   inventory.Destination
	repo   *restic.Repository

	// paths and artifacts are the parts of the run this target gets.
	paths     []string
	artifacts []strategy.Artifact

	snapshotID string
	checks     []verifyCheck
	// err is why the target failed before it could be verified
	// (destination unreadable, upload failed, parts select nothing).
	err    error
	state  engine.State
	detail string
}

// selectParts picks what one target receives out of everything the run
// produced: all of it when the target names no parts, otherwise only
// the named dumps/archives and folder groups. Naming a part the
// service does not have is an error that lists the ones it has.
func selectParts(res *strategy.Result, t inventory.Target) ([]string, []strategy.Artifact, error) {
	partOf := func(path string) string {
		if n, ok := res.PathParts[path]; ok {
			return n
		}
		return strategy.DefaultPathPart
	}
	if len(t.Parts) > 0 {
		known := map[string]bool{}
		for _, a := range res.Artifacts {
			known[a.Name] = true
		}
		for _, path := range res.Paths {
			known[partOf(path)] = true
		}
		for _, want := range t.Parts {
			if !known[want] {
				var have []string
				for k := range known {
					have = append(have, k)
				}
				sort.Strings(have)
				return nil, nil, fmt.Errorf("destination %s: this service has no part called %q (it has: %s)", t.Destination, want, strings.Join(have, ", "))
			}
		}
	}
	var paths []string
	for _, path := range res.Paths {
		if t.Wants(partOf(path)) {
			paths = append(paths, path)
		}
	}
	var arts []strategy.Artifact
	for _, a := range res.Artifacts {
		if t.Wants(a.Name) {
			arts = append(arts, a)
		}
	}
	if len(paths) == 0 && len(arts) == 0 {
		return nil, nil, fmt.Errorf("destination %s: the parts chosen for it (%s) leave nothing to send", t.Destination, strings.Join(t.Parts, ", "))
	}
	return paths, arts, nil
}

// forEachTarget runs fn for every target that has not already failed,
// at most p.TargetParallel at a time (default one).
func (p *Pipeline) forEachTarget(targets []*runTarget, fn func(*runTarget)) {
	par := p.TargetParallel
	if par < 1 {
		par = 1
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for _, rt := range targets {
		if rt.err != nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(rt *runTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(rt)
		}(rt)
	}
	wg.Wait()
}

// uploadTarget sends one target's parts to its repository, logging
// progress so a first upload of many gigabytes is visibly alive.
func (p *Pipeline) uploadTarget(ctx context.Context, service string, rt *runTarget, res *strategy.Result) (*restic.BackupResult, error) {
	// Uploading holds the repository's shared lock, so another
	// service's prune never runs under it (see repoLocks).
	shared := repoLock(rt.repo.Location)
	shared.RLock()
	defer shared.RUnlock()
	var last time.Time
	br, err := rt.repo.Backup(ctx, restic.BackupOptions{
		Paths:    append(append([]string(nil), rt.paths...), artifactPaths(rt.artifacts)...),
		Excludes: res.Excludes,
		Tags:     []string{"ballast", serviceTag(service)},
		Progress: func(bp restic.BackupProgress) {
			if time.Since(last) < progressEvery || bp.TotalBytes == 0 {
				return
			}
			last = time.Now()
			msg := fmt.Sprintf("%s: uploading to %s: %.0f%% (%s of %s)", service, rt.dest.Name, bp.PercentDone*100, humanBytes(bp.BytesDone), humanBytes(bp.TotalBytes))
			if bp.SecondsRemaining > 0 {
				msg += fmt.Sprintf(", about %s left", (time.Duration(bp.SecondsRemaining) * time.Second).Round(time.Minute))
			}
			p.logf("%s", msg)
		},
	})
	if err != nil {
		return nil, p.targetErr(rt, err)
	}
	return br, nil
}

// progressEvery is how often an upload's progress is logged.
const progressEvery = 30 * time.Second

// humanBytes renders a byte count for a log line.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// targetErr names the destination in an error when the run has more
// than one, so a notification says which copy failed.
func (p *Pipeline) targetErr(rt *runTarget, err error) error {
	return &targetError{dest: rt.dest.Name, err: err}
}

// targetError is a failure of one destination's copy.
type targetError struct {
	dest string
	err  error
}

func (e *targetError) Error() string { return e.err.Error() }
func (e *targetError) Unwrap() error { return e.err }

func allFailed(targets []*runTarget) bool {
	for _, rt := range targets {
		if rt.err == nil {
			return false
		}
	}
	return true
}

func anyTargetFailed(targets []*runTarget) bool {
	for _, rt := range targets {
		if rt.state == engine.FAILED {
			return true
		}
	}
	return false
}

func anyTargetUnverified(targets []*runTarget) bool {
	for _, rt := range targets {
		if rt.state == engine.SUCCESS_UNVERIFIED {
			return true
		}
	}
	return false
}

// targetsError describes the failed targets. With one target it is that
// target's own error, unchanged; with several it names each destination
// and says which copies did succeed.
func targetsError(targets []*runTarget) error {
	if len(targets) == 1 {
		rt := targets[0]
		if rt.err != nil {
			return rt.err
		}
		return fmt.Errorf("%s", rt.detail)
	}
	var failed, ok []string
	for _, rt := range targets {
		msg := rt.detail
		if rt.err != nil {
			msg = rt.err.Error()
		}
		if rt.state == engine.FAILED || rt.err != nil {
			failed = append(failed, fmt.Sprintf("%s: %s", rt.dest.Name, msg))
		} else {
			ok = append(ok, rt.dest.Name)
		}
	}
	out := "backup failed for " + strings.Join(failed, "; ")
	if len(ok) > 0 {
		out += " (the copy to " + strings.Join(ok, ", ") + " is fine)"
	}
	return fmt.Errorf("%s", out)
}

// targetsSummary lists the details of the targets in the given state,
// naming each destination when there is more than one.
func targetsSummary(targets []*runTarget, state engine.State) string {
	var parts []string
	for _, rt := range targets {
		if rt.state != state {
			continue
		}
		if len(targets) == 1 {
			parts = append(parts, rt.detail)
		} else {
			parts = append(parts, rt.dest.Name+": "+rt.detail)
		}
	}
	return strings.Join(parts, "; ")
}

// retainTargets applies each verified target's own retention policy.
func (p *Pipeline) retainTargets(ctx context.Context, service string, targets []*runTarget) {
	for _, rt := range targets {
		if rt.state == engine.SUCCESS {
			p.applyRetention(ctx, service, rt.repo, rt.target.EffectiveRetention(rt.dest))
		}
	}
}
