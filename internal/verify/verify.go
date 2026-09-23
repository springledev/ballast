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
// This step's checkers (PLAN step 8):
//
//   - archive-integrity: every artifact is readable and structurally
//     sound — tar headers walk to EOF, gzip streams checksum to EOF,
//     zip members decompress; plain formats must at least exist and
//     be non-empty (spec §7.1, the mandatory minimum).
//   - container-restore: a database dump is fed into a throwaway
//     container of the catalog-declared image; stderr is filtered
//     whitelist-style so only catalog-declared benign patterns pass,
//     and the container is destroyed afterwards no matter what
//     (spec §7.2, including all its normative MUSTs).
//
// The remaining spec §7 methods (read-back — the restic package
// already provides the primitive; restart-smoke; exec-script — the
// Phase-2 reserved hook) are reached here in later steps; For rejects
// them with a plain-English error until implemented.
package verify

import (
	"context"
	"fmt"
	"time"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
	"github.com/springledev/ballast/internal/strategy"
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
	// implements ("archive-integrity", "container-restore").
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

	// DockerBin is the docker CLI to invoke; empty means "docker"
	// resolved against $PATH. Tests point it at a stub.
	DockerBin string
}

// docker returns the configured docker binary name.
func (o Options) docker() string {
	if o.DockerBin != "" {
		return o.DockerBin
	}
	return "docker"
}

// For returns the Checker implementing the entry's verify.method,
// validating that the entry carries everything that method needs. The
// entry must already be resolved (template substitution done, see
// catalog.Entry.Resolve) — a still-templated image would restore into
// the wrong container, which is exactly what verification exists to
// catch.
func For(entry *catalog.Entry, opts Options) (Checker, error) {
	if entry == nil {
		return nil, fmt.Errorf("catalog entry must not be nil")
	}
	switch entry.Verify.Method {
	case "archive-integrity":
		return ArchiveIntegrity{}, nil
	case "container-restore":
		if opts.Runner == nil {
			return nil, fmt.Errorf("service %s: container-restore verification needs a command runner", entry.Service)
		}
		if entry.Verify.Image == "" {
			return nil, fmt.Errorf("service %s: container-restore verification needs verify.image (the schema enforces this — was the entry built with catalog.Load?)", entry.Service)
		}
		return &ContainerRestore{
			service: entry.Service,
			image:   entry.Verify.Image,
			filter:  entry.Verify.FilterStderr,
			opts:    opts,
		}, nil
	default:
		// "read-back", "restart-smoke", "exec-script" arrive in
		// later §14 steps; fail loudly rather than silently skipping
		// verification a catalog entry asked for.
		return nil, fmt.Errorf("service %s: verification method %q is not implemented yet (implemented: archive-integrity, container-restore)", entry.Service, entry.Verify.Method)
	}
}
