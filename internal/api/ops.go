// Package api is the shared operations layer behind both the REST API
// and the CLI (spec §9: "All operations are also full REST API
// endpoints ... the GUI uses exactly this API [and so does the CLI] —
// no privileged GUI-only paths"). Ops methods are called in-process by
// cobra commands and by the HTTP handlers in this package; no backup
// logic lives in either caller.
package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/springledev/ballast/internal/progress"
	"github.com/springledev/keel/dockerhost"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/springledev/ballast/internal/backend"
	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/detect"
	"github.com/springledev/ballast/internal/dotenv"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/protondrive"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/springledev/ballast/internal/repohealth"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"
)

// Ops is the shared operations layer. Every exported method wraps its
// errors with service/destination context (AGENTS.md: "always include
// the service/job context") and returns the same sentinel errors the
// underlying packages define (inventory.ErrNotFound, inventory.ErrInUse,
// scheduler.ErrAlreadyRunning, scheduler.ErrPreviousRunInterrupted),
// so callers can map them with errors.Is instead of string matching.
type Ops struct {
	// ListenAddr is where `ballast serve` listens ("" outside serve);
	// Doctor warns when it is open to the network.
	ListenAddr string

	// Version is the build version shown by doctor and GET /api/v1/version
	// (spec §21.5). Empty reads as "dev".
	Version string

	// InTerminal is true when `ballast serve` was started by hand in a
	// terminal rather than as a service; Doctor and Health then warn that
	// schedules stop when the terminal closes.
	InTerminal bool

	// Progress holds the latest upload progress of running backups; nil
	// means status shows none.
	Progress *progress.Tracker

	DB      *sql.DB
	Catalog *catalog.Registry
	Coord   *scheduler.Coordinator

	// Health runs and records each destination's repository health
	// checks (spec §19.7). Nil makes the repository-health calls
	// report that it is not set up.
	RepoHealth *repohealth.Checker

	// ProtonBin overrides the Proton Drive command-line tool (empty
	// uses "proton-drive"). Sign-ins in progress are kept per
	// destination in protonLogins.
	ProtonBin    string
	protonMu     sync.Mutex
	protonLogins map[string]*protondrive.Login

	// CatalogDirs are the user catalog directories; custom entries
	// created in the web UI are installed into the first one.
	CatalogDirs []string

	// Repo builds a restic.Repository for a destination, using the
	// same Runner/CredDir/ResticBin the pipeline uses. Built by wiring.
	Repo func(dest inventory.Destination) (*restic.Repository, error)

	// Runner runs `docker ps` for DetectServices and `restic version`
	// for CheckPreflight (the first-run wizard's service suggestions
	// and tool checks, spec §5.3.6). Nil disables both with a clear
	// error rather than a nil-pointer panic — they are always
	// best-effort conveniences, never required (a service can still be
	// added manually, and Init will still fail loudly on its own if a
	// tool is missing).
	Runner *runner.Runner

	// DockerHosts resolves a configured Docker host into the handle
	// every Docker operation runs through (discovery, detection,
	// verification drills, connection tests). Wiring passes the
	// pipeline's own resolver, so both reach a host identically.
	DockerHosts dockerhost.Resolver

	// Proxmox resolves the configured Proxmox connection into an API
	// client (spec §17.3). Zero value works: CredDirs/Redact are
	// filled in by wiring the same way DockerHosts is.
	Proxmox proxmox.Resolver

	// ResticBin overrides the restic binary CheckPreflight probes
	// (tests only); empty resolves "restic" against $PATH, matching
	// every other restic invocation in production.
	ResticBin string

	// CredentialSearchDirs lists every directory a credential file may
	// already be in (config.Config.CredentialDirs). ensurePassword
	// never generates a password that exists in any of them. Nil
	// checks CredentialsDir only.
	CredentialSearchDirs []string

	// CredentialsDir is where PutDestination's ensurePassword
	// writes a freshly generated repository password for a brand-new
	// local-disk destination (spec §5.3.6). Empty disables
	// auto-generation: PutDestination then behaves exactly as before,
	// requiring the credential file to already exist.
	CredentialsDir string

	// WorkRoot is the parent of each service's stable per-run work
	// directory (<WorkRoot>/<service>), the same value given to
	// pipeline.Pipeline. VerifyService uses it to tell a run's own
	// backup artifacts (dump/archive files, always written under here)
	// apart from a files/folder backup's raw source paths, which can
	// be arbitrarily large and are never re-downloaded just to
	// re-verify a snapshot. Empty disables VerifyService's declared
	// checker (no artifacts are ever matched) but not its read-back.
	WorkRoot string

	// SelfBackupFiles says which of Ballast's own files SelfBackup saves
	// (config file and user catalog directories; the status DB is
	// always included). Zero value backs up the DB only.
	SelfBackupFiles SelfBackupSources

	// SelfNotifier receives the outcome of the scheduled self-backup
	// (RunSelfBackup) like any service's run. Nil means no
	// notifications for it.
	SelfNotifier engine.Notifier

	// HoldRepository and RetainTagged, when set, make self-backup share
	// the pipeline's per-repository gate, so its prune never runs under
	// another service's upload. Wiring passes the pipeline's own; nil
	// means no gate (tests).
	HoldRepository RepoHolder
	RetainTagged   TagRetainer

	// Log receives Ops' own one-line notes (retention results). Nil
	// means os.Stderr.
	Log io.Writer

	// Redact holds secret strings that must never reach a returned
	// error's text (spec rule 2), the last line of defence before an
	// error becomes an API response body or CLI output. A repository
	// password PutDestination generates is added to it immediately.
	Redact *secret.Set

	// Ctx, when set, is the context StartBackup's background run uses
	// instead of the ctx passed to StartBackup itself. This matters
	// for `ballast serve`: an HTTP request's Context() is canceled the
	// instant its handler returns (net/http's documented behavior),
	// which for an async 202-Accepted endpoint is almost immediately —
	// long before an hours-long backup finishes. Wiring sets this to
	// the server's own long-lived, shutdown-cancelled context, so a
	// backup started by a request survives that request's completion
	// and is only canceled when the server itself shuts down. Nil (the
	// default, e.g. in tests and for any caller that isn't `serve`)
	// falls back to the ctx StartBackup was called with.
	Ctx context.Context

	// DestinationLocks, when set, extends PutDestination's per-name
	// serialization across processes (a `ballast destination add` or
	// `ballast wizard` running while `ballast serve` handles a web UI
	// request for the same new destination). Wiring uses a
	// scheduler.FileLocker in its own directory, so a destination and
	// a service with the same name never share a lock. Nil serializes
	// within this process only.
	DestinationLocks scheduler.Locker

	runs sync.WaitGroup

	// destMu guards destLocks, the lazily-built set of per-destination
	// locks PutDestination serializes on (see lockDestination). Ops is
	// constructed as a struct literal all over the codebase (wiring,
	// tests), so the zero value of both must work with no
	// initialization step.
	destMu    sync.Mutex
	destLocks map[string]*sync.Mutex
}

// lockDestination serializes every PutDestination call for
// destination name, so two concurrent calls for the *same* new
// destination (e.g. a GUI double-click, an API retry racing the
// original request, or the CLI and the web UI at once) can never
// interleave their "does it already exist" check, restic init and
// failure cleanup. Without this, two callers could both generate a
// repository password, both see the destination as new, and the
// second's cleanup could delete the row or password file the first
// one just finished setting up. It returns an unlock func to defer.
//
// Within this process an in-memory mutex does the work; across
// processes, DestinationLocks (when set) is polled until it is free
// or ctx ends — a setup in another process can take as long as a
// restic init, so this waits rather than failing straight away.
//
// In-memory locks are created lazily, one per distinct name, and
// never removed; for the small, operator-sized set of destinations
// Ballast targets that's a trivial amount of memory, not a leak worth
// guarding against.
func (o *Ops) lockDestination(ctx context.Context, name string) (func(), error) {
	o.destMu.Lock()
	l, ok := o.destLocks[name]
	if !ok {
		l = &sync.Mutex{}
		if o.destLocks == nil {
			o.destLocks = make(map[string]*sync.Mutex)
		}
		o.destLocks[name] = l
	}
	o.destMu.Unlock()
	l.Lock()
	if o.DestinationLocks == nil {
		return l.Unlock, nil
	}
	for {
		release, ok, err := o.DestinationLocks.TryLock(name)
		if err != nil {
			l.Unlock()
			return nil, fmt.Errorf("destination %s: %w", name, err)
		}
		if ok {
			return func() { release(); l.Unlock() }, nil
		}
		select {
		case <-ctx.Done():
			l.Unlock()
			return nil, fmt.Errorf("destination %s: waiting for another Ballast command to finish setting it up: %w", name, ctx.Err())
		case <-time.After(destinationLockPoll):
		}
	}
}

// destinationLockPoll is how often lockDestination retries a
// destination lock another process holds.
const destinationLockPoll = 100 * time.Millisecond

// repoFor builds a restic.Repository for dest, or a clear
// configuration error if wiring never set Repo.
func (o *Ops) repoFor(dest inventory.Destination) (*restic.Repository, error) {
	if o.Repo == nil {
		return nil, fmt.Errorf("no restic repository builder is configured (internal wiring error)")
	}
	return o.Repo(dest)
}

// redact scrubs every configured secret from s.
func (o *Ops) redact(s string) string {
	return o.Redact.Redact(s)
}

// redactErr returns err with any secret removed from its text, while
// still unwrapping to err itself so errors.Is/As keep working (the
// same approach scheduler.Coordinator's redactedError uses) — e.g. so
// h.writeErr's status mapping still recognizes a redacted
// scheduler.ErrAlreadyRunning as a 409, not a generic 500.
func (o *Ops) redactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if clean := o.redact(msg); clean != msg {
		return &redactedError{err: err, msg: clean}
	}
	return err
}

// redactedError is an error whose text has had secrets scrubbed out,
// while still unwrapping to the original error. Code that unwraps it
// must not print what it finds.
type redactedError struct {
	err error
	msg string
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// wrapf wraps err with "prefix: " unless err's text already starts
// with that exact prefix. inventory and scheduler errors already name
// their service/destination, so wrapping unconditionally would double
// it ("service x: service x: not found"); this keeps every returned
// error guaranteed to name its subject exactly once.
func wrapf(prefix string, err error) error {
	if err == nil {
		return nil
	}
	if strings.HasPrefix(err.Error(), prefix+":") {
		return err
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// --- Status ----------------------------------------------------------

// TierStatus is one backup tier's status for a service (spec rule 3:
// per-tier reporting so a secondary tier can slot in without masking
// the primary one — v0.1 has exactly one tier, "restic").
type TierStatus struct {
	Tier           string     `json:"tier"`
	Light          string     `json:"light"` // green, amber, red, running, none
	LastRunID      int64      `json:"last_run_id,omitempty"`
	LastState      string     `json:"last_state,omitempty"`
	LastStartedAt  *time.Time `json:"last_started_at,omitempty"`
	LastFinishedAt *time.Time `json:"last_finished_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	SnapshotID     string     `json:"snapshot_id,omitempty"`
	// Waiting says what a running backup is waiting for, such as
	// "waiting for repository lock on b2 (held by immich)"; empty when
	// it is not waiting or not running.
	Waiting string `json:"waiting,omitempty"`
	// Phase is the plain-English step a running backup is on (such as
	// "checking the backup can be restored"); empty when not running.
	Phase string `json:"phase,omitempty"`
	// ElapsedSeconds is how long the running backup has been going;
	// zero when not running.
	ElapsedSeconds int64 `json:"elapsed_seconds,omitempty"`
	// LastVerifiedAt is when this tier's last verified (SUCCESS, never
	// SUCCESS_UNVERIFIED) backup finished — i.e. the last restore-checked
	// copy — or nil if there never was one.
	LastVerifiedAt *time.Time `json:"last_verified_at,omitempty"`
	// VerifiedAgeSeconds is the age of LastVerifiedAt.
	VerifiedAgeSeconds *int64 `json:"verified_age_seconds,omitempty"`
	// MaxAgeSeconds is how old a verified backup may get before the
	// tier counts as stale (spec §11).
	MaxAgeSeconds int64 `json:"max_age_seconds,omitempty"`
	// Stale is true when there is no verified backup within
	// MaxAgeSeconds. A stale tier is never shown green.
	Stale bool `json:"stale"`
}

// TargetStatus is one destination target's status for a service (spec
// §19.4): its own light, so a failed copy to one destination is never
// hidden behind a good copy to another.
type TargetStatus struct {
	Destination string   `json:"destination"`
	Parts       []string `json:"parts,omitempty"`
	Light       string   `json:"light"` // green, amber, red, running, none
	LastState   string   `json:"last_state,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	SnapshotID  string   `json:"snapshot_id,omitempty"`
	// Secondary marks a report-only second copy (Proton Drive); its
	// light never changes the service's own.
	Secondary bool `json:"secondary,omitempty"`
	// SignInNeeded is true when the copy failed because the Proton
	// Drive login ended; the UI offers "Sign in again".
	SignInNeeded bool `json:"sign_in_needed,omitempty"`
	// Upload is the latest upload progress while this destination is
	// being written to; nil otherwise (and after a daemon restart, until
	// the next report).
	Upload *progress.Upload `json:"upload,omitempty"`
}

// VerificationRecord is the API/CLI-facing shape of an
// engine.VerificationRecord.
type VerificationRecord struct {
	// Target names the destination a result belongs to; empty means the
	// whole run.
	Target     string    `json:"target,omitempty"`
	Method     string    `json:"method"`
	Passed     bool      `json:"passed"`
	Warning    string    `json:"warning,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Output     string    `json:"output,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ScheduleStatus summarizes a service's schedule for the status view.
type ScheduleStatus struct {
	Cron    string `json:"cron"`
	Enabled bool   `json:"enabled"`
}

// ServiceStatus is one configured service's full status.
type ServiceStatus struct {
	Service string `json:"service"`
	// Self is true for Ballast's own settings backup, which has a light
	// like a service but is not one: it cannot be restored from the
	// service picker or edited as a service.
	Self         bool                 `json:"self,omitempty"`
	CatalogEntry string               `json:"catalog_entry"`
	Destination  string               `json:"destination"`
	Schedule     *ScheduleStatus      `json:"schedule,omitempty"`
	Tiers        []TierStatus         `json:"tiers"`
	Verification []VerificationRecord `json:"verification,omitempty"`
	// Targets is one entry per destination the service backs up to; the
	// service's own light is the worst of them.
	Targets []TargetStatus `json:"targets,omitempty"`
	// FreshnessAgeSeconds is how long ago the last SUCCESS (never
	// SUCCESS_UNVERIFIED — spec §11) finished, or nil if there has
	// never been one.
	FreshnessAgeSeconds *int64 `json:"freshness_age_seconds,omitempty"`
	// AlertDecisions is the last few alert decisions for the service
	// (sent, held back or failed), newest first. Never holds a topic,
	// URL or token.
	AlertDecisions []notify.AlertDecision `json:"alert_decisions,omitempty"`
}

// Status returns the status of the named service, or every configured
// service when service is "". withVerify adds each service's last
// run's verification records (spec §7: "shown in GUI and
// `ballast status --verify`").
func (o *Ops) Status(ctx context.Context, service string, withVerify bool) ([]ServiceStatus, error) {
	var services []inventory.Service
	if service == SelfBackupService {
		s, err := o.selfService()
		if err != nil {
			return nil, err
		}
		services = []inventory.Service{s}
	} else if service != "" {
		s, err := inventory.GetService(o.DB, service)
		if err != nil {
			return nil, wrapf("service "+service, err)
		}
		services = []inventory.Service{s}
	} else {
		var err error
		services, err = inventory.ListServices(o.DB)
		if err != nil {
			return nil, err
		}
	}

	schedules, err := scheduler.ListSchedules(o.DB)
	if err != nil {
		return nil, err
	}
	scheduleByService := make(map[string]scheduler.ScheduledJob, len(schedules))
	for _, s := range schedules {
		scheduleByService[s.Service] = s
	}

	if service == "" {
		// Ballast's own settings backup gets a light of its own once it
		// is scheduled or has run.
		_, scheduled := scheduleByService[SelfBackupService]
		last, err := engine.LastStartedRun(o.DB, SelfBackupService)
		if err != nil {
			return nil, err
		}
		if scheduled || last.RunID != 0 {
			s, err := o.selfService()
			if err != nil {
				return nil, err
			}
			services = append(services, s)
		}
	}

	fresh := notify.NewFreshnessChecker(o.DB)
	out := make([]ServiceStatus, 0, len(services))
	for _, s := range services {
		st, err := o.statusOne(s, scheduleByService[s.Name], withVerify, fresh)
		if err != nil {
			return nil, err
		}
		st.Self = s.Name == SelfBackupService
		out = append(out, st)
	}
	return out, nil
}

// statusOne builds the status of a single configured service.
func (o *Ops) statusOne(s inventory.Service, sched scheduler.ScheduledJob, withVerify bool, fresh *notify.FreshnessChecker) (ServiceStatus, error) {
	st := ServiceStatus{
		Service:      s.Name,
		CatalogEntry: s.CatalogEntry,
		Destination:  s.Destination,
	}
	if sched.Cron != "" {
		st.Schedule = &ScheduleStatus{Cron: sched.Cron, Enabled: sched.Enabled}
	}
	if ds, err := notify.ListAlertDecisions(o.DB, s.Name, 5); err != nil {
		return ServiceStatus{}, wrapf("service "+s.Name, err)
	} else {
		st.AlertDecisions = ds
	}

	last, err := engine.LastStartedRun(o.DB, s.Name)
	if err != nil {
		return ServiceStatus{}, wrapf("service "+s.Name, err)
	}
	tier := TierStatus{Tier: "restic", Light: "none"}
	if last.RunID != 0 {
		m, err := engine.LoadRun(o.DB, last.RunID)
		if err != nil {
			return ServiceStatus{}, wrapf("service "+s.Name, err)
		}
		run := m.Run()
		tier.LastRunID = run.ID
		tier.LastState = string(run.State)
		tier.LastStartedAt = &run.StartedAt
		tier.LastFinishedAt = run.FinishedAt
		tier.LastError = run.Error
		tier.SnapshotID = run.SnapshotID
		tier.Light = lightFor(run.State, last.InProgress)
		tier.Waiting = last.Waiting
		if last.InProgress {
			tier.Phase = PhaseLabel(run.State)
			if el := time.Since(run.StartedAt); el > 0 {
				tier.ElapsedSeconds = int64(el.Seconds())
			}
		}

		if withVerify {
			recs, err := engine.VerificationResults(o.DB, run.ID)
			if err != nil {
				return ServiceStatus{}, wrapf("service "+s.Name, err)
			}
			st.Verification = make([]VerificationRecord, 0, len(recs))
			for _, r := range recs {
				st.Verification = append(st.Verification, VerificationRecord{
					Method: r.Method, Passed: r.Passed, Warning: r.Warning,
					Detail: r.Detail, Output: r.Output,
					DurationMS: r.Duration.Milliseconds(), RecordedAt: r.RecordedAt, Target: r.Target,
				})
			}
		}
	}
	targets, err := o.targetStatuses(s, last, tier)
	if err != nil {
		return ServiceStatus{}, wrapf("service "+s.Name, err)
	}
	st.Targets = targets

	lastSuccess, err := fresh.LastSuccessfulRun(s.Name)
	if err != nil {
		return ServiceStatus{}, wrapf("service "+s.Name, err)
	}
	maxAge, err := fresh.MaxAgeFor(s.Name)
	if err != nil {
		return ServiceStatus{}, wrapf("service "+s.Name, err)
	}
	tier.MaxAgeSeconds = int64(maxAge.Seconds())
	if !lastSuccess.IsZero() {
		age := int64(time.Since(lastSuccess).Seconds())
		st.FreshnessAgeSeconds = &age
		tier.LastVerifiedAt = &lastSuccess
		tier.VerifiedAgeSeconds = &age
	}
	// Only a tier that has been backed up at least once can be stale
	// ("none" already says nothing exists), and a stale tier must not
	// read green: its last good copy is older than the allowed window.
	if tier.Light != "none" && tier.Light != "running" && time.Since(lastSuccess) > maxAge {
		tier.Stale = true
		if tier.Light == "green" {
			tier.Light = "amber"
		}
	}
	st.Tiers = []TierStatus{tier}
	return st, nil
}

// targetStatuses reports each configured destination's outcome in the
// service's last run. A run from before targets existed has no
// per-destination rows, so its single outcome is the primary target's.
func (o *Ops) targetStatuses(s inventory.Service, last engine.LatestRun, tier TierStatus) ([]TargetStatus, error) {
	var recorded map[string]engine.TargetResult
	if last.RunID != 0 {
		rows, err := engine.RunTargets(o.DB, last.RunID)
		if err != nil {
			return nil, err
		}
		recorded = make(map[string]engine.TargetResult, len(rows))
		for _, r := range rows {
			recorded[r.Destination] = r
		}
	}
	var out []TargetStatus
	for i, t := range s.EffectiveTargets() {
		ts := TargetStatus{Destination: t.Destination, Parts: t.Parts, Light: "none"}
		if d, err := inventory.GetDestination(o.DB, t.Destination); err == nil {
			ts.Secondary = !d.Type.IsResticRepository()
		}
		switch r, ok := recorded[t.Destination]; {
		case last.InProgress && last.RunID != 0:
			ts.Light = "running"
			if u, ok := o.Progress.Get(s.Name, t.Destination); ok {
				ts.Upload = &u
			}
		case ok:
			ts.Light = lightFor(r.State, false)
			ts.LastState, ts.SnapshotID = string(r.State), r.SnapshotID
			if r.State != engine.SUCCESS {
				ts.LastError = r.Detail
				ts.SignInNeeded = ts.Secondary && strings.Contains(r.Detail, protondrive.SignInAgainText)
			}
		case i == 0 && len(recorded) == 0 && tier.LastRunID != 0:
			ts.Light, ts.LastState, ts.LastError, ts.SnapshotID = tier.Light, tier.LastState, tier.LastError, tier.SnapshotID
		}
		out = append(out, ts)
	}
	return out, nil
}

// lightFor maps a run's state to a traffic-light color (spec §4.1,
// §5.3): green only for a verified SUCCESS, amber for
// SUCCESS_UNVERIFIED, red for anything else terminal, "running" while
// still in progress.
// PhaseLabel describes a run state in plain English for status output.
func PhaseLabel(state engine.State) string {
	switch state {
	case engine.PENDING:
		return "starting"
	case engine.PREPARING:
		return "getting ready"
	case engine.DUMPING:
		return "making the backup copy"
	case engine.UPLOADING:
		return "uploading to the destination"
	case engine.VERIFYING:
		return "checking the backup can be restored"
	default:
		return strings.ToLower(string(state))
	}
}

func lightFor(state engine.State, inProgress bool) string {
	if inProgress {
		return "running"
	}
	switch state {
	case engine.SUCCESS:
		return "green"
	case engine.SUCCESS_UNVERIFIED:
		return "amber"
	default:
		return "red"
	}
}

// --- Backups -----------------------------------------------------------

// Backup runs service synchronously via the Coordinator (CLI use:
// `ballast backup` blocks until the run finishes).
func (o *Ops) Backup(ctx context.Context, service string) error {
	return o.Coord.RunNow(ctx, service)
}

// StartBackup starts service in the background via the Coordinator
// (HTTP use: a handler must not hold a request open for an
// hours-long backup). It tracks the run so Wait can block for
// in-flight runs during shutdown. interrupted reports, synchronously,
// whether an earlier unfinished run was found and marked failed.
func (o *Ops) StartBackup(ctx context.Context, service string) (interrupted bool, err error) {
	done, interrupted, err := o.Coord.Start(o.runCtx(ctx), service)
	if err != nil {
		return false, err
	}
	o.track(done)
	return interrupted, nil
}

// StartTrigger authenticates service's trigger token and, on success,
// starts it in the background exactly like StartBackup (HTTP use: the
// trigger endpoint must not hold a request open for an hours-long
// backup, nor let the request's own cancellation kill the run — see
// Ctx). scheduler.ErrInvalidTriggerToken is returned synchronously so
// the caller can map it to 401 without waiting for anything.
func (o *Ops) StartTrigger(ctx context.Context, service, token string) (interrupted bool, err error) {
	done, interrupted, err := o.Coord.StartTrigger(o.runCtx(ctx), service, token)
	if err != nil {
		return false, err
	}
	o.track(done)
	return interrupted, nil
}

// runCtx returns Ctx when set (see its doc comment), else ctx.
func (o *Ops) runCtx(ctx context.Context) context.Context {
	if o.Ctx != nil {
		return o.Ctx
	}
	return ctx
}

// track records done as an in-flight background run so Wait can block
// for it during shutdown.
func (o *Ops) track(done <-chan error) {
	o.runs.Add(1)
	go func() {
		defer o.runs.Done()
		<-done // the Coordinator already logs a failure; nothing more to do
	}()
}

// Wait blocks until every StartBackup/StartTrigger-started run still
// in flight has finished. Call it during `ballast serve` shutdown,
// after cancelling ctx, so a run gets to clean up before the process
// exits.
func (o *Ops) Wait() { o.runs.Wait() }

// --- Snapshots and restore ---------------------------------------------

// Snapshots lists service's snapshots, newest first as restic itself
// orders them, filtered to this service's tag so one destination
// shared by several services never mixes their snapshots.
func (o *Ops) Snapshots(ctx context.Context, service string) ([]restic.Snapshot, error) {
	return o.SnapshotsFrom(ctx, service, "")
}

// SnapshotsFrom is Snapshots read from the named destination, which
// must be one of the service's configured targets ("" means the main
// destination).
func (o *Ops) SnapshotsFrom(ctx context.Context, service, from string) ([]restic.Snapshot, error) {
	svc, err := inventory.GetService(o.DB, service)
	if err != nil {
		return nil, wrapf("service "+service, err)
	}
	dest, err := o.restoreSource(svc, from)
	if err != nil {
		return nil, err
	}
	repo, err := o.repoFor(dest)
	if err != nil {
		return nil, wrapf("service "+service, err)
	}
	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{Tags: []string{"ballast-service:" + service}})
	if err != nil {
		return nil, o.redactErr(wrapf("service "+service, err))
	}
	return snaps, nil
}

// RestoreRequest is the input to Restore.
type RestoreRequest struct {
	Service  string
	From     string // destination to restore from; "" means the service's main destination
	Snapshot string // "" or "latest" means this service's newest snapshot
	Paths    []string
	To       string
}

// Restore restores a snapshot of service to a fresh directory
// (read-only-by-default, spec principle 5: never restore over live
// data). Snapshot defaults to the latest snapshot carrying this
// service's tag; an explicit ID must also carry that tag, so one
// service's restore can never reach into another's snapshots even in
// a shared destination.
func (o *Ops) Restore(ctx context.Context, req RestoreRequest) error {
	if req.To == "" {
		return fmt.Errorf("service %s: %w: restore requires a target directory (--to)", req.Service, ErrValidation)
	}
	if !filepath.IsAbs(req.To) {
		return fmt.Errorf("service %s: %w: restore target %q must be an absolute path", req.Service, ErrValidation, req.To)
	}
	if err := ensureEmptyOrAbsent(req.To); err != nil {
		return fmt.Errorf("service %s: %w: %w", req.Service, ErrValidation, err)
	}

	svc, err := inventory.GetService(o.DB, req.Service)
	if err != nil {
		return wrapf("service "+req.Service, err)
	}
	dest, err := o.restoreSource(svc, req.From)
	if err != nil {
		return err
	}
	repo, err := o.repoFor(dest)
	if err != nil {
		return wrapf("service "+req.Service, err)
	}

	tag := "ballast-service:" + req.Service
	snapID, err := resolveServiceSnapshot(ctx, repo, req.Service, tag, req.Snapshot)
	if err != nil {
		return err
	}
	if err := repo.Restore(ctx, restic.RestoreOptions{SnapshotID: snapID, TargetDir: req.To, Paths: req.Paths}); err != nil {
		return o.redactErr(wrapf("service "+req.Service, err))
	}
	return nil
}

// restoreSource picks the destination a restore or snapshot listing
// reads from: the service's main destination when from is empty,
// otherwise from, which must be one of the service's configured
// targets (so an unrelated repository is never read).
func (o *Ops) restoreSource(svc inventory.Service, from string) (inventory.Destination, error) {
	name := svc.Destination
	if from != "" {
		found := false
		var names []string
		for _, t := range svc.EffectiveTargets() {
			names = append(names, t.Destination)
			if t.Destination == from {
				found = true
			}
		}
		if !found {
			return inventory.Destination{}, fmt.Errorf("service %s: %w: %q is not one of this service's destinations (%s)", svc.Name, ErrValidation, from, strings.Join(names, ", "))
		}
		name = from
	}
	dest, err := inventory.GetDestination(o.DB, name)
	if err != nil {
		return inventory.Destination{}, wrapf("service "+svc.Name, err)
	}
	return dest, nil
}

// resolveServiceSnapshot resolves want ("", "latest", or an explicit
// ID/prefix) to a concrete snapshot ID that is guaranteed to carry
// service's tag, so a restore can never cross into another service's
// snapshots in a shared destination.
func resolveServiceSnapshot(ctx context.Context, repo *restic.Repository, service, tag, want string) (string, error) {
	if want == "" || want == "latest" {
		snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{Tags: []string{tag}, Latest: 1})
		if err != nil {
			return "", wrapf("service "+service, err)
		}
		if len(snaps) == 0 {
			return "", fmt.Errorf("service %s: %w: no snapshots found for this service", service, ErrValidation)
		}
		return snaps[0].ID, nil
	}
	snaps, err := repo.Snapshots(ctx, restic.SnapshotFilter{Tags: []string{tag}})
	if err != nil {
		return "", wrapf("service "+service, err)
	}
	for _, s := range snaps {
		if s.ID == want || s.ShortID == want || strings.HasPrefix(s.ID, want) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("service %s: %w: snapshot %q was not found among this service's own snapshots (cross-service restore is not allowed)", service, ErrValidation, want)
}

// ensureEmptyOrAbsent checks dir does not exist, or exists empty —
// restic itself will create it. A non-empty existing directory is
// refused: restoring into it could silently overwrite live data.
//
// This is a best-effort guard, not a hard guarantee: nothing locks
// dir between this check and restic actually writing to it, so a
// concurrent process could populate it in between (TOCTOU). Given
// Ballast's target (a single operator driving the CLI/API by hand,
// not an untrusted multi-tenant service), that race is not worth the
// complexity of a stronger guarantee here.
func ensureEmptyOrAbsent(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking restore target %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("restore target %s already exists and is not empty; choose an empty or new directory (Ballast never restores over existing data)", dir)
	}
	return nil
}

// --- Services ------------------------------------------------------------

// ListServices lists every configured service.
func (o *Ops) ListServices(ctx context.Context) ([]inventory.Service, error) {
	return inventory.ListServices(o.DB)
}

// ListProjects lists the distinct project names in use, for the
// full-stack restore wizard (spec §14 step 23) to offer as choices.
func (o *Ops) ListProjects(ctx context.Context) ([]string, error) {
	return inventory.ListProjects(o.DB)
}

// GetService returns one configured service.
func (o *Ops) GetService(ctx context.Context, name string) (inventory.Service, error) {
	s, err := inventory.GetService(o.DB, name)
	if err != nil {
		return inventory.Service{}, wrapf("service "+name, err)
	}
	return s, nil
}

// PutService configures a service: the catalog entry must exist, and
// Resolve must succeed against a copy of it with s's params/
// containers, so a missing required parameter fails loudly at add
// time rather than at the first scheduled run.
func (o *Ops) PutService(ctx context.Context, s inventory.Service) error {
	if s.IsGuestJob() {
		return o.putGuestJob(ctx, s)
	}
	if s.IsConfigSnapshot() {
		return o.putConfigSnapshot(ctx, s)
	}
	entry, ok := o.Catalog.Get(s.CatalogEntry)
	if !ok {
		return fmt.Errorf("service %s: %w: catalog entry %q not found", s.Name, ErrValidation, s.CatalogEntry)
	}
	docker, err := o.dockerFor(s.DockerHost)
	if err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	// Adding a service must not require its containers (or Docker) to
	// be running right now; a declared fallback is enough to validate.
	discovered, err := detect.ResolveDiscoveryOffline(ctx, o.Runner, docker, entry.Discovery, entry.ParamsWithDefaults(s.Params))
	if err != nil {
		return fmt.Errorf("service %s: %w: %w", s.Name, ErrValidation, err)
	}
	if err := entry.Resolve(catalog.TemplateContext{Params: s.Params, Services: s.Containers, Discovery: discovered}); err != nil {
		return fmt.Errorf("service %s: %w: %w", s.Name, ErrValidation, err)
	}
	if _, err := inventory.GetDestination(o.DB, s.Destination); err != nil {
		return wrapf("service "+s.Name, err)
	}
	// Folders on the service's Docker host are checked on that host
	// now, so a typo shows up when the service is added, not at 3am.
	if len(entry.Backup.HostPaths) > 0 && o.Runner != nil {
		strat, err := strategy.For(entry, strategy.Options{Runner: o.Runner, Docker: docker})
		if err != nil {
			return fmt.Errorf("service %s: %w: %w", s.Name, ErrValidation, err)
		}
		if pf, ok := strat.(strategy.Preflighter); ok {
			if err := pf.Preflight(ctx); err != nil {
				return fmt.Errorf("%w: %w", ErrValidation, o.redactErr(err))
			}
		}
	}
	if err := inventory.PutService(o.DB, s); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, wrapf("service "+s.Name, err))
	}
	return nil
}

// PreviewExcludes shows what the given skip patterns would leave out
// of service's next backup, without backing anything up. patterns are
// the draft list from the form (not yet saved); they apply on top of
// the catalog entry's own.
func (o *Ops) PreviewExcludes(ctx context.Context, name string, patterns []string) (*strategy.ExcludePreview, error) {
	if err := catalog.ValidateExcludes(patterns); err != nil {
		return nil, fmt.Errorf("service %s: %w: %w", name, ErrValidation, err)
	}
	_, entry, err := o.resolvedEntry(ctx, name)
	if err != nil {
		return nil, err
	}
	entry.AddExcludes(patterns)
	return strategy.PreviewExcludes(entry)
}

// resolvedEntry loads service name and its catalog entry with every
// placeholder filled in from the service's settings, reading nothing
// from a running container. Nothing is started or stopped.
func (o *Ops) resolvedEntry(ctx context.Context, name string) (inventory.Service, *catalog.Entry, error) {
	s, err := inventory.GetService(o.DB, name)
	if err != nil {
		return s, nil, wrapf("service "+name, err)
	}
	entry, ok := o.Catalog.Get(s.CatalogEntry)
	if !ok {
		return s, nil, fmt.Errorf("service %s: %w: catalog entry %q not found", name, ErrValidation, s.CatalogEntry)
	}
	docker, err := o.dockerFor(s.DockerHost)
	if err != nil {
		return s, nil, fmt.Errorf("service %s: %w", name, err)
	}
	discovered, err := detect.ResolveDiscoveryOffline(ctx, o.Runner, docker, entry.Discovery, entry.ParamsWithDefaults(s.Params))
	if err != nil {
		return s, nil, fmt.Errorf("service %s: %w: %w", name, ErrValidation, err)
	}
	if err := entry.Resolve(catalog.TemplateContext{Params: s.Params, Services: s.Containers, Discovery: discovered}); err != nil {
		return s, nil, fmt.Errorf("service %s: %w: %w", name, ErrValidation, err)
	}
	return s, entry, nil
}

// DeleteService removes a configured service.
func (o *Ops) DeleteService(ctx context.Context, name string) error {
	if err := inventory.DeleteService(o.DB, name); err != nil {
		return wrapf("service "+name, err)
	}
	return nil
}

// SetSecret stores value in a root-only credential file backing one
// service's {{ .Secrets.<name> }} catalog reference (spec §14 step
// 15: "a password-protected DB can be dumped without a plaintext
// secret anywhere in config or DB") and records that file's *name*
// against the service (inventory.Service.Secrets), so the next run
// resolves it automatically via internal/pipeline. The file name
// defaults to "ballast-secret.<service>.<name>" the first time a
// given secret is set, then stays fixed — matching how
// PutDestination defaults PasswordCredential — and, unlike a
// destination's generated repository password, this value is
// user-supplied and may be rotated by calling SetSecret again: the
// file is overwritten, never created with O_EXCL.
//
// value is registered with o.Redact immediately, before this method
// returns, so it can never reach a log line or error even before any
// run uses it.
func (o *Ops) SetSecret(ctx context.Context, service, name, value string) error {
	if value == "" {
		return fmt.Errorf("service %s: %w: secret %q value must not be empty", service, ErrValidation, name)
	}
	svc, err := inventory.GetService(o.DB, service)
	if err != nil {
		return wrapf("service "+service, err)
	}
	if o.CredentialsDir == "" {
		return fmt.Errorf("service %s: %w: no writable credential directory is configured", service, ErrValidation)
	}
	cred := svc.Secrets[name]
	if _, _, isEnv := inventory.ParseEnvRef(cred); isEnv || cred == "" {
		cred = "ballast-secret." + service + "." + name
	}
	// A credential file of this name already provided elsewhere (e.g.
	// an operator's LoadCredential=) always wins over anything written
	// here: internal/runner.FindCredentialDir and
	// pipeline.Pipeline.resolveSecrets both search o.CredentialsDir
	// last. Writing to it anyway would silently do nothing — the run
	// would keep using the operator's value — so this fails loudly
	// instead, the same guard PutDestination's ensurePassword
	// already applies to a generated repository password.
	for _, dir := range o.CredentialSearchDirs {
		if dir == o.CredentialsDir {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, cred)); err == nil {
			return fmt.Errorf("service %s: %w: a credential file named %q already exists in %s (likely provided by the operator); it takes precedence over anything Ballast saves itself, so edit or replace that file directly instead", service, ErrValidation, cred, dir)
		}
	}
	if err := os.MkdirAll(o.CredentialsDir, 0o700); err != nil {
		return fmt.Errorf("service %s: creating credential directory %s: %w", service, o.CredentialsDir, err)
	}
	path := filepath.Join(o.CredentialsDir, cred)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		return fmt.Errorf("service %s: saving secret %s: %w", service, name, err)
	}
	// WriteFile keeps an existing file's mode; tighten it so a
	// rotated secret never stays readable by others.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("service %s: restricting secret file %s: %w", service, name, err)
	}
	o.Redact.Add(value)

	if svc.Secrets == nil {
		svc.Secrets = map[string]string{}
	}
	if svc.Secrets[name] != cred {
		svc.Secrets[name] = cred
		if err := inventory.PutService(o.DB, svc); err != nil {
			return wrapf("service "+service, err)
		}
	}
	return nil
}

// SetEnvSecret points one of a service's {{ .Secrets.<name> }}
// references at a key in an application's .env file (spec §18.2)
// instead of storing a copy. Only the path and key are saved; the value
// is read on every run, so a password rotated in the app's .env works
// with no change here. The file and key are checked now so a typo is
// caught at setup, but the value itself is never stored or returned.
func (o *Ops) SetEnvSecret(ctx context.Context, service, name, path, key string) error {
	ref := inventory.EnvRef(path, key)
	if _, _, err := inventory.ValidateEnvRef(ref); err != nil {
		return fmt.Errorf("service %s: %w: %v", service, ErrValidation, err)
	}
	svc, err := inventory.GetService(o.DB, service)
	if err != nil {
		return wrapf("service "+service, err)
	}
	v, err := dotenv.Lookup(path, key)
	if err != nil {
		return fmt.Errorf("service %s: %w: %v", service, ErrValidation, err)
	}
	o.Redact.Add(v)
	if svc.Secrets == nil {
		svc.Secrets = map[string]string{}
	}
	svc.Secrets[name] = ref
	if err := inventory.PutService(o.DB, svc); err != nil {
		return wrapf("service "+service, err)
	}
	return nil
}

// TestEnvSource checks that the .env file at path can be read and
// returns the key names found in it — never any values — so the UI and
// CLI can offer "test this source" before anything is saved.
func (o *Ops) TestEnvSource(ctx context.Context, path string) ([]string, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: the .env file path %q must be an absolute path (for example /opt/immich/.env)", ErrValidation, path)
	}
	keys, err := dotenv.Keys(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return keys, nil
}

// EnvSourceKeyStatus is one expected .env key and whether it was found.
// It never carries a value for secret keys.
type EnvSourceKeyStatus struct {
	Key    string `json:"key"`
	Found  bool   `json:"found"`
	Secret string `json:"secret,omitempty"`
	Param  string `json:"param,omitempty"`
	// Value is set only for non-secret parameter keys (a database
	// user, a folder), so setup can show them for confirmation.
	Value string `json:"value,omitempty"`
}

// EnvSourceProposal is the confirmation screen for one-path setup.
type EnvSourceProposal struct {
	EnvFile string               `json:"env_file"`
	Keys    []EnvSourceKeyStatus `json:"keys"`
	// Params are the suggested catalog parameter values.
	Params map[string]string `json:"params"`
	// Secrets maps a secret name to its env-file reference
	// (path + key only, never a value).
	Secrets map[string]string `json:"secrets"`
	// Missing lists required keys that are not in the file.
	Missing []string `json:"missing,omitempty"`
}

// ReadEnvSource finds the app's .env inside dir using the catalog
// entry's envSource keys (spec §18.2) and proposes parameter values
// and secret references. Nothing is saved, and secret values are never
// returned: only their reference.
func (o *Ops) ReadEnvSource(ctx context.Context, catalogEntry, dir string) (EnvSourceProposal, error) {
	entry, ok := o.Catalog.Get(catalogEntry)
	if !ok {
		return EnvSourceProposal{}, fmt.Errorf("%w: catalog entry %q not found", ErrValidation, catalogEntry)
	}
	if entry.EnvSource == nil {
		return EnvSourceProposal{}, fmt.Errorf("%w: %s has no .env shortcut; fill in its settings by hand", ErrValidation, catalogEntry)
	}
	if !filepath.IsAbs(dir) {
		return EnvSourceProposal{}, fmt.Errorf("%w: the folder path %q must be an absolute path (for example /opt/immich)", ErrValidation, dir)
	}
	file := entry.EnvSource.File
	if file == "" {
		file = ".env"
	}
	envPath := filepath.Join(dir, file)
	vars, err := dotenv.ReadFile(envPath)
	if err != nil {
		return EnvSourceProposal{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	prop := EnvSourceProposal{EnvFile: envPath, Params: map[string]string{}, Secrets: map[string]string{}}
	for _, k := range entry.EnvSource.Keys {
		v, found := vars[k.Key]
		st := EnvSourceKeyStatus{Key: k.Key, Found: found, Secret: k.Secret, Param: k.Param}
		switch {
		case !found:
			if !k.Optional {
				prop.Missing = append(prop.Missing, k.Key)
			}
		case k.Secret != "":
			o.Redact.Add(v)
			prop.Secrets[k.Secret] = inventory.EnvRef(envPath, k.Key)
		default:
			// Docker resolves relative paths against the compose folder.
			if v != "" && strings.HasPrefix(v, ".") {
				v = filepath.Join(dir, v)
			}
			st.Value = v
			prop.Params[k.Param] = v
		}
		prop.Keys = append(prop.Keys, st)
	}
	return prop, nil
}

// --- Destinations --------------------------------------------------------

// ListDestinations lists every configured destination.
func (o *Ops) ListDestinations(ctx context.Context) ([]inventory.Destination, error) {
	return inventory.ListDestinations(o.DB)
}

// GetDestination returns one configured destination.
func (o *Ops) GetDestination(ctx context.Context, name string) (inventory.Destination, error) {
	d, err := inventory.GetDestination(o.DB, name)
	if err != nil {
		return inventory.Destination{}, wrapf("destination "+name, err)
	}
	return d, nil
}

// PutDestination configures a destination with no new backend
// credentials (see SetupDestination). When init is true, it also
// initializes the restic repository (`ballast destination add
// --init`, or first-time API setup).
func (o *Ops) PutDestination(ctx context.Context, d inventory.Destination, init bool) error {
	return o.SetupDestination(ctx, DestinationSetup{Destination: d, Init: init})
}

// DestinationSetup is one "save destination" request: the non-secret
// destination itself, plus the backend credentials the user typed
// into the form, if any.
type DestinationSetup struct {
	Destination inventory.Destination
	// Credentials are write-only: saved straight to the destination's
	// root-only backend credential file (spec rule 2), never stored in
	// the database and never returned. Empty keeps the existing file.
	Credentials backend.Credentials
	// Init also creates the restic repository when none exists yet,
	// which proves the credentials and password work end to end.
	Init bool
}

// SetupDestination saves a destination and, when req.Credentials is
// set, its backend credential file; with req.Init it then makes sure
// the repository exists, which is also the moment any bad key, wrong
// bucket or unreachable server fails loudly.
//
// A failed init leaves nothing half-made behind: a destination that
// did not exist before is removed again, a repository password
// generated for it is deleted — otherwise a retry would silently
// reuse a password that never opened anything — and a credential file
// written by this call is removed (or its previous contents restored).
func (o *Ops) SetupDestination(ctx context.Context, req DestinationSetup) (err error) {
	d := req.Destination
	unlock, err := o.lockDestination(ctx, d.Name)
	if err != nil {
		return err
	}
	defer unlock()

	if err := d.Normalize(); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, wrapf("destination "+d.Name, err))
	}
	_, getErr := inventory.GetDestination(o.DB, d.Name)
	if getErr != nil && !errors.Is(getErr, inventory.ErrNotFound) {
		return wrapf("destination "+d.Name, getErr)
	}
	isNew := getErr != nil

	var restoreCred func() error
	if !req.Credentials.Empty() {
		restoreCred, err = o.saveBackendCredential(d, req.Credentials)
		if err != nil {
			return o.redactErr(wrapf("destination "+d.Name, err))
		}
	} else if req.Init && d.BackendCredential != "" && !o.credentialExists(d.BackendCredential) {
		return fmt.Errorf("destination %s: %w: %s", d.Name, ErrValidation, missingCredentialHelp(d.Type))
	}
	undoCred := func() error {
		if restoreCred == nil {
			return nil
		}
		return restoreCred()
	}

	if err := inventory.PutDestination(o.DB, d); err != nil {
		if uErr := undoCred(); uErr != nil {
			err = errors.Join(err, uErr)
		}
		return o.redactErr(fmt.Errorf("%w: %w", ErrValidation, wrapf("destination "+d.Name, err)))
	}
	if !req.Init || !d.Type.IsResticRepository() {
		return nil // a Proton Drive copy has no repository to set up
	}
	got, err := inventory.GetDestination(o.DB, d.Name)
	if err != nil {
		return wrapf("destination "+d.Name, err)
	}
	generated, err := o.ensurePassword(got)
	if err != nil {
		return wrapf("destination "+d.Name, err)
	}
	// otherRepo is set when a repository that the generated password
	// never opened was already there: that password protects nothing
	// and must go, so a retry asks for the real one again.
	otherRepo := false
	defer func() {
		if err == nil {
			return
		}
		var cleanup []error
		if generated != "" {
			if !otherRepo && o.repoMayHoldData(got) {
				err = fmt.Errorf("%w (the password file %s was kept because a repository may already have been created with it)", err, generated)
			} else if rmErr := os.Remove(generated); rmErr != nil && !os.IsNotExist(rmErr) {
				cleanup = append(cleanup, fmt.Errorf("removing the unused password file %s: %w", generated, rmErr))
			}
		}
		if uErr := undoCred(); uErr != nil {
			cleanup = append(cleanup, uErr)
		}
		if isNew {
			if delErr := inventory.DeleteDestination(o.DB, d.Name); delErr != nil {
				cleanup = append(cleanup, fmt.Errorf("removing the half-created destination: %w", delErr))
			}
		}
		if len(cleanup) > 0 {
			err = errors.Join(append([]error{err}, cleanup...)...)
		}
		err = o.redactErr(err)
	}()

	repo, err := o.repoFor(got)
	if err != nil {
		return wrapf("destination "+d.Name, err)
	}
	if err := ensureRepoInitialized(ctx, repo); err != nil {
		if generated != "" && restic.IsWrongPassword(err) {
			otherRepo = true
			return o.redactErr(fmt.Errorf(
				"destination %s: there is already a backup repository at %s, but Ballast does not have its password. "+
					"Put that repository's original password in %s and try again, or choose an empty folder for a new repository: %w",
				d.Name, got.Location, generated, err))
		}
		return o.redactErr(fmt.Errorf("destination %s: setting up the backup repository: %w", d.Name, err))
	}
	return nil
}

// missingCredentialHelp explains, per backend, which credentials a
// destination still needs.
func missingCredentialHelp(t backend.Type) string {
	switch t.CredentialKind() {
	case backend.SSHCredential:
		return "an SFTP destination needs an SSH private key; paste one into the form (or use --ssh-key-file on the command line)"
	case backend.AWSCredential:
		return "this destination needs an access key ID and secret access key from your storage provider; enter them in the form (or use --access-key-id on the command line)"
	}
	return "credentials are missing"
}

// credentialExists reports whether a credential file named name is in
// any credential directory.
func (o *Ops) credentialExists(name string) bool {
	dirs := o.CredentialSearchDirs
	if len(dirs) == 0 {
		dirs = []string{o.CredentialsDir}
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// saveBackendCredential renders creds for d's backend and writes them
// to d's backend credential file (0600) in the writable credential
// directory, registering every secret with Redact first. The user
// typed these values, so replacing an existing file is expected
// (rotating a key), unlike a generated repository password. It
// returns a func that undoes the write: removing a new file, or
// restoring the previous contents.
func (o *Ops) saveBackendCredential(d inventory.Destination, creds backend.Credentials) (func() error, error) {
	data, secrets, err := backend.RenderCredentials(d.Type.CredentialKind(), creds)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	o.Redact.Add(secrets...)
	if o.CredentialsDir == "" {
		return nil, fmt.Errorf("%w: no writable credential directory is configured", ErrValidation)
	}
	// An operator-provided file of the same name (e.g. LoadCredential=)
	// always wins over one saved here, so saving would silently do
	// nothing — fail loudly instead, like SetSecret does.
	for _, dir := range o.CredentialSearchDirs {
		if dir == o.CredentialsDir {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, d.BackendCredential)); err == nil {
			return nil, fmt.Errorf("%w: a credential file named %q already exists in %s (likely provided by the operator); it takes precedence over anything Ballast saves itself, so edit or replace that file directly instead", ErrValidation, d.BackendCredential, dir)
		}
	}
	if err := os.MkdirAll(o.CredentialsDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating credential directory %s: %w", o.CredentialsDir, err)
	}
	path := filepath.Join(o.CredentialsDir, d.BackendCredential)
	prev, readErr := os.ReadFile(path)
	existed := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, fmt.Errorf("reading existing credential file %s: %w", path, readErr)
	}
	if err := writePrivateFile(path, data); err != nil {
		return nil, fmt.Errorf("saving credentials to %s: %w", path, err)
	}
	return func() error {
		if existed {
			if err := writePrivateFile(path, prev); err != nil {
				return fmt.Errorf("restoring the previous credential file %s: %w", path, err)
			}
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing the unused credential file %s: %w", path, err)
		}
		return nil
	}, nil
}

// writePrivateFile writes data to path atomically (temp file +
// rename) with 0600 permissions, so a reader never sees a half-written
// key file and an existing file's looser mode is never inherited.
func writePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ensurePassword generates and saves a repository password for a
// brand-new destination when its credential file does not
// exist yet, so neither the CLI wizard nor the GUI wizard/Destinations
// page ever asks a first-time user to invent or paste a secret for
// the common case (spec §5.3.6, spec rule 2: it is written straight
// to a root-only 0600 file, never the DB, and added to Redact at
// once). An existing file is never touched — this only fills a
// genuine gap. It returns the path of the file it wrote, or "" when
// it wrote nothing.
//
// This applies to every backend type: a beginner adding a B2 bucket
// from the web UI should no more have to invent a repository password
// than one adding a local folder (spec §17 acceptance: "install → B2
// destination → ... entirely in the UI").
func (o *Ops) ensurePassword(d inventory.Destination) (string, error) {
	if o.CredentialsDir == "" || d.PasswordCredential == "" {
		return "", nil
	}
	// A password the operator already provided in any credential
	// directory (e.g. via LoadCredential=) is used as is, never
	// shadowed by a second, generated one.
	for _, dir := range o.CredentialSearchDirs {
		if _, err := os.Lstat(filepath.Join(dir, d.PasswordCredential)); err == nil {
			return "", nil
		}
	}
	if err := os.MkdirAll(o.CredentialsDir, 0o700); err != nil {
		return "", fmt.Errorf("creating credential directory %s: %w", o.CredentialsDir, err)
	}
	path := filepath.Join(o.CredentialsDir, d.PasswordCredential)

	// Create the file atomically rather than checking os.Stat first
	// and writing second: a Stat-then-Write has a window in which
	// another writer could create the file in between, and this call
	// would then overwrite it with a different password, orphaning
	// whatever restic init the first writer already ran with the
	// original one. PutDestination's per-name lock covers Ballast's
	// own callers (DestinationLocks spans processes), but not an Ops
	// without DestinationLocks or an operator writing the file by hand
	// at that moment. O_EXCL makes "the file already exists" a creation
	// error instead of silent data loss.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return "", nil // someone else already created it; never touch it
		}
		return "", fmt.Errorf("creating repository password file %s: %w", path, err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("generating repository password: %w", err)
	}
	pw := hex.EncodeToString(raw)
	o.Redact.Add(pw)
	if _, err := f.WriteString(pw + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("saving repository password to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("saving repository password to %s: %w", path, err)
	}
	return path, nil
}

// ensureRepoInitialized creates repo only if it does not already
// exist, probed with a harmless read (Snapshots) rather than calling
// Init unconditionally: restic init on an already-initialized repo
// fails, so this is what makes it safe to add/re-add a destination
// through the GUI or CLI more than once, and safe for the GUI to
// always initialize new destinations by default (spec §10: a
// beginner should never need to know restic has a separate "init"
// step, or that skipping it silently leaves every later backup
// failing with a raw "repository does not exist" error).
//
// This relies on restic's "no repository yet" exit status, which
// only exists from restic.MinimumVersion on. Older releases report an
// empty folder as a generic failure, so when the probe fails for any
// other reason the version is checked and a too-old restic is named
// as the cause instead of the misleading raw error.
func ensureRepoInitialized(ctx context.Context, repo *restic.Repository) error {
	_, err := repo.Snapshots(ctx, restic.SnapshotFilter{Latest: 1})
	switch {
	case err == nil:
		return nil
	case restic.IsRepoMissing(err), restic.IsBucketMissing(err):
		// restic init also creates a bucket that does not exist yet.
		return repo.Init(ctx)
	case restic.IsWrongPassword(err):
		return err
	}
	if v, verr := repo.Version(ctx); verr == nil && v.TooOld() {
		return fmt.Errorf("%w (%w)", v.TooOldError(), err)
	}
	return err
}

// repoMayHoldData re-probes dest's repository, on a fresh,
// independent context, to decide whether a password PutDestination
// just generated might be the only copy of a repository's key that
// now actually exists — e.g. restic init wrote the repository's key
// file and was then killed (or ctx was canceled) before it returned
// success. ctx itself cannot be reused for this: by the time
// PutDestination's failure cleanup runs, it may already be canceled,
// which would make every probe fail and look exactly like "repository
// missing".
//
// Any outcome other than restic's own "no repository here yet" signal
// — success, wrong-password, anything else — is treated as "may hold
// data", so cleanup keeps the password file rather than deleting it
// out from under a repository that could depend on it. If repo
// cannot even be built, Init could never have run, so it's safe to
// say no and let cleanup remove the file as before.
func (o *Ops) repoMayHoldData(dest inventory.Destination) bool {
	repo, err := o.repoFor(dest)
	if err != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = repo.Snapshots(probeCtx, restic.SnapshotFilter{Latest: 1})
	return !restic.IsRepoMissing(err) && !restic.IsBucketMissing(err)
}

// DeleteDestination removes a configured destination (ErrInUse if a
// service still references it).
func (o *Ops) DeleteDestination(ctx context.Context, name string) error {
	if err := inventory.DeleteDestination(o.DB, name); err != nil {
		return wrapf("destination "+name, err)
	}
	return nil
}

// InitDestination makes sure the named destination's restic
// repository exists (`ballast destination init <name>`, or the web
// UI's "Set up repository" button), for a destination that was added
// without --init/"set up the storage now", or whose storage was later
// emptied out from under it. It is idempotent: an already-initialized
// repository is left untouched (see ensureRepoInitialized).
//
// It re-runs SetupDestination with Init on the saved destination, so
// it gets the same handling as setting up at add time: a repository
// password is generated if the destination never had one, an existing
// repository whose password Ballast lacks is reported rather than
// overwritten, and a failure leaves no unused password file behind.
func (o *Ops) InitDestination(ctx context.Context, name string) error {
	dest, err := inventory.GetDestination(o.DB, name)
	if err != nil {
		return wrapf("destination "+name, err)
	}
	return o.SetupDestination(ctx, DestinationSetup{Destination: dest, Init: true})
}

// TestDestination checks that a destination is reachable and that its
// credentials and repository password work, for the
// GUI/CLI "Test connection" action (spec §10).
func (o *Ops) TestDestination(ctx context.Context, name string) error {
	dest, err := inventory.GetDestination(o.DB, name)
	if err != nil {
		return wrapf("destination "+name, err)
	}
	if !dest.Type.IsResticRepository() {
		return o.testProton(ctx, dest)
	}
	repo, err := o.repoFor(dest)
	if err != nil {
		return wrapf("destination "+name, err)
	}
	// A read of the repository itself, not a mere ping: it needs the
	// backend credentials and the repository password to both work
	// (spec §17.1 "validates credentials, not just reachability").
	if _, err := repo.Snapshots(ctx, restic.SnapshotFilter{Latest: 1}); err != nil {
		if restic.IsRepoMissing(err) {
			return o.redactErr(fmt.Errorf("destination %s: Ballast reached %s and was let in, but there is no backup repository there yet — save the destination with \"set up the repository\" ticked (or run `ballast destination add --init`) to create it: %w", name, dest.Location, err))
		}
		return o.redactErr(fmt.Errorf("could not use destination %s: %w", name, err))
	}
	return nil
}

// --- Schedules -------------------------------------------------------

// ListSchedules lists every configured schedule.
func (o *Ops) ListSchedules(ctx context.Context) ([]scheduler.ScheduledJob, error) {
	return scheduler.ListSchedules(o.DB)
}

// GetSchedule returns the named service's configured schedule, or
// inventory.ErrNotFound wrapped if it has none.
func (o *Ops) GetSchedule(ctx context.Context, service string) (scheduler.ScheduledJob, error) {
	list, err := scheduler.ListSchedules(o.DB)
	if err != nil {
		return scheduler.ScheduledJob{}, err
	}
	for _, s := range list {
		if s.Service == service {
			return s, nil
		}
	}
	return scheduler.ScheduledJob{}, fmt.Errorf("service %s: no schedule configured: %w", service, inventory.ErrNotFound)
}

// SetSchedule sets service's cron schedule, enabling it. The
// expression is validated before it is stored (spec: invalid
// schedules must be rejected loudly, not silently skipped later).
func (o *Ops) SetSchedule(ctx context.Context, service, cronExpr string) error {
	if _, err := scheduler.ParseCron(cronExpr); err != nil {
		return fmt.Errorf("service %s: %w: %w", service, ErrValidation, err)
	}
	return scheduler.SetSchedule(o.DB, service, cronExpr)
}

// SetScheduleEnabled enables or disables service's existing schedule
// without discarding its cron expression.
func (o *Ops) SetScheduleEnabled(ctx context.Context, service string, enabled bool) error {
	if err := scheduler.SetEnabled(o.DB, service, enabled); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, err)
	}
	return nil
}

// --- Notification settings -----------------------------------------------

// GetNotificationSettings returns service's notification settings
// (spec §10 "Settings (notifications)"), defaulting to the same
// values notify.SweepFreshness itself falls back to when a service
// has never been configured. An unknown service is ErrNotFound, not a
// set of made-up defaults.
func (o *Ops) GetNotificationSettings(ctx context.Context, service string) (notify.Settings, error) {
	if _, err := inventory.GetService(o.DB, service); err != nil {
		return notify.Settings{}, wrapf("service "+service, err)
	}
	s, err := notify.GetSettings(o.DB, service)
	if err != nil {
		return notify.Settings{}, wrapf("service "+service, err)
	}
	return s, nil
}

// SetNotificationSettings updates service's notification settings.
// Only a value the caller got wrong is ErrValidation (400); an
// unknown service is ErrNotFound (404) and a storage failure stays a
// plain error (500).
func (o *Ops) SetNotificationSettings(ctx context.Context, service string, s notify.Settings) error {
	if _, err := inventory.GetService(o.DB, service); err != nil {
		return wrapf("service "+service, err)
	}
	if err := notify.SetSettings(o.DB, service, s); err != nil {
		if errors.Is(err, notify.ErrInvalidSettings) {
			return fmt.Errorf("%w: %w", ErrValidation, err)
		}
		return err
	}
	return nil
}

// --- Trigger tokens ----------------------------------------------------

// GenerateTriggerToken issues a new external-trigger token for
// service, shown to the caller exactly once (spec rule 2): neither
// this method nor anything downstream logs or returns it again.
func (o *Ops) GenerateTriggerToken(ctx context.Context, service string) (string, error) {
	return scheduler.GenerateTriggerToken(o.DB, service)
}

// Trigger authenticates and runs an externally-triggered backup (spec
// §12).
func (o *Ops) Trigger(ctx context.Context, service, token string) error {
	return o.Coord.Trigger(ctx, service, token)
}

// --- Catalog -----------------------------------------------------------

// CatalogSummary is the display-facing shape of a catalog entry (spec
// §10: "Services (add from catalog, parameters)").
type CatalogSummary struct {
	Service     string              `json:"service"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Parameters  []catalog.Parameter `json:"parameters,omitempty"`
	// EnvSource, when set, offers one-path setup from the app's .env.
	EnvSource *catalog.EnvSource `json:"env_source,omitempty"`
}

// ListCatalog returns every loaded catalog entry, sorted by service
// name (named ListCatalog, not Catalog, because Ops.Catalog is
// already the *catalog.Registry field).
func (o *Ops) ListCatalog() []CatalogSummary {
	entries := o.Catalog.List()
	out := make([]CatalogSummary, 0, len(entries))
	for _, e := range entries {
		out = append(out, CatalogSummary{
			Service:     e.Service,
			Name:        e.Display.Name,
			Description: e.Display.Description,
			Parameters:  e.Parameters,
			EnvSource:   e.EnvSource,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// DetectResult is what DetectServices found: every running container,
// and the catalog entries some of them look like.
type DetectResult struct {
	Containers  []detect.Container  `json:"containers"`
	Suggestions []detect.Suggestion `json:"suggestions"`
}

// DetectServices lists running Docker containers on the named Docker
// host ("" = the local host) and suggests which
// catalog entries match them (spec §5.3.6: "detected Docker services
// → pick one (Immich suggested)"). It never errors just because no
// containers matched — an empty Suggestions slice is a normal
// outcome — but does error if Docker itself could not be reached, so
// the wizard can say so rather than silently show nothing.
func (o *Ops) DetectServices(ctx context.Context, host string) (DetectResult, error) {
	if o.Runner == nil {
		return DetectResult{}, fmt.Errorf("service detection is not configured (internal wiring error)")
	}
	docker, err := o.dockerFor(host)
	if err != nil {
		return DetectResult{}, err
	}
	containers, err := detect.ListRunning(ctx, o.Runner, docker)
	if err != nil {
		return DetectResult{}, o.redactErr(err)
	}
	return DetectResult{Containers: containers, Suggestions: detect.Suggest(o.Catalog.List(), containers)}, nil
}

// PreflightResult reports whether the external tools Ballast wraps
// (spec rule 4: restic, Docker — never reimplemented) are actually
// usable, so the wizard can say so clearly up front instead of
// failing with a raw exec error after five questions' worth of typing
// (this is exactly the gap that let a missing restic binary and a
// missing repository password file both surface as confusing errors
// deep into the wizard flow).
//
// ResticError is set whenever restic cannot be used — not installed,
// or installed but older than restic.MinimumVersion (ResticInstalled
// is then still true, so the message can say "update" not "install").
type PreflightResult struct {
	ResticInstalled bool   `json:"restic_installed"`
	ResticVersion   string `json:"restic_version,omitempty"`
	ResticWarning   string `json:"restic_warning,omitempty"`
	ResticError     string `json:"restic_error,omitempty"`
	DockerInstalled bool   `json:"docker_installed"`
	DockerError     string `json:"docker_error,omitempty"`
}

// CheckPreflight probes restic and Docker. Docker is optional (only
// the "files"/"dump" strategies against Docker containers need it —
// the bundled "folder" entry needs no Docker at all), so a Docker
// failure is reported but never treated as fatal by callers; restic
// is required for every strategy, since it is the only way a backup
// ever leaves the machine.
func (o *Ops) CheckPreflight(ctx context.Context) PreflightResult {
	var res PreflightResult
	if o.Runner == nil {
		res.ResticError = "internal wiring error: no command runner configured"
		res.DockerError = res.ResticError
		return res
	}

	probe, err := restic.Open("ballast-preflight-check", restic.Options{Runner: o.Runner, ResticBin: o.ResticBin})
	if err != nil {
		res.ResticError = o.redact(err.Error())
	} else if v, verr := probe.Version(ctx); verr != nil {
		res.ResticError = o.redact(verr.Error())
	} else {
		res.ResticInstalled = true
		res.ResticVersion = v.Number
		if v.TooOld() {
			res.ResticError = v.TooOldError().Error()
		} else {
			res.ResticWarning = v.Warning()
		}
	}

	if docker, err := o.dockerFor(dockerhost.LocalName); err != nil {
		res.DockerError = o.redact(err.Error())
	} else if _, err := detect.ListRunning(ctx, o.Runner, docker); err != nil {
		res.DockerError = o.redact(err.Error())
	} else {
		res.DockerInstalled = true
	}
	return res
}

// ErrValidation marks an error as caused by bad caller input (spec
// §14 step 11 D3: "validation -> 400"), distinct from ErrNotFound
// (404), ErrInUse/ErrAlreadyRunning (409), and everything else (500).
var ErrValidation = errors.New("invalid input")
