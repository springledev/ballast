package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/detect"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/ballast/internal/verify"
)

// VerifyCheck is one verification method's outcome from a re-run
// against an already-uploaded snapshot (spec §7, §14 step 14).
type VerifyCheck struct {
	Method  string `json:"method"`
	Passed  bool   `json:"passed"`
	Warning string `json:"warning,omitempty"`
	Detail  string `json:"detail"`
}

// VerifyReport is the result of re-verifying one service's latest
// snapshot (`ballast verify <service>|--all`, spec §9).
type VerifyReport struct {
	Service    string `json:"service"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	// Light mirrors the traffic-light values Status uses: "green"
	// (every check passed), "amber" (unverifiable — e.g. the
	// destination could not be read back — but nothing actively
	// failed), or "red" (a check failed, or the service could not be
	// verified at all; see Error).
	Light  string        `json:"light"`
	Checks []VerifyCheck `json:"checks,omitempty"`
	// Error is set instead of Checks when the service could not even
	// be verified (no snapshot, misconfigured destination, ...).
	Error string `json:"error,omitempty"`
}

// VerifyService re-verifies the latest snapshot of one configured
// service, standing in for a fresh backup's VERIFYING phase without
// running one: it restores the snapshot's own backup artifacts (files
// the strategy wrote under the service's stable work directory —
// never a files/folder backup's raw source paths, which can be
// arbitrarily large and are not what the catalog's declared checker
// inspects anyway) into a throwaway directory, re-runs the catalog's
// declared verification method against them exactly as the pipeline's
// VERIFYING phase does (spec §7), and re-runs restic's own read-back.
//
// Nothing is written to the database: this is a standalone, ad hoc
// check, not a job run — it never touches internal/engine's state
// machine and is intentionally a thin wrapper over the same
// catalog/restic/verify logic the real backup pipeline already uses
// (spec §14 step 14).
func (o *Ops) VerifyService(ctx context.Context, service string) (VerifyReport, error) {
	rep := VerifyReport{Service: service}

	svc, err := inventory.GetService(o.DB, service)
	if err != nil {
		return rep, wrapf("service "+service, err)
	}
	dest, err := inventory.GetDestination(o.DB, svc.Destination)
	if err != nil {
		return rep, wrapf("service "+service, err)
	}
	entry, ok := o.Catalog.Get(svc.CatalogEntry)
	if !ok {
		return rep, fmt.Errorf("service %s: catalog entry %q is not loaded", service, svc.CatalogEntry)
	}
	discovered, err := detect.ResolveDiscovery(ctx, o.Runner, "", entry.Discovery, entry.ParamsWithDefaults(svc.Params))
	if err != nil {
		return rep, fmt.Errorf("service %s: %w", service, err)
	}
	if err := entry.Resolve(catalog.TemplateContext{Params: svc.Params, Services: svc.Containers, Discovery: discovered}); err != nil {
		return rep, fmt.Errorf("service %s: resolving catalog entry %q: %w", service, svc.CatalogEntry, err)
	}
	repo, err := o.repoFor(dest)
	if err != nil {
		return rep, wrapf("service "+service, err)
	}

	tag := "ballast-service:" + service
	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{Tags: []string{tag}, Latest: 1})
	if err != nil {
		return rep, o.redactErr(wrapf("service "+service, err))
	}
	if len(snaps) == 0 {
		return rep, fmt.Errorf("service %s: %w: no snapshots found for this service yet", service, ErrValidation)
	}
	snap := snaps[0]
	rep.SnapshotID = snap.ShortID

	checker, err := verify.For(entry, verify.Options{Runner: o.Runner})
	if err != nil {
		return rep, wrapf("service "+service, err)
	}

	// Only paths under this service's own stable work directory are
	// the strategy's produced artifacts (dumps, tar archives); every
	// other path in the snapshot is a raw source path (e.g. a media
	// library) that the declared checker never inspects and that
	// would be far too expensive to restore just to re-verify.
	var workPrefix string
	if o.WorkRoot != "" {
		workPrefix = filepath.Join(o.WorkRoot, service) + string(filepath.Separator)
	}
	var artifactPaths []string
	for _, p := range snap.Paths {
		if workPrefix != "" && strings.HasPrefix(p, workPrefix) {
			artifactPaths = append(artifactPaths, p)
		}
	}

	restoreDir, err := os.MkdirTemp("", "ballast-verify-*")
	if err != nil {
		return rep, fmt.Errorf("service %s: creating a verification directory: %w", service, err)
	}
	defer os.RemoveAll(restoreDir)

	var artifacts []strategy.Artifact
	if len(artifactPaths) > 0 {
		if err := repo.Restore(ctx, restic.RestoreOptions{SnapshotID: snap.ID, TargetDir: restoreDir, Paths: artifactPaths}); err != nil {
			return rep, o.redactErr(wrapf("service "+service, err))
		}
		for _, p := range artifactPaths {
			ext := filepath.Ext(p)
			artifacts = append(artifacts, strategy.Artifact{
				Name:   strings.TrimSuffix(filepath.Base(p), ext),
				Path:   filepath.Join(restoreDir, p),
				Format: strings.TrimPrefix(ext, "."),
			})
		}
	}

	var sourceContainer string
	if len(entry.Backup.Dumps) > 0 {
		sourceContainer = entry.Backup.Dumps[0].Container
	}

	checks, unverifiable, failed := o.runVerifyChecks(ctx, service, checker, artifacts, sourceContainer, repo, snap.ID)
	if ctx.Err() != nil {
		return rep, ctx.Err()
	}
	rep.Checks = checks
	switch {
	case failed:
		rep.Light = "red"
	case unverifiable:
		rep.Light = "amber"
	default:
		rep.Light = "green"
	}
	return rep, nil
}

// runVerifyChecks runs the declared checker (unless this is an
// archive-integrity check with no artifacts — folded into the
// read-back result instead, mirroring pipeline.runChecks's step 11
// user decision) and restic's own read-back, redacting every result
// (spec rule 2, last line of defence before it reaches an API
// response or CLI output).
func (o *Ops) runVerifyChecks(ctx context.Context, service string, checker verify.Checker, artifacts []strategy.Artifact, sourceContainer string, repo *restic.Repository, snapshotID string) (checks []VerifyCheck, unverifiable, failed bool) {
	runDeclaredChecker := len(artifacts) > 0 || checker.Method() != "archive-integrity"
	if runDeclaredChecker {
		vres := checker.Check(ctx, verify.Request{Service: service, Artifacts: artifacts, SourceContainer: sourceContainer})
		c := VerifyCheck{Method: vres.Method, Passed: vres.Passed, Warning: o.redact(vres.Warning), Detail: o.redact(vres.Detail)}
		checks = append(checks, c)
		if !c.Passed {
			failed = true
		}
	}

	rb, rbErr := repo.ReadBack(ctx, snapshotID)
	var readBackPassed bool
	if rbErr != nil {
		checks = append(checks, VerifyCheck{
			Method: "restic-read-back",
			Passed: false,
			Detail: o.redact(fmt.Sprintf("could not read the backup back from the destination to confirm it is restorable: %v", rbErr)),
		})
		unverifiable = true
	} else {
		readBackPassed = true
		checks = append(checks, VerifyCheck{
			Method: "restic-read-back",
			Passed: true,
			Detail: fmt.Sprintf("read %d sample file(s) back from snapshot %s to confirm the backup is restorable", len(rb.Samples), rb.Snapshot.ShortID),
		})
	}

	if len(artifacts) == 0 && checker.Method() == "archive-integrity" {
		checks = append(checks, VerifyCheck{
			Method: "archive-integrity",
			Passed: readBackPassed,
			Detail: "No archive files were produced (files are uploaded directly); integrity was proven by reading the files back from the backup.",
		})
	}
	return checks, unverifiable, failed
}

// VerifyAllServices re-verifies every configured service's latest
// snapshot, isolating each service's failure from the others (spec
// rule 3): one service that cannot be verified never stops the rest
// from being checked, and the returned error names every service that
// did not come back green.
func (o *Ops) VerifyAllServices(ctx context.Context) ([]VerifyReport, error) {
	svcs, err := inventory.ListServices(o.DB)
	if err != nil {
		return nil, err
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })

	reports := make([]VerifyReport, 0, len(svcs))
	var problems []string
	for _, s := range svcs {
		rep, err := o.VerifyService(ctx, s.Name)
		if err != nil {
			rep = VerifyReport{Service: s.Name, Light: "red", Error: o.redact(err.Error())}
		}
		if rep.Light != "green" {
			problems = append(problems, s.Name)
		}
		reports = append(reports, rep)
	}
	if len(problems) > 0 {
		return reports, fmt.Errorf("verification found problems for: %s", strings.Join(problems, ", "))
	}
	return reports, nil
}
