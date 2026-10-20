package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/springledev/ballast/internal/progress"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	bundledcatalog "github.com/springledev/ballast/catalog"
	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/config"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/pipeline"
	"github.com/springledev/ballast/internal/protondrive"
	"github.com/springledev/ballast/internal/proxmox"
	"github.com/springledev/ballast/internal/repohealth"
	"github.com/springledev/ballast/internal/restic"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
	execrunner "github.com/springledev/keel/runner"
	"github.com/springledev/keel/secret"
)

// apiTokenCredential is the credential file `ballast serve` reads its
// API bearer token from.
const apiTokenCredential = "ballast-api-token"

// runner is the backup pipeline every command's Coordinator runs
// through. nil (the default) means production: openApp builds the
// real pipeline.Pipeline from the resolved config. Tests swap in a
// stub before exercising a command (spec §12: the reserved run
// interfaces are tested with stubs, never real per-service logic).
//
// This is the only test seam of its kind: every command reaches the
// backup pipeline exclusively through scheduler.Coordinator
// (RunNow/Start/RunDue/Trigger), never directly, so swapping this one
// var is enough to make every command hermetic in tests.
var runner scheduler.Runner

// app bundles everything a command needs, built once by openApp so
// every command wires its dependencies identically (spec rule 5: the
// same Redact list reaches the Runner, the Pipeline, and the
// Coordinator).
type app struct {
	cfg      config.Config
	store    *store.Store
	ops      *api.Ops
	coord    *scheduler.Coordinator
	notifier *notify.Notifier
	apiToken string // "" when no ballast-api-token credential is configured
	secrets  *secret.Set
	logw     io.Writer
	health   *repohealth.Checker
}

// Close closes the underlying database.
func (a *app) Close() error { return a.store.Close() }

// redact scrubs every configured secret from s, the same last line of
// defence api.Ops and scheduler.Coordinator apply to their own error
// text (spec rule 2).
func (a *app) redact(s string) string {
	return a.secrets.Redact(s)
}

// logf writes one redacted, plain-English line to the app's log
// writer (os.Stderr when unset).
func (a *app) logf(format string, args ...any) {
	w := a.logw
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintln(w, a.redact(fmt.Sprintf("ballast: "+format, args...)))
}

// forceCredentialNotice makes openApp announce the credential
// directory; `ballast doctor` sets it.
var forceCredentialNotice bool

// openApp resolves every dependency a command needs, in the order the
// step 11 plan specifies: config (already resolved into cfg) → store
// → catalog registry → notifier (+ its secrets) → API token (+ its
// secret) → runner.Runner → pipeline.Pipeline → scheduler.Coordinator
// → api.Ops. The caller must call (*app).Close when done.
func openApp(cfg config.Config, logw io.Writer) (*app, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data directory %s: %w", cfg.DataDir, err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "ballast.db"))
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	reg, err := catalog.LoadRegistry(bundledcatalog.Bundled, cfg.CatalogDirs)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("loading catalog: %w", err)
	}

	services, err := monitoredServices(st)
	if err != nil {
		st.Close()
		return nil, err
	}
	// One shared, growable Set: anything added later (a generated
	// repository password, a notification URL read for a service added
	// after startup) is redacted everywhere at once.
	secrets := secret.NewSet()
	credDirs := cfg.CredentialDirs()
	notifier, notifyProblems := pipeline.BuildNotifier(st.DB(), credDirs, services, secrets)

	apiToken, err := loadOptionalCredential(credDirs, apiTokenCredential)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("loading API token: %w", err)
	}

	secrets.Add(apiToken)
	if err := loadRepositoryPasswords(st, credDirs, secrets); err != nil {
		st.Close()
		return nil, err
	}

	r := &execrunner.Runner{Redact: secrets}
	adapter := &notify.EngineAdapter{DB: st.DB(), Notifier: notifier}

	uploads := &progress.Tracker{}
	pl := &pipeline.Pipeline{
		Progress:   uploads,
		DB:         st.DB(),
		Catalog:    reg,
		Runner:     r,
		CredDir:    cfg.CredentialsDir,
		OwnCredDir: cfg.WritableCredentialsDir,
		WorkRoot:   filepath.Join(cfg.DataDir, "work"),

		TargetParallel: cfg.MaxParallelJobs,

		KnownHostsFile: filepath.Join(cfg.DataDir, "ssh", "known_hosts"),
		DockerStateDir: filepath.Join(cfg.DataDir, "ssh", "docker-hosts"),
		Notifier:       adapter,
		Log:            logw,
		Redact:         secrets,
	}

	realRunner := scheduler.Runner(pl)
	if runner != nil {
		// Test seam: a stub takes over the whole pipeline.
		realRunner = runner
	}

	coord := &scheduler.Coordinator{
		DB:          st.DB(),
		Locks:       scheduler.FileLocker{Dir: filepath.Join(cfg.DataDir, "locks")},
		Runner:      realRunner,
		Log:         logw,
		MaxParallel: cfg.MaxParallelJobs,
		Notifier:    adapter,
		Redact:      secrets,
	}

	resolver := pl.Resolver()
	resolver.CredDirs = credDirs
	repoFor := func(dest inventory.Destination) (*restic.Repository, error) {
		// Credential files are re-read on every use (inside Open), not
		// only at startup: a destination added (or a key file replaced)
		// while `ballast serve` runs must be redacted before restic
		// ever sees it.
		return resolver.Open(dest.Target())
	}
	health := &repohealth.Checker{
		DB:     st.DB(),
		Repo:   repoFor,
		Alerts: &repohealth.NotifyAlerter{DB: st.DB(), Notifier: notifier},
		Redact: secrets.Redact,
		Locks:  scheduler.FileLocker{Dir: filepath.Join(cfg.DataDir, "locks", "repo-checks")},
	}
	pl.RepoHealth = health
	pl.CopyAlerts = &protondrive.NotifyAlerter{DB: st.DB(), Notifier: notifier}
	dockerHosts := pl.DockerHosts()
	dockerHosts.CredDirs = credDirs
	proxmoxResolver := proxmox.Resolver{CredDirs: credDirs, Redact: secrets}
	pl.Proxmox = proxmoxResolver
	ops := &api.Ops{Progress: uploads, DB: st.DB(), Catalog: reg, CatalogDirs: cfg.CatalogDirs, Coord: coord, Repo: repoFor, RepoHealth: health, DestinationLocks: scheduler.FileLocker{Dir: filepath.Join(cfg.DataDir, "locks", "destinations")}, Runner: r, DockerHosts: dockerHosts, Proxmox: proxmoxResolver, CredentialsDir: cfg.WritableCredentialsDir, CredentialSearchDirs: credDirs, Redact: secrets, WorkRoot: pl.WorkRoot, SelfBackupFiles: api.SelfBackupSources{ConfigFile: filepath.Join(cfg.ConfigDir, "ballast.yaml"), CatalogDirs: cfg.CatalogDirs}}

	a := &app{health: health, cfg: cfg, store: st, ops: ops, coord: coord, notifier: notifier, apiToken: apiToken, secrets: secrets, logw: logw}
	// The notice is useful once, noisy every time: show it only when
	// asked (--verbose, doctor) or when the directory is not the
	// default (spec §21.2).
	nonDefault := cfg.WritableCredentialsDir != cfg.CredentialsDir
	if verbose || forceCredentialNotice || nonDefault {
		if nonDefault {
			a.logf("using credential directories %s (provided) and %s (secrets Ballast creates are saved here)", cfg.CredentialsDir, cfg.WritableCredentialsDir)
		} else {
			a.logf("using credential directory %s", cfg.CredentialsDir)
		}
	}
	for _, p := range notifyProblems {
		// Per service, never fatal (spec §3 principle 3): one broken
		// file must not stop every other service's backups. Sending for
		// that service keeps failing loudly until it is fixed.
		a.logf("warning: notifications will not work until this is fixed: %v", p)
	}
	checkResticVersion(a, r)
	return a, nil
}

// checkResticVersion runs `restic version` once and logs its Warning
// loudly (spec §4: "Version-pin check at startup; warn loudly on
// mismatch"). A failure to even run restic (e.g. not installed) is
// logged, not fatal: many commands (status, service list, ...) never
// touch restic at all. The probe repository's Location is never
// actually contacted — `restic version` ignores RESTIC_REPOSITORY.
func checkResticVersion(a *app, r *execrunner.Runner) {
	probe, err := restic.Open("ballast-version-check", restic.Options{Runner: r})
	if err != nil {
		return
	}
	// A failure here (commonly: restic just isn't installed yet) is
	// silent: it surfaces loudly and specifically the moment a command
	// actually tries to run restic. This check only adds value when
	// restic runs fine but is an untested version.
	v, err := probe.Version(context.Background())
	if err != nil {
		return
	}
	if w := v.Warning(); w != "" {
		a.logf("%s", w)
	}
}

// loadRepositoryPasswords adds every configured destination's restic
// repository password to secrets, so none of them can reach a log
// line or error message even if restic (or a wrapper) echoes one. It
// reads the files directly rather than through the strict credential
// checks: redaction must cover whatever restic itself can read, and a
// missing or unreadable file is reported loudly by the first command
// that actually uses that destination, not here.
func loadRepositoryPasswords(st *store.Store, credDirs []string, secrets *secret.Set) error {
	dests, err := inventory.ListDestinations(st.DB())
	if err != nil {
		return fmt.Errorf("listing destinations: %w", err)
	}
	for _, d := range dests {
		if d.PasswordCredential == "" {
			continue // a Proton Drive copy has no repository password
		}
		dir := execrunner.FindCredentialDir(d.PasswordCredential, credDirs...)
		secrets.AddFile(filepath.Join(dir, d.PasswordCredential))
	}
	return nil
}

// monitoredServices returns the union of every service name from
// inventory.ListServices and scheduler.ListSchedules, sorted and
// de-duplicated: BuildNotifier registers a channel for each (spec
// §14 step 11 notifiers.go doc).
func monitoredServices(st *store.Store) ([]string, error) {
	svcs, err := inventory.ListServices(st.DB())
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	scheds, err := scheduler.ListSchedules(st.DB())
	if err != nil {
		return nil, fmt.Errorf("listing schedules: %w", err)
	}
	seen := make(map[string]bool, len(svcs)+len(scheds))
	var out []string
	for _, s := range svcs {
		if !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, s.Name)
		}
	}
	for _, s := range scheds {
		if !seen[s.Service] {
			seen[s.Service] = true
			out = append(out, s.Service)
		}
	}
	sort.Strings(out)
	return out, nil
}

// loadOptionalCredential reads a root-only credential file, returning
// "" (not an error) when it simply does not exist — many credentials
// (the API token, notification channels) are optional. The first of
// dirs holding the file is used (execrunner.FindCredentialDir).
func loadOptionalCredential(dirs []string, name string) (string, error) {
	v, err := notify.LoadCredentialValue(execrunner.FindCredentialDir(name, dirs...), name)
	if err == nil {
		return v, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return "", err
}
