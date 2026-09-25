package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	bundledcatalog "github.com/springledev/ballast/catalog"
	"github.com/springledev/ballast/internal/api"
	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/config"
	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
	"github.com/springledev/ballast/internal/pipeline"
	"github.com/springledev/ballast/internal/restic"
	execrunner "github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/scheduler"
	"github.com/springledev/ballast/internal/store"
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
	secrets  []string
	logw     io.Writer
}

// Close closes the underlying database.
func (a *app) Close() error { return a.store.Close() }

// redact scrubs every configured secret from s, the same last line of
// defence api.Ops and scheduler.Coordinator apply to their own error
// text (spec rule 2).
func (a *app) redact(s string) string {
	for _, secret := range a.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
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
	notifier, notifySecrets, err := pipeline.BuildNotifier(cfg.CredentialsDir, services)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("loading notification channels: %w", err)
	}

	apiToken, err := loadOptionalCredential(cfg.CredentialsDir, apiTokenCredential)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("loading API token: %w", err)
	}

	secrets := append([]string(nil), notifySecrets...)
	if apiToken != "" {
		secrets = append(secrets, apiToken)
	}

	r := &execrunner.Runner{Redact: secrets}
	adapter := &notify.EngineAdapter{DB: st.DB(), Notifier: notifier}

	pl := &pipeline.Pipeline{
		DB:       st.DB(),
		Catalog:  reg,
		Runner:   r,
		CredDir:  cfg.CredentialsDir,
		WorkRoot: filepath.Join(cfg.DataDir, "work"),
		Notifier: adapter,
		Log:      logw,
		Redact:   secrets,
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

	repoFor := func(dest inventory.Destination) (*restic.Repository, error) {
		return restic.Open(dest.Location, restic.Options{
			Runner:       r,
			PasswordFile: filepath.Join(cfg.CredentialsDir, dest.PasswordCredential),
		})
	}
	ops := &api.Ops{DB: st.DB(), Catalog: reg, Coord: coord, Repo: repoFor, Redact: secrets}

	a := &app{cfg: cfg, store: st, ops: ops, coord: coord, notifier: notifier, apiToken: apiToken, secrets: secrets, logw: logw}
	a.logf("using credential directory %s", cfg.CredentialsDir)
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
// (the API token, notification channels) are optional.
func loadOptionalCredential(dir, name string) (string, error) {
	v, err := notify.LoadCredentialValue(dir, name)
	if err == nil {
		return v, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return "", err
}
