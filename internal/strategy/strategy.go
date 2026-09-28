// Package strategy implements the per-service backup strategies of
// spec §4.2. A strategy turns a resolved catalog entry into backup
// artifacts (files in a per-run work directory) and/or host paths,
// which the engine then uploads (restic) and verifies in later
// phases. Strategies never upload and never verify themselves.
//
// The five strategies are:
//
//   - files: back up filesystem paths directly (nothing is produced;
//     the paths are handed to the uploader as-is).
//   - dump: run a dump command (typically `docker exec`) and capture
//     its stdout into an artifact file, e.g. pg_dumpall → database.sql.
//   - stop-start: stop containers, tar their data directories, and
//     guarantee the containers are started again — even when the run
//     fails or is cancelled mid-way (spec §4.1 cleanup obligation).
//   - exec: run a user-supplied script; exit 0 means the script left
//     its artifact(s) in the work directory. This is the reserved
//     Phase-2 contract (pymobiledevice3 gets wrapped, never imported);
//     v0.1 ships and tests it with stub scripts only.
//   - volume: archive named Docker volumes through a throwaway helper
//     container on the service's Docker host — live, or with the
//     service's containers stopped (spec §17.5).
//   - compose: snapshot a Docker Compose project's config files,
//     .env and container labels, discovered by compose labels rather
//     than a host path (spec §14 step 22).
//
// All external work goes through internal/runner (pipefail, SIGPIPE,
// redaction) — strategies contain no per-service logic (catalog is
// data, spec rule 5).
package strategy

import (
	"context"
	"fmt"
	"github.com/springledev/ballast/internal/dockerhost"
	"path/filepath"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/ballast/internal/runner"
)

// Strategy is the backup-side plugin interface of spec §4.2. One
// implementation exists per catalog backup.strategy value.
type Strategy interface {
	// Backup produces the backup for one service run. On error the
	// returned error names the service and the failing step; any
	// partial artifacts are left in Request.WorkDir for diagnostics
	// but must not be treated as uploadable.
	Backup(ctx context.Context, req Request) (*Result, error)
}

// Request carries the per-run inputs every strategy needs.
type Request struct {
	// WorkDir is a scratch directory owned by this run. Strategies
	// that produce artifacts write them here. It must be empty when
	// the run starts (the exec strategy defines every top-level entry
	// present after its script ran as an artifact). Created with mode
	// 0700 when missing: dump output counts as sensitive data.
	WorkDir string

	// Env is extra KEY=VALUE environment for spawned commands. It is
	// for NON-SECRET values only: credential-file paths resolved by
	// the caller (runner.FileEnv), feature flags, and the like. A
	// command's stderr tail can quote its environment, and stderr
	// tails ride along in errors — so a plaintext secret must never
	// be placed here; use SecretEnv. For container dumps the keys
	// are forwarded into the container via `docker exec -e KEY`
	// (values travel in the environment, never in argv).
	Env []string

	// SecretEnv is extra KEY=VALUE environment whose VALUES are
	// plaintext secrets (e.g. MYSQL_PWD for mysqldump). Values are
	// passed to spawned commands exactly like Env, and additionally
	// registered with the runner's redaction list for the duration
	// of the call, so they can never leak through a captured stderr
	// tail into an error, a log line, or the database. Keys follow
	// the same `docker exec -e KEY` forwarding as Env.
	SecretEnv []string
}

// Artifact is one file a strategy produced in the work directory.
type Artifact struct {
	// Name is the logical name from the catalog (e.g. "database").
	Name string
	// Path is the absolute path of the produced file.
	Path string
	// Format is the catalog-declared payload format (e.g. "sql",
	// "tar"); empty means unspecified.
	Format string
}

// Result describes what a strategy run produced.
type Result struct {
	// Artifacts are files created in Request.WorkDir, to be uploaded.
	Artifacts []Artifact
	// Paths are host filesystem paths to include in the upload
	// directly (files strategy; the library dir of a dump entry).
	Paths []string
	// Skipped lists paths that were deliberately not archived (e.g.
	// sockets inside a data directory) so the run log can name them.
	Skipped []string
}

// Options are the shared dependencies of the strategy
// implementations.
type Options struct {
	// Runner executes every external command. Required by dump,
	// stop-start and exec (files spawns no commands).
	Runner *runner.Runner

	// Docker runs every docker command, against the service's own
	// Docker host (spec §17.2). The zero value is the local daemon
	// with "docker" from $PATH; tests use dockerhost.NewLocal(stub).
	Docker dockerhost.Docker

	// PctBin is the Proxmox `pct` CLI a pct-exec transport dump
	// invokes; empty means "pct" resolved against $PATH. Tests point
	// it at a stub. Ballast shells out to the guest-local `pct` binary
	// directly (it assumes Ballast itself runs on the Proxmox host, or
	// in a guest with `pct` available) rather than the Proxmox API —
	// the API-mediated path is step 24's Proxmox connection (spec
	// §17.3); this is a self-contained interim implementation so the
	// transport exists and is testable ahead of that.
	PctBin string

	// VolumeHelperImage is the image the volume strategy runs tar in;
	// empty means DefaultVolumeHelperImage.
	VolumeHelperImage string
}

// pct returns the configured Proxmox pct binary name.
func (o Options) pct() string {
	if o.PctBin != "" {
		return o.PctBin
	}
	return "pct"
}

// env combines the request's non-secret and secret environment into
// one KEY=VALUE list for spawning commands.
func (r Request) env() []string {
	if len(r.SecretEnv) == 0 {
		return r.Env
	}
	return append(append([]string(nil), r.Env...), r.SecretEnv...)
}

// runnerFor returns the runner to use for one request: when the
// request carries secret environment values, a shallow copy of the
// configured runner with those values added to its redaction list.
// The shared runner is never mutated, so concurrent runs with
// different secrets stay race-free.
func (o Options) runnerFor(req Request) *runner.Runner {
	values := secretValues(req.SecretEnv)
	if len(values) == 0 {
		return o.Runner
	}
	r := *o.Runner
	r.Redact = o.Runner.Redact.With(values...)
	return &r
}

// secretValues extracts the non-empty values of KEY=VALUE entries.
// Empty values are skipped: redacting the empty string is meaningless
// and a secret that was never set hides nothing.
func secretValues(env []string) []string {
	var out []string
	for _, e := range env {
		if _, v, _ := strings.Cut(e, "="); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// For returns the Strategy implementing the entry's backup.strategy,
// validating that the entry carries everything that strategy needs.
// The entry must already be resolved (template substitution done,
// see catalog.Entry.Resolve).
func For(entry *catalog.Entry, opts Options) (Strategy, error) {
	if entry == nil {
		return nil, fmt.Errorf("catalog entry must not be nil")
	}
	switch entry.Backup.Strategy {
	case "files":
		if len(entry.Backup.Files) == 0 {
			return nil, fmt.Errorf("service %s: files strategy needs at least one backup.files entry", entry.Service)
		}
		return &Files{entry: entry}, nil
	case "dump":
		if opts.Runner == nil {
			return nil, fmt.Errorf("service %s: dump strategy needs a command runner", entry.Service)
		}
		if len(entry.Backup.Dumps) == 0 {
			return nil, fmt.Errorf("service %s: dump strategy needs at least one backup.dumps entry", entry.Service)
		}
		for _, d := range entry.Backup.Dumps {
			if err := validArtifactName(d.Name); err != nil {
				return nil, fmt.Errorf("service %s: dump name: %w", entry.Service, err)
			}
			if len(d.Command) == 0 {
				return nil, fmt.Errorf("service %s: dump %q has an empty command", entry.Service, d.Name)
			}
			if d.Format != "" && d.Format != pathBase(d.Format) {
				return nil, fmt.Errorf("service %s: dump %q format %q must not contain a path separator", entry.Service, d.Name, d.Format)
			}
			if d.Transport != "" && d.Transport != "docker-exec" && d.Transport != "pct-exec" {
				return nil, fmt.Errorf("service %s: dump %q has unknown transport %q (want docker-exec or pct-exec)", entry.Service, d.Name, d.Transport)
			}
			if d.Transport == "pct-exec" {
				if d.Container == "" {
					return nil, fmt.Errorf("service %s: dump %q: pct-exec transport needs container (the guest's VMID)", entry.Service, d.Name)
				}
				if len(d.SecretEnv) != 0 {
					return nil, fmt.Errorf("service %s: dump %q: pct-exec transport does not support secretEnv (no argv-safe way to forward it into a bare LXC guest)", entry.Service, d.Name)
				}
			}
		}
		return &Dump{entry: entry, opts: opts}, nil
	case "stop-start":
		if opts.Runner == nil {
			return nil, fmt.Errorf("service %s: stop-start strategy needs a command runner", entry.Service)
		}
		if entry.Backup.StopStart == nil || len(entry.Backup.StopStart.Containers) == 0 {
			return nil, fmt.Errorf("service %s: stop-start strategy needs backup.stopStart.containers", entry.Service)
		}
		if len(entry.Backup.Files) == 0 {
			return nil, fmt.Errorf("service %s: stop-start strategy needs at least one backup.files data directory to archive", entry.Service)
		}
		return &StopStart{entry: entry, opts: opts}, nil
	case "exec":
		if opts.Runner == nil {
			return nil, fmt.Errorf("service %s: exec strategy needs a command runner", entry.Service)
		}
		if entry.Backup.Exec == nil || entry.Backup.Exec.Script == "" {
			return nil, fmt.Errorf("service %s: exec strategy needs backup.exec.script", entry.Service)
		}
		if !filepath.IsAbs(entry.Backup.Exec.Script) {
			return nil, fmt.Errorf("service %s: exec script %q must be an absolute path, so the same file runs under systemd, cron and interactive shells", entry.Service, entry.Backup.Exec.Script)
		}
		return &Exec{entry: entry, opts: opts}, nil
	case "volume":
		if opts.Runner == nil {
			return nil, fmt.Errorf("service %s: volume strategy needs a command runner", entry.Service)
		}
		if err := validateVolumeEntry(entry); err != nil {
			return nil, err
		}
		return &Volume{entry: entry, opts: opts}, nil
	case "compose":
		if opts.Runner == nil {
			return nil, fmt.Errorf("service %s: compose strategy needs a command runner", entry.Service)
		}
		if err := validateComposeEntry(entry); err != nil {
			return nil, err
		}
		return &Compose{entry: entry, opts: opts}, nil
	default:
		return nil, fmt.Errorf("service %s: unknown backup strategy %q (want one of: files, dump, stop-start, exec, volume, compose)", entry.Service, entry.Backup.Strategy)
	}
}

// validArtifactName rejects names that could escape the work
// directory: artifact names come from catalog YAML and become file
// names, so path separators and dot-files are not allowed.
func validArtifactName(name string) error {
	if name == "" {
		return fmt.Errorf("must not be empty")
	}
	if name == "." || name == ".." || name != pathBase(name) {
		return fmt.Errorf("%q must be a plain file name, not a path", name)
	}
	return nil
}

// pathBase is filepath.Base, named for readability at call sites.
func pathBase(p string) string { return filepath.Base(p) }
