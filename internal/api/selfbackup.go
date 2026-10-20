package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/scheduler"
)

// SelfBackupTag marks snapshots of Ballast's own state.
const SelfBackupTag = "ballast-self"

// SelfBackupSources names Ballast's own files, beyond the status DB.
type SelfBackupSources struct {
	// ConfigFile is ballast.yaml. Skipped when empty or missing.
	ConfigFile string
	// CatalogDirs are the user catalog directories.
	CatalogDirs []string
}

// SelfBackupResult reports one self-backup.
type SelfBackupResult struct {
	Destination string `json:"destination"`
	SnapshotID  string `json:"snapshot_id"`
	// Files lists what the snapshot holds, relative to its root.
	Files []string `json:"files"`
	// Verified is true only when the snapshot was read back from the
	// repository (spec rule 1); a self-backup is never reported good
	// without it.
	Verified bool `json:"verified"`
}

// DefaultSelfBackupCron is when Ballast's own settings are backed up
// unless the schedule is changed: daily at 04:30, after the usual
// nightly batch.
const DefaultSelfBackupCron = "30 4 * * *"

// SelfBackupService is the reserved service name Ballast's own settings
// backup is scheduled, run, shown and alerted on under.
const SelfBackupService = inventory.SelfBackupService

// selfReadBackMethod is the verification method recorded for each
// destination's read-back of a self-backup snapshot.
const selfReadBackMethod = "restic-read-back"

// selfStaging is the private folder a self-backup is assembled in.
type selfStaging struct {
	dir   string
	files []string
}

func (s *selfStaging) remove() { _ = os.RemoveAll(s.dir) }

// stageSelf assembles what a self-backup saves in a clean private
// folder: a consistent copy of the status DB (VACUUM INTO), the config
// file and the user catalog entries. Credential files are left out on
// purpose (spec rule 2). The caller removes the staging folder.
func (o *Ops) stageSelf(ctx context.Context) (*selfStaging, error) {
	if o.WorkRoot == "" {
		return nil, fmt.Errorf("self-backup: no work directory is configured (internal wiring error)")
	}
	stage := filepath.Join(o.WorkRoot, "_self")
	if err := os.RemoveAll(stage); err != nil {
		return nil, fmt.Errorf("self-backup: clearing %s: %w", stage, err)
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return nil, fmt.Errorf("self-backup: creating %s: %w", stage, err)
	}
	st := &selfStaging{dir: stage}
	fail := func(err error) (*selfStaging, error) {
		st.remove()
		return nil, err
	}

	dbCopy := filepath.Join(stage, "ballast.db")
	if _, err := o.DB.ExecContext(ctx, "VACUUM INTO ?", dbCopy); err != nil {
		return fail(fmt.Errorf("self-backup: copying the status database: %w", err))
	}
	if err := os.Chmod(dbCopy, 0o600); err != nil {
		return fail(fmt.Errorf("self-backup: %w", err))
	}
	if cf := o.SelfBackupFiles.ConfigFile; cf != "" {
		if err := copyFile(cf, filepath.Join(stage, "ballast.yaml")); err != nil && !os.IsNotExist(err) {
			return fail(fmt.Errorf("self-backup: copying %s: %w", cf, err))
		}
	}
	for i, dir := range o.SelfBackupFiles.CatalogDirs {
		if err := copyTree(dir, filepath.Join(stage, "catalog.d", fmt.Sprint(i))); err != nil && !os.IsNotExist(err) {
			return fail(fmt.Errorf("self-backup: copying catalog %s: %w", dir, err))
		}
	}
	err := filepath.WalkDir(stage, func(p string, d fs.DirEntry, werr error) error {
		if werr == nil && !d.IsDir() {
			rel, _ := filepath.Rel(stage, p)
			st.files = append(st.files, rel)
		}
		return werr
	})
	if err != nil {
		return fail(fmt.Errorf("self-backup: %w", err))
	}
	return st, nil
}

// selfUpload saves the staged files to repo as one snapshot carrying
// the self-backup tag.
func (o *Ops) selfUpload(ctx context.Context, repo *restic.Repository, st *selfStaging) (string, error) {
	release, err := o.holdRepo(ctx, repo.Location)
	if err != nil {
		return "", err
	}
	defer release()
	br, err := repo.Backup(ctx, restic.BackupOptions{Paths: []string{st.dir}, Tags: []string{SelfBackupTag}, Host: "ballast-self"})
	if err != nil {
		return "", err
	}
	return br.SnapshotID, nil
}

// selfReadBack reads snapshot id back from repo (spec rule 1).
func (o *Ops) selfReadBack(ctx context.Context, repo *restic.Repository, id string) error {
	release, err := o.holdRepo(ctx, repo.Location)
	if err != nil {
		return err
	}
	defer release()
	_, err = repo.ReadBack(ctx, id)
	return err
}

// retainSelf applies the destination's retention policy to the
// self-backup snapshots in repo, and only to those: the forget is
// scoped to the self-backup tag, so no service's snapshots in a shared
// repository are touched. Call it only after a verified self-backup to
// that destination. It never changes an outcome; failures are logged.
func (o *Ops) retainSelf(ctx context.Context, dest inventory.Destination, repo *restic.Repository) {
	policy := inventory.Retention{KeepLast: dest.KeepLast, KeepDaily: dest.KeepDaily, KeepWeekly: dest.KeepWeekly, KeepMonthly: dest.KeepMonthly}
	retain := o.RetainTagged
	if retain == nil {
		retain = retainTaggedDirect
	}
	retain(ctx, o.logf, o.redact, SelfBackupService, dest.Name, repo, SelfBackupTag, policy)
}

// holdRepo takes the shared side of the repository gate when one is
// wired (see Ops.HoldRepository).
func (o *Ops) holdRepo(ctx context.Context, location string) (func(), error) {
	if o.HoldRepository == nil {
		return func() {}, nil
	}
	return o.HoldRepository(ctx, SelfBackupService, location)
}

// RepoHolder takes a repository's shared gate for label and returns the
// release func.
type RepoHolder func(ctx context.Context, label, location string) (func(), error)

// TagRetainer forgets and prunes, in repo, the snapshots tagged tag that
// policy no longer keeps, logging (never returning) failures.
type TagRetainer func(ctx context.Context, logf func(string, ...any), redact func(string) string, label, dest string, repo *restic.Repository, tag string, policy inventory.Retention)

// retainGroupBy keeps one retention series per tag set.
const retainGroupBy = "tags"

// retainTaggedDirect is the ungated TagRetainer used when none is wired
// (tests): wiring passes pipeline.RetainTagged, which also waits for
// uploads of other services to the same repository.
func retainTaggedDirect(ctx context.Context, logf func(string, ...any), redact func(string) string, label, dest string, repo *restic.Repository, tag string, policy inventory.Retention) {
	res, err := repo.Forget(ctx, policy.Policy(), restic.ForgetOptions{Tags: []string{tag}, GroupBy: retainGroupBy})
	if err != nil {
		logf("%s: retention cleanup (forget) failed, the backup itself succeeded: %s", label, redact(err.Error()))
		return
	}
	if len(res.Removed) == 0 {
		return
	}
	if err := repo.Prune(ctx); err != nil {
		logf("%s: removed %d old backup(s) but reclaiming their space (prune) failed, the backup itself succeeded: %s", label, len(res.Removed), redact(err.Error()))
	}
}

// logf writes one plain-English line to Log (stderr when unset).
func (o *Ops) logf(format string, args ...any) {
	w := o.Log
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintln(w, o.redact(fmt.Sprintf("ballast: "+format, args...)))
}

// SelfBackup saves Ballast's own status DB (a consistent copy made with
// SQLite's VACUUM INTO), its config file and its user catalog entries to
// a destination, then reads the snapshot back. Credential files are
// deliberately left out: they are secrets (spec rule 2) and the operator
// provisions them; the database holds none. A snapshot that cannot be
// read back is returned as an error. After a verified self-backup, older
// self-backup snapshots in that destination are removed by its
// retention policy. This is the manual, single-destination form; the
// scheduled form is RunSelfBackup.
func (o *Ops) SelfBackup(ctx context.Context, destination string) (SelfBackupResult, error) {
	res := SelfBackupResult{Destination: destination}
	if o.WorkRoot == "" {
		return res, fmt.Errorf("self-backup: no work directory is configured (internal wiring error)")
	}
	dest, err := inventory.GetDestination(o.DB, destination)
	if err != nil {
		return res, wrapf("destination "+destination, err)
	}
	repo, err := o.repoFor(dest)
	if err != nil {
		return res, wrapf("self-backup to "+destination, err)
	}
	st, err := o.stageSelf(ctx)
	if err != nil {
		return res, err
	}
	defer st.remove()
	res.Files = st.files

	id, err := o.selfUpload(ctx, repo, st)
	if err != nil {
		return res, o.redactErr(fmt.Errorf("self-backup to %s: %w", destination, err))
	}
	res.SnapshotID = id
	if err := o.selfReadBack(ctx, repo, id); err != nil {
		return res, o.redactErr(fmt.Errorf("self-backup to %s: saved as snapshot %s but it could not be read back, so it is NOT verified: %w", destination, id, err))
	}
	res.Verified = true
	if err := o.recordSelfBackup(ctx, time.Now()); err != nil {
		return res, fmt.Errorf("self-backup to %s: %w", destination, err)
	}
	o.retainSelf(ctx, dest, repo)
	return res, nil
}

// selfTarget is one destination's part of a scheduled self-backup.
type selfTarget struct {
	dest     inventory.Destination
	repo     *restic.Repository
	snapshot string
	err      error
	verified bool
}

// selfDestinations lists every destination a self-backup goes to: all
// restic repositories (a Proton Drive copy is not one).
func (o *Ops) selfDestinations() ([]inventory.Destination, error) {
	all, err := inventory.ListDestinations(o.DB)
	if err != nil {
		return nil, err
	}
	var out []inventory.Destination
	for _, d := range all {
		if d.Type.IsResticRepository() {
			out = append(out, d)
		}
	}
	return out, nil
}

// selfService describes the self-backup as a service for the status
// view: one target per destination it goes to.
func (o *Ops) selfService() (inventory.Service, error) {
	dests, err := o.selfDestinations()
	if err != nil {
		return inventory.Service{}, err
	}
	svc := inventory.Service{Name: SelfBackupService, CatalogEntry: "ballast-self"}
	for i, d := range dests {
		if i == 0 {
			svc.Destination = d.Name
		}
		svc.Targets = append(svc.Targets, inventory.Target{Destination: d.Name})
	}
	return svc, nil
}

// RunSelfBackup is the scheduled self-backup. It runs under the
// reserved service name SelfBackupService through the same state
// machine as every service, so it has its own light, its own freshness
// alert and its own failure notification, and can never change another
// service's. It saves Ballast's settings to every destination
// (PREPARING, DUMPING, UPLOADING), reads each snapshot back (VERIFYING)
// and is SUCCESS only when every destination verified; a destination
// that fails is reported on its own and does not stop the others.
// Retention (scoped to the self-backup tag) runs afterwards, and only
// for destinations whose self-backup verified. It satisfies
// scheduler.Runner's contract for the reserved name.
func (o *Ops) RunSelfBackup(ctx context.Context) (engine.State, error) {
	const svc = SelfBackupService
	if o.WorkRoot == "" {
		return "", fmt.Errorf("%s: no work directory is configured (internal wiring error)", svc)
	}
	dests, err := o.selfDestinations()
	if err != nil {
		return "", fmt.Errorf("%s: %w", svc, err)
	}
	m, err := engine.StartRun(o.DB, svc)
	if err != nil {
		return "", fmt.Errorf("%s: %w", svc, err)
	}
	if o.SelfNotifier != nil {
		m.SetNotifier(ctx, o.SelfNotifier)
	}
	var targets []*selfTarget
	// Retention runs once the outcome is recorded, whatever it was,
	// but only for destinations that verified.
	defer func() {
		for _, t := range targets {
			if t.verified {
				o.retainSelf(ctx, t.dest, t.repo)
			}
		}
	}()

	fail := func(cause error) (engine.State, error) {
		if ctx.Err() != nil {
			if terr := m.Transition(engine.CANCELLED, "cancelled: Ballast is shutting down"); terr != nil {
				o.logf("%s: recording cancellation: %v", svc, terr)
			}
			return engine.CANCELLED, fmt.Errorf("%s: %w", svc, ctx.Err())
		}
		err := o.redactErr(fmt.Errorf("%s: %w", svc, cause))
		if terr := m.Transition(engine.FAILED, err.Error()); terr != nil {
			o.logf("%s: recording failure: %v", svc, terr)
		}
		o.logf("%s: run %d failed: %s", svc, m.Run().ID, err)
		return engine.FAILED, err
	}
	failures := func() error {
		var parts []string
		for _, t := range targets {
			if !t.verified {
				parts = append(parts, fmt.Sprintf("%s: %v", t.dest.Name, t.err))
			}
		}
		return errors.New(strings.Join(parts, "; "))
	}

	if err := m.Transition(engine.PREPARING, "preparing"); err != nil {
		return fail(err)
	}
	if len(dests) == 0 {
		return fail(errors.New("there is no destination to back Ballast's settings up to; add a destination first"))
	}
	m.RequireVerifications(selfReadBackMethod)
	for _, d := range dests {
		t := &selfTarget{dest: d}
		t.repo, t.err = o.repoFor(d)
		targets = append(targets, t)
	}

	if err := m.Transition(engine.DUMPING, "copying Ballast's settings"); err != nil {
		return fail(err)
	}
	st, err := o.stageSelf(ctx)
	if err != nil {
		return fail(err)
	}
	defer st.remove()

	if err := m.Transition(engine.UPLOADING, "uploading"); err != nil {
		return fail(err)
	}
	uploaded := 0
	for _, t := range targets {
		if t.err != nil {
			continue
		}
		t.snapshot, t.err = o.selfUpload(ctx, t.repo, st)
		if t.err == nil {
			uploaded++
		}
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	if uploaded == 0 {
		return fail(failures())
	}
	for _, t := range targets {
		if t.snapshot != "" {
			if err := m.SetSnapshotID(t.snapshot); err != nil {
				return fail(err)
			}
			break
		}
	}

	if err := m.Transition(engine.VERIFYING, "verifying"); err != nil {
		return fail(err)
	}
	for _, t := range targets {
		if t.err != nil {
			continue
		}
		rbErr := o.selfReadBack(ctx, t.repo, t.snapshot)
		rec := engine.VerificationRecord{Method: selfReadBackMethod, Target: t.dest.Name, Passed: rbErr == nil, Detail: "the snapshot was read back from the repository"}
		if rbErr != nil {
			t.err = fmt.Errorf("saved as snapshot %s but it could not be read back, so it is NOT verified: %w", t.snapshot, rbErr)
			rec.Detail = o.redact(t.err.Error())
		} else {
			t.verified = true
		}
		if err := m.RecordVerification(rec); err != nil {
			return fail(err)
		}
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	for _, t := range targets {
		tr := engine.TargetResult{Destination: t.dest.Name, SnapshotID: t.snapshot}
		if t.verified {
			tr.State, tr.Detail = engine.SUCCESS, "copy verified"
		} else {
			tr.State, tr.Detail = engine.FAILED, o.redact(t.err.Error())
		}
		if err := m.RecordTarget(tr); err != nil {
			return fail(err)
		}
	}
	for _, t := range targets {
		if !t.verified {
			return fail(failures())
		}
	}
	if err := m.Transition(engine.SUCCESS, "backup verified"); err != nil {
		return fail(err)
	}
	if err := o.recordSelfBackup(ctx, time.Now()); err != nil {
		o.logf("%s: %v", svc, err)
	}
	o.logf("%s: run %d succeeded and is verified", svc, m.Run().ID)
	return engine.SUCCESS, nil
}

// EnsureSelfBackupSchedule gives Ballast's own settings backup its
// default schedule (DefaultSelfBackupCron) the first time a destination
// exists. It is idempotent and respects an opt-out: once a schedule row
// exists, enabled or disabled, it is never recreated or changed. Creating
// it never runs it by surprise: its last firing is set to now, so the
// first backup happens at the next scheduled time, or when the operator
// runs it ("ballast backup ballast-self").
func (o *Ops) EnsureSelfBackupSchedule(ctx context.Context, now time.Time) error {
	jobs, err := scheduler.ListSchedules(o.DB)
	if err != nil {
		return fmt.Errorf("self-backup schedule: %w", err)
	}
	for _, j := range jobs {
		if j.Service == SelfBackupService {
			return nil
		}
	}
	dests, err := o.selfDestinations()
	if err != nil {
		return fmt.Errorf("self-backup schedule: %w", err)
	}
	if len(dests) == 0 {
		return nil
	}
	if err := scheduler.SetSchedule(o.DB, SelfBackupService, DefaultSelfBackupCron); err != nil {
		return err
	}
	return scheduler.MarkFired(o.DB, SelfBackupService, now)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTree copies regular files under src to dst, skipping anything
// else (symlinks, sockets).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("unexpected path %s", p)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}
