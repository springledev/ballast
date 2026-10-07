package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/springledev/ballast/internal/catalog"
	"github.com/springledev/keel/runner"
)

// Dump implements the dump strategy (spec §4.2): each catalog
// backup.dumps entry runs its command — inside the named container
// via `docker exec` when container is set, directly otherwise — and
// the command's stdout is captured into an artifact file in the work
// directory (e.g. pg_dumpall → database.sql). Any backup.files paths
// of the same entry (Immich's library dir, for example) are validated
// and returned for direct upload alongside the artifacts.
type Dump struct {
	entry *catalog.Entry
	opts  Options
}

// Backup runs every dump in catalog order, streaming each into its
// artifact file, then validates the entry's plain file paths.
func (s *Dump) Backup(ctx context.Context, req Request) (*Result, error) {
	if err := ensureWorkDir(req.WorkDir); err != nil {
		return nil, fmt.Errorf("service %s: %w", s.entry.Service, err)
	}

	res := &Result{}
	r := s.opts.runnerFor(req)
	env := req.env()
	for _, d := range s.entry.Backup.Dumps {
		if d.Transport == "pct-exec" && len(env) > 0 {
			// pct exec cannot forward environment variables into the
			// guest, so they would be silently dropped; fail instead.
			return nil, fmt.Errorf("service %s: dump %q: pct-exec transport cannot pass environment variables into the guest", s.entry.Service, d.Name)
		}
		dumpEnv, dumpRunner := withDumpSecrets(r, env, d.SecretEnv)
		artifact, err := s.runDump(ctx, dumpRunner, dumpEnv, req.WorkDir, d)
		if err != nil {
			return nil, err
		}
		res.Artifacts = append(res.Artifacts, artifact)
	}

	paths, err := validatePaths(s.entry.Service, s.entry.Backup.Files)
	if err != nil {
		return nil, err
	}
	res.Paths = paths
	res.PathParts = pathParts(s.entry.Backup.Files)
	return res, nil
}

// runDump executes one dump and returns its artifact. The artifact
// file is created 0600: dump output is database content and counts
// as sensitive. r carries the request's secret env values in its
// redaction list; env is the request's combined environment.
func (s *Dump) runDump(ctx context.Context, r *runner.Runner, env []string, workDir string, d catalog.Dump) (Artifact, error) {
	path := filepath.Join(workDir, artifactFileName(d))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, fmt.Errorf("service %s: dump %q: creating artifact file: %w", s.entry.Service, d.Name, err)
	}

	cmd := s.dumpCommand(d, env)
	_, runErr := r.Run(ctx, nil, f, cmd)
	closeErr := f.Close()
	if runErr != nil {
		return Artifact{}, fmt.Errorf("service %s: dump %q: %w", s.entry.Service, d.Name, runErr)
	}
	if closeErr != nil {
		return Artifact{}, fmt.Errorf("service %s: dump %q: writing artifact file: %w", s.entry.Service, d.Name, closeErr)
	}
	return Artifact{Name: d.Name, Path: path, Format: d.Format}, nil
}

// withDumpSecrets appends one dump's own secretEnv (already resolved
// to plaintext values by catalog.Entry.ResolveSecrets, keyed by env
// var name) to env, and returns a runner whose redaction list also
// covers those values. secretEnv is declared per catalog dump, unlike
// Request.SecretEnv which is shared by every dump in the entry, so
// each dump gets its own scoped copy rather than mutating r.
func withDumpSecrets(r *runner.Runner, env []string, secretEnv map[string]string) ([]string, *runner.Runner) {
	if len(secretEnv) == 0 {
		return env, r
	}
	keys := make([]string, 0, len(secretEnv))
	for k := range secretEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	extra := make([]string, 0, len(keys))
	for _, k := range keys {
		extra = append(extra, k+"="+secretEnv[k])
	}
	dumpEnv := append(append([]string(nil), env...), extra...)
	rr := *r
	rr.Redact = r.Redact.With(secretValues(extra)...)
	return dumpEnv, &rr
}

// dumpCommand builds the command for one dump. With no container, the
// command runs directly. With a container and the default docker-exec
// transport, it runs via `docker exec`: every environment key
// (non-secret and secret alike) is forwarded into the container as
// `-e KEY` so docker resolves the value from its own environment — the
// value never appears in argv. The pct-exec transport runs via
// Proxmox's `pct exec` for a bare-LXC guest with no Docker daemon
// (spec §4.2); strategy.For already rejects a pct-exec dump that
// declares SecretEnv, since pct exec has no equivalent argv-safe
// forwarding mechanism, so env here is never secret when this branch
// runs.
func (s *Dump) dumpCommand(d catalog.Dump, env []string) runner.Command {
	if d.Container == "" {
		return runner.Command{Name: d.Command[0], Args: d.Command[1:], Env: env}
	}
	if d.Transport == "pct-exec" {
		args := []string{"exec", d.Container, "--"}
		args = append(args, d.Command...)
		return runner.Command{Name: s.opts.pct(), Args: args}
	}
	args := []string{"exec"}
	if d.User != "" {
		args = append(args, "-u", d.User)
	}
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		args = append(args, "-e", k)
	}
	args = append(args, d.Container)
	args = append(args, d.Command...)
	return s.opts.Docker.Command(env, args...)
}

// artifactFileName derives the artifact's file name from the dump's
// catalog name and format: "database"+"sql" → "database.sql". The
// name was validated by For, so this cannot escape the work dir.
func artifactFileName(d catalog.Dump) string {
	if d.Format == "" {
		return d.Name
	}
	return d.Name + "." + d.Format
}

// ensureWorkDir creates the run's work directory when missing,
// restricting it to its owner: artifacts may hold database content.
func ensureWorkDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("work directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating work directory %s: %w", dir, err)
	}
	return nil
}
