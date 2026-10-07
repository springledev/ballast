// Package verify implements the verification framework of spec §7 —
// the "a backup is not done until verified" principle that is
// Ballast's whole reason to exist (spec §3.2).
//
// Every catalog entry declares one verification method (see
// catalog.Entry.Verify); For maps it to a Checker, and the engine
// runs the checker during the VERIFYING phase of the job state
// machine (spec §4.1). A run can only reach SUCCESS when its checks
// pass; if no checker can run, the run stays amber
// (SUCCESS_UNVERIFIED) — never green.
//
// Methods are pluggable (spec §17.6): a Registry maps each catalog
// method name to a Factory, and DefaultRegistry holds the built-in
// ones:
//
//   - archive-integrity: every artifact is readable and structurally
//     sound — tar headers walk to EOF, gzip streams checksum to EOF,
//     zip members decompress; plain formats must at least exist and
//     be non-empty (spec §7.1, the mandatory minimum).
//   - db-restore-drill (formerly container-restore, still accepted):
//     a database dump is fed into a throwaway container of the
//     catalog-declared image; stderr is filtered whitelist-style so
//     only catalog-declared benign patterns pass, and the container is
//     destroyed afterwards no matter what (spec §7.2).
//   - restart-smoke: the service's container is running (and healthy,
//     if it has a health check) and an optional HTTP/TCP probe from
//     inside its own network succeeds (spec §7.4).
//
// An entry may declare several methods; every one must pass. A method
// with no registered factory ("read-back" today) is
// a hard error from For — never a silent pass.
//
//   - exec-script: runs the entry's exec.verifyScript over the backup
//     files; exit 0 verifies. With no verifyScript the result is
//     unverifiable (amber).
package verify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/springledev/keel/dockerhost"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/strategy"
	"github.com/springledev/keel/runner"
)

// Result is the first-class verification record of spec §7: one
// checker's pass/fail verdict with a plain-English summary and the
// captured output the verdict was based on. Results are what the CLI,
// the REST API and the web GUI show per check
// (`ballast status --verify`).
type Result struct {
	// Method is the verification method that produced this result,
	// e.g. "archive-integrity".
	Method string

	// Passed is the verdict. Anything that is not Passed fails the
	// VERIFYING phase.
	Passed bool

	// Unverifiable marks a result that is not a failure but could not
	// prove anything (for example an exec service with no verify
	// script). The run ends amber, never green and never red.
	Unverifiable bool

	// Warning is a non-fatal notice worth showing (image drift
	// between the live container and the verify image, a command
	// that exited non-zero with only benign-filtered errors, ...).
	// Empty means no notice.
	Warning string

	// Detail is the plain-English summary of what was checked or of
	// why the check failed (audience: homelab beginners).
	Detail string

	// Output is the captured output the verdict rests on (e.g. the
	// benign-filtered stderr of a container restore). It is small:
	// checkers keep it bounded like runner's stderr tails and it has
	// already been redacted by the runner.
	Output string

	// Duration is the check's wall time.
	Duration time.Duration
}

// Checker verifies one backup. Implementations are constructed by For.
type Checker interface {
	// Method returns the catalog verification method this checker
	// implements ("archive-integrity", "db-restore-drill", ...).
	Method() string

	// Check inspects the run. It never panics and never blocks
	// forever: every loop honours ctx cancellation. A cancelled
	// context is reported as a failed result (not passed) or as the
	// context error — either way the run cannot turn green.
	Check(ctx context.Context, req Request) Result
}

// Request is the per-run input every checker consumes.
type Request struct {
	// Service is the service being verified (for error context).
	Service string

	// Artifacts are the files the backup strategy produced.
	Artifacts []strategy.Artifact

	// SourceContainer names the live container the dump came from,
	// when known. The container-restore checker compares its image
	// against the catalog's verify image and warns on drift
	// (spec §7.2 normative MUST). Empty skips the comparison.
	SourceContainer string

	// Env is extra KEY=VALUE environment for spawned commands
	// (non-secret only, same rule as strategy.Request.Env).
	Env []string
}

// Options are the shared dependencies of the checkers.
type Options struct {
	// Runner executes every external command. Required by
	// container-restore (archive-integrity is pure Go).
	Runner *runner.Runner

	// Docker runs every docker command, against the service's own
	// Docker host (spec §17.2). The zero value is the local daemon
	// with "docker" from $PATH; tests use dockerhost.NewLocal(stub).
	Docker dockerhost.Docker

	// HelperImage is the small image restart-smoke runs its HTTP/TCP
	// probe in ("" = strategy.DefaultVolumeHelperImage, busybox wget
	// and nc).
	HelperImage string
}

// Built-in verification method names.
const (
	MethodArchiveIntegrity = "archive-integrity"
	MethodDBRestoreDrill   = "db-restore-drill"
	MethodRestartSmoke     = "restart-smoke"
	MethodComposeConfig    = "compose-config"
)

// Factory builds the Checker for one method from a resolved catalog
// entry, validating that the entry carries everything the method
// needs.
type Factory func(entry *catalog.Entry, opts Options) (Checker, error)

// Registry maps catalog verification method names to their factories.
// The zero value is empty; use DefaultRegistry for the built-ins.
type Registry struct {
	factories map[string]Factory
}

// Register adds (or replaces) the factory for method.
func (r *Registry) Register(method string, f Factory) {
	if r.factories == nil {
		r.factories = make(map[string]Factory)
	}
	r.factories[method] = f
}

// Methods lists the registered method names, sorted.
func (r *Registry) Methods() []string {
	out := make([]string, 0, len(r.factories))
	for m := range r.factories {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// For returns one Checker per method the entry declares
// (catalog.Verify.MethodList), in catalog order. It fails if any
// declared method is not registered or its factory rejects the entry:
// a method that cannot run must fail the job, never be skipped.
//
// The entry must already be resolved (template substitution done, see
// catalog.Entry.Resolve) — a still-templated image would restore into
// the wrong container, which is exactly what verification exists to
// catch.
func (r *Registry) For(entry *catalog.Entry, opts Options) ([]Checker, error) {
	if entry == nil {
		return nil, fmt.Errorf("catalog entry must not be nil")
	}
	methods := entry.Verify.MethodList()
	if len(methods) == 0 {
		return nil, fmt.Errorf("service %s: the catalog entry declares no verification method", entry.Service)
	}
	checkers := make([]Checker, 0, len(methods))
	for _, m := range methods {
		f, ok := r.factories[m]
		if !ok {
			return nil, fmt.Errorf("service %s: verification method %q is not available in this version of Ballast (available: %s)", entry.Service, m, strings.Join(r.Methods(), ", "))
		}
		c, err := f(entry, opts)
		if err != nil {
			return nil, err
		}
		checkers = append(checkers, c)
	}
	return checkers, nil
}

// DefaultRegistry returns a new Registry holding the built-in methods.
func DefaultRegistry() *Registry {
	r := &Registry{}
	r.Register(MethodArchiveIntegrity, func(*catalog.Entry, Options) (Checker, error) {
		return ArchiveIntegrity{}, nil
	})
	r.Register(MethodDBRestoreDrill, newDBRestoreDrill)
	r.Register(MethodRestartSmoke, newRestartSmoke)
	r.Register(MethodComposeConfig, newComposeConfig)
	r.Register(MethodExecScript, newExecScript)
	return r
}

// For is DefaultRegistry().For.
func For(entry *catalog.Entry, opts Options) ([]Checker, error) {
	return DefaultRegistry().For(entry, opts)
}

// newDBRestoreDrill is the db-restore-drill Factory.
func newDBRestoreDrill(entry *catalog.Entry, opts Options) (Checker, error) {
	if opts.Runner == nil {
		return nil, fmt.Errorf("service %s: db-restore-drill verification needs a command runner", entry.Service)
	}
	if entry.Verify.Image == "" {
		return nil, fmt.Errorf("service %s: db-restore-drill verification needs verify.image (the schema enforces this — was the entry built with catalog.Load?)", entry.Service)
	}
	return &ContainerRestore{
		service: entry.Service,
		image:   entry.Verify.Image,
		filter:  entry.Verify.FilterStderr,
		opts:    opts,
	}, nil
}
