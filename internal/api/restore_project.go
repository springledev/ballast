package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/strategy"
)

// projectTier orders a project's services for restore (spec §14 step
// 23: "compose config, volumes, and DB dumps ... in dependency order")
// — a stack's config should exist before its volumes are populated,
// and its volumes before a database dump is restored into a database
// that expects them. It is presentation/sequencing only: each
// service's restore below is independent, so one tier's ordering
// never depends on another tier having actually succeeded.
const (
	tierCompose = iota
	tierVolume
	tierDump
	tierOther
)

// serviceTier classifies entry's backup strategy into a restore tier.
// It reads only the catalog entry's static shape (never resolves
// templates), so it works without the service's Docker host or
// discovered container names being reachable — restoring a stack is
// exactly the situation where its containers may be down.
func serviceTier(entry *catalog.Entry) int {
	switch {
	case entry.Backup.Compose != nil:
		return tierCompose
	case len(entry.Backup.Volumes) > 0:
		return tierVolume
	case len(entry.Backup.Dumps) > 0:
		return tierDump
	default:
		return tierOther
	}
}

// tierName is the human-readable label for a RestoreProjectResult.Kind.
func tierName(tier int) string {
	switch tier {
	case tierCompose:
		return "compose-config"
	case tierVolume:
		return "volume"
	case tierDump:
		return "dump"
	default:
		return "other"
	}
}

// RestoreProjectRequest is the input to RestoreProject.
type RestoreProjectRequest struct {
	Project string
	// AsOf restores each service's newest snapshot taken at or before
	// this time; nil means each service's latest snapshot. Services in
	// one project commonly back up on independent schedules, so "the
	// same instant" rarely exists — this is "as it stood by this
	// point," not a single shared snapshot.
	AsOf *time.Time
	// From restores every service from this destination instead of its
	// main one. A service that does not use it is reported as failed
	// (the others still restore).
	From string
	// To is the directory config/dump files are restored into, one
	// subdirectory per service (<To>/<service>/...). Docker volumes are
	// restored straight into fresh Docker volumes instead (see
	// RestoreProjectResult.Volumes) and are not affected by To beyond
	// also landing their raw tar archive there for reference.
	To string
}

// RestoreProjectResult is one service's outcome within a full-stack
// restore.
type RestoreProjectResult struct {
	Service    string `json:"service"`
	Kind       string `json:"kind"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	// RestoredTo is the directory this service's snapshot was restored
	// into, set whenever the restore itself succeeded.
	RestoredTo string `json:"restored_to,omitempty"`
	// Volumes are the newly created Docker volume names a volume-kind
	// service's archives were extracted into (never an existing
	// volume: spec principle "never restore over live data").
	Volumes []string `json:"volumes,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// RestoreProjectReport is the outcome of restoring every service in a
// project.
type RestoreProjectReport struct {
	Project string                 `json:"project"`
	Results []RestoreProjectResult `json:"results"`
}

// RestoreProject restores every service in project — a Docker Compose
// stack's compose-config snapshot, Docker volumes, and DB dump(s),
// added as separate catalog services and tied together by
// inventory.Service.Project — in dependency order (spec §14 step 23):
// compose config first, volumes next, DB dumps last.
//
// Each service's own latest snapshot at or before req.AsOf is restored
// on its own: a snapshot's config files and DB dump artifacts land
// under <To>/<service>/ exactly as Ops.Restore would restore them, and
// a volume-strategy service's volume-*.tar artifacts are additionally
// extracted into freshly created Docker volumes (never an existing
// one — restoring over live data is never done silently). One
// service's failure never stops the rest (spec rule 3): every
// service's own outcome is reported, and the returned error names
// whichever services could not be restored.
func (o *Ops) RestoreProject(ctx context.Context, req RestoreProjectRequest) (RestoreProjectReport, error) {
	rep := RestoreProjectReport{Project: req.Project}

	if req.Project == "" {
		return rep, fmt.Errorf("%w: a project name is required", ErrValidation)
	}
	if req.To == "" {
		return rep, fmt.Errorf("%w: restore requires a target directory (--to)", ErrValidation)
	}
	if !filepath.IsAbs(req.To) {
		return rep, fmt.Errorf("%w: restore target %q must be an absolute path", ErrValidation, req.To)
	}

	svcs, err := inventory.ListServicesByProject(o.DB, req.Project)
	if err != nil {
		return rep, err
	}
	if len(svcs) == 0 {
		return rep, fmt.Errorf("project %s: %w: no services belong to this project", req.Project, ErrValidation)
	}

	type staged struct {
		svc  inventory.Service
		tier int
	}
	plan := make([]staged, 0, len(svcs))
	for _, s := range svcs {
		entry, ok := o.Catalog.Get(s.CatalogEntry)
		if !ok {
			rep.Results = append(rep.Results, RestoreProjectResult{Service: s.Name, Error: fmt.Sprintf("catalog entry %q is not loaded", s.CatalogEntry)})
			continue
		}
		plan = append(plan, staged{svc: s, tier: serviceTier(entry)})
	}
	sort.SliceStable(plan, func(i, j int) bool { return plan[i].tier < plan[j].tier })

	var problems []string
	for _, p := range plan {
		res := o.restoreProjectService(ctx, p.svc, tierName(p.tier), req)
		if res.Error != "" {
			problems = append(problems, p.svc.Name)
		}
		rep.Results = append(rep.Results, res)
	}
	for _, r := range rep.Results {
		if r.Error != "" && !contains(problems, r.Service) {
			problems = append(problems, r.Service)
		}
	}
	if len(problems) > 0 {
		return rep, fmt.Errorf("project %s: %w: could not restore: %s", req.Project, ErrValidation, strings.Join(problems, ", "))
	}
	return rep, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// restoreProjectService restores one service's snapshot at-or-before
// req.AsOf into <req.To>/<service>/, and — for a volume-kind service —
// also extracts every volume-*.tar artifact it finds into a freshly
// created Docker volume.
func (o *Ops) restoreProjectService(ctx context.Context, svc inventory.Service, kind string, req RestoreProjectRequest) RestoreProjectResult {
	res := RestoreProjectResult{Service: svc.Name, Kind: kind}

	dest, err := o.restoreSource(svc, req.From)
	if err != nil {
		res.Error = o.redact(err.Error())
		return res
	}
	repo, err := o.repoFor(dest)
	if err != nil {
		res.Error = o.redact(err.Error())
		return res
	}

	tag := "ballast-service:" + svc.Name
	snap, err := resolveServiceSnapshotAt(ctx, repo, svc.Name, tag, req.AsOf)
	if err != nil {
		res.Error = o.redact(err.Error())
		return res
	}
	res.SnapshotID = snap.ShortID

	target := filepath.Join(req.To, svc.Name)
	if err := repo.Restore(ctx, restic.RestoreOptions{SnapshotID: snap.ID, TargetDir: target}); err != nil {
		res.Error = o.redact(err.Error())
		return res
	}
	res.RestoredTo = target

	if kind != tierName(tierVolume) {
		return res
	}
	volumes, err := o.restoreProjectVolumes(ctx, svc, target)
	if err != nil {
		res.Error = o.redact(err.Error())
		return res
	}
	res.Volumes = volumes
	return res
}

// resolveServiceSnapshotAt resolves the newest snapshot carrying tag
// that was taken at or before asOf (nil asOf means "the latest
// snapshot"), so a full-stack restore can restore each service "as it
// stood by" a chosen point in time even though services in one
// project commonly back up on independent schedules.
func resolveServiceSnapshotAt(ctx context.Context, repo *restic.Repository, service, tag string, asOf *time.Time) (restic.Snapshot, error) {
	if asOf == nil {
		snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{Tags: []string{tag}, Latest: 1})
		if err != nil {
			return restic.Snapshot{}, wrapf("service "+service, err)
		}
		if len(snaps) == 0 {
			return restic.Snapshot{}, fmt.Errorf("service %s: %w: no snapshots found for this service", service, ErrValidation)
		}
		return snaps[0], nil
	}

	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{Tags: []string{tag}})
	if err != nil {
		return restic.Snapshot{}, wrapf("service "+service, err)
	}
	var best *restic.Snapshot
	for i := range snaps {
		s := snaps[i]
		if s.Time.After(*asOf) {
			continue
		}
		if best == nil || s.Time.After(best.Time) {
			best = &s
		}
	}
	if best == nil {
		return restic.Snapshot{}, fmt.Errorf("service %s: %w: no snapshot at or before %s was found for this service", service, ErrValidation, asOf.Format(time.RFC3339))
	}
	return *best, nil
}

// restoreProjectVolumes finds every volume-*.tar artifact already
// restored under restoredDir (by restoreProjectService's whole-snapshot
// restic restore) and extracts each into a freshly created Docker
// volume named "<original>-restore-<random>" on the service's Docker
// host — never an existing volume, so a full-stack restore can never
// silently overwrite live data.
func (o *Ops) restoreProjectVolumes(ctx context.Context, svc inventory.Service, restoredDir string) ([]string, error) {
	docker, err := o.dockerFor(svc.DockerHost)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", svc.Name, err)
	}
	artifacts, err := findVolumeArtifacts(restoredDir)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", svc.Name, err)
	}
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("service %s: no volume archive was found in the restored snapshot", svc.Name)
	}

	opts := strategy.Options{Runner: o.Runner, Docker: docker}
	var created []string
	for _, a := range artifacts {
		target := a.volumeName + "-restore-" + randomSuffix()
		f, err := openArtifact(a.path)
		if err != nil {
			return created, fmt.Errorf("service %s: volume %s: %w", svc.Name, a.volumeName, err)
		}
		restoreErr := strategy.RestoreVolume(ctx, opts, target, f)
		closeErr := f.Close()
		if restoreErr != nil {
			return created, fmt.Errorf("service %s: restoring volume %s into %s: %w", svc.Name, a.volumeName, target, restoreErr)
		}
		if closeErr != nil {
			return created, fmt.Errorf("service %s: volume %s: %w", svc.Name, a.volumeName, closeErr)
		}
		created = append(created, target)
	}
	return created, nil
}

// volumeArtifact is one volume-*.tar file found under a restored
// snapshot, with the original volume name it archived recovered from
// its file name (the same "volume-<name>.tar" naming the volume
// strategy's own Backup gives it).
type volumeArtifact struct {
	path       string
	volumeName string
}

// findVolumeArtifacts walks dir for every file the volume strategy
// could have written (spec §17.5's "volume-<name>.tar" naming),
// wherever restic placed it under the service's original artifact
// path.
func findVolumeArtifacts(dir string) ([]volumeArtifact, error) {
	var out []volumeArtifact
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "volume-") || !strings.HasSuffix(name, ".tar") {
			return nil
		}
		vol := strings.TrimSuffix(strings.TrimPrefix(name, "volume-"), ".tar")
		if vol == "" {
			return nil
		}
		out = append(out, volumeArtifact{path: path, volumeName: vol})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("looking for volume archives under %s: %w", dir, err)
	}
	return out, nil
}

// openArtifact opens a restored artifact file for streaming into
// strategy.RestoreVolume.
func openArtifact(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return f, nil
}

// randomSuffix returns a short random hex string, so a restored
// volume's name can never collide with one from a previous restore of
// the same project.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
