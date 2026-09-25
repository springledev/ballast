// Package api is the shared operations layer behind both the REST API
// and the CLI (spec §9: "All operations are also full REST API
// endpoints ... the GUI uses exactly this API [and so does the CLI] —
// no privileged GUI-only paths"). Ops methods are called in-process by
// cobra commands and by the HTTP handlers in this package; no backup
// logic lives in either caller.
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/scheduler"
)

// Ops is the shared operations layer. Every exported method wraps its
// errors with service/destination context (AGENTS.md: "always include
// the service/job context") and returns the same sentinel errors the
// underlying packages define (inventory.ErrNotFound, inventory.ErrInUse,
// scheduler.ErrAlreadyRunning, scheduler.ErrPreviousRunInterrupted),
// so callers can map them with errors.Is instead of string matching.
type Ops struct {
	DB      *sql.DB
	Catalog *catalog.Registry
	Coord   *scheduler.Coordinator

	// Repo builds a restic.Repository for a destination, using the
	// same Runner/CredDir/ResticBin the pipeline uses. Built by wiring.
	Repo func(dest inventory.Destination) (*restic.Repository, error)

	// Redact lists secret strings that must never reach a returned
	// error's text (spec rule 2), the last line of defence before an
	// error becomes an API response body or CLI output.
	Redact []string

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

	runs sync.WaitGroup
}

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
	for _, secret := range o.Redact {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
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
}

// VerificationRecord is the API/CLI-facing shape of an
// engine.VerificationRecord.
type VerificationRecord struct {
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
	Service      string               `json:"service"`
	CatalogEntry string               `json:"catalog_entry"`
	Destination  string               `json:"destination"`
	Schedule     *ScheduleStatus      `json:"schedule,omitempty"`
	Tiers        []TierStatus         `json:"tiers"`
	Verification []VerificationRecord `json:"verification,omitempty"`
	// FreshnessAgeSeconds is how long ago the last SUCCESS (never
	// SUCCESS_UNVERIFIED — spec §11) finished, or nil if there has
	// never been one.
	FreshnessAgeSeconds *int64 `json:"freshness_age_seconds,omitempty"`
}

// Status returns the status of the named service, or every configured
// service when service is "". withVerify adds each service's last
// run's verification records (spec §7: "shown in GUI and
// `ballast status --verify`").
func (o *Ops) Status(ctx context.Context, service string, withVerify bool) ([]ServiceStatus, error) {
	var services []inventory.Service
	if service != "" {
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

	fresh := notify.NewFreshnessChecker(o.DB)
	out := make([]ServiceStatus, 0, len(services))
	for _, s := range services {
		st, err := o.statusOne(s, scheduleByService[s.Name], withVerify, fresh)
		if err != nil {
			return nil, err
		}
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
					DurationMS: r.Duration.Milliseconds(), RecordedAt: r.RecordedAt,
				})
			}
		}
	}
	st.Tiers = []TierStatus{tier}

	lastSuccess, err := fresh.LastSuccessfulRun(s.Name)
	if err != nil {
		return ServiceStatus{}, wrapf("service "+s.Name, err)
	}
	if !lastSuccess.IsZero() {
		age := int64(time.Since(lastSuccess).Seconds())
		st.FreshnessAgeSeconds = &age
	}
	return st, nil
}

// lightFor maps a run's state to a traffic-light color (spec §4.1,
// §5.3): green only for a verified SUCCESS, amber for
// SUCCESS_UNVERIFIED, red for anything else terminal, "running" while
// still in progress.
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
	svc, err := inventory.GetService(o.DB, service)
	if err != nil {
		return nil, wrapf("service "+service, err)
	}
	dest, err := inventory.GetDestination(o.DB, svc.Destination)
	if err != nil {
		return nil, wrapf("service "+service, err)
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
	dest, err := inventory.GetDestination(o.DB, svc.Destination)
	if err != nil {
		return wrapf("service "+req.Service, err)
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
	entry, ok := o.Catalog.Get(s.CatalogEntry)
	if !ok {
		return fmt.Errorf("service %s: %w: catalog entry %q not found", s.Name, ErrValidation, s.CatalogEntry)
	}
	if err := entry.Resolve(catalog.TemplateContext{Params: s.Params, Services: s.Containers}); err != nil {
		return fmt.Errorf("service %s: %w: %w", s.Name, ErrValidation, err)
	}
	if _, err := inventory.GetDestination(o.DB, s.Destination); err != nil {
		return wrapf("service "+s.Name, err)
	}
	if err := inventory.PutService(o.DB, s); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, wrapf("service "+s.Name, err))
	}
	return nil
}

// DeleteService removes a configured service.
func (o *Ops) DeleteService(ctx context.Context, name string) error {
	if err := inventory.DeleteService(o.DB, name); err != nil {
		return wrapf("service "+name, err)
	}
	return nil
}

// --- Destinations --------------------------------------------------------

// ListDestinations lists every configured destination.
func (o *Ops) ListDestinations(ctx context.Context) ([]inventory.Destination, error) {
	return inventory.ListDestinations(o.DB)
}

// PutDestination configures a destination. When init is true, it also
// initializes the restic repository at d.Location (`ballast
// destination add --init`, or first-time API setup).
func (o *Ops) PutDestination(ctx context.Context, d inventory.Destination, init bool) error {
	if err := inventory.PutDestination(o.DB, d); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, wrapf("destination "+d.Name, err))
	}
	if !init {
		return nil
	}
	got, err := inventory.GetDestination(o.DB, d.Name)
	if err != nil {
		return wrapf("destination "+d.Name, err)
	}
	repo, err := o.repoFor(got)
	if err != nil {
		return wrapf("destination "+d.Name, err)
	}
	if err := repo.Init(ctx); err != nil {
		return o.redactErr(fmt.Errorf("destination %s: initializing the backup repository: %w", d.Name, err))
	}
	return nil
}

// DeleteDestination removes a configured destination (ErrInUse if a
// service still references it).
func (o *Ops) DeleteDestination(ctx context.Context, name string) error {
	if err := inventory.DeleteDestination(o.DB, name); err != nil {
		return wrapf("destination "+name, err)
	}
	return nil
}

// TestDestination checks that a destination is reachable, for the
// GUI/CLI "Test connection" action (spec §10).
func (o *Ops) TestDestination(ctx context.Context, name string) error {
	dest, err := inventory.GetDestination(o.DB, name)
	if err != nil {
		return wrapf("destination "+name, err)
	}
	repo, err := o.repoFor(dest)
	if err != nil {
		return wrapf("destination "+name, err)
	}
	if _, err := repo.Snapshots(ctx, restic.SnapshotFilter{Latest: 1}); err != nil {
		return o.redactErr(fmt.Errorf("could not reach destination %s: %w", name, err))
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
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// ErrValidation marks an error as caused by bad caller input (spec
// §14 step 11 D3: "validation -> 400"), distinct from ErrNotFound
// (404), ErrInUse/ErrAlreadyRunning (409), and everything else (500).
var ErrValidation = errors.New("invalid input")
